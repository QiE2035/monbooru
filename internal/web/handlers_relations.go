package web

import (
	"database/sql"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/relations"
)

// relatedTile is the small struct the relations partial reads from.
// Each tile carries the image's id, thumbnail URL, and a single-letter
// type marker as documented in RELATIONS.md §8.2. Collection siblings
// (rows sharing images.series with the current image) reuse the same
// shape but stash the series label and optional order so the template
// can render a series-pill chip instead of the relation marker.
type relatedTile struct {
	ID          int64
	Marker      string // O, D, A, V<-, V->, S, >; empty when the tile is a collection sibling
	Label       string // hover-title and badge text
	Series      string // collection label; empty for relation tiles
	SeriesOrder *int64 // 1-based collection order; nil when unset or for relation tiles
}

// relatedEntriesGet renders the lazy-loaded "Related entries" panel
// sitting below "Similar entries" on the detail page. Returns an empty
// 204-equivalent fragment when the image carries no declared relations
// so the caller's HTMX swap drops the empty section entirely.
func (s *Server) relatedEntriesGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	rels, err := relations.LoadImageRelations(cx.DB, id)
	if err != nil {
		logx.Warnf("related entries load %d: %v", id, err)
		http.Error(w, "load relations", http.StatusInternalServerError)
		return
	}
	siblings, sErr := loadCollectionSiblings(cx, id)
	if sErr != nil {
		logx.Warnf("collection siblings load %d: %v", id, sErr)
	}
	tiles := flattenRelationsForPanel(rels, id, siblings)
	paths := loadImagePaths(r.Context(), cx.DB, id)
	s.renderTemplate(w, "partials/related_entries.html", map[string]any{
		"ImageID":       id,
		"Tiles":         tiles,
		"ImagePaths":    paths,
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
		"ActiveGallery": s.activeName,
		"BackQ":         r.URL.Query().Get("back_q"),
		"BackSort":      r.URL.Query().Get("back_sort"),
		"BackOrder":     r.URL.Query().Get("back_order"),
		"BackSeed":      r.URL.Query().Get("back_seed"),
		"BackPage":      r.URL.Query().Get("back_page"),
	})
}

// collectionSibling carries the bits the related-entries panel and the
// see-all grid need to render a series-pill for a row that shares
// images.series with the current image. Empty Series means "this image
// has no collection"; callers skip the section in that case.
type collectionSibling struct {
	ID     int64
	Series string
	Order  *int64
}

// loadCollectionSiblings returns every non-missing image that shares
// images.series with the named row, oldest order first then by id. The
// result excludes the row itself; an empty slice means the image has no
// collection or is the only member.
func loadCollectionSiblings(cx *galleryCtx, imageID int64) ([]collectionSibling, error) {
	var series sql.NullString
	if err := cx.DB.Read.QueryRow(`SELECT series FROM images WHERE id = ?`, imageID).Scan(&series); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if !series.Valid || series.String == "" {
		return nil, nil
	}
	rows, err := cx.DB.Read.Query(
		`SELECT id, series_order FROM images
		 WHERE series = ? AND id != ? AND is_missing = 0
		 ORDER BY series_order IS NULL, series_order, id
		 LIMIT 200`,
		series.String, imageID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []collectionSibling
	for rows.Next() {
		var sib collectionSibling
		sib.Series = series.String
		var ord sql.NullInt64
		if scanErr := rows.Scan(&sib.ID, &ord); scanErr != nil {
			return nil, scanErr
		}
		if ord.Valid {
			v := ord.Int64
			sib.Order = &v
		}
		out = append(out, sib)
	}
	return out, rows.Err()
}

// flattenRelationsForPanel produces an ordered tile list (max 6) for
// the compact Relations panel: dup-group siblings (original first),
// alt-group siblings, version neighbours, derivative neighbours, and
// collection siblings sharing images.series with the current row. A
// declared relation outranks a collection link so a duplicate already
// listed is not re-added as a collection sibling.
func flattenRelationsForPanel(rels *relations.ImageRelations, self int64, siblings []collectionSibling) []relatedTile {
	const cap = 6
	var tiles []relatedTile
	seen := map[int64]bool{self: true}
	add := func(t relatedTile) {
		if len(tiles) >= cap || seen[t.ID] {
			return
		}
		seen[t.ID] = true
		tiles = append(tiles, t)
	}
	if rels.DupGroup != nil {
		for _, m := range rels.DupGroup.Members {
			if m == self {
				continue
			}
			marker := "Duplicate"
			label := "duplicate"
			if m == rels.DupGroup.Original {
				marker = "Original"
				label = "original"
			}
			add(relatedTile{ID: m, Marker: marker, Label: label})
		}
	}
	for _, m := range rels.AltGroupMembers {
		add(relatedTile{ID: m, Marker: "Alternate", Label: "alternate"})
	}
	if rels.VersionParent != nil {
		add(relatedTile{ID: *rels.VersionParent, Marker: "Earlier", Label: "previous version"})
	}
	if rels.VersionChild != nil {
		add(relatedTile{ID: *rels.VersionChild, Marker: "Newer", Label: "newer version"})
	}
	if rels.DerivativeSource != nil {
		add(relatedTile{ID: *rels.DerivativeSource, Marker: "Source", Label: "source"})
	}
	for _, m := range rels.Derivatives {
		add(relatedTile{ID: m, Marker: "Derivative", Label: "derivative"})
	}
	for _, sib := range siblings {
		add(relatedTile{ID: sib.ID, Label: "collection", Series: sib.Series, SeriesOrder: sib.Order})
	}
	return tiles
}

// imageRelationsPage renders the full-page /images/{id}/relations
// grid analogue of /images/{id}/pages: per-type sections, each a
// thumbnail strip the operator can click through.
func (s *Server) imageRelationsPage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	img, err := loadImage(r.Context(), cx.DB, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rels, err := relations.LoadImageRelations(cx.DB, id)
	if err != nil {
		logx.Warnf("relations page load %d: %v", id, err)
		http.Error(w, "load relations", http.StatusInternalServerError)
		return
	}
	siblings, sErr := loadCollectionSiblings(cx, id)
	if sErr != nil {
		logx.Warnf("relations page collection siblings %d: %v", id, sErr)
	}
	// Strip declared-relation neighbours from the collection strip so
	// the same row never appears twice on the page.
	siblings = filterCollectionSiblings(siblings, rels)
	// When the operator is about to unlink the current original from a
	// group with 3+ members, the post-step promotes a new original.
	// Surface that id so the confirm prompt can name it.
	var nextOriginal int64
	if rels.DupGroup != nil && rels.DupGroup.Original != id && len(rels.DupGroup.Members) >= 3 {
		next, nErr := cx.RelationsSvc.NextOriginalIfRemoved(rels.DupGroup.ID, rels.DupGroup.Original)
		if nErr != nil {
			logx.Warnf("relations page next-original %d: %v", id, nErr)
		}
		nextOriginal = next
	}
	thumbURL := fmt.Sprintf("/thumbnails/%s/%d.jpg", s.activeName, id)
	s.renderTemplate(w, "relations_image.html", relationsImagePageData{
		baseData:                       s.base(r, "gallery", fmt.Sprintf("Relations - %d", id)),
		Image:                          *img,
		Relations:                      rels,
		Self:                           id,
		Collection:                     siblings,
		ThumbnailURL:                   thumbURL,
		NextOriginalIfOriginalUnlinked: nextOriginal,
		BackQ:                          r.URL.Query().Get("back_q"),
		BackSort:                       r.URL.Query().Get("back_sort"),
		BackOrder:                      r.URL.Query().Get("back_order"),
		BackSeed:                       r.URL.Query().Get("back_seed"),
		BackPage:                       r.URL.Query().Get("back_page"),
	})
}

// filterCollectionSiblings drops every sibling that is already named in
// one of the declared relation slots so the see-all page can render the
// collection strip alongside the relation sections without duplicating
// any thumbnail.
func filterCollectionSiblings(siblings []collectionSibling, rels *relations.ImageRelations) []collectionSibling {
	if len(siblings) == 0 || rels == nil {
		return siblings
	}
	taken := map[int64]bool{}
	if rels.DupGroup != nil {
		for _, m := range rels.DupGroup.Members {
			taken[m] = true
		}
	}
	for _, m := range rels.AltGroupMembers {
		taken[m] = true
	}
	if rels.VersionParent != nil {
		taken[*rels.VersionParent] = true
	}
	if rels.VersionChild != nil {
		taken[*rels.VersionChild] = true
	}
	if rels.DerivativeSource != nil {
		taken[*rels.DerivativeSource] = true
	}
	for _, m := range rels.Derivatives {
		taken[m] = true
	}
	out := siblings[:0]
	for _, sib := range siblings {
		if taken[sib.ID] {
			continue
		}
		out = append(out, sib)
	}
	return out
}

type relationsImagePageData struct {
	baseData
	Image        models.Image
	Relations    *relations.ImageRelations
	Self         int64
	Collection   []collectionSibling
	ThumbnailURL string
	// NextOriginalIfOriginalUnlinked is the image id that would be
	// promoted to original if the current group's original tile is
	// unlinked. Non-zero only when the group has 3+ members.
	NextOriginalIfOriginalUnlinked int64
	BackQ                          string
	BackSort                       string
	BackOrder                      string
	BackSeed                       string
	BackPage                       string
}

// recomputePhashPost recomputes phash for the named image. Hooked from
// the detail-page [backfill] chip when images.phash is NULL.
func (s *Server) recomputePhashPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	if err := gallery.RecomputeAndStorePhash(r.Context(), cx.DB, id, cx.ThumbnailsPath); err != nil {
		logx.Warnf("recompute phash %d: %v", id, err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="flash flash-err">phash recompute failed (is the thumbnail present?)</div>`))
		return
	}
	w.Write([]byte(`<div class="flash flash-ok">phash recomputed.</div>`))
}

// addRelationPost installs a relation between two images. Form fields:
//   - type: duplicate | alternate | version | derivative | not_related
//   - a, b: image ids (integers); `a` defaults to "this image"
//   - direction: "ab" (default) or "ba" - "ba" treats `b` as the
//     operator's left, so every relation reads with the same
//     left-to-right convention regardless of which input slot the
//     swap arrow ended on
func (s *Server) addRelationPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.Active()
	if cx == nil || cx.RelationsSvc == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	a, b, ok := parseRelationPair(w, r)
	if !ok {
		return
	}
	if r.FormValue("direction") == "ba" {
		a, b = b, a
	}
	relType := r.FormValue("type")
	force := r.FormValue("force") == "true"
	if force {
		if err := cx.RelationsSvc.ClearBetween(a, b); err != nil {
			writeRelationError(w, err)
			return
		}
	}
	var err error
	switch relType {
	case "duplicate":
		err = cx.RelationsSvc.AddDuplicate(a, b)
	case "alternate":
		err = cx.RelationsSvc.AddAlternate(a, b)
	case "version":
		if force {
			// ClearBetween only touched the a-b pair; a version conflict
			// with a third image still blocks the insert. Drop every
			// version_edge that names either side so AddVersionEdge can
			// land cleanly.
			if cErr := cx.RelationsSvc.ClearVersionEdgesInvolving(a, b); cErr != nil {
				writeRelationError(w, cErr)
				return
			}
		}
		err = cx.RelationsSvc.AddVersionEdge(a, b)
	case "derivative":
		if force {
			// Same reasoning as version: the schema allows only one
			// source per derivative; drop the existing row so the new
			// source can attach.
			if cErr := cx.RelationsSvc.ClearDerivativeSourceOf(b); cErr != nil {
				writeRelationError(w, cErr)
				return
			}
		}
		err = cx.RelationsSvc.AddDerivativeEdge(a, b)
	case "not_related":
		err = cx.RelationsSvc.AddNotRelated(a, b)
	default:
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">Unknown relation type.</div>`))
		return
	}
	if err != nil {
		writeRelationError(w, err)
		return
	}
	w.Write([]byte(`<div class="flash flash-ok">Relation added.</div>`))
}

// removeRelationPost / removeRelationDelete unlinks a relation. Form
// fields:
//   - type: duplicate | alternate | version | derivative | not_related |
//           promote-original | dissolve-dup | dissolve-alt
//   - a, b: image ids (most types); group_id for dissolve and promote
//   - image_id for promote-original (the new original)
func (s *Server) removeRelationPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.Active()
	if cx == nil || cx.RelationsSvc == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	relType := r.FormValue("type")
	switch relType {
	case "duplicate":
		// "duplicate" unlink takes one image_id and removes it from its
		// dup group; "dissolve-dup" wipes the whole group instead.
		id, ok := formInt64(w, r, "image_id")
		if !ok {
			return
		}
		if err := cx.RelationsSvc.RemoveDupMember(id); err != nil {
			writeRelationError(w, err)
			return
		}
	case "alternate":
		id, ok := formInt64(w, r, "image_id")
		if !ok {
			return
		}
		if err := cx.RelationsSvc.RemoveAltMember(id); err != nil {
			writeRelationError(w, err)
			return
		}
	case "version":
		a, b, ok := parseRelationPair(w, r)
		if !ok {
			return
		}
		// Form posts a, b in chain order (parent, child).
		if err := cx.RelationsSvc.RemoveVersionEdge(a, b); err != nil {
			writeRelationError(w, err)
			return
		}
	case "derivative":
		a, b, ok := parseRelationPair(w, r)
		if !ok {
			return
		}
		if err := cx.RelationsSvc.RemoveDerivativeEdge(a, b); err != nil {
			writeRelationError(w, err)
			return
		}
	case "not_related":
		a, b, ok := parseRelationPair(w, r)
		if !ok {
			return
		}
		if err := cx.RelationsSvc.RemoveNotRelated(a, b); err != nil {
			writeRelationError(w, err)
			return
		}
	case "dissolve-dup":
		gid, ok := formInt64(w, r, "group_id")
		if !ok {
			return
		}
		if err := cx.RelationsSvc.DissolveDupGroup(gid); err != nil {
			writeRelationError(w, err)
			return
		}
	case "dissolve-alt":
		gid, ok := formInt64(w, r, "group_id")
		if !ok {
			return
		}
		if err := cx.RelationsSvc.DissolveAltGroup(gid); err != nil {
			writeRelationError(w, err)
			return
		}
	case "promote-original":
		gid, ok := formInt64(w, r, "group_id")
		if !ok {
			return
		}
		id, ok := formInt64(w, r, "image_id")
		if !ok {
			return
		}
		if err := cx.RelationsSvc.PromoteToOriginal(gid, id); err != nil {
			writeRelationError(w, err)
			return
		}
	default:
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">Unknown relation type.</div>`))
		return
	}
	w.Write([]byte(`<div class="flash flash-ok">Relation removed.</div>`))
}

// copyTagsPreviewGroup is one category bucket of new tag names the
// preview dialog renders. Tags inside the bucket are ordered by name;
// buckets are ordered the way `groupByCategory` orders the rest of the
// app's tag lists (general first, custom last).
type copyTagsPreviewGroup struct {
	Category string
	Color    string
	Tags     []string
}

// copyTagsToOriginalPreview renders a small partial listing the tag
// names CopyTagsFromDuplicatesToOriginal would insert onto the
// original. Used by the marked-duplicates walker's preview dialog so
// the operator sees what they are about to merge before confirming.
func (s *Server) copyTagsToOriginalPreview(w http.ResponseWriter, r *http.Request) {
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	gid, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	var original int64
	if err := cx.DB.Read.QueryRow(`SELECT original_image_id FROM dup_groups WHERE id = ?`, gid).Scan(&original); err != nil {
		http.NotFound(w, r)
		return
	}
	rows, err := cx.DB.Read.Query(`
		SELECT DISTINCT t.name, COALESCE(c.name, ''), COALESCE(c.color, '')
		FROM image_tags it
		JOIN dup_group_members m ON m.image_id = it.image_id
		JOIN tags t ON t.id = it.tag_id
		LEFT JOIN tag_categories c ON c.id = t.category_id
		WHERE m.group_id = ?
		  AND m.image_id != ?
		  AND (c.name IS NULL OR c.name != 'rating')
		  AND NOT EXISTS (
		    SELECT 1 FROM image_tags it2 WHERE it2.image_id = ? AND it2.tag_id = it.tag_id
		  )
		ORDER BY c.name, t.name`,
		gid, original, original,
	)
	if err != nil {
		http.Error(w, "load preview", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type row struct {
		name     string
		category string
		color    string
	}
	var entries []row
	for rows.Next() {
		var rec row
		if scanErr := rows.Scan(&rec.name, &rec.category, &rec.color); scanErr != nil {
			http.Error(w, "scan preview", http.StatusInternalServerError)
			return
		}
		entries = append(entries, rec)
	}
	bucketOrder := []string{}
	buckets := map[string]*copyTagsPreviewGroup{}
	for _, e := range entries {
		cat := e.category
		if cat == "" {
			cat = "(uncategorised)"
		}
		b, ok := buckets[cat]
		if !ok {
			b = &copyTagsPreviewGroup{Category: cat, Color: e.color}
			buckets[cat] = b
			bucketOrder = append(bucketOrder, cat)
		}
		b.Tags = append(b.Tags, e.name)
	}
	groups := make([]copyTagsPreviewGroup, 0, len(bucketOrder))
	total := 0
	for _, k := range bucketOrder {
		groups = append(groups, *buckets[k])
		total += len(buckets[k].Tags)
	}
	s.renderTemplate(w, "partials/copy_tags_preview.html", map[string]any{
		"GroupID":    gid,
		"OriginalID": original,
		"Groups":     groups,
		"Total":      total,
		"CSRFToken":  s.csrfToken(sessionFromContext(r.Context())),
	})
}

// copyTagsToOriginalPost runs CopyTagsFromDuplicatesToOriginal for a
// duplicate group. Wired to the per-card button on the Relations page.
func (s *Server) copyTagsToOriginalPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.Active()
	if cx == nil || cx.RelationsSvc == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	gid, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	added, err := cx.RelationsSvc.CopyTagsFromDuplicatesToOriginal(gid)
	if err != nil {
		writeRelationError(w, err)
		return
	}
	w.Write([]byte(fmt.Sprintf(`<div class="flash flash-ok">Copied %d tag(s) to the original.</div>`, added)))
}

// parseRelationPair reads form fields a and b, returning the validated
// ids or false (already writing the error flash).
func parseRelationPair(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	a, ok := formInt64(w, r, "a")
	if !ok {
		return 0, 0, false
	}
	b, ok := formInt64(w, r, "b")
	if !ok {
		return 0, 0, false
	}
	if a == b {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">Cannot relate an image to itself.</div>`))
		return 0, 0, false
	}
	return a, b, true
}

// formInt64 parses an integer form field, writing the error flash on
// failure.
func formInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf(`<div class="flash flash-err">Missing %s.</div>`, html.EscapeString(name))))
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf(`<div class="flash flash-err">Invalid %s.</div>`, html.EscapeString(name))))
		return 0, false
	}
	return v, true
}

// writeRelationError maps a relations.Service error to a friendly
// flash. ErrRelationConflict and the type-specific Exists errors
// surface so the operator knows to unlink first. The status stays 200
// for HTMX callers so the in-dialog target swap actually paints the
// message - 4xx responses don't swap by default.
func writeRelationError(w http.ResponseWriter, err error) {
	msg := err.Error()
	switch {
	case errors.Is(err, relations.ErrRelationConflict):
		msg = "Pair already has a different relation; remove the existing one first."
	case errors.Is(err, relations.ErrVersionExists):
		msg = "One of the images already has a version edge; remove it first."
	case errors.Is(err, relations.ErrDerivativeExists):
		msg = "The chosen derivative already has a source; remove it first."
	case errors.Is(err, relations.ErrSelfRelation):
		msg = "Cannot relate an image to itself."
	case errors.Is(err, relations.ErrNotInGroup):
		msg = "Image isn't a member of that group."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(msg) + `</div>`))
}

// pathInt64 parses a numeric path segment, writing 404 on failure.
func pathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return v, true
}
