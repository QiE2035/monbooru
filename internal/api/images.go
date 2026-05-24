package api

import (
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"  // register gif decoder for canDecodeImage
	_ "image/jpeg" // register jpeg decoder for canDecodeImage
	_ "image/png"  // register png decoder for canDecodeImage
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/webp" // register webp decoder for canDecodeImage

	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/search"
	"github.com/leqwin/monbooru/internal/tagger"
)

// validateVia rejects caller-supplied `via` strings that carry
// characters the downstream CSS attribute selectors and HTML attribute
// renderers don't survive cleanly. The detail page's tag-source group
// is JS-selected with `[data-source="<name>"]`; whitespace or a literal
// quote / bracket in the stored value produces a malformed selector
// and the tag-focus cursor loses its place. Empty `via` is fine and
// means "no attribution".
func validateVia(via string) error {
	if via == "" {
		return nil
	}
	if len(via) > 200 {
		return fmt.Errorf("via must be 200 characters or less")
	}
	for _, r := range via {
		switch r {
		case ' ', '\t', '\n', '\r', '"', '\'', '<', '>', '[', ']', '\\':
			return fmt.Errorf("via must not contain whitespace or any of: \" ' < > [ ] \\")
		}
	}
	return nil
}

// canDecodeImage opens path and runs image.DecodeConfig on the first
// few bytes. Used as a fast post-DetectFileType guard so a text file
// with an image extension is rejected before the row reaches the DB
// with a null width / height. Archive and video file types skip this
// check; the cbz path does its own integrity verification inside
// Ingest and video frames decode via ffmpeg later in the thumbnail
// step.
func canDecodeImage(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	_, _, err = image.DecodeConfig(f)
	return err == nil
}

// imageResponse is the JSON representation of an image.
type imageResponse struct {
	ID            int64          `json:"id"`
	SHA256        string         `json:"sha256"`
	CanonicalPath string         `json:"canonical_path"`
	Aliases       []string       `json:"aliases"`
	FileType      string         `json:"file_type"`
	Width         *int           `json:"width"`
	Height        *int           `json:"height"`
	FileSize      int64          `json:"file_size"`
	IsFavorited   bool           `json:"is_favorited"`
	IsInbox       bool           `json:"is_inbox"`
	IsMissing     bool           `json:"is_missing"`
	AutoTaggedAt  *time.Time     `json:"auto_tagged_at"`
	SourceType    string         `json:"source_type"`
	Origin        string         `json:"origin"`
	Source        string         `json:"source"`
	URL           string         `json:"url"`
	PageCount     *int           `json:"page_count"`
	Series        string         `json:"collection"`
	SeriesOrder   *int           `json:"collection_order"`
	Phash         *string        `json:"phash"`
	IngestedAt    time.Time      `json:"ingested_at"`
	ThumbnailURL  string         `json:"thumbnail_url"`
	Tags          []imageTagJSON `json:"tags"`
}

type imageTagJSON struct {
	Name       string   `json:"name"`
	Category   string   `json:"category"`
	IsAuto     bool     `json:"is_auto"`
	Confidence *float64 `json:"confidence"`
	TaggerName *string  `json:"tagger_name"`
}

// buildImageResponse fetches an image plus its tags and assembles the
// JSON response struct.
func (h *Handler) buildImageResponse(g Gallery, imageID int64) (*imageResponse, error) {
	var img models.Image
	var isMissing, isFavorited, isInbox int
	var autoTaggedAt *string
	var ingestedAt string

	var pageCount, seriesOrder *int
	var durationSec *float64
	var phash *int64
	err := g.DB.Read.QueryRow(`
		SELECT id, sha256, canonical_path, file_type, width, height, file_size,
		       is_missing, is_favorited, is_inbox, auto_tagged_at, source_type, origin, source, url, page_count, duration_seconds, series, series_order, phash, ingested_at
		FROM images WHERE id = ?`, imageID,
	).Scan(&img.ID, &img.SHA256, &img.CanonicalPath, &img.FileType, &img.Width, &img.Height,
		&img.FileSize, &isMissing, &isFavorited, &isInbox, &autoTaggedAt, &img.SourceType, &img.Origin, &img.Source, &img.URL, &pageCount, &durationSec, &img.Series, &seriesOrder, &phash, &ingestedAt)
	if err != nil {
		return nil, err
	}
	img.DurationSec = durationSec
	img.SeriesOrder = seriesOrder
	img.Phash = phash
	img.IsMissing = isMissing == 1
	img.IsFavorited = isFavorited == 1
	img.IsInbox = isInbox == 1
	img.IngestedAt, _ = time.Parse(time.RFC3339, ingestedAt)
	if autoTaggedAt != nil {
		t, _ := time.Parse(time.RFC3339, *autoTaggedAt)
		img.AutoTaggedAt = &t
	}

	// Close the alias rows immediately rather than deferring, so the
	// read connection is freed before the tag query.
	aliases := []string{}
	aliasRows, err := g.DB.Read.Query(`SELECT path FROM image_paths WHERE image_id = ? AND is_canonical = 0`, imageID)
	if err != nil {
		logx.Warnf("buildImageResponse aliases: %v", err)
	} else {
		for aliasRows.Next() {
			var p string
			if err := aliasRows.Scan(&p); err == nil {
				aliases = append(aliases, p)
			}
		}
		aliasRows.Close()
	}

	tags := []imageTagJSON{}
	tagRows, err := g.DB.Read.Query(`
		SELECT t.name, tc.name, it.is_auto, it.confidence, it.tagger_name
		FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		JOIN tag_categories tc ON tc.id = t.category_id
		WHERE it.image_id = ?
		ORDER BY tc.name, t.name`, imageID)
	if err != nil {
		logx.Warnf("buildImageResponse tags: %v", err)
	} else {
		defer tagRows.Close()
		for tagRows.Next() {
			var tj imageTagJSON
			var tn *string
			if err := tagRows.Scan(&tj.Name, &tj.Category, &tj.IsAuto, &tj.Confidence, &tn); err == nil {
				tj.TaggerName = tn
				tags = append(tags, tj)
			}
		}
	}

	resp := &imageResponse{
		ID:            img.ID,
		SHA256:        img.SHA256,
		CanonicalPath: img.CanonicalPath,
		Aliases:       aliases,
		FileType:      img.FileType,
		Width:         img.Width,
		Height:        img.Height,
		FileSize:      img.FileSize,
		IsFavorited:   img.IsFavorited,
		IsInbox:       img.IsInbox,
		IsMissing:     img.IsMissing,
		AutoTaggedAt:  img.AutoTaggedAt,
		SourceType:    img.SourceType,
		Origin:        img.Origin,
		Source:        img.Source,
		URL:           img.URL,
		PageCount:     pageCount,
		Series:        img.Series,
		SeriesOrder:   img.SeriesOrder,
		Phash:         phashHexPtr(img.Phash),
		IngestedAt:    img.IngestedAt,
		ThumbnailURL:  "/thumbnails/" + g.Name + "/" + strconv.FormatInt(imageID, 10) + ".jpg",
		Tags:          tags,
	}
	return resp, nil
}

// phashHexPtr renders the optional perceptual hash as a 16-char
// lowercase hex string, or nil when the column is NULL.
func phashHexPtr(p *int64) *string {
	if p == nil {
		return nil
	}
	s := fmt.Sprintf("%016x", uint64(*p))
	return &s
}

func (h *Handler) getImage(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid image id")
		return
	}

	resp, err := h.buildImageResponse(g, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// createImage handles POST /api/v1/images. Accepts either multipart
// (with `file`, `tags`, `folder`, `autotag`, `tagger_name`, `via`) or
// JSON (with `path`, `tags`, `folder`, `autotag`, `tagger_name`,
// `via`). In JSON mode `folder` only applies to relative paths;
// absolute paths are used verbatim. `via` lands on `images.origin` and
// is attached to each initial tag's `image_tags.tagger_name`.
func (h *Handler) createImage(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	ct := r.Header.Get("Content-Type")

	var (
		imgPath        string
		initialTags    []string
		folder         string
		autotag        bool
		taggerName     string
		via            string // caller-supplied label; stored on images.origin and inherited by initial tags
		uploadedToDisk bool   // true when we wrote the file ourselves (multipart)
	)

	if isMultipart(ct) {
		maxBytes := int64(h.cfg.Gallery.MaxFileSizeMB) * 1024 * 1024
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes+4096)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			apiError(w, http.StatusRequestEntityTooLarge, "file_too_large", "file exceeds max size")
			return
		}
		file, fh, err := r.FormFile("file")
		if err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "missing file field")
			return
		}
		defer file.Close()

		folder = strings.TrimSpace(r.FormValue("folder"))
		autotag = isTrue(r.FormValue("autotag"))
		taggerName = strings.TrimSpace(r.FormValue("tagger_name"))
		via = strings.TrimSpace(r.FormValue("via"))
		if err := validateVia(via); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}

		destDir, destErr := gallery.ResolveSubdir(g.GalleryPath, folder)
		if destErr != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", destErr.Error())
			return
		}
		if err := os.MkdirAll(destDir, 0755); err != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", "failed to create folder: "+err.Error())
			return
		}

		// Write directly to the final destination so the watcher sees
		// the real filename rather than a temp one (which would get
		// marked missing as soon as we renamed it).
		dstPath := gallery.UniqueDestPath(destDir, fh.Filename)
		dst, err := os.Create(dstPath)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", "failed to create destination file")
			return
		}
		if _, err := io.Copy(dst, file); err != nil {
			dst.Close()
			os.Remove(dstPath)
			apiError(w, http.StatusInternalServerError, "internal_error", "failed to save upload")
			return
		}
		dst.Close()

		if tagsJSON := r.FormValue("tags"); tagsJSON != "" {
			json.Unmarshal([]byte(tagsJSON), &initialTags)
		}
		imgPath = dstPath
		uploadedToDisk = true
	} else {
		var body struct {
			Path       string   `json:"path"`
			Tags       []string `json:"tags"`
			Folder     string   `json:"folder"`
			Autotag    bool     `json:"autotag"`
			TaggerName string   `json:"tagger_name"`
			Via        string   `json:"via"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
			return
		}
		if body.Path == "" {
			apiError(w, http.StatusBadRequest, "invalid_request", "path is required")
			return
		}
		imgPath = body.Path
		initialTags = body.Tags
		folder = strings.TrimSpace(body.Folder)
		autotag = body.Autotag
		taggerName = strings.TrimSpace(body.TaggerName)
		via = strings.TrimSpace(body.Via)
		if err := validateVia(via); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}

		// Relative path + folder: resolve under <gallery>/<folder>/<path>.
		// Absolute paths go through the gate just below.
		if folder != "" && !filepath.IsAbs(imgPath) {
			destDir, destErr := gallery.ResolveSubdir(g.GalleryPath, folder)
			if destErr != nil {
				apiError(w, http.StatusBadRequest, "invalid_request", destErr.Error())
				return
			}
			imgPath = filepath.Join(destDir, imgPath)
		}

		// Constrain the caller-supplied path to the gallery root. The
		// operator owns the gallery folder and the API is the operator-
		// facing surface, so an ingest-by-path that quietly registers a
		// row pointing outside the gallery would have a later
		// DELETE /api/v1/images/{id} unlink files the operator never
		// meant to manage. Mirror the upload form's containment.
		absPath, absErr := filepath.Abs(imgPath)
		if absErr != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "invalid path")
			return
		}
		galleryAbs, gErr := filepath.Abs(g.GalleryPath)
		if gErr != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", "gallery path unresolvable")
			return
		}
		if !gallery.PathInside(galleryAbs, absPath) {
			apiError(w, http.StatusBadRequest, "invalid_request", "path must be inside the gallery root")
			return
		}
		imgPath = absPath

		// Translate the common client-side mistake (path doesn't exist)
		// to a 400 with a sanitised message so the response body doesn't
		// echo the operator's filesystem layout and the status class
		// reflects the caller error rather than a server failure.
		if _, statErr := os.Stat(imgPath); os.IsNotExist(statErr) {
			apiError(w, http.StatusBadRequest, "not_found", "file not found")
			return
		}
	}

	// Enforce gallery.max_file_size_mb for both modes. Multipart also
	// has MaxBytesReader; this mainly guards the JSON path-reference
	// mode where the caller supplies an absolute path.
	if maxMB := h.cfg.Gallery.MaxFileSizeMB; maxMB > 0 {
		if info, err := os.Stat(imgPath); err == nil {
			if info.Size() > int64(maxMB)*1024*1024 {
				if uploadedToDisk {
					os.Remove(imgPath)
				}
				apiError(w, http.StatusRequestEntityTooLarge, "file_too_large",
					fmt.Sprintf("file exceeds max size (%d MB)", maxMB))
				return
			}
		}
	}

	fileType, ftErr := gallery.DetectFileType(imgPath)
	if ftErr != nil {
		if uploadedToDisk {
			os.Remove(imgPath)
		}
		apiError(w, http.StatusBadRequest, "unsupported_type", "unsupported or unrecognised file type")
		return
	}
	// DetectFileType only checks the extension, so a follow-up
	// DecodeConfig confirms the bytes parse as an image before the row
	// lands in the DB. cbz integrity is verified inside Ingest and
	// video frames decode later via ffmpeg, so both buckets skip this.
	if !gallery.IsVideoType(fileType) && fileType != models.FileTypeCBZ {
		if !canDecodeImage(imgPath) {
			if uploadedToDisk {
				os.Remove(imgPath)
			}
			apiError(w, http.StatusUnsupportedMediaType, "unsupported_type", "file does not decode as an image")
			return
		}
	}

	// Caller-supplied `via` wins; otherwise multipart defaults to
	// "upload" and JSON path-reference defaults to "ingest".
	origin := via
	if origin == "" {
		if uploadedToDisk {
			origin = models.OriginUpload
		} else {
			origin = models.OriginIngest
		}
	}

	img, isDuplicate, err := gallery.Ingest(g.DB, g.GalleryPath, g.ThumbnailsPath, imgPath, fileType, origin)
	if err != nil {
		if uploadedToDisk {
			os.Remove(imgPath)
		}
		logx.Warnf("api createImage ingest: %v", err)
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if g.InvalidateCaches != nil {
		g.InvalidateCaches()
	}
	if isDuplicate {
		// gallery.Ingest already recorded the new file as an alias path
		// on the existing canonical row; leaving the file on disk keeps
		// the alias valid. Surface the existing image with alias_added=
		// true so a retry-on-409 client can recognise the partial-success.
		resp, respErr := h.buildImageResponse(g, img.ID)
		if respErr != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", "failed to build response")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"image":       resp,
			"alias_added": true,
		})
		return
	}

	tagWarnings := h.applyInitialTags(g, img.ID, initialTags, via)

	var autotagNote string
	if autotag {
		if !tagger.IsAvailable(h.cfg) {
			autotagNote = "autotag skipped: tagger not available"
		} else {
			selected, selErr := h.selectedTaggers(g.Name, taggerName)
			if selErr != nil {
				autotagNote = "autotag skipped: " + selErr.Error()
			} else if err := h.jobs.Start("autotag"); err != nil {
				autotagNote = "autotag skipped: a job is already running"
			} else {
				imgID := img.ID
				database := g.DB
				invalidate := g.InvalidateCaches
				mangaCache := gallery.MangaCacheDir(g.ThumbnailsPath)
				go func() {
					skipped, err := tagger.RunWithTaggers(h.jobs.Context(), database, h.cfg, []int64{imgID}, selected, h.jobs, h.cfg.Tagger.UseCUDA, mangaCache)
					if invalidate != nil {
						invalidate()
					}
					if err != nil {
						h.jobs.Fail(err.Error())
						return
					}
					if skipped > 0 {
						h.jobs.Complete(fmt.Sprintf("auto-tagger skipped image #%d", imgID))
						return
					}
					h.jobs.Complete(fmt.Sprintf("auto-tagged image #%d", imgID))
				}()
				autotagNote = "autotag job started"
			}
		}
	}

	resp, err := h.buildImageResponse(g, img.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to build response")
		return
	}

	// Wrap the response when we have side-channel info to attach.
	if len(tagWarnings) > 0 || autotagNote != "" {
		envelope := map[string]any{"image": resp}
		if len(tagWarnings) > 0 {
			envelope["tag_warnings"] = tagWarnings
		}
		if autotagNote != "" {
			envelope["autotag"] = autotagNote
		}
		writeJSON(w, http.StatusCreated, envelope)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// selectedTaggers resolves a caller-supplied tagger_name to a concrete
// list of taggers running on the named gallery. Empty name means every
// tagger enabled + available + applicable to that gallery.
func (h *Handler) selectedTaggers(gallery, name string) ([]tagger.TaggerStatus, error) {
	enabled := tagger.EnabledTaggersForGallery(h.cfg, gallery)
	if name == "" {
		return enabled, nil
	}
	for _, t := range enabled {
		if t.Name == name {
			return []tagger.TaggerStatus{t}, nil
		}
	}
	return nil, fmt.Errorf("tagger %q is not enabled or available for gallery %q", name, gallery)
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (h *Handler) deleteImage(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid image id")
		return
	}

	result, err := gallery.DeleteImage(g.DB, g.GalleryPath, g.ThumbnailsPath, id, g.TagSvc.RemoveAllTagsFromImage, relationsOnDelete(g.RelationsSvc))
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	if g.InvalidateCaches != nil {
		g.InvalidateCaches()
	}

	// Empty-source-folder cleanup mirrors the UI delete handler so the
	// API doesn't leave the operator's tree with empty parent folders
	// after a delete that emptied them. The structured 200 response
	// (folder_deleted + folder) is opt-in via ?delete_empty_folder=true
	// to keep the wire shape for callers that just want 204.
	folderRemoved := false
	if !result.IsMissing && result.FolderPath != "" {
		fullFolderPath := filepath.Join(g.GalleryPath, result.FolderPath)
		if entries, readErr := os.ReadDir(fullFolderPath); readErr == nil && len(entries) == 0 {
			if removeErr := os.Remove(fullFolderPath); removeErr == nil {
				folderRemoved = true
			} else {
				logx.Warnf("api deleteImage: failed to remove empty folder %q: %v", fullFolderPath, removeErr)
			}
		}
	}

	if folderRemoved && r.URL.Query().Get("delete_empty_folder") == "true" {
		writeJSON(w, http.StatusOK, map[string]any{
			"folder_deleted": true,
			"folder":         result.FolderPath,
		})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) searchImages(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	queryStr := q.Get("q")
	sortStr := q.Get("sort")
	if sortStr == "" {
		sortStr = "newest"
	}
	orderStr := q.Get("order")
	if orderStr == "" {
		orderStr = "desc"
	}

	offset, limit := parsePage(r, h.cfg.UI.PageSize, 200)
	pageNum := offset/limit + 1

	expr, parseErr := search.Parse(queryStr)
	if parseErr != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid search query: "+parseErr.Error())
		return
	}
	// Stable random ordering across paginated calls relies on the caller
	// passing the same seed back; without one, every call reseeds and
	// pages overlap. Spec §8.3.
	var randomSeed int64
	if seedStr := q.Get("seed"); seedStr != "" {
		if s, err := strconv.ParseInt(seedStr, 10, 64); err == nil && s != 0 {
			randomSeed = s
		}
	}
	sq := search.Query{
		Expr:       expr,
		Sort:       sortStr,
		Order:      orderStr,
		Page:       pageNum,
		Limit:      limit,
		RandomSeed: randomSeed,
	}

	result, err := search.Execute(g.DB, sq)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	ids := make([]int64, 0, len(result.Results))
	for _, img := range result.Results {
		ids = append(ids, img.ID)
	}
	tagsByID, tagsErr := loadTagsForImages(g, ids)
	if tagsErr != nil {
		// Tags load failure shouldn't blank the whole search response;
		// log and fall through with empty tag lists per row.
		logx.Warnf("api searchImages tag load: %v", tagsErr)
		tagsByID = nil
	}
	aliasesByID, aliasErr := loadAliasesForImages(g, ids)
	if aliasErr != nil {
		logx.Warnf("api searchImages alias load: %v", aliasErr)
		aliasesByID = nil
	}

	images := make([]imageResponse, 0, len(result.Results))
	for _, img := range result.Results {
		tags := tagsByID[img.ID]
		if tags == nil {
			tags = []imageTagJSON{}
		}
		aliases := aliasesByID[img.ID]
		if aliases == nil {
			aliases = []string{}
		}
		images = append(images, imageResponse{
			ID:            img.ID,
			SHA256:        img.SHA256,
			CanonicalPath: img.CanonicalPath,
			Aliases:       aliases,
			FileType:      img.FileType,
			Width:         img.Width,
			Height:        img.Height,
			FileSize:      img.FileSize,
			IsFavorited:   img.IsFavorited,
			IsInbox:       img.IsInbox,
			IsMissing:     img.IsMissing,
			AutoTaggedAt:  img.AutoTaggedAt,
			SourceType:    img.SourceType,
			Origin:        img.Origin,
			Source:        img.Source,
			URL:           img.URL,
			PageCount:     img.PageCount,
			Series:        img.Series,
			SeriesOrder:   img.SeriesOrder,
			Phash:         phashHexPtr(img.Phash),
			IngestedAt:    img.IngestedAt,
			ThumbnailURL:  "/thumbnails/" + g.Name + "/" + strconv.FormatInt(img.ID, 10) + ".jpg",
			Tags:          tags,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"page":    result.Page,
		"limit":   result.Limit,
		"total":   result.Total,
		"results": images,
	})
}

// loadAliasesForImages batch-loads non-canonical image_paths rows for
// every id in the slice in one round-trip, mirroring the per-row read
// in buildImageResponse. Used by the search projection so a multi-id
// response carries the same alias array shape as the single-image GET.
func loadAliasesForImages(g Gallery, ids []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := g.DB.Read.Query(
		`SELECT image_id, path FROM image_paths
		 WHERE is_canonical = 0 AND image_id IN (`+placeholders+`)
		 ORDER BY image_id, id`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var imageID int64
		var path string
		if err := rows.Scan(&imageID, &path); err != nil {
			return nil, err
		}
		out[imageID] = append(out[imageID], path)
	}
	return out, rows.Err()
}

// loadTagsForImages batch-loads image_tags ⋈ tags ⋈ tag_categories for
// every id in the slice with a single round-trip. Empty input returns
// an empty map so callers can skip the if-empty check.
func loadTagsForImages(g Gallery, ids []int64) (map[int64][]imageTagJSON, error) {
	out := make(map[int64][]imageTagJSON, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := g.DB.Read.Query(`
		SELECT it.image_id, t.name, tc.name, it.is_auto, it.confidence, it.tagger_name
		FROM image_tags it
		JOIN tags t ON t.id = it.tag_id
		JOIN tag_categories tc ON tc.id = t.category_id
		WHERE it.image_id IN (`+placeholders+`)
		ORDER BY it.image_id, tc.name, t.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var imageID int64
		var tj imageTagJSON
		var tn *string
		if err := rows.Scan(&imageID, &tj.Name, &tj.Category, &tj.IsAuto, &tj.Confidence, &tn); err != nil {
			return nil, err
		}
		tj.TaggerName = tn
		out[imageID] = append(out[imageID], tj)
	}
	return out, rows.Err()
}

// listImageTags handles GET /api/v1/images/:id/tags. Mirrors the
// post-mutation response shape from addImageTags / removeImageTags so
// a caller has one tag-listing endpoint to pin against. The full image
// object remains reachable via GET /api/v1/images/:id for callers who
// need adjacent metadata.
func (h *Handler) listImageTags(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid image id")
		return
	}
	if !imageExists(g, id) {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	resp, err := h.buildImageResponse(g, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	writeJSON(w, http.StatusOK, resp.Tags)
}

// addImageTags handles POST /api/v1/images/:id/tags. Each entry can
// be a plain name (general category) or "category:name", matching the
// web UI's tag input.
func (h *Handler) addImageTags(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid image id")
		return
	}
	if !imageExists(g, id) {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}

	var body struct {
		Tags []string `json:"tags"`
		Via  string   `json:"via"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if len(body.Tags) == 0 {
		// A missing or empty `tags` field would loop zero times and
		// return 200 + the existing tag list - a silent success the
		// caller can't tell apart from a real no-op. The OpenAPI
		// declares `tags` required; reject the request shape.
		apiError(w, http.StatusBadRequest, "invalid_request",
			"`tags` is required and must contain at least one name")
		return
	}
	via := strings.TrimSpace(body.Via)
	if err := validateVia(via); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	tagWarnings := h.applyInitialTags(g, id, body.Tags, via)
	if g.InvalidateCaches != nil {
		g.InvalidateCaches()
	}

	resp, err := h.buildImageResponse(g, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	if len(tagWarnings) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"tags":         resp.Tags,
			"tag_warnings": tagWarnings,
		})
		return
	}
	writeJSON(w, http.StatusOK, resp.Tags)
}

// imageExists short-circuits the tag-mutation handlers so a request
// against a missing id returns 404 before any per-token work runs.
// Without it the per-tag inserts hit the FK constraint and surface as
// warnings the caller never sees (the final buildImageResponse 404
// supersedes them), and a token's GetOrCreateTag run still leaves a
// stray vocabulary row behind.
func imageExists(g Gallery, id int64) bool {
	var n int
	return g.DB.Read.QueryRow(`SELECT 1 FROM images WHERE id = ?`, id).Scan(&n) == nil
}

// applyInitialTags resolves each raw token (`bare` or `category:bare`)
// to a tag id (creating missing rows), then fans the batch through
// AddTagsToOneImage in one writer tx. Per-tag failures land in
// warnings without aborting; the apply call's own failure does too.
func (h *Handler) applyInitialTags(g Gallery, imgID int64, rawTags []string, via string) []string {
	var warnings []string
	tagIDs := make([]int64, 0, len(rawTags))
	for _, tagName := range rawTags {
		catID, bareName, err := h.resolveCategoryTag(g, tagName)
		if err != nil {
			warnings = append(warnings, "tag "+tagName+": "+err.Error())
			continue
		}
		tag, err := g.TagSvc.GetOrCreateTag(bareName, catID)
		if err != nil {
			warnings = append(warnings, "tag "+tagName+": "+err.Error())
			continue
		}
		tagIDs = append(tagIDs, tag.ID)
	}
	if len(tagIDs) > 0 {
		if _, err := g.TagSvc.AddTagsToOneImage(imgID, tagIDs, via); err != nil {
			warnings = append(warnings, "apply tags: "+err.Error())
		}
	}
	return warnings
}

// resolveCategoryTag splits "artist:foo" into (artist_id, "foo") when
// "artist" names a real category, otherwise returns (general_id, input)
// so colon-bearing tag names like "nier:automata" or ":3" round-trip
// without a warning.
func (h *Handler) resolveCategoryTag(g Gallery, input string) (int64, string, error) {
	input = strings.TrimSpace(input)
	catName := "general"
	tagName := input
	if idx := strings.Index(input, ":"); idx > 0 {
		var catID int64
		if err := g.DB.Read.QueryRow(
			`SELECT id FROM tag_categories WHERE name = ?`, input[:idx],
		).Scan(&catID); err == nil {
			return catID, input[idx+1:], nil
		}
	}
	var catID int64
	if err := g.DB.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = ?`, catName,
	).Scan(&catID); err != nil {
		return 0, "", fmt.Errorf("unknown category %q", catName)
	}
	return catID, tagName, nil
}

// removeImageTags handles DELETE /api/v1/images/:id/tags. Each entry
// is plain (any single match) or "category:name" (exact category). A
// plain name matching more than one category on the image returns 409
// so the caller can disambiguate.
func (h *Handler) removeImageTags(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid image id")
		return
	}
	if !imageExists(g, id) {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}

	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if len(body.Tags) == 0 {
		// Match the POST path: a wrong-shape body must be rejected
		// rather than 200ing with the current tag list and no diagnostic.
		apiError(w, http.StatusBadRequest, "invalid_request",
			"`tags` is required and must contain at least one name")
		return
	}

	var tagWarnings []string
	tagIDs := make([]int64, 0, len(body.Tags))
	for _, tagName := range body.Tags {
		tagID, err := h.resolveImageTagID(g, id, tagName)
		if err != nil {
			apiError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		if tagID == 0 {
			// Tag not on this image; silently ignored per the docs.
			continue
		}
		tagIDs = append(tagIDs, tagID)
	}
	if len(tagIDs) > 0 {
		if err := g.TagSvc.RemoveTagsFromOneImage(id, tagIDs); err != nil {
			tagWarnings = append(tagWarnings, "remove tags: "+err.Error())
		}
	}
	if g.InvalidateCaches != nil {
		g.InvalidateCaches()
	}

	resp, err := h.buildImageResponse(g, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	if len(tagWarnings) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"tags":         resp.Tags,
			"tag_warnings": tagWarnings,
		})
		return
	}
	writeJSON(w, http.StatusOK, resp.Tags)
}

// resolveImageTagID returns the tag_id attached to imageID that matches
// tagName. A "category:name" input targets that exact category when
// the prefix is a real category; otherwise the whole string is matched
// as a literal tag name. A real-category prefix that misses on the
// image falls through to the literal-name branch so an oddly-stored
// general tag like "artist:foo" is still removable. A plain name is
// accepted only when it resolves to exactly one tag on the image.
// (0, nil) means the tag isn't present.
func (h *Handler) resolveImageTagID(g Gallery, imageID int64, tagName string) (int64, error) {
	tagName = strings.TrimSpace(tagName)
	if idx := strings.Index(tagName, ":"); idx > 0 {
		catName := tagName[:idx]
		var catID int64
		if g.DB.Read.QueryRow(
			`SELECT id FROM tag_categories WHERE name = ?`, catName,
		).Scan(&catID) == nil {
			bareName := tagName[idx+1:]
			var tagID int64
			if err := g.DB.Read.QueryRow(
				`SELECT t.id FROM image_tags it
				 JOIN tags t             ON t.id  = it.tag_id
				 JOIN tag_categories tc  ON tc.id = t.category_id
				 WHERE it.image_id = ? AND t.name = ? AND tc.name = ?`,
				imageID, bareName, catName,
			).Scan(&tagID); err == nil {
				return tagID, nil
			}
			// Category-qualified miss: fall through.
		}
	}

	rows, err := g.DB.Read.Query(
		`SELECT t.id FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.name = ?`,
		imageID, tagName,
	)
	if err != nil {
		return 0, fmt.Errorf("tag lookup failed: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	switch len(ids) {
	case 0:
		return 0, nil
	case 1:
		return ids[0], nil
	default:
		return 0, fmt.Errorf("tag %q exists on this image in multiple categories; use category:name", tagName)
	}
}

func isMultipart(ct string) bool {
	return strings.HasPrefix(ct, "multipart/form-data")
}
