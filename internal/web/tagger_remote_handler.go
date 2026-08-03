package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tagger"
)

// remoteDrainWait is how long a drain request blocks waiting for new
// results before returning an empty page. The B-side's client timeout
// is longer than this so a full round-trip always fits.
const remoteDrainWait = 5 * time.Second

// remoteTaggerAuth verifies the Bearer token against the configured
// tokens and requires the write or tag scope. It returns the raw
// secret - used as the queue's per-peer result routing key - and
// whether the request is authorised.
func (s *Server) remoteTaggerAuth(r *http.Request) (string, bool) {
	secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if secret == "" {
		return "", false
	}
	s.cfgMu.RLock()
	tok := s.cfg.FindTokenByHash(config.HashToken(secret))
	s.cfgMu.RUnlock()
	if tok == nil || (!tok.HasScope(config.ScopeWrite) && !tok.HasScope(config.ScopeTag)) {
		return "", false
	}
	return secret, true
}

// taggerRemoteRun handles POST /api/v1/tagger/remote-run. It accepts
// exactly one multipart image from a paired remote instance, enqueues
// it on the local tagger queue, and returns immediately with 202 and
// the job id (inference runs asynchronously). When the queue is at
// capacity it returns 429 with the current watermarks so the peer can
// back off. Results are collected via GET /api/v1/tagger/remote-results.
func (s *Server) taggerRemoteRun(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	allowRemote := s.cfg.Tagger.RemoteServer.AllowRemote
	s.cfgMu.RUnlock()

	if !allowRemote {
		writeRemoteTaggerError(w, http.StatusForbidden, "remote tagging not enabled on this server")
		return
	}

	secret, ok := s.remoteTaggerAuth(r)
	if !ok {
		writeRemoteTaggerError(w, http.StatusUnauthorized, "invalid or insufficient token")
		return
	}

	if err := r.ParseMultipartForm(100 << 20); err != nil { // 100 MB max
		writeRemoteTaggerError(w, http.StatusBadRequest, "failed to parse multipart form: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()

	files := r.MultipartForm.File["images"]
	if len(files) != 1 {
		writeRemoteTaggerError(w, http.StatusBadRequest, "exactly one image required per remote submission")
		return
	}

	s.cfgMu.RLock()
	cfg := s.cfg
	taggers := tagger.EnabledTaggersForGallery(cfg, s.activeName)
	s.cfgMu.RUnlock()

	if len(taggers) == 0 {
		writeRemoteTaggerError(w, http.StatusServiceUnavailable, "no enabled taggers on this server")
		return
	}

	// Read cat IDs from the active gallery.
	cx := s.Active()
	catIDs := map[string]int64{}
	if cx != nil {
		catRows, err := cx.DB.Read.QueryContext(r.Context(), `SELECT id, name FROM tag_categories`)
		if err == nil {
			for catRows.Next() {
				var id int64
				var name string
				if scanErr := catRows.Scan(&id, &name); scanErr == nil {
					catIDs[name] = id
				}
			}
			catRows.Close()
		}
	}

	fh := files[0]
	f, err := fh.Open()
	if err != nil {
		writeRemoteTaggerError(w, http.StatusBadRequest, "failed to read uploaded image")
		return
	}
	data, readErr := io.ReadAll(f)
	f.Close()
	if readErr != nil {
		writeRemoteTaggerError(w, http.StatusBadRequest, "failed to read uploaded image")
		return
	}
	if detectMultipartImageType(data) == "" {
		logx.Warnf("remote-tagger: rejected %d-byte upload from peer: unsupported image type (only jpeg, png, webp, gif accepted)", len(data))
		writeRemoteTaggerError(w, http.StatusBadRequest, "unsupported image type (only jpeg, png, webp, gif accepted)")
		return
	}

	params := tagger.RemoteRunParams{
		Cfg:            cfg,
		Taggers:        taggers,
		CatIDs:         catIDs,
		Provider:       cfg.Tagger.ExecutionProvider,
		GeneralCatID:   catIDs["general"],
		InferredCats:   map[string]int64{},
		MinHitFraction: cfg.Tagger.Aggregation.MinHitFraction,
		Parallel:       cfg.Tagger.Parallel,
	}

	jobID, err := tagger.SubmitRemoteImage(r.Context(), params, tagger.BackendImageRequest{
		FrameBytes: [][]byte{data},
	}, secret)
	if err != nil {
		if errors.Is(err, tagger.ErrRemoteQueueFull) {
			capacity, queued, inflight := tagger.RemoteQueueStatus()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":    "remote tagger queue full",
				"capacity": capacity,
				"queued":   queued,
				"inflight": inflight,
			})
			return
		}
		logx.Errorf("remote-tagger: submit failed: %v", err)
		writeRemoteTaggerError(w, http.StatusInternalServerError, "failed to enqueue image")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
}

// taggerRemoteResults handles GET /api/v1/tagger/remote-results?after=<cursor>.
// It blocks up to remoteDrainWait for results newer than the cursor
// belonging to this token and returns them in ascending completion
// order with the new cursor. Idempotent: re-draining from the same
// cursor yields the same results, so a peer that reconnects can resume
// without loss or duplication.
func (s *Server) taggerRemoteResults(w http.ResponseWriter, r *http.Request) {
	secret, ok := s.remoteTaggerAuth(r)
	if !ok {
		writeRemoteTaggerError(w, http.StatusUnauthorized, "invalid or insufficient token")
		return
	}

	after := int64(0)
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeRemoteTaggerError(w, http.StatusBadRequest, "invalid after cursor")
			return
		}
		after = n
	}

	cursor, entries, err := tagger.RemoteDrainResults(secret, after, remoteDrainWait)
	if err != nil {
		logx.Errorf("remote-tagger: drain failed: %v", err)
		writeRemoteTaggerError(w, http.StatusInternalServerError, "drain failed")
		return
	}

	results := make([]remoteDrainResultOut, 0, len(entries))
	for _, e := range entries {
		out := remoteDrainResultOut{JobID: e.JobID}
		if e.Err != "" {
			out.Error = e.Err
		} else {
			tags := make([]remoteTagEntryOut, 0, len(e.Tags))
			for k, sc := range e.Tags {
				tags = append(tags, remoteTagEntryOut{
					Name:       k.Name,
					CategoryID: k.CatID,
					Score:      sc.Score,
					TaggerName: sc.TaggerName,
				})
			}
			out.Tags = tags
		}
		results = append(results, out)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"cursor":  cursor,
		"results": results,
	})
}

// taggerRemoteStatus handles GET /api/v1/tagger/remote-status. It
// returns the queue's capacity, queued, and in-flight image counts so
// a paired B-side can size its sliding window.
func (s *Server) taggerRemoteStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.remoteTaggerAuth(r); !ok {
		writeRemoteTaggerError(w, http.StatusUnauthorized, "invalid or insufficient token")
		return
	}
	capacity, queued, inflight := tagger.RemoteQueueStatus()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]int{
		"capacity": capacity,
		"queued":   queued,
		"inflight": inflight,
	})
}

// taggerRemoteCancel handles POST /api/v1/tagger/remote-cancel. It
// aborts the calling peer's queued and in-flight jobs - all of them
// when "all" is true, otherwise just the listed job ids - and returns
// the number of jobs cancelled. Cancelled jobs complete with the zero
// result so the peer's drain advances immediately instead of waiting
// for its own timeout.
func (s *Server) taggerRemoteCancel(w http.ResponseWriter, r *http.Request) {
	secret, ok := s.remoteTaggerAuth(r)
	if !ok {
		writeRemoteTaggerError(w, http.StatusUnauthorized, "invalid or insufficient token")
		return
	}
	var req struct {
		JobIDs []string `json:"job_ids"`
		All    bool     `json:"all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRemoteTaggerError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	cancelled, err := tagger.RemoteCancelJobs(secret, req.JobIDs, req.All)
	if err != nil {
		logx.Errorf("remote-tagger: cancel failed: %v", err)
		writeRemoteTaggerError(w, http.StatusInternalServerError, "failed to cancel jobs")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]int{"cancelled": cancelled})
}

// taggerRemoteJobs handles GET /api/v1/tagger/remote-jobs. It lists
// the calling peer's queued and in-flight jobs (oldest first) for
// debugging and cancellation workflows.
func (s *Server) taggerRemoteJobs(w http.ResponseWriter, r *http.Request) {
	secret, ok := s.remoteTaggerAuth(r)
	if !ok {
		writeRemoteTaggerError(w, http.StatusUnauthorized, "invalid or insufficient token")
		return
	}
	jobs := tagger.RemoteListJobs(secret)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"jobs": jobs})
}

type remoteDrainResultOut struct {
	JobID string              `json:"job_id"`
	Tags  []remoteTagEntryOut `json:"tags"`
	Error string              `json:"error"`
}

type remoteTagEntryOut struct {
	Name       string  `json:"name"`
	CategoryID int64   `json:"category_id"`
	Score      float32 `json:"score"`
	TaggerName string  `json:"tagger_name,omitempty"`
}

func writeRemoteTaggerError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func detectMultipartImageType(data []byte) string {
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
