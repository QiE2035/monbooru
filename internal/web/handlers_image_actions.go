package web

import (
	"database/sql"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
)

// ratingCeilingPost sets or clears the monbooru_rating_ceiling cookie.
// Posting level=explicit (or any out-of-enum value) clears the cookie so
// the empty-storage steady state means "no ceiling".
func (s *Server) ratingCeilingPost(w http.ResponseWriter, r *http.Request) {
	level := r.URL.Query().Get("level")
	if level == "" {
		level = r.FormValue("level")
	}
	writeRatingCookie(w, level)
	w.WriteHeader(http.StatusNoContent)
}

// toggleBoolColumn flips a 0/1 column in images via RETURNING, drops
// the per-gallery caches, and writes one of two pre-rendered button
// fragments depending on the new value. Shared by toggleFavorite and
// toggleInbox.
func (s *Server) toggleBoolColumn(w http.ResponseWriter, r *http.Request, column, onHTML, offHTML string) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var newVal int
	if err := s.db().Write.QueryRow(
		`UPDATE images SET `+column+` = 1 - `+column+` WHERE id = ? RETURNING `+column, id,
	).Scan(&newVal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Cached match-id sets keyed off the toggled column (`?q=fav:true`,
	// `?q=inbox:true`) and the cached inbox count both go stale on flip.
	if cx := s.Active(); cx != nil {
		cx.InvalidateCaches()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if newVal == 1 {
		w.Write([]byte(onHTML))
	} else {
		w.Write([]byte(offHTML))
	}
}

func (s *Server) toggleFavorite(w http.ResponseWriter, r *http.Request) {
	s.toggleBoolColumn(w, r, "is_favorited",
		`<button type="submit" id="fav-btn" class="btn-fav active" title="Unfavorite">♥</button>`,
		`<button type="submit" id="fav-btn" class="btn-fav" title="Favorite">♡</button>`,
	)
}

// toggleInbox returns the swap HTML for the inbox button. The title
// names the click action (Archive / Send to inbox); the label names
// the row's current state (In inbox / Archived) so the button reads
// as "this is what it is" with the action surfaced on hover.
func (s *Server) toggleInbox(w http.ResponseWriter, r *http.Request) {
	s.toggleBoolColumn(w, r, "is_inbox",
		`<button type="submit" id="inbox-btn" class="btn-inbox active" title="Archive (i)">✱ In inbox</button>`,
		`<button type="submit" id="inbox-btn" class="btn-inbox" title="Send to inbox (i)">✱ Archived</button>`,
	)
}

func (s *Server) deleteImage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}

	backQ := r.URL.Query().Get("back_q")
	backSort := r.URL.Query().Get("back_sort")
	backOrder := r.URL.Query().Get("back_order")
	backPage := r.URL.Query().Get("back_page")
	backSeed := r.URL.Query().Get("back_seed")

	// When the caller arrived via a Similar-images click the URL carries
	// ref=<sourceID> and the back_* params describe the source's gallery
	// context, not a search the current image is part of. Snapshotting the
	// valid source id up front keeps the post-delete redirect aimed at the
	// original image instead of jumping to an arbitrary neighbour of the
	// unrelated back_* search.
	var refID *int64
	if refStr := r.URL.Query().Get("ref"); refStr != "" {
		if parsed, err := strconv.ParseInt(refStr, 10, 64); err == nil && parsed != id {
			refID = &parsed
		}
	}

	// Compute the neighbour before the delete so we don't miss it once the
	// current row is gone. When the caller carried back_* params the detail
	// page had a search context; we keep the user in that stream by jumping
	// to the adjacent image instead of bouncing back to the gallery. Ref
	// takes precedence over adjacency: the current image may not even be in
	// the referring search.
	var prevID, nextID *int64
	if refID == nil && (backSort != "" || backQ != "") {
		sortStr := backSort
		if sortStr == "" {
			sortStr = "newest"
		}
		orderStr := backOrder
		if orderStr == "" {
			orderStr = "desc"
		}
		prevID, nextID = s.findAdjacentImages(id, backQ, sortStr, orderStr, backSeed, resolveCeiling(r, s.Active()))
	}

	result, err := gallery.DeleteImage(s.db(), s.galleryPath(), s.thumbnailsPath(), id, s.tagSvc().RemoveAllTagsFromImage, s.onImageDeleteCallback())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.Active().InvalidateCaches()

	if !result.IsMissing {
		gallery.DeleteEmptyFolderIfEmpty(s.galleryPath(), result.FolderPath)
	}

	redirectURL := ""
	switch {
	case refID != nil:
		redirectURL = buildDetailURL(*refID, backQ, backSort, backOrder, backPage, backSeed)
	case nextID != nil:
		redirectURL = buildDetailURL(*nextID, backQ, backSort, backOrder, backPage, backSeed)
	case prevID != nil:
		redirectURL = buildDetailURL(*prevID, backQ, backSort, backOrder, backPage, backSeed)
	default:
		redirectURL = buildGalleryURL(backQ, backSort, backOrder, backPage, backSeed)
	}

	if isHTMXRequest(r) {
		// Ref case: the user arrived here via a Similar-images click, which
		// itself may be any depth into a chain. Redirecting to the source
		// would push a fresh history entry that drops the ref chain - the
		// post-delete source page then has no data-ref and Escape escapes
		// straight to the gallery. Fire a delete-go-back trigger instead so
		// the client can prefer history.back(), landing on the source's
		// original URL (with its own ref intact) and keeping the chain
		// walkable. The fallback URL handles the cold-load case where the
		// browser has no predecessor (direct link, bookmarked tab).
		if refID != nil {
			w.Header().Set("HX-Trigger", fmt.Sprintf(`{"delete-go-back":{"fallback":%q}}`, redirectURL))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("HX-Redirect", redirectURL)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

func (s *Server) promoteCanonical(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	newCanonical := r.FormValue("path")
	if newCanonical == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}

	// Refuse to promote anything that isn't already a tracked alias of this
	// image. Without this check, a user could write an arbitrary string
	// into images.canonical_path and coerce serveImageFile into serving a
	// sibling file whose path happens to live inside the gallery root.
	var aliasExists int
	if err := s.db().Read.QueryRow(
		`SELECT COUNT(*) FROM image_paths WHERE image_id = ? AND path = ?`,
		id, newCanonical,
	).Scan(&aliasExists); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if aliasExists == 0 {
		http.Error(w, "path is not an alias of this image", http.StatusBadRequest)
		return
	}

	newFolder := gallery.FolderPath(s.galleryPath(), newCanonical)

	tx, err := s.db().Write.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE image_paths SET is_canonical = 0 WHERE image_id = ?`, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(
		`UPDATE image_paths SET is_canonical = 1 WHERE image_id = ? AND path = ?`,
		id, newCanonical,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(
		`UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`,
		newCanonical, newFolder, id,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// folder_path drives folder:/folderonly: search and the cached
	// folder tree; promoting a different canonical can land the image
	// in a different folder, so the per-gallery and adjacency caches
	// have to drop.
	s.Active().InvalidateCaches()

	http.Redirect(w, r, "/images/"+idStr, http.StatusSeeOther)
}

const (
	maxExternalSourceLen = 200
	maxExternalURLLen    = 2048
)

// updateExternal writes the operator-edited images.source / images.url
// / collection / collection_order fields. The form may carry any
// subset; an absent key leaves the existing value alone, while an empty
// key clears it. The detail-page dialogs each ship only their own field
// (Source, URL, Collection, Order), so opening one and saving never
// clobbers the others. URLs must start with http:// or https:// so the
// rendered <a href> survives both the html/template scheme sanitiser
// and the explicit allowlist below.
//
// The DB column is still named `series` (kept for schema stability
// across the rename); the form keys and validation messages carry the
// new "collection" vocabulary.
//
// HTMX callers (the detail-page dialogs) get a flash-err fragment on
// validation failures so the dialog stays open with the user's input
// intact, and HX-Refresh on success so the detail page reloads with
// the new value rendered. Non-HTMX callers see the legacy plain text
// + 303 redirect.
func (s *Server) updateExternal(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		externalErr(w, r, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		externalErr(w, r, "bad form", http.StatusBadRequest)
		return
	}

	updates := []string{}
	args := []any{}
	if r.Form.Has("source") {
		src := strings.TrimSpace(r.FormValue("source"))
		if len(src) > maxExternalSourceLen {
			externalErr(w, r, fmt.Sprintf("source too long (max %d chars)", maxExternalSourceLen), http.StatusBadRequest)
			return
		}
		updates = append(updates, "source = ?")
		args = append(args, src)
	}
	if r.Form.Has("url") {
		raw := strings.TrimSpace(r.FormValue("url"))
		if raw != "" {
			if len(raw) > maxExternalURLLen {
				externalErr(w, r, fmt.Sprintf("url too long (max %d chars)", maxExternalURLLen), http.StatusBadRequest)
				return
			}
			lower := strings.ToLower(raw)
			if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
				externalErr(w, r, "url must start with http:// or https://", http.StatusBadRequest)
				return
			}
		}
		updates = append(updates, "url = ?")
		args = append(args, raw)
	}
	collectionChanged := false
	hasCollection := r.Form.Has("collection")
	collectionVal := strings.TrimSpace(r.FormValue("collection"))
	if hasCollection {
		// Cap mirrors images.source - the field is small free-form text
		// and search hits an exact-match index either way.
		if len(collectionVal) > maxExternalSourceLen {
			externalErr(w, r, fmt.Sprintf("collection too long (max %d chars)", maxExternalSourceLen), http.StatusBadRequest)
			return
		}
		updates = append(updates, "series = ?")
		args = append(args, collectionVal)
		collectionChanged = true
		// Symmetry with the "order has no collection to anchor" reject:
		// clearing the collection while leaving an order behind would
		// leave a `#N` chip stranded next to "(none)" and a row the
		// `collection:` search filter never surfaces. Null the order in
		// the same write unless the form is also setting one explicitly.
		if collectionVal == "" && !r.Form.Has("collection_order") {
			updates = append(updates, "series_order = ?")
			args = append(args, nil)
		}
	}
	if r.Form.Has("collection_order") {
		raw := strings.TrimSpace(r.FormValue("collection_order"))
		var val any
		if raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				externalErr(w, r, "collection_order must be an integer or empty", http.StatusBadRequest)
				return
			}
			if n < 1 {
				externalErr(w, r, "collection_order must be 1 or higher", http.StatusBadRequest)
				return
			}
			// An order without a collection anchor is nonsense - the
			// detail page renders "(none) #5" and collection: search
			// never surfaces it. Check the incoming value when present,
			// fall back to the stored row otherwise.
			if hasCollection {
				if collectionVal == "" {
					externalErr(w, r, "collection_order requires a non-empty collection label", http.StatusBadRequest)
					return
				}
			} else {
				var existing string
				if err := s.db().Read.QueryRow(`SELECT series FROM images WHERE id = ?`, id).Scan(&existing); err != nil {
					externalErr(w, r, "image not found", http.StatusNotFound)
					return
				}
				if strings.TrimSpace(existing) == "" {
					externalErr(w, r, "collection_order requires a non-empty collection label", http.StatusBadRequest)
					return
				}
			}
			val = n
		}
		updates = append(updates, "series_order = ?")
		args = append(args, val)
		collectionChanged = true
	}
	if len(updates) == 0 {
		externalErr(w, r, "no fields supplied", http.StatusBadRequest)
		return
	}

	args = append(args, id)
	res, err := s.db().Write.Exec(
		`UPDATE images SET `+strings.Join(updates, ", ")+` WHERE id = ?`, args...,
	)
	if err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		externalErr(w, r, "image not found", http.StatusNotFound)
		return
	}
	// images.source/url feed the exact-match `source:` filter and the
	// detail-page render, but not the cached folder/source-counts
	// aggregates - which key off source_type, not source. The collection
	// label IS surfaced in the cached SeriesCounts list, though, so
	// invalidate when it changed.
	if collectionChanged {
		s.Active().InvalidateCaches()
	}

	if isHTMXRequest(r) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/images/"+idStr, http.StatusSeeOther)
}

// externalErr renders the validation error inline for HTMX callers
// (so the dialog can keep its target slot up to date) and falls back
// to plain http.Error for non-HTMX callers.
func externalErr(w http.ResponseWriter, r *http.Request, msg string, code int) {
	if isHTMXRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(msg) + `</div>`))
		return
	}
	http.Error(w, msg, code)
}

func (s *Server) deleteAlias(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	pathIDStr := r.PathValue("pathID")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	pathID, err := strconv.ParseInt(pathIDStr, 10, 64)
	if err != nil {
		http.Error(w, "bad pathID", http.StatusBadRequest)
		return
	}

	// Refuse to remove the canonical path; callers must promote another alias
	// first, otherwise the image would lose its on-disk reference entirely.
	var isCanon int
	var aliasPath string
	if err := s.db().Read.QueryRow(
		`SELECT is_canonical, path FROM image_paths WHERE id = ? AND image_id = ?`, pathID, id,
	).Scan(&isCanon, &aliasPath); err != nil {
		http.Error(w, "alias path not found", http.StatusNotFound)
		return
	}
	if isCanon == 1 {
		http.Error(w, "cannot delete canonical path", http.StatusBadRequest)
		return
	}

	if _, err := s.db().Write.Exec(`DELETE FROM image_paths WHERE id = ?`, pathID); err != nil {
		logx.Warnf("delete alias row %d: %v", pathID, err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	if aliasPath != "" {
		// Defense-in-depth: every legitimate insert into image_paths goes
		// through ingest / sync / rebaseImagePaths and stays under the
		// gallery root, so the gate below is a no-op today. A future
		// compatibility translator that forgets the relative-path rule
		// would otherwise let this os.Remove unlink any file the process
		// can reach.
		if err := unlinkUnderGallery(s.galleryPath(), aliasPath); err != nil {
			logx.Warnf("delete alias file %q: %v", aliasPath, err)
		}
	}

	if isHTMXRequest(r) {
		// Empty body for HTMX outerHTML swap - removes the row.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(""))
		return
	}
	http.Redirect(w, r, "/images/"+idStr, http.StatusSeeOther)
}

// unlinkUnderGallery is os.Remove gated on gallery.PathInside so a
// stray absolute path in image_paths can never let an alias-deletion
// or duplicate-prune handler unlink files outside the active gallery.
func unlinkUnderGallery(galleryRoot, victim string) error {
	galleryAbs, err := filepath.Abs(galleryRoot)
	if err != nil {
		return fmt.Errorf("resolve gallery root: %w", err)
	}
	victimAbs, err := filepath.Abs(victim)
	if err != nil {
		return fmt.Errorf("resolve victim: %w", err)
	}
	if !gallery.PathInside(galleryAbs, victimAbs) {
		return fmt.Errorf("refuse: path %q is outside gallery root", victim)
	}
	if err := os.Remove(victim); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// moveImage relocates the one image at {id} into the requested folder. A `move`
// job is used even for single-image moves to reuse the watcher suppression
// pattern from batch moves; the job is brief and auto-dismisses like any other.
func (s *Server) moveImage(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	targetFolder := strings.TrimSpace(r.FormValue("folder"))

	if err := s.jobs.Start(models.JobTypeMove); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}

	res, moveErr := gallery.MoveImage(s.db(), s.galleryPath(), id, targetFolder)
	if moveErr != nil {
		s.jobs.Fail(moveErr.Error())
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(moveErr.Error()) + `</div>`))
		return
	}
	if res.OldFolderPath != res.NewFolderPath && res.OldFolderPath != "" {
		gallery.DeleteEmptyFolderIfEmpty(s.galleryPath(), res.OldFolderPath)
	}
	s.Active().InvalidateCaches()
	s.jobs.Complete("Moved image.")

	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/images/"+idStr)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/images/"+idStr, http.StatusSeeOther)
}

// nextPrefix returns the smallest string strictly greater than prefix
// under the default BINARY collation: prefix with its last byte
// incremented, or prefix+"\xff" when the last byte already saturates.
// Used by folder autocomplete to bound a `>=, <` range scan instead of
// a LIKE-trailing-wildcard scan.
func nextPrefix(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xFF {
			b[i]++
			return string(b[:i+1])
		}
	}
	return prefix + "\xff"
}
