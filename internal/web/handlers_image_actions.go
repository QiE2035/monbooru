package web

import (
	"database/sql"
	"errors"
	"fmt"
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
// toggleInbox; oob (nil for favorite) appends an out-of-band fragment
// so a layout counter can follow the flip.
func (s *Server) toggleBoolColumn(w http.ResponseWriter, r *http.Request, column, onHTML, offHTML string, oob func(*http.Request) string) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
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
		_, _ = w.Write([]byte(onHTML))
	} else {
		_, _ = w.Write([]byte(offHTML))
	}
	if oob != nil {
		_, _ = w.Write([]byte(oob(r)))
	}
}

func (s *Server) toggleFavorite(w http.ResponseWriter, r *http.Request) {
	s.toggleBoolColumn(w, r, "is_favorited",
		`<button type="submit" id="fav-btn" class="btn-fav active" title="Unfavorite">♥</button>`,
		`<button type="submit" id="fav-btn" class="btn-fav" title="Favorite">♡</button>`,
		nil,
	)
}

// toggleInbox returns the swap HTML for the inbox button. The title
// names the click action (Archive / Send to inbox); the label names
// the row's current state (In inbox / Archived) so the button reads
// as "this is what it is" with the action surfaced on hover.
func (s *Server) toggleInbox(w http.ResponseWriter, r *http.Request) {
	s.toggleBoolColumn(w, r, "is_inbox",
		`<button type="submit" id="inbox-btn" class="btn-inbox active" title="Archive (i)">In inbox</button>`,
		`<button type="submit" id="inbox-btn" class="btn-inbox" title="Send to inbox (i)">Archived</button>`,
		s.inboxNavOOB,
	)
}

// inboxNavOOB re-renders the topbar inbox link out-of-band so its count
// follows the toggle; swapping the detail button alone leaves the layout
// counter stale until the next full render. Mirrors base()'s ceiling-aware
// InboxCountUnder so the OOB value matches a full render.
func (s *Server) inboxNavOOB(r *http.Request) string {
	cx := s.Active()
	if cx == nil {
		return ""
	}
	n, err := cx.InboxCountUnder(resolveCeiling(r, cx))
	if err != nil {
		return ""
	}
	suffix := ""
	if n > 0 {
		suffix = fmt.Sprintf(" (%d)", n)
	}
	return fmt.Sprintf(`<a id="inbox-nav" href="/?q=inbox:true" hx-swap-oob="true">Inbox%s</a>`, suffix)
}

func (s *Server) deleteImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}

	back := parseBackContext(r)

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
	if refID == nil && (back.Sort != "" || back.Q != "") {
		sortStr := back.Sort
		if sortStr == "" {
			sortStr = "newest"
		}
		orderStr := back.Order
		if orderStr == "" {
			orderStr = "desc"
		}
		prevID, nextID = s.findAdjacentImages(id, back.Q, sortStr, orderStr, back.Seed, resolveCeiling(r, s.Active()))
	}

	_, err := gallery.DeleteImage(s.db(), s.galleryPath(), s.thumbnailsPath(), id, s.tagSvc().RemoveAllTagsFromImage, s.onImageDeleteCallback())
	if err != nil {
		// ErrNoRows on the initial canonical-path lookup is the genuine
		// "no such image id" case; everything else (write-pool busy,
		// FK constraint, filesystem permission) is a server-side
		// failure and should not masquerade as 404.
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		logx.Errorf("delete image %d: %v", id, err)
		w.WriteHeader(http.StatusInternalServerError)
		writeInlineFlash(w, "err", "Delete failed; check server log.")
		return
	}
	s.Active().InvalidateCaches()

	redirectURL := ""
	switch {
	case refID != nil:
		redirectURL = back.DetailURL(*refID)
	case nextID != nil:
		redirectURL = back.DetailURL(*nextID)
	case prevID != nil:
		redirectURL = back.DetailURL(*prevID)
	default:
		redirectURL = back.GalleryURL()
	}

	flashText := fmt.Sprintf("Deleted image #%d.", id)
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
			setFlashHeader(w, flashText, "ok", map[string]any{
				"delete-go-back": map[string]any{"fallback": redirectURL},
			})
			w.WriteHeader(http.StatusOK)
			return
		}
		setFlashHeader(w, flashText, "ok", nil)
		w.Header().Set("HX-Redirect", redirectURL)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

func (s *Server) promoteCanonical(w http.ResponseWriter, r *http.Request) {
	// HTMX callers get the failure as a flash and stay on the detail page;
	// a plain form submit falls back to http.Error.
	fail := func(msg string, code int) {
		if isHTMXRequest(r) {
			setFlashHeader(w, msg, "err", nil)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, msg, code)
	}
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	newCanonical := r.FormValue("path")
	if newCanonical == "" {
		fail("path required", http.StatusBadRequest)
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
		fail(err.Error(), http.StatusInternalServerError)
		return
	}
	if aliasExists == 0 {
		fail("path is not an alias of this image", http.StatusBadRequest)
		return
	}
	if _, statErr := os.Stat(newCanonical); statErr != nil {
		fail("cannot set canonical: file is missing on disk", http.StatusBadRequest)
		return
	}

	newFolder := gallery.FolderPath(s.galleryPath(), newCanonical)

	tx, err := s.db().Write.Begin()
	if err != nil {
		fail(err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`UPDATE image_paths SET is_canonical = 0 WHERE image_id = ?`, id); err != nil {
		fail(err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(
		`UPDATE image_paths SET is_canonical = 1 WHERE image_id = ? AND path = ?`,
		id, newCanonical,
	); err != nil {
		fail(err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(
		`UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`,
		newCanonical, newFolder, id,
	); err != nil {
		fail(err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		fail(err.Error(), http.StatusInternalServerError)
		return
	}
	// folder_path drives folder:/folderonly: search and the cached
	// folder tree; promoting a different canonical can land the image
	// in a different folder, so the per-gallery and adjacency caches
	// have to drop.
	s.Active().InvalidateCaches()

	if isHTMXRequest(r) {
		setFlashHeader(w, "Canonical path updated.", "ok", nil)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/images/%d", id), http.StatusSeeOther)
}

const (
	maxExternalSourceLen = 200
	maxExternalURLLen    = 2048
)

// updateExternal writes the operator-edited images.source / images.url
// fields. The form may carry either; an absent key leaves the existing
// value alone, while an empty key clears it. The detail-page dialogs
// each ship only their own field (Source, URL), so opening one and
// saving never clobbers the other. URLs must start with http:// or
// https:// so the rendered <a href> survives both the html/template
// scheme sanitiser and the explicit allowlist below. Collections live in
// image_collections and are edited through setCollection / removeCollection.
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
	if isHTMXRequest(r) {
		// The detail dialogs each ship one field per submit; the first one
		// present names the flash. Order matches the dialog list order.
		label := ""
		switch {
		case r.Form.Has("source"):
			label = "Source"
		case r.Form.Has("url"):
			label = "URL"
		}
		if label != "" {
			setFlashHeader(w, label+" updated.", "ok", nil)
		}
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
		writeInlineFlash(w, "err", msg)
		return
	}
	http.Error(w, msg, code)
}

// setCollection upserts one membership for an image: adding the image to a
// collection, updating that collection's position, or (with a prev value)
// renaming an existing membership. The detail dialog ships one membership
// per submit and gets HX-Refresh on success, matching updateExternal.
func (s *Server) setCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		externalErr(w, r, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("collection"))
	if name == "" {
		externalErr(w, r, "collection label required", http.StatusBadRequest)
		return
	}
	if len(name) > maxExternalSourceLen {
		externalErr(w, r, fmt.Sprintf("collection too long (max %d chars)", maxExternalSourceLen), http.StatusBadRequest)
		return
	}
	var order *int
	if raw := strings.TrimSpace(r.FormValue("collection_order")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			externalErr(w, r, "order must be an integer or empty", http.StatusBadRequest)
			return
		}
		if n < 1 {
			externalErr(w, r, "order must be 1 or higher", http.StatusBadRequest)
			return
		}
		order = &n
	}
	if prev := strings.TrimSpace(r.FormValue("prev")); prev != "" && !strings.EqualFold(prev, name) {
		if err := gallery.RemoveCollectionMembership(s.db(), id, prev); err != nil {
			externalErr(w, r, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := gallery.AddCollectionMembership(s.db(), id, name, order); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()
	if isHTMXRequest(r) {
		setFlashHeader(w, "Collection updated.", "ok", nil)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/images/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// removeCollection drops one membership from an image.
func (s *Server) removeCollection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		externalErr(w, r, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("collection"))
	if name == "" {
		externalErr(w, r, "collection label required", http.StatusBadRequest)
		return
	}
	if err := gallery.RemoveCollectionMembership(s.db(), id, name); err != nil {
		externalErr(w, r, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()
	if isHTMXRequest(r) {
		setFlashHeader(w, "Collection removed.", "ok", nil)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/images/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) deleteAlias(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	pathID, ok := pathInt64(w, r, "pathID")
	if !ok {
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
		_, _ = w.Write([]byte(""))
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/images/%d", id), http.StatusSeeOther)
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
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	targetFolder := strings.TrimSpace(r.FormValue("folder"))

	if !s.startJob(w, models.JobTypeMove) {
		return
	}

	if _, moveErr := gallery.MoveImage(s.db(), s.galleryPath(), id, targetFolder); moveErr != nil {
		s.jobs.Fail(moveErr.Error())
		w.WriteHeader(http.StatusBadRequest)
		writeInlineFlash(w, "err", moveErr.Error())
		return
	}
	s.Active().InvalidateCaches()
	s.jobs.Complete("Moved image.")

	if isHTMXRequest(r) {
		dest := targetFolder
		if dest == "" {
			dest = "gallery root"
		}
		setFlashHeader(w, fmt.Sprintf("Moved image to %s.", dest), "ok", nil)
		w.Header().Set("HX-Redirect", fmt.Sprintf("/images/%d", id))
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/images/%d", id), http.StatusSeeOther)
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

// nocasePrefixRange returns the half-open [lo, hi) bounds for a
// case-insensitive prefix match. Folding to lower case keeps the
// byte-incremented upper bound consistent with COLLATE NOCASE (which folds
// to lower case); a raw upper-case-ending prefix like "Z" would otherwise
// exclude every lower-case continuation.
func nocasePrefixRange(prefix string) (lo, hi string) {
	lo = strings.ToLower(prefix)
	return lo, nextPrefix(lo)
}
