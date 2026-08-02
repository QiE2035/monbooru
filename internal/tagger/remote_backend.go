package tagger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// remoteClientTimeout bounds every outbound call. Submissions get a
// fast 202, status is instant, and a drain holds server-side for ~5s;
// 15s leaves comfortable headroom for the network hop on top of the
// longest of those.
const remoteClientTimeout = 15 * time.Second

type remoteBackend struct {
	url    string
	token  string
	client *http.Client
}

func newRemoteBackend(url, token string) *remoteBackend {
	return &remoteBackend{
		url:    strings.TrimRight(url, "/"),
		token:  token,
		client: &http.Client{Timeout: remoteClientTimeout},
	}
}

// remoteQueueFullError is returned by Submit when the A-side queue is
// at capacity. It carries the A-side watermarks so the B-side can size
// its sliding window and back off until a drain frees slots.
type remoteQueueFullError struct {
	capacity int
	queued   int
	inflight int
}

func (e *remoteQueueFullError) Error() string {
	return fmt.Sprintf("remote tagger queue full (capacity=%d queued=%d inflight=%d)", e.capacity, e.queued, e.inflight)
}

// Is lets errors.Is(err, errRemoteQueueFull) match any queue-full error
// regardless of the concrete watermarks it carries.
func (e *remoteQueueFullError) Is(target error) bool {
	_, ok := target.(*remoteQueueFullError)
	return ok
}

// errRemoteQueueFull is the sentinel the sliding-window loop matches
// against with errors.Is.
var errRemoteQueueFull = &remoteQueueFullError{}

// remoteDrainedResult is one A-side result entry returned by Drain,
// mapped back to the B-side image by JobID.
type remoteDrainedResult struct {
	JobID string
	Tags  map[TagKey]Scored
	Err   string
}

// Submit uploads a single image and enqueues it on the A-side. It
// returns the job id; when the A-side queue is full it returns a
// *remoteQueueFullError so the caller can drain before refilling.
func (b *remoteBackend) Submit(ctx context.Context, image BackendImageRequest) (string, error) {
	var buf bytes.Buffer
	mp := multipart.NewWriter(&buf)

	var src io.Reader
	var f *os.File
	contentType := ""
	switch {
	case len(image.FrameBytes) > 0 && len(image.FrameBytes[0]) > 0:
		src = bytes.NewReader(image.FrameBytes[0])
		contentType = detectImageContentType(image.FrameBytes[0])
	case len(image.FramePaths) > 0:
		var err error
		f, err = os.Open(image.FramePaths[0])
		if err != nil {
			return "", fmt.Errorf("open image: %w", err)
		}
		// Sniff the header before committing to the request so a
		// corrupt file is rejected without ever being read whole.
		head := make([]byte, 12)
		n, _ := io.ReadFull(f, head)
		contentType = detectImageContentType(head[:n])
		src = io.MultiReader(bytes.NewReader(head[:n]), f)
	default:
		return "", errors.New("remote image has no frame data")
	}
	if contentType == "" {
		if f != nil {
			f.Close()
		}
		return "", errors.New("unsupported image type")
	}
	part, err := mp.CreateFormFile("images", fmt.Sprintf("%d%s", image.ID, extForContentType(contentType)))
	if err != nil {
		if f != nil {
			f.Close()
		}
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(part, src); err != nil {
		if f != nil {
			f.Close()
		}
		return "", fmt.Errorf("write form file: %w", err)
	}
	if f != nil {
		f.Close()
	}
	if err := mp.Close(); err != nil {
		return "", fmt.Errorf("close multipart: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", b.url+"/api/v1/tagger/remote-run", &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", mp.FormDataContentType())
	if b.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.token)
	}

	httpResp, err := b.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("remote tagger submit: %w", err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	switch httpResp.StatusCode {
	case http.StatusAccepted:
		var out struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return "", fmt.Errorf("decode submit response: %w", err)
		}
		if out.JobID == "" {
			return "", errors.New("remote tagger submit returned an empty job id")
		}
		return out.JobID, nil
	case http.StatusTooManyRequests:
		var out struct {
			Capacity int `json:"capacity"`
			Queued   int `json:"queued"`
			Inflight int `json:"inflight"`
		}
		_ = json.Unmarshal(body, &out)
		return "", &remoteQueueFullError{capacity: out.Capacity, queued: out.Queued, inflight: out.Inflight}
	default:
		return "", fmt.Errorf("remote tagger submit returned %d: %s", httpResp.StatusCode, string(body))
	}
}

// Status queries the A-side queue watermarks.
func (b *remoteBackend) Status(ctx context.Context) (int, int, int, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", b.url+"/api/v1/tagger/remote-status", nil)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("create request: %w", err)
	}
	if b.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.token)
	}
	httpResp, err := b.client.Do(httpReq)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("remote tagger status: %w", err)
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return 0, 0, 0, fmt.Errorf("remote tagger status returned %d: %s", httpResp.StatusCode, string(body))
	}
	var out struct {
		Capacity int `json:"capacity"`
		Queued   int `json:"queued"`
		Inflight int `json:"inflight"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, 0, 0, fmt.Errorf("decode status response: %w", err)
	}
	return out.Capacity, out.Queued, out.Inflight, nil
}

// Drain fetches results newer than the cursor. The A-side holds the
// request up to `wait` for new results; the request context is given a
// little slack on top so a full round-trip always fits within the
// client timeout. Returns the new cursor and the results (empty when
// the wait elapsed with nothing new).
func (b *remoteBackend) Drain(ctx context.Context, after int64, wait time.Duration) (int64, []remoteDrainedResult, error) {
	reqCtx, cancel := context.WithTimeout(ctx, wait+3*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, "GET", b.url+"/api/v1/tagger/remote-results?after="+strconv.FormatInt(after, 10), nil)
	if err != nil {
		return after, nil, fmt.Errorf("create request: %w", err)
	}
	if b.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.token)
	}
	httpResp, err := b.client.Do(httpReq)
	if err != nil {
		return after, nil, fmt.Errorf("remote tagger drain: %w", err)
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return after, nil, fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return after, nil, fmt.Errorf("remote tagger drain returned %d: %s", httpResp.StatusCode, string(body))
	}
	var out struct {
		Cursor  int64 `json:"cursor"`
		Results []struct {
			JobID string `json:"job_id"`
			Tags  []struct {
				Name       string  `json:"name"`
				CategoryID int64   `json:"category_id"`
				Score      float32 `json:"score"`
				TaggerName string  `json:"tagger_name,omitempty"`
			} `json:"tags"`
			Error string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return after, nil, fmt.Errorf("decode drain response: %w", err)
	}
	results := make([]remoteDrainedResult, 0, len(out.Results))
	for _, r := range out.Results {
		d := remoteDrainedResult{JobID: r.JobID, Err: r.Error}
		if r.Error == "" {
			d.Tags = make(map[TagKey]Scored, len(r.Tags))
			for _, e := range r.Tags {
				d.Tags[TagKey{Name: e.Name, CatID: e.CategoryID}] = Scored{Score: e.Score, TaggerName: e.TaggerName}
			}
		}
		results = append(results, d)
	}
	return out.Cursor, results, nil
}

func detectImageContentType(data []byte) string {
	if len(data) < 12 {
		return ""
	}
	switch {
	case len(data) > 2 && data[0] == 0xFF && data[1] == 0xD8:
		return "image/jpeg"
	case len(data) > 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) > 4 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case len(data) > 1 && data[0] == 0x47 && data[1] == 0x49:
		return "image/gif"
	}
	return ""
}

func extForContentType(ct string) string {
	switch ct {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	return ".bin"
}
