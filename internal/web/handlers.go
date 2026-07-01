package web

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"os"
	"time"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/logx"
	meta "github.com/leqwin/monbooru/internal/metadata"
	"github.com/leqwin/monbooru/internal/models"
)

// setFlashHeader merges a `monbooru:flash` HX-Trigger event into the
// response so a post-redirect / post-reload page can surface the
// summary via the shared #gallery-flash / #detail-flash slot. extras
// carries other triggers the handler also wants to fire (e.g. the
// delete handler's delete-go-back). kind picks the flash-ok / flash-err
// palette; pass "" for ok.
func setFlashHeader(w http.ResponseWriter, text, kind string, extras map[string]any) {
	if kind == "" {
		kind = "ok"
	}
	// The client renders this through innerHTML (showActionFlash), so the
	// text is escaped here at the single boundary, the same way
	// writeInlineFlash escapes the body path. Without it an operator-
	// supplied value spliced into the message (e.g. a folder name in the
	// move flash) would land as live markup.
	triggers := map[string]any{
		"monbooru:flash": map[string]any{"text": html.EscapeString(text), "kind": kind},
	}
	for k, v := range extras {
		triggers[k] = v
	}
	if b, err := json.Marshal(triggers); err == nil {
		w.Header().Set("HX-Trigger", string(b))
	}
}

// writeInlineFlash writes a `<div class="flash flash-{kind}">...</div>`
// fragment with text HTML-escaped, for handlers that need the flash
// payload in the response body (htmx partial swap target) rather than
// only as an HX-Trigger. kind is "ok" / "err" / "warn" (callers pass
// the bare suffix; the function adds the `flash-` prefix). text is
// taken verbatim and HTML-escaped here so every call site shares one
// escape boundary.
func writeInlineFlash(w http.ResponseWriter, kind, text string) {
	if kind == "" {
		kind = "ok"
	}
	_, _ = w.Write([]byte(`<div class="flash flash-` + kind + `">` + html.EscapeString(text) + `</div>`))
}

// writeInlineFlashHTML mirrors writeInlineFlash but takes a body that is
// already valid HTML; escaping is the caller's responsibility. Used by
// the few flashes that carry markup (e.g. links to affected rows) which
// the plain-text escaper would render as literal angle brackets.
func writeInlineFlashHTML(w http.ResponseWriter, kind, body string) {
	if kind == "" {
		kind = "ok"
	}
	_, _ = w.Write([]byte(`<div class="flash flash-` + kind + `">` + body + `</div>`))
}

// writeFlashOOB swaps a flash into a slot out-of-band so the message outlives a
// polling fragment that would otherwise overwrite the region it sat in. Empty
// text clears the slot.
func writeFlashOOB(w http.ResponseWriter, id, kind, text string) {
	body := ""
	if text != "" {
		if kind == "" {
			kind = "ok"
		}
		body = `<div class="flash flash-` + kind + `">` + html.EscapeString(text) + `</div>`
	}
	_, _ = w.Write([]byte(`<div id="` + id + `" hx-swap-oob="true">` + body + `</div>`))
}

func (s *Server) helpHandler(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "help.html", s.base(r, "help", "Help - "+s.booruName()).AsMap())
}

// notFoundHandler renders a styled 404 for any unmatched GET path. The
// mux's default behaviour is unstyled `404 page not found` text on a
// white page; routing through the standard layout keeps the user inside
// the app with a back link.
func (s *Server) notFoundHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	s.renderTemplate(w, "notfound.html", s.base(r, "", "Not found - "+s.booruName()))
}

func loadImage(ctx context.Context, database *db.DB, id int64) (*models.Image, error) {
	var img models.Image
	var isMissing, isFav, isInbox int
	var width, height, pageCount, seriesOrder *int
	var durationSec *float64
	var phash *int64
	var autoTaggedAt *string
	var ingestedAt string

	err := database.Read.QueryRowContext(ctx,
		`SELECT id, sha256, canonical_path, folder_path, file_type,
		        width, height, file_size, is_missing, is_favorited,
		        is_inbox, auto_tagged_at, source_type, origin, source, url, page_count, duration_seconds, series, series_order, phash, ingested_at
		 FROM images WHERE id = ?`, id,
	).Scan(
		&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType,
		&width, &height, &img.FileSize, &isMissing, &isFav,
		&isInbox, &autoTaggedAt, &img.SourceType, &img.Origin, &img.Source, &img.URL, &pageCount, &durationSec, &img.Series, &seriesOrder, &phash, &ingestedAt,
	)
	if err != nil {
		return nil, err
	}
	img.IsMissing = isMissing == 1
	img.IsFavorited = isFav == 1
	img.IsInbox = isInbox == 1
	img.Width = width
	img.Height = height
	img.PageCount = pageCount
	img.DurationSec = durationSec
	img.SeriesOrder = seriesOrder
	img.Phash = phash
	if autoTaggedAt != nil {
		t, _ := time.Parse(time.RFC3339, *autoTaggedAt)
		img.AutoTaggedAt = &t
	}
	img.IngestedAt, _ = time.Parse(time.RFC3339, ingestedAt)
	return &img, nil
}

func loadSDMeta(ctx context.Context, database *db.DB, id int64) *models.SDMetadata {
	var m models.SDMetadata
	var rawParams, genHash *string
	err := database.Read.QueryRowContext(ctx,
		`SELECT image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash
		 FROM sd_metadata WHERE image_id = ?`, id,
	).Scan(&m.ImageID, &m.Prompt, &m.NegativePrompt, &m.Model, &m.Seed, &m.Sampler, &m.Steps, &m.CFGScale, &rawParams, &genHash)
	if err != nil {
		return nil
	}
	if rawParams != nil {
		m.RawParams = *rawParams
	}
	if genHash != nil {
		m.GenerationHash = *genHash
	}
	if m.RawParams != "" {
		m.ParsedParams = meta.ParseAllSDParams(m.RawParams)
	}
	return &m
}

func loadComfyMeta(ctx context.Context, database *db.DB, id int64) *models.ComfyUIMetadata {
	var m models.ComfyUIMetadata
	var genHash *string
	err := database.Read.QueryRowContext(ctx,
		`SELECT image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash
		 FROM comfyui_metadata WHERE image_id = ?`, id,
	).Scan(&m.ImageID, &m.Prompt, &m.ModelCheckpoint, &m.Seed, &m.Sampler, &m.Steps, &m.CFGScale, &m.RawWorkflow, &genHash)
	if err != nil {
		return nil
	}
	if genHash != nil {
		m.GenerationHash = *genHash
	}
	return &m
}

func loadImagePaths(ctx context.Context, database *db.DB, id int64) []models.ImagePath {
	rows, err := database.Read.QueryContext(ctx,
		`SELECT id, image_id, path, is_canonical FROM image_paths WHERE image_id = ? ORDER BY is_canonical DESC, id`,
		id,
	)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var paths []models.ImagePath
	for rows.Next() {
		var p models.ImagePath
		var isCanon int
		if err := rows.Scan(&p.ID, &p.ImageID, &p.Path, &isCanon); err != nil {
			logx.Warnf("load image paths scan: %v", err)
			continue
		}
		p.IsCanonical = isCanon == 1
		// A non-canonical path whose file is gone is move/copy history, not
		// a live duplicate; keep it out of the Duplicates panel.
		if !p.IsCanonical {
			if _, statErr := os.Stat(p.Path); os.IsNotExist(statErr) {
				continue
			}
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		logx.Warnf("load image paths: %v", err)
	}
	return paths
}
