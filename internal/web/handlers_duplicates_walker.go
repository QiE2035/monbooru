package web

import (
	"fmt"
	"html"
	"net/http"
	"strconv"

	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
)

// sha256DuplicateRow is one alias path on the SHA-256 duplicates table:
// the owning image, its canonical path on disk, and the alias path to
// remove. The walker page renders one row per alias so a single image
// with three non-canonical paths shows up three times.
type sha256DuplicateRow struct {
	ImageID       int64
	CanonicalPath string
	PathID        int64
	AliasPath     string
}

// markedDuplicateRow names one (group, original, non-original) pairing
// for the marked-duplicates walker table. HasTagsToCopy is true when
// at least one non-rating tag carried by a duplicate of the group is
// absent on the original - it gates the [copy tags] button so empty
// groups don't surface a no-op action.
type markedDuplicateRow struct {
	GroupID       int64
	OriginalID    int64
	DuplicateID   int64
	MarkedAt      string
	HasTagsToCopy bool
}

// relationsWalkerData is the shared template payload for the two
// duplicate walkers. Both render as tables.
type relationsWalkerData struct {
	baseData
	ActiveGallery string
	Kind          string // "sha256" or "marked"
	Sha256Rows    []sha256DuplicateRow
	MarkedRows    []markedDuplicateRow
}

// sha256WalkerPage renders every non-canonical alias path the gallery
// carries in one table. The Walk button on the Relations page links
// here.
func (s *Server) sha256WalkerPage(w http.ResponseWriter, r *http.Request) {
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	excludeIDs, _ := ratingExcludeTagIDs(cx, ratingCeilingFromRequest(r))
	q := `SELECT i.id, i.canonical_path, ip.id, ip.path
		FROM images i
		JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 0`
	args := []any{}
	if where, wargs := ratingExcludeWhereClauseSingle("i.id", excludeIDs); where != "" {
		q += ` WHERE ` + where
		args = append(args, wargs...)
	}
	q += ` ORDER BY i.id, ip.id`
	rows, err := cx.DB.Read.Query(q, args...)
	if err != nil {
		logx.Warnf("sha256 walker query: %v", err)
		http.Error(w, "load duplicates", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var out []sha256DuplicateRow
	for rows.Next() {
		var dr sha256DuplicateRow
		if scanErr := rows.Scan(&dr.ImageID, &dr.CanonicalPath, &dr.PathID, &dr.AliasPath); scanErr != nil {
			logx.Warnf("sha256 walker scan: %v", scanErr)
			http.Error(w, "scan duplicates", http.StatusInternalServerError)
			return
		}
		out = append(out, dr)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "iterate duplicates", http.StatusInternalServerError)
		return
	}
	s.renderTemplate(w, "relations_duplicates_sha256.html", relationsWalkerData{
		baseData:      s.base(r, "relations", "Duplicate files - Monbooru"),
		ActiveGallery: s.activeName,
		Kind:          "sha256",
		Sha256Rows:    out,
	})
}

// markedWalkerPage lists every (original, duplicate) pairing across
// every dup_group in one table. One row per non-original member,
// ordered by the membership's created_at descending so the freshest
// markings land at the top - the operator's most recent decisions are
// the ones most likely to need a follow-up action.
func (s *Server) markedWalkerPage(w http.ResponseWriter, r *http.Request) {
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	excludeIDs, _ := ratingExcludeTagIDs(cx, ratingCeilingFromRequest(r))
	q := `SELECT g.id, g.original_image_id, m.image_id, m.created_at
		FROM dup_group_members m
		JOIN dup_groups g ON g.id = m.group_id
		WHERE m.image_id != g.original_image_id`
	args := []any{}
	if where, wargs := ratingExcludeWhereClause("g.original_image_id", "m.image_id", excludeIDs); where != "" {
		q += ` AND ` + where
		args = append(args, wargs...)
	}
	q += ` ORDER BY m.created_at DESC, g.id DESC, m.image_id`
	rows, err := cx.DB.Read.Query(q, args...)
	if err != nil {
		logx.Warnf("marked walker query: %v", err)
		http.Error(w, "load duplicates", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var out []markedDuplicateRow
	for rows.Next() {
		var dr markedDuplicateRow
		var marked string
		if scanErr := rows.Scan(&dr.GroupID, &dr.OriginalID, &dr.DuplicateID, &marked); scanErr != nil {
			logx.Warnf("marked walker scan: %v", scanErr)
			http.Error(w, "scan duplicates", http.StatusInternalServerError)
			return
		}
		dr.MarkedAt = humanISOTime(marked)
		out = append(out, dr)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "iterate duplicates", http.StatusInternalServerError)
		return
	}
	if err := annotateTagsToCopy(cx, out); err != nil {
		logx.Warnf("marked walker tags-to-copy: %v", err)
	}
	s.renderTemplate(w, "relations_duplicates_marked.html", relationsWalkerData{
		baseData:      s.base(r, "relations", "Duplicate images - Monbooru"),
		ActiveGallery: s.activeName,
		Kind:          "marked",
		MarkedRows:    out,
	})
}

// annotateTagsToCopy sets HasTagsToCopy on every row whose group still
// carries at least one non-rating tag missing from its original. Runs
// one SELECT to gather the set of eligible group ids and stamps the
// result across the slice in O(N) so the walker doesn't pay a per-row
// query.
func annotateTagsToCopy(cx *galleryCtx, rows []markedDuplicateRow) error {
	if len(rows) == 0 {
		return nil
	}
	eligible := map[int64]bool{}
	q, err := cx.DB.Read.Query(`
		SELECT DISTINCT g.id
		FROM dup_groups g
		JOIN dup_group_members m ON m.group_id = g.id
		JOIN image_tags it ON it.image_id = m.image_id
		LEFT JOIN tags t ON t.id = it.tag_id
		LEFT JOIN tag_categories c ON c.id = t.category_id
		WHERE m.image_id != g.original_image_id
		  AND (c.name IS NULL OR c.name != 'rating')
		  AND NOT EXISTS (
		    SELECT 1 FROM image_tags it2
		    WHERE it2.image_id = g.original_image_id AND it2.tag_id = it.tag_id
		  )`)
	if err != nil {
		return err
	}
	defer q.Close()
	for q.Next() {
		var gid int64
		if err := q.Scan(&gid); err != nil {
			return err
		}
		eligible[gid] = true
	}
	if err := q.Err(); err != nil {
		return err
	}
	for i := range rows {
		if eligible[rows[i].GroupID] {
			rows[i].HasTagsToCopy = true
		}
	}
	return nil
}

// sha256WalkerRemoveOnePost removes a specific alias path (by id) and
// its file from disk, then bounces back to the walker so the refreshed
// table no longer carries the deleted row.
func (s *Server) sha256WalkerRemoveOnePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	pathID, err := strconv.ParseInt(r.FormValue("path_id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">Invalid path id.</div>`))
		return
	}
	var aliasPath string
	if err := s.db().Read.QueryRow(
		`SELECT path FROM image_paths WHERE id = ? AND is_canonical = 0`,
		pathID,
	).Scan(&aliasPath); err != nil {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`<div class="flash flash-err">Not a non-canonical path.</div>`))
		return
	}
	if _, err := s.db().Write.Exec(`DELETE FROM image_paths WHERE id = ?`, pathID); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	if aliasPath != "" {
		if err := unlinkUnderGallery(s.galleryPath(), aliasPath); err != nil {
			logx.Warnf("sha256 walker unlink %q: %v", aliasPath, err)
		}
	}
	redirectWalker(w, r, "sha256")
}

// markedWalkerDeleteOnePost deletes one image from a dup group through
// the same gallery.DeleteImage path the detail page uses; the
// relations service's OnImageDelete hook cleans the group membership.
func (s *Server) markedWalkerDeleteOnePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	imageID, err := strconv.ParseInt(r.FormValue("image_id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">Invalid image id.</div>`))
		return
	}
	if _, err := gallery.DeleteImage(s.db(), s.galleryPath(), s.thumbnailsPath(), imageID, s.tagSvc().RemoveAllTagsFromImage, s.onImageDeleteCallback()); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	s.Active().InvalidateCaches()
	redirectWalker(w, r, "marked")
}

// markedWalkerDeleteAllPost removes every non-original member from
// every dup_group. Each delete rides gallery.DeleteImage so the file
// is removed alongside the row.
func (s *Server) markedWalkerDeleteAllPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	rows, err := cx.DB.Read.Query(`
		SELECT m.image_id
		FROM dup_group_members m
		JOIN dup_groups g ON g.id = m.group_id
		WHERE m.image_id != g.original_image_id
	`)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	var victims []int64
	for rows.Next() {
		var id int64
		if scanErr := rows.Scan(&id); scanErr != nil {
			rows.Close()
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(scanErr.Error()) + `</div>`))
			return
		}
		victims = append(victims, id)
	}
	rows.Close()
	if iterErr := rows.Err(); iterErr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(iterErr.Error()) + `</div>`))
		return
	}
	removed := 0
	for _, id := range victims {
		if _, err := gallery.DeleteImage(s.db(), s.galleryPath(), s.thumbnailsPath(), id, s.tagSvc().RemoveAllTagsFromImage, s.onImageDeleteCallback()); err != nil {
			logx.Warnf("marked delete-all image %d: %v", id, err)
			continue
		}
		removed++
	}
	s.Active().InvalidateCaches()
	w.Write([]byte(fmt.Sprintf(`<div class="flash flash-ok">Removed %d marked duplicate(s).</div>`, removed)))
}

// redirectWalker writes an HX-Redirect (or 303) back to the walker
// page so the refreshed table reflects the just-completed action.
func redirectWalker(w http.ResponseWriter, r *http.Request, kind string) {
	target := "/relations/duplicates/" + kind
	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
