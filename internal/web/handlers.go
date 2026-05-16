package web

import (
	"context"
	"net/http"
	"time"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/logx"
	meta "github.com/leqwin/monbooru/internal/metadata"
	"github.com/leqwin/monbooru/internal/models"
)

func (s *Server) helpHandler(w http.ResponseWriter, r *http.Request) {
	base := s.base(r, "help", "Help - Monbooru")
	data := map[string]any{
		"Title":         base.Title,
		"ActiveNav":     base.ActiveNav,
		"CSRFToken":     base.CSRFToken,
		"AuthEnabled":   base.AuthEnabled,
		"Degraded":      base.Degraded,
		"Version":       base.Version,
		"RepoURL":       base.RepoURL,
		"Variant":       base.Variant,
		"CustomCSS":     base.CustomCSS,
		"ActiveGallery": base.ActiveGallery,
		"Galleries":     base.Galleries,
		"VisibleCount":     base.VisibleCount,
		"TagCount":         base.TagCount,
		"CollectionsCount": base.CollectionsCount,
		"RatingLevels":  base.RatingLevels,
		"ActiveRating":  base.ActiveRating,
		"RequestStart":  base.RequestStart,
	}
	s.renderTemplate(w, "help.html", data)
}

// notFoundHandler renders a styled 404 for any unmatched GET path. The
// mux's default behaviour is unstyled `404 page not found` text on a
// white page; routing through the standard layout keeps the user inside
// the app with a back link.
func (s *Server) notFoundHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	s.renderTemplate(w, "notfound.html", s.base(r, "", "Not found - Monbooru"))
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
	defer rows.Close()
	var paths []models.ImagePath
	for rows.Next() {
		var p models.ImagePath
		var isCanon int
		if err := rows.Scan(&p.ID, &p.ImageID, &p.Path, &isCanon); err != nil {
			logx.Warnf("load image paths scan: %v", err)
			continue
		}
		p.IsCanonical = isCanon == 1
		paths = append(paths, p)
	}
	return paths
}
