package api

import (
	"database/sql"
	"encoding/json"
	"errors"
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

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/search"
	"github.com/leqwin/monbooru/internal/tagger"
)

// applyCreateProvenance writes the supplied provenance fields onto a
// freshly-ingested row. Only non-empty values are written; the row's
// empty-string / NULL defaults already cover the unset case, so a bare
// create touches nothing. Validation has already run, so a failure here
// is a DB-level error.
func applyCreateProvenance(g Gallery, imageID int64, source, url, md5, collection, commentary string, order *int) error {
	if source != "" || url != "" {
		if err := gallery.AddSourceMembership(g.DB, imageID, source, "", url); err != nil {
			return err
		}
		if err := gallery.SetSourceMD5(g.DB, imageID, source, "", md5); err != nil {
			return err
		}
	}
	if source != "" && commentary != "" {
		if err := gallery.SetSourceCommentary(g.DB, imageID, source, "", commentary); err != nil {
			return err
		}
	}
	if collection != "" {
		return gallery.SetHomeCollection(g.DB, imageID, collection, order)
	}
	return nil
}

// mergeSummary reports what a duplicate-merge folded into an existing image.
type mergeSummary struct {
	TagsAdded    int  `json:"tags_added"`
	TagsRemoved  int  `json:"tags_removed"`
	RatingFilled bool `json:"rating_filled"`
	SourceAdded  bool `json:"source_added"`
}

// mergeSource folds a re-pushed file's provenance and tags into an existing
// image instead of discarding them (issue #6): the origin is recorded and the
// tags imported from that source are reconciled to the incoming set, with the
// rating protected. Attribution is the source label so each source owns a
// prunable slice. A push with no source label leaves tags untouched. The
// second return carries unresolvable-tag warnings for the response envelope.
func (h *Handler) mergeSource(g Gallery, imageID int64, source, url, md5 string, rawTags []string) (mergeSummary, []string, error) {
	var sum mergeSummary
	if source != "" || url != "" {
		if err := gallery.AddSourceMembership(g.DB, imageID, source, "", url); err != nil {
			return sum, nil, err
		}
		if err := gallery.SetSourceMD5(g.DB, imageID, source, "", md5); err != nil {
			return sum, nil, err
		}
		sum.SourceAdded = true
	}
	var warnings []string
	if source != "" && len(rawTags) > 0 {
		tagIDs, warns := h.resolveTagNames(g, rawTags)
		warnings = warns
		r, err := g.TagSvc.SyncSourceTags(imageID, tagIDs, source)
		if err != nil {
			return sum, warnings, err
		}
		sum.TagsAdded, sum.TagsRemoved, sum.RatingFilled = r.Added, r.Removed, r.RatingFilled
	}
	return sum, warnings, nil
}

// enrichImage handles POST /api/v1/images/{id}/enrich: applies fetched
// metadata (tags, provenance, artist commentary, positional notes) to an
// existing image with no file upload - the metadata-only counterpart of a
// push, used by monloader's source refetch. It shares mergeSource with the
// duplicate branch for tags + provenance. When verify is set and a
// source_md5 is supplied, the image's stored bytes are md5'd on demand and
// compared first; a mismatch means the post no longer serves the same file,
// so nothing changes (409 hash_mismatch).
func (h *Handler) enrichImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var canonPath string
	switch err := g.DB.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&canonPath); {
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	case err != nil:
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	var body struct {
		Tags       []string         `json:"tags"`
		Source     string           `json:"source"`
		URL        string           `json:"url"`
		SourceMD5  string           `json:"source_md5"`
		Verify     bool             `json:"verify"`
		Commentary string           `json:"commentary"`
		Notes      []annotationJSON `json:"notes"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := validateMaxLen("commentary", strings.TrimSpace(body.Commentary), maxImageCommentaryLen); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := validateMaxLen("source_md5", strings.TrimSpace(body.SourceMD5), maxSourceMD5Len); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	verified := true
	if body.Verify {
		if body.SourceMD5 == "" {
			verified = false // asked to verify, but the source reported no md5
		} else {
			got, err := gallery.Md5File(canonPath)
			if err != nil {
				g.recordFetch(id, "error", "could not verify the file; fetch not applied")
				apiError(w, http.StatusInternalServerError, "internal_error", "cannot hash image: "+err.Error())
				return
			}
			if !strings.EqualFold(got, body.SourceMD5) {
				g.recordFetch(id, "mismatch", "the source no longer serves this file (hash mismatch); no tags applied")
				apiError(w, http.StatusConflict, "hash_mismatch", "the source no longer serves this file")
				return
			}
		}
	}
	source := strings.TrimSpace(body.Source)
	sum, tagWarnings, err := h.mergeSource(g, id, source, strings.TrimSpace(body.URL), strings.TrimSpace(body.SourceMD5), body.Tags)
	if err != nil {
		g.recordFetch(id, "error", "fetch failed while applying tags")
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	// Artist commentary and positional notes are attributed to the same
	// source, so a refetch pulls them in alongside the tags. Both replace what
	// the source last carried; an empty payload leaves the stored value be.
	if source != "" {
		if commentary := strings.TrimSpace(body.Commentary); commentary != "" {
			if err := gallery.SetSourceCommentary(g.DB, id, source, "", commentary); err != nil {
				g.recordFetch(id, "error", "fetch failed while applying commentary")
				apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
				return
			}
		}
		if notes := annotationsFromInput(body.Notes); len(notes) > 0 {
			if err := gallery.ReplaceSourceAnnotations(g.DB, id, source, "", notes); err != nil {
				g.recordFetch(id, "error", "fetch failed while applying notes")
				apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
				return
			}
		}
	}
	g.invalidate()
	g.recordFetch(id, "ok", fetchSummary(sum))
	resp := map[string]any{"merge": sum, "verified": verified}
	if len(tagWarnings) > 0 {
		resp["tag_warnings"] = tagWarnings
	}
	writeJSON(w, http.StatusOK, resp)
}

// fetchSummary is the operator-facing confirmation a source refetch surfaces
// once the enrich lands; it names the tag delta when the fetch changed anything.
func fetchSummary(sum mergeSummary) string {
	switch {
	case sum.TagsAdded > 0 && sum.TagsRemoved > 0:
		return fmt.Sprintf("Fetched tags from the source (+%d, -%d).", sum.TagsAdded, sum.TagsRemoved)
	case sum.TagsAdded > 0:
		return fmt.Sprintf("Fetched tags from the source (+%d).", sum.TagsAdded)
	case sum.TagsRemoved > 0:
		return fmt.Sprintf("Fetched tags from the source (-%d).", sum.TagsRemoved)
	default:
		return "Fetched tags from the source."
	}
}

// fetchStatusReport handles POST /api/v1/images/{id}/fetch-status: monloader
// reports a source-fetch outcome that never reached enrich (a fetch that hit an
// unsupported URL, timed out, or was blocked) so the detail page's poll can
// surface it instead of spinning to the poll cap. Body: {state, message}.
func (h *Handler) fetchStatusReport(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	var body struct {
		State   string `json:"state"`
		Message string `json:"message"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.State) == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "state is required")
		return
	}
	g.recordFetch(id, body.State, body.Message)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
	defer func() { _ = f.Close() }()
	_, _, err = image.DecodeConfig(f)
	return err == nil
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
		       is_missing, is_favorited, is_inbox, auto_tagged_at, source_type, origin, source, url, note, page_count, duration_seconds, series, series_order, phash, ingested_at
		FROM images WHERE id = ?`, imageID,
	).Scan(&img.ID, &img.SHA256, &img.CanonicalPath, &img.FileType, &img.Width, &img.Height,
		&img.FileSize, &isMissing, &isFavorited, &isInbox, &autoTaggedAt, &img.SourceType, &img.Origin, &img.Source, &img.URL, &img.Note, &pageCount, &durationSec, &img.Series, &seriesOrder, &phash, &ingestedAt)
	if err != nil {
		return nil, err
	}
	img.PageCount = pageCount
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

	var aliases []string
	if byID, err := loadAliasesForImages(g, []int64{imageID}); err != nil {
		logx.Warnf("buildImageResponse aliases: %v", err)
	} else {
		aliases = byID[imageID]
	}
	var tags []imageTagJSON
	if byID, err := loadTagsForImages(g, []int64{imageID}); err != nil {
		logx.Warnf("buildImageResponse tags: %v", err)
	} else {
		tags = byID[imageID]
	}

	resp := makeImageResponse(g, img, tags, aliases)
	if cols, err := gallery.CollectionsForImage(g.DB, imageID); err != nil {
		logx.Warnf("buildImageResponse collections: %v", err)
	} else {
		for _, c := range cols {
			resp.Collections = append(resp.Collections, collectionJSON{Name: c.Name, Order: c.Order})
		}
	}
	if srcs, err := gallery.SourcesForImage(g.DB, imageID); err != nil {
		logx.Warnf("buildImageResponse sources: %v", err)
	} else {
		for _, s := range srcs {
			resp.Sources = append(resp.Sources, sourceJSON{Site: s.Site, PostID: s.PostID, URL: s.URL, Commentary: s.Commentary})
		}
	}
	if anns, err := gallery.AnnotationsForImage(g.DB, imageID); err != nil {
		logx.Warnf("buildImageResponse annotations: %v", err)
	} else {
		for _, a := range anns {
			resp.Annotations = append(resp.Annotations, annotationJSON{Site: a.Site, PostID: a.PostID, X: a.X, Y: a.Y, W: a.W, H: a.H, Body: a.Body})
		}
	}
	return &resp, nil
}

func (h *Handler) getImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}

	resp, err := h.buildImageResponse(g, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// patchImage handles PATCH /api/v1/images/{id}: edits the operator-
// editable fields source, url, collection, collection_order,
// is_favorited, and is_inbox. Pointer fields carry presence: an absent
// (or JSON null) field is left alone, a present one is written. An empty
// string clears a text field; clearing collection nulls a stranded
// collection_order in the same write (mirroring the detail-page editor)
// unless an order is supplied alongside. To clear collection_order on
// its own, clear the collection. Returns the updated image object.
func (h *Handler) patchImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	if !imageExists(g, id) {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}

	var body struct {
		Source          *string `json:"source"`
		URL             *string `json:"url"`
		Collection      *string `json:"collection"`
		CollectionOrder *int    `json:"collection_order"`
		IsFavorited     *bool   `json:"is_favorited"`
		IsInbox         *bool   `json:"is_inbox"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	updates := []string{}
	args := []any{}
	cacheAffecting := false

	// source / url edit the primary origin. Fill the unpatched half from the
	// current primary (the scalar mirror) so a one-field PATCH keeps the
	// other, then apply through SetPrimarySource after the main UPDATE.
	setSrc := false
	var srcSite, srcURL string
	if body.Source != nil || body.URL != nil {
		if err := g.DB.Read.QueryRow(`SELECT source, url FROM images WHERE id = ?`, id).Scan(&srcSite, &srcURL); err != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		if body.Source != nil {
			s := strings.TrimSpace(*body.Source)
			if err := validateImageSource(s); err != nil {
				apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			srcSite = s
		}
		if body.URL != nil {
			u := strings.TrimSpace(*body.URL)
			if err := validateImageURL(u); err != nil {
				apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			srcURL = u
		}
		setSrc = true
		cacheAffecting = true
	}
	// Collection / collection_order map onto the home membership; the
	// resolved label and order are applied through SetHomeCollection after
	// the main UPDATE so image_collections stays in sync. An absent order
	// next to a present label keeps the stored position (rename is sticky).
	setHome := false
	var homeName string
	var homeOrder *int
	if body.Collection != nil || body.CollectionOrder != nil {
		var curSeries string
		var curOrder sql.NullInt64
		if err := g.DB.Read.QueryRow(`SELECT series, series_order FROM images WHERE id = ?`, id).Scan(&curSeries, &curOrder); err != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		homeName = curSeries
		if body.Collection != nil {
			c := strings.TrimSpace(*body.Collection)
			if err := validateImageCollection(c); err != nil {
				apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
			homeName = c
		}
		if body.CollectionOrder != nil {
			n := *body.CollectionOrder
			if n < 1 {
				apiError(w, http.StatusBadRequest, "invalid_request", "collection_order must be 1 or higher")
				return
			}
			if homeName == "" {
				apiError(w, http.StatusBadRequest, "invalid_request", "collection_order requires a non-empty collection")
				return
			}
			homeOrder = &n
		} else if homeName != "" && curOrder.Valid {
			v := int(curOrder.Int64)
			homeOrder = &v
		}
		setHome = true
		cacheAffecting = true
	}
	if body.IsFavorited != nil {
		updates = append(updates, "is_favorited = ?")
		args = append(args, boolToInt(*body.IsFavorited))
		cacheAffecting = true
	}
	if body.IsInbox != nil {
		updates = append(updates, "is_inbox = ?")
		args = append(args, boolToInt(*body.IsInbox))
		cacheAffecting = true
	}
	if len(updates) == 0 && !setHome && !setSrc {
		apiError(w, http.StatusBadRequest, "invalid_request", "no editable fields supplied")
		return
	}

	if len(updates) > 0 {
		args = append(args, id)
		if _, err := g.DB.Write.Exec(`UPDATE images SET `+strings.Join(updates, ", ")+` WHERE id = ?`, args...); err != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}
	if setHome {
		if err := gallery.SetHomeCollection(g.DB, id, homeName, homeOrder); err != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}
	if setSrc {
		if err := gallery.SetPrimarySource(g.DB, id, srcSite, srcURL); err != nil {
			if errors.Is(err, gallery.ErrSourceIdentityExists) {
				apiError(w, http.StatusConflict, "conflict", err.Error())
				return
			}
			apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
	}
	// source, collection, favorite, and inbox all feed cached aggregates
	// (the sidebar source / collection lists, fav/inbox counts, and the
	// match-id cache keyed on fav:/inbox:), so invalidate on any of them.
	if cacheAffecting {
		g.invalidate()
	}

	resp, err := h.buildImageResponse(g, id)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// createInput carries one create request's parsed and validated fields,
// whichever mode supplied them.
type createInput struct {
	imgPath         string
	initialTags     []string
	folder          string
	autotag         bool
	taggerName      string
	via             string              // caller-supplied label; stored on images.origin and inherited by initial tags
	source          string              // operator-edited provenance label; set on the new row when non-empty
	url             string              // canonical web URL; set on the new row when non-empty
	md5             string              // md5 the source claimed; recorded on the origin row as the audit trail
	commentary      string              // artist commentary for the pushed source; folded in on create/merge
	notes           []models.Annotation // positional note boxes for the pushed source
	collection      string              // collection label (images.series); set on the new row when non-empty
	collectionOrder *int                // 1-based position within collection; nil = unset
	uploadedToDisk  bool                // true when we wrote the file ourselves (multipart)
}

// parseCreateMultipart reads mode A (multipart upload): validates the
// fields, then writes the file straight to its final destination so the
// watcher sees the real filename. ok=false means the error response was
// already written.
func (h *Handler) parseCreateMultipart(w http.ResponseWriter, r *http.Request, g Gallery) (createInput, bool) {
	var in createInput
	maxBytes := int64(h.cfg.Gallery.MaxFileSizeMB) * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+4096)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, "file_too_large", "file exceeds max size")
		return in, false
	}
	file, fh, err := r.FormFile("file")
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "missing file field")
		return in, false
	}
	defer func() { _ = file.Close() }()

	in.folder = strings.TrimSpace(r.FormValue("folder"))
	in.autotag = isTrue(r.FormValue("autotag"))
	in.taggerName = strings.TrimSpace(r.FormValue("tagger_name"))
	in.via = strings.TrimSpace(r.FormValue("via"))
	if err := validateVia(in.via); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return in, false
	}
	in.source = strings.TrimSpace(r.FormValue("source"))
	in.url = strings.TrimSpace(r.FormValue("url"))
	in.md5 = strings.TrimSpace(r.FormValue("md5"))
	in.commentary = strings.TrimSpace(r.FormValue("commentary"))
	in.notes = parseNotesField(r.FormValue("notes"))
	in.collection = strings.TrimSpace(r.FormValue("collection"))
	if raw := strings.TrimSpace(r.FormValue("collection_order")); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", "collection_order must be an integer")
			return in, false
		}
		in.collectionOrder = &n
	}
	if err := validateCreateProvenance(in.source, in.url, in.md5, in.collection, in.commentary, in.collectionOrder); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return in, false
	}

	destDir, destErr := gallery.ResolveSubdir(g.GalleryPath, in.folder)
	if destErr != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", destErr.Error())
		return in, false
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to create folder: "+err.Error())
		return in, false
	}

	// Write directly to the final destination so the watcher sees
	// the real filename rather than a temp one (which would get
	// marked missing as soon as we renamed it).
	dstPath := gallery.UniqueDestPath(destDir, fh.Filename)
	dst, err := os.Create(dstPath)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to create destination file")
		return in, false
	}
	if _, err := io.Copy(dst, file); err != nil {
		_ = dst.Close()
		_ = os.Remove(dstPath)
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to save upload")
		return in, false
	}
	_ = dst.Close()

	if tagsJSON := r.FormValue("tags"); tagsJSON != "" {
		_ = json.Unmarshal([]byte(tagsJSON), &in.initialTags)
	}
	in.imgPath = dstPath
	in.uploadedToDisk = true
	return in, true
}

// parseCreateJSON reads mode B (path reference): validates the fields and
// constrains the caller-supplied path to the gallery root. ok=false means
// the error response was already written.
func (h *Handler) parseCreateJSON(w http.ResponseWriter, r *http.Request, g Gallery) (createInput, bool) {
	var in createInput
	var body struct {
		Path            string           `json:"path"`
		Tags            []string         `json:"tags"`
		Folder          string           `json:"folder"`
		Autotag         bool             `json:"autotag"`
		TaggerName      string           `json:"tagger_name"`
		Via             string           `json:"via"`
		Source          string           `json:"source"`
		URL             string           `json:"url"`
		MD5             string           `json:"md5"`
		Commentary      string           `json:"commentary"`
		Notes           []annotationJSON `json:"notes"`
		Collection      string           `json:"collection"`
		CollectionOrder *int             `json:"collection_order"`
	}
	if !decodeJSON(w, r, &body) {
		return in, false
	}
	if body.Path == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "path is required")
		return in, false
	}
	in.imgPath = body.Path
	in.initialTags = body.Tags
	in.folder = strings.TrimSpace(body.Folder)
	in.autotag = body.Autotag
	in.taggerName = strings.TrimSpace(body.TaggerName)
	in.via = strings.TrimSpace(body.Via)
	if err := validateVia(in.via); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return in, false
	}
	in.source = strings.TrimSpace(body.Source)
	in.url = strings.TrimSpace(body.URL)
	in.md5 = strings.TrimSpace(body.MD5)
	in.commentary = strings.TrimSpace(body.Commentary)
	in.notes = annotationsFromInput(body.Notes)
	in.collection = strings.TrimSpace(body.Collection)
	in.collectionOrder = body.CollectionOrder
	if err := validateCreateProvenance(in.source, in.url, in.md5, in.collection, in.commentary, in.collectionOrder); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return in, false
	}

	// Relative path + folder: resolve under <gallery>/<folder>/<path>.
	// Absolute paths go through the gate just below.
	if in.folder != "" && !filepath.IsAbs(in.imgPath) {
		destDir, destErr := gallery.ResolveSubdir(g.GalleryPath, in.folder)
		if destErr != nil {
			apiError(w, http.StatusBadRequest, "invalid_request", destErr.Error())
			return in, false
		}
		in.imgPath = filepath.Join(destDir, in.imgPath)
	}

	// Constrain the caller-supplied path to the gallery root. The
	// operator owns the gallery folder and the API is the operator-
	// facing surface, so an ingest-by-path that quietly registers a
	// row pointing outside the gallery would have a later
	// DELETE /api/v1/images/{id} unlink files the operator never
	// meant to manage. Mirror the upload form's containment.
	absPath, absErr := filepath.Abs(in.imgPath)
	if absErr != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid path")
		return in, false
	}
	galleryAbs, gErr := filepath.Abs(g.GalleryPath)
	if gErr != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "gallery path unresolvable")
		return in, false
	}
	if !gallery.PathInside(galleryAbs, absPath) {
		apiError(w, http.StatusBadRequest, "invalid_request", "path must be inside the gallery root")
		return in, false
	}
	in.imgPath = absPath

	// Translate the common client-side mistake (path doesn't exist)
	// to a 400 with a sanitised message so the response body doesn't
	// echo the operator's filesystem layout and the status class
	// reflects the caller error rather than a server failure.
	if _, statErr := os.Stat(in.imgPath); os.IsNotExist(statErr) {
		apiError(w, http.StatusBadRequest, "not_found", "file not found")
		return in, false
	}
	return in, true
}

// createImage handles POST /api/v1/images. Accepts either multipart
// (with `file`, `tags`, `folder`, `autotag`, `tagger_name`, `via`) or
// JSON (with `path`, `tags`, `folder`, `autotag`, `tagger_name`,
// `via`). In JSON mode `folder` only applies to relative paths;
// absolute paths are used verbatim. `via` lands on `images.origin` and
// is attached to each initial tag's `image_tags.tagger_name`. The
// optional provenance fields `source`, `url`, `collection`, and
// `collection_order` are written onto the new row; a duplicate-SHA
// insert instead merges the pushed source, tags, commentary and notes
// into the existing row (collection fields are ignored there).
func (h *Handler) createImage(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var in createInput
	if isMultipart(r.Header.Get("Content-Type")) {
		in, ok = h.parseCreateMultipart(w, r, g)
	} else {
		in, ok = h.parseCreateJSON(w, r, g)
	}
	if !ok {
		return
	}

	// Enforce gallery.max_file_size_mb for both modes. Multipart also
	// has MaxBytesReader; this mainly guards the JSON path-reference
	// mode where the caller supplies an absolute path.
	if maxMB := h.cfg.Gallery.MaxFileSizeMB; maxMB > 0 {
		if info, err := os.Stat(in.imgPath); err == nil {
			if info.Size() > int64(maxMB)*1024*1024 {
				if in.uploadedToDisk {
					_ = os.Remove(in.imgPath)
				}
				apiError(w, http.StatusRequestEntityTooLarge, "file_too_large",
					fmt.Sprintf("file exceeds max size (%d MB)", maxMB))
				return
			}
		}
	}

	fileType, ftErr := gallery.DetectFileType(in.imgPath)
	if ftErr != nil {
		if in.uploadedToDisk {
			_ = os.Remove(in.imgPath)
		}
		apiError(w, http.StatusBadRequest, "unsupported_type", "unsupported or unrecognised file type")
		return
	}
	// DetectFileType only checks the extension, so a follow-up
	// DecodeConfig confirms the bytes parse as an image before the row
	// lands in the DB. cbz integrity is verified inside Ingest and
	// video frames decode later via ffmpeg, so both buckets skip this.
	if !gallery.IsVideoType(fileType) && fileType != models.FileTypeCBZ {
		if !canDecodeImage(in.imgPath) {
			// ffmpeg decodes JPEGs with a chroma subsampling ratio Go's
			// image/jpeg refuses (some CDN resizers emit these); re-encode the
			// uploaded file in place so the dimension probe, thumbnail, and
			// phash that follow can read it. Only a file we just wrote is
			// rewritten, never an operator's path-referenced original.
			rescued := in.uploadedToDisk && fileType == models.FileTypeJPEG &&
				gallery.NormalizeImage(in.imgPath) == nil && canDecodeImage(in.imgPath)
			if !rescued {
				if in.uploadedToDisk {
					_ = os.Remove(in.imgPath)
				}
				apiError(w, http.StatusUnsupportedMediaType, "unsupported_type", "file does not decode as an image")
				return
			}
		}
	}

	// Caller-supplied `via` wins; otherwise multipart defaults to
	// "upload" and JSON path-reference defaults to "ingest".
	origin := in.via
	if origin == "" {
		if in.uploadedToDisk {
			origin = models.OriginUpload
		} else {
			origin = models.OriginIngest
		}
	}

	img, isDuplicate, err := gallery.Ingest(g.DB, g.GalleryPath, g.ThumbnailsPath, in.imgPath, fileType, origin)
	if err != nil {
		if in.uploadedToDisk {
			_ = os.Remove(in.imgPath)
		}
		logx.Warnf("api createImage ingest: %v", err)
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	g.invalidate()
	if isDuplicate {
		// A multipart upload just wrote a second copy of bytes the gallery
		// already holds; keeping it leaves a redundant file (and alias) on
		// disk. Drop both so a re-push is metadata-only. A JSON path-reference
		// duplicate is the operator's own second path, so it stays recorded as
		// an alias. Either way the pushed tags and provenance fold into the
		// existing row instead of being discarded (issue #6).
		aliasAdded := true
		if in.uploadedToDisk {
			aliasAdded = false
			if _, delErr := g.DB.Write.Exec(
				`DELETE FROM image_paths WHERE image_id = ? AND path = ? AND is_canonical = 0`,
				img.ID, in.imgPath,
			); delErr != nil {
				logx.Warnf("api createImage drop duplicate alias: %v", delErr)
			}
			if rmErr := os.Remove(in.imgPath); rmErr != nil && !os.IsNotExist(rmErr) {
				logx.Warnf("api createImage remove duplicate upload %q: %v", in.imgPath, rmErr)
			}
		}
		sum, tagWarnings, mergeErr := h.mergeSource(g, img.ID, in.source, in.url, in.md5, in.initialTags)
		if mergeErr != nil {
			logx.Warnf("api createImage merge: %v", mergeErr)
			apiError(w, http.StatusInternalServerError, "internal_error", "duplicate detected but the merge failed: "+mergeErr.Error())
			return
		}
		if in.source != "" && in.commentary != "" {
			if err := gallery.SetSourceCommentary(g.DB, img.ID, in.source, "", in.commentary); err != nil {
				logx.Warnf("api createImage commentary: %v", err)
				apiError(w, http.StatusInternalServerError, "internal_error", "duplicate detected but the merge failed: "+err.Error())
				return
			}
		}
		if in.source != "" && len(in.notes) > 0 {
			if err := gallery.ReplaceSourceAnnotations(g.DB, img.ID, in.source, "", in.notes); err != nil {
				logx.Warnf("api createImage annotations: %v", err)
				apiError(w, http.StatusInternalServerError, "internal_error", "duplicate detected but the merge failed: "+err.Error())
				return
			}
		}
		g.invalidate()
		resp, respErr := h.buildImageResponse(g, img.ID)
		if respErr != nil {
			apiError(w, http.StatusInternalServerError, "internal_error", "failed to build response")
			return
		}
		envelope := map[string]any{
			"image":       resp,
			"alias_added": aliasAdded,
			"merge":       sum,
		}
		if len(tagWarnings) > 0 {
			envelope["tag_warnings"] = tagWarnings
		}
		writeJSON(w, http.StatusOK, envelope)
		return
	}

	// A freshly-created row records its provenance directly; the duplicate
	// path above merges instead.
	if err := applyCreateProvenance(g, img.ID, in.source, in.url, in.md5, in.collection, in.commentary, in.collectionOrder); err != nil {
		logx.Warnf("api createImage provenance: %v", err)
		apiError(w, http.StatusInternalServerError, "internal_error", "failed to set provenance fields")
		return
	}
	if in.source != "" && len(in.notes) > 0 {
		if err := gallery.ReplaceSourceAnnotations(g.DB, img.ID, in.source, "", in.notes); err != nil {
			logx.Warnf("api createImage annotations: %v", err)
		}
	}

	// Imported tags are attributed to their source so each source owns a
	// prunable slice; a sourceless push keeps the caller's via label.
	tagVia := in.via
	if in.source != "" {
		tagVia = in.source
	}
	tagWarnings := h.applyInitialTags(g, img.ID, in.initialTags, tagVia)

	var autotagNote string
	if in.autotag {
		if !tagger.IsAvailable(h.cfg) {
			autotagNote = "autotag skipped: tagger not available"
		} else {
			selected, selErr := h.selectedTaggers(g.Name, in.taggerName)
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (h *Handler) deleteImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}

	result, err := gallery.DeleteImage(g.DB, g.GalleryPath, g.ThumbnailsPath, id, g.TagSvc.RemoveAllTagsFromImage, relationsOnDelete(g.RelationsSvc))
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	g.invalidate()

	// Empty-source-folder cleanup is opt-in via ?delete_empty_folder=true.
	// Operators create folders deliberately, so a delete leaves an emptied
	// folder in place by default, matching the UI (§7.6); when asked, prune
	// it and report the removal in a structured 200.
	folderRemoved := false
	if r.URL.Query().Get("delete_empty_folder") == "true" && !result.IsMissing && result.FolderPath != "" {
		fullFolderPath := filepath.Join(g.GalleryPath, result.FolderPath)
		if !gallery.PathInside(g.GalleryPath, fullFolderPath) {
			logx.Warnf("api deleteImage: refusing to remove folder %q outside gallery root %q", fullFolderPath, g.GalleryPath)
		} else if entries, readErr := os.ReadDir(fullFolderPath); readErr == nil && len(entries) == 0 {
			if removeErr := os.Remove(fullFolderPath); removeErr == nil {
				folderRemoved = true
			} else {
				logx.Warnf("api deleteImage: failed to remove empty folder %q: %v", fullFolderPath, removeErr)
			}
		}
	}

	if folderRemoved {
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
	if sortStr == "order" {
		sq.OrderCollection = search.PinnedCollectionName(expr)
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
		images = append(images, makeImageResponse(g, img, tagsByID[img.ID], aliasesByID[img.ID]))
	}

	writePage(w, result.Page, result.Limit, result.Total, images)
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
	placeholders, args := db.InPlaceholders(ids)
	rows, err := g.DB.Read.Query(
		`SELECT image_id, path FROM image_paths
		 WHERE is_canonical = 0 AND image_id IN (`+placeholders+`)
		 ORDER BY image_id, id`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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
	placeholders, args := db.InPlaceholders(ids)
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
	defer func() { _ = rows.Close() }()
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
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
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
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
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
	if !decodeJSON(w, r, &body) {
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
	g.invalidate()

	h.writeImageTagsResponse(w, g, id, tagWarnings)
}

// writeImageTagsResponse is the shared post-mutation tail of the tag add /
// remove handlers: the image's tag list, wrapped with warnings when any
// token failed to resolve.
func (h *Handler) writeImageTagsResponse(w http.ResponseWriter, g Gallery, id int64, tagWarnings []string) {
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
	// Tags applied through the REST API are attributed to "api" when the
	// caller gives no explicit source, so they read with an api origin on
	// the tags page (and an api source group on the detail page) rather
	// than looking like anonymous UI adds. A caller-supplied via still
	// wins and is recorded verbatim.
	if via == "" {
		via = "api"
	}
	tagIDs, warnings := h.resolveTagNames(g, rawTags)
	if len(tagIDs) > 0 {
		if _, err := g.TagSvc.AddTagsToOneImage(imgID, tagIDs, via); err != nil {
			warnings = append(warnings, "apply tags: "+err.Error())
		}
	}
	return warnings
}

// resolveTagNames turns raw `bare` / `category:bare` tokens into tag ids,
// creating missing rows. Per-token failures land in warnings without
// aborting the batch.
func (h *Handler) resolveTagNames(g Gallery, rawTags []string) ([]int64, []string) {
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
	return tagIDs, warnings
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
	g, id, ok := h.galleryAndID(w, r)
	if !ok {
		return
	}
	if !imageExists(g, id) {
		apiError(w, http.StatusNotFound, "not_found", "image not found")
		return
	}

	var body struct {
		Tags []string `json:"tags"`
	}
	if !decodeJSON(w, r, &body) {
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
	g.invalidate()

	h.writeImageTagsResponse(w, g, id, tagWarnings)
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
	defer func() { _ = rows.Close() }()
	ids, err := db.ScanIDs(rows)
	if err != nil {
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
