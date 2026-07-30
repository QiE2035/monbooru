package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tagger"
)

// taggerRemoteRun handles POST /api/v1/tagger/remote-run.
// It accepts multipart images from a paired remote instance, runs the
// configured local taggers, and returns merged tag results as JSON.
func (s *Server) taggerRemoteRun(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	allowRemote := s.cfg.Tagger.RemoteServer.AllowRemote
	s.cfgMu.RUnlock()

	if !allowRemote {
		writeRemoteTaggerError(w, http.StatusForbidden, "remote tagging not enabled on this server")
		return
	}

	// Verify Bearer token with write or tag scope.
	secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if secret == "" {
		writeRemoteTaggerError(w, http.StatusUnauthorized, "authorization required")
		return
	}
	s.cfgMu.RLock()
	tok := s.cfg.FindTokenByHash(config.HashToken(secret))
	s.cfgMu.RUnlock()
	if tok == nil || (!tok.HasScope(config.ScopeWrite) && !tok.HasScope(config.ScopeTag)) {
		writeRemoteTaggerError(w, http.StatusForbidden, "invalid or insufficient token")
		return
	}

	if err := r.ParseMultipartForm(100 << 20); err != nil { // 100 MB max
		writeRemoteTaggerError(w, http.StatusBadRequest, "failed to parse multipart form: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()

	files := r.MultipartForm.File["images"]
	if len(files) == 0 {
		writeRemoteTaggerError(w, http.StatusBadRequest, "no images provided")
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

	images := make([]tagger.BackendImageRequest, 0, len(files))
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			continue
		}
		ct := detectMultipartImageType(data)
		if ct == "" {
			continue
		}
		images = append(images, tagger.BackendImageRequest{
			FrameBytes: [][]byte{data},
		})
	}

	if len(images) == 0 {
		writeRemoteTaggerError(w, http.StatusBadRequest, "no supported image files found (only jpeg, png, webp, gif accepted)")
		return
	}

	resp, err := tagger.RunRemoteImages(r.Context(), cfg, taggers, catIDs, images)
	if err != nil {
		logx.Errorf("remote-tagger: RunRemoteImages failed: %v", err)
		writeRemoteTaggerError(w, http.StatusInternalServerError, "inference failed")
		return
	}

	results := make([]remoteTagResultOut, 0, len(resp.Results))
	for i, r := range resp.Results {
		out := remoteTagResultOut{Index: i}
		if r.Err != "" {
			out.Error = r.Err
		} else {
			entries := make([]remoteTagEntryOut, 0, len(r.Tags))
			for k, s := range r.Tags {
				entries = append(entries, remoteTagEntryOut{
					Name:       k.Name,
					CategoryID: k.CatID,
					Score:      s.Score,
					TaggerName: s.TaggerName,
				})
			}
			out.Tags = entries
		}
		results = append(results, out)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(results)
}

type remoteTagResultOut struct {
	Index int                 `json:"index"`
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
