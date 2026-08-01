package tagger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

type remoteBackend struct {
	url    string
	token  string
	client *http.Client
}

func newRemoteBackend(url, token string) *remoteBackend {
	return &remoteBackend{
		url:    strings.TrimRight(url, "/"),
		token:  token,
		client: &http.Client{Timeout: 5 * time.Minute},
	}
}

type remoteTagResult struct {
	Index int              `json:"index"`
	Tags  []remoteTagEntry `json:"tags"`
	Error string           `json:"error"`
}

type remoteTagEntry struct {
	Name       string  `json:"name"`
	CategoryID int64   `json:"category_id"`
	Score      float32 `json:"score"`
	TaggerName string  `json:"tagger_name,omitempty"`
}

func (b *remoteBackend) Run(ctx context.Context, req RunRequest) (RunResponse, error) {
	var buf bytes.Buffer
	mp := multipart.NewWriter(&buf)

	// Pre-fill one result per requested image so images that never make
	// it into the multipart body still advance the orchestrator's
	// done/skipped accounting (progress reaches 100%).
	out := RunResponse{Results: make([]BackendImageResult, len(req.Images))}
	for i, im := range req.Images {
		out.Results[i] = BackendImageResult{ID: im.ID, Err: "image not sent to remote tagger"}
	}

	// sent records the req.Images positions that actually got written to
	// the body. The server indexes its results by this filtered order, so
	// a result's Index maps back through sent to the original request.
	var sent []int

	for i, im := range req.Images {
		var src io.Reader
		var f *os.File
		contentType := ""
		switch {
		case len(im.FrameBytes) > 0 && len(im.FrameBytes[0]) > 0:
			src = bytes.NewReader(im.FrameBytes[0])
			contentType = detectImageContentType(im.FrameBytes[0])
		case len(im.FramePaths) > 0:
			var err error
			f, err = os.Open(im.FramePaths[0])
			if err != nil {
				continue
			}
			// Sniff the header before committing to the request so a
			// corrupt file is rejected without ever being read whole.
			head := make([]byte, 12)
			n, _ := io.ReadFull(f, head)
			contentType = detectImageContentType(head[:n])
			src = io.MultiReader(bytes.NewReader(head[:n]), f)
		default:
			continue
		}
		if contentType == "" {
			if f != nil {
				f.Close()
			}
			continue
		}
		part, err := mp.CreateFormFile("images", fmt.Sprintf("%d%s", im.ID, extForContentType(contentType)))
		if err != nil {
			if f != nil {
				f.Close()
			}
			return RunResponse{}, fmt.Errorf("create form file: %w", err)
		}
		if _, err := io.Copy(part, src); err != nil {
			if f != nil {
				f.Close()
			}
			return RunResponse{}, fmt.Errorf("write form file: %w", err)
		}
		if f != nil {
			f.Close()
		}
		sent = append(sent, i)
	}

	if err := mp.Close(); err != nil {
		return RunResponse{}, fmt.Errorf("close multipart: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", b.url+"/api/v1/tagger/remote-run", &buf)
	if err != nil {
		return RunResponse{}, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", mp.FormDataContentType())
	if b.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.token)
	}

	httpResp, err := b.client.Do(httpReq)
	if err != nil {
		return RunResponse{}, fmt.Errorf("remote tagger request: %w", err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return RunResponse{}, fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return RunResponse{}, fmt.Errorf("remote tagger returned %d: %s", httpResp.StatusCode, string(body))
	}

	var results []remoteTagResult
	if err := json.Unmarshal(body, &results); err != nil {
		return RunResponse{}, fmt.Errorf("decode response: %w", err)
	}

	// Overlay the server results onto the pre-filled entries. Resulted
	// tracks which sent images got a server entry so the rest can be
	// counted as skipped instead of silently succeeding with no tags.
	resulted := make([]bool, len(req.Images))
	for _, r := range results {
		if r.Index < 0 || r.Index >= len(sent) {
			continue
		}
		br := &out.Results[sent[r.Index]]
		if r.Error != "" {
			br.Err = r.Error
		} else {
			br.Err = ""
			br.Tags = make(map[TagKey]Scored, len(r.Tags))
			for _, e := range r.Tags {
				br.Tags[TagKey{Name: e.Name, CatID: e.CategoryID}] = Scored{Score: e.Score, TaggerName: e.TaggerName}
			}
		}
		resulted[sent[r.Index]] = true
	}
	for _, idx := range sent {
		if !resulted[idx] {
			out.Results[idx].Err = "no result from remote tagger"
		}
	}
	return out, nil
}

func (b *remoteBackend) Status() CacheStatus {
	return CacheStatus{}
}

func (b *remoteBackend) ReleaseIdle(after time.Duration) bool {
	return false
}

func (b *remoteBackend) ReleaseAll() {}

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
