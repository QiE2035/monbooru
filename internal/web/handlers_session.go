package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/leqwin/monbooru/internal/logx"
)

// validOrderModes enumerates the three session walk orders per
// RELATIONS.md §6.1. Anything else collapses to the default.
var validOrderModes = map[string]bool{
	"smallest_distance_first": true,
	"largest_file_first":      true,
	"random":                  true,
}

// orderClauseForMode returns the ORDER BY tail the queue SELECT uses.
// `skipped_at IS NULL DESC` keeps unskipped pairs ahead of skipped
// ones; the mode-specific second key drives the order inside each
// half.
func orderClauseForMode(mode string) string {
	base := "ORDER BY (p.skipped_at IS NULL) DESC, "
	switch mode {
	case "largest_file_first":
		return base + "(COALESCE(ia.file_size, 0) + COALESCE(ib.file_size, 0)) DESC, p.distance ASC, p.a_image_id ASC"
	case "random":
		return base + "random()"
	}
	return base + "p.distance ASC, (COALESCE(ia.file_size, 0) + COALESCE(ib.file_size, 0)) DESC, p.a_image_id ASC"
}

// sessionPairView is everything the swipe page needs about one pair.
// A nil view signals an empty queue; the template renders the
// "nothing left" stub.
type sessionPairView struct {
	A         sessionImageView
	B         sessionImageView
	Distance  int
	Remaining int
	Order     string
	// LeftID names whichever of A or B the template should render in
	// the left slot. The handler picks the bigger-filesize side so the
	// default Duplicate decision treats the most likely "original" as
	// left; W swap reassigns it client-side.
	LeftID int64
}

// sessionImageView mirrors models.Image but only carries the bits the
// session UI renders (id, size, dimensions, tag count). Loaded
// alongside the queue row in a single SELECT.
type sessionImageView struct {
	ID       int64
	Width    sql.NullInt64
	Height   sql.NullInt64
	FileSize int64
	Filename string
	TagCount int
}

// sessionPage renders /relations/session. Picks the next queue row
// according to the persisted order mode (or the ?order= override)
// and serves the two-cell swipe view.
func (s *Server) sessionPage(w http.ResponseWriter, r *http.Request) {
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	order := r.URL.Query().Get("order")
	if order == "" {
		order = loadSessionOrder(cx)
	}
	if !validOrderModes[order] {
		order = "smallest_distance_first"
	}
	if r.URL.Query().Get("order") != "" {
		// Operator switched modes from the picker; persist so a
		// reload picks up the same shuffle.
		saveSessionOrder(cx, order)
	}
	ceiling := resolveCeiling(r, cx)
	pair, remaining, rawRemaining, err := loadNextPair(cx, order, ceiling)
	if err != nil {
		logx.Warnf("session next pair: %v", err)
		http.Error(w, "load pair", http.StatusInternalServerError)
		return
	}
	var leftFacts, rightFacts relationCompareFacts
	if pair != nil {
		// The template puts the bigger-filesize side in slot "left". The
		// compare table mirrors that orientation so the operator's eye
		// reads "left vs right" without the W swap reshuffling the rows.
		leftID := pair.LeftID
		rightID := pair.A.ID
		if leftID == pair.A.ID {
			rightID = pair.B.ID
		}
		leftFacts, rightFacts, err = loadCompareFacts(cx, leftID, rightID)
		if err != nil {
			logx.Debugf("session compare facts: %v", err)
		}
	}
	s.renderTemplate(w, "relations_session.html", sessionPageData{
		baseData:        s.base(r, "relations", "Session - Monbooru"),
		Pair:            pair,
		Remaining:       remaining,
		HiddenByCeiling: rawRemaining - remaining,
		Ceiling:         ceiling.Level(),
		Order:           order,
		ActiveGallery:   s.activeName,
		Left:            leftFacts,
		Right:           rightFacts,
	})
}

type sessionPageData struct {
	baseData
	Pair *sessionPairView
	// Remaining is the visible queue total (post-ceiling). HiddenByCeiling
	// is the number of unresolved pairs filtered out because at least one
	// side carries a rating tag above the cookie ceiling.
	Remaining       int
	HiddenByCeiling int
	Ceiling         string
	Order           string
	ActiveGallery   string
	// Left / Right hold the comparison-table data oriented to the
	// template's left/right slots so the table reads consistently with
	// the thumbs.
	Left  relationCompareFacts
	Right relationCompareFacts
}

// loadSessionOrder reads the order_mode for the singleton session
// row, defaulting if the row is missing.
func loadSessionOrder(cx *galleryCtx) string {
	var mode string
	err := cx.DB.Read.QueryRow(`SELECT order_mode FROM relation_session WHERE id = 1`).Scan(&mode)
	if err == sql.ErrNoRows {
		return "smallest_distance_first"
	}
	if err != nil {
		return "smallest_distance_first"
	}
	return mode
}

// saveSessionOrder upserts the singleton row.
func saveSessionOrder(cx *galleryCtx, mode string) {
	_, err := cx.DB.Write.Exec(
		`INSERT INTO relation_session (id, order_mode) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET order_mode = excluded.order_mode`,
		mode,
	)
	if err != nil {
		logx.Debugf("save session order: %v", err)
	}
}

// loadNextPair pulls the next queue row plus both image rows and the
// total remaining count. The total-remaining COUNT is a lightweight
// covering query against the queue table. ceiling gates each side of
// the pair on the absence of a rating tag above the cookie level so
// the session walks only what the operator's ceiling already lets them
// see in the gallery.
//
// Returns (pair, visible, raw, err): visible is the post-filter count,
// raw is the unfiltered total - the difference is the "N pairs hidden
// by your ceiling" the empty-queue branch surfaces.
func loadNextPair(cx *galleryCtx, order string, ceiling *Ceiling) (*sessionPairView, int, int, error) {
	var rawRemaining int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM potential_relation_pairs`).Scan(&rawRemaining); err != nil {
		return nil, 0, 0, err
	}
	if rawRemaining == 0 {
		return nil, 0, 0, nil
	}
	where, args := ceiling.WhereTwo("ia.id", "ib.id")
	visible := rawRemaining
	if where != "" {
		countQ := `
			SELECT COUNT(*)
			FROM potential_relation_pairs p
			JOIN images ia ON ia.id = p.a_image_id
			JOIN images ib ON ib.id = p.b_image_id
			WHERE ` + where
		if err := cx.DB.Read.QueryRow(countQ, args...).Scan(&visible); err != nil {
			return nil, 0, rawRemaining, err
		}
	}
	if visible == 0 {
		return nil, 0, rawRemaining, nil
	}
	selectQ := `
		SELECT p.a_image_id, p.b_image_id, p.distance,
		       ia.canonical_path, COALESCE(ia.width, 0), COALESCE(ia.height, 0), ia.file_size,
		       ib.canonical_path, COALESCE(ib.width, 0), COALESCE(ib.height, 0), ib.file_size
		FROM potential_relation_pairs p
		JOIN images ia ON ia.id = p.a_image_id
		JOIN images ib ON ib.id = p.b_image_id`
	if where != "" {
		selectQ += "\n\t\tWHERE " + where
	}
	selectQ += "\n\t\t" + orderClauseForMode(order) + "\n\t\tLIMIT 1"
	var aPath, bPath string
	var aW, aH, bW, bH sql.NullInt64
	view := sessionPairView{Order: order, Remaining: visible}
	if err := cx.DB.Read.QueryRow(selectQ, args...).Scan(
		&view.A.ID, &view.B.ID, &view.Distance,
		&aPath, &aW, &aH, &view.A.FileSize,
		&bPath, &bW, &bH, &view.B.FileSize,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, visible, rawRemaining, nil
		}
		return nil, visible, rawRemaining, err
	}
	view.A.Width = aW
	view.A.Height = aH
	view.B.Width = bW
	view.B.Height = bH
	view.A.Filename = baseNameOf(aPath)
	view.B.Filename = baseNameOf(bPath)
	view.A.TagCount = countTags(cx, view.A.ID)
	view.B.TagCount = countTags(cx, view.B.ID)
	view.LeftID = view.A.ID
	if view.B.FileSize > view.A.FileSize ||
		(view.B.FileSize == view.A.FileSize && view.B.ID < view.A.ID) {
		view.LeftID = view.B.ID
	}
	return &view, visible, rawRemaining, nil
}

// relationCompareFacts is one side of the under-thumbs comparison
// table. Strings are pre-formatted so the template just renders the
// rows. UniqueTags lists tag names this side carries that the other
// does not; UniqueTagsTotal is the full count (the template caps the
// visible names and shows "+N more").
type relationCompareFacts struct {
	ImageID          int64
	ResolutionW      int64
	ResolutionH      int64
	FileSize         int64
	AddedAt          string
	TagCount         int
	UniqueTags       []string
	UniqueTagsTotal  int
	Format           string
}

// loadCompareFacts loads the comparison table data for two image ids.
// One SELECT per side covers width/height/file_size/ingested_at/
// canonical_path; a second SELECT computes the tag-delta lists. Tag
// counts are loaded through the existing countTags helper.
func loadCompareFacts(cx *galleryCtx, leftID, rightID int64) (relationCompareFacts, relationCompareFacts, error) {
	left := relationCompareFacts{ImageID: leftID}
	right := relationCompareFacts{ImageID: rightID}
	if err := scanCompareFacts(cx, leftID, &left); err != nil {
		return left, right, err
	}
	if err := scanCompareFacts(cx, rightID, &right); err != nil {
		return left, right, err
	}
	leftUnique, rightUnique, err := loadTagDelta(cx, leftID, rightID)
	if err != nil {
		return left, right, err
	}
	const cap = 5
	left.UniqueTagsTotal = len(leftUnique)
	right.UniqueTagsTotal = len(rightUnique)
	if len(leftUnique) > cap {
		left.UniqueTags = leftUnique[:cap]
	} else {
		left.UniqueTags = leftUnique
	}
	if len(rightUnique) > cap {
		right.UniqueTags = rightUnique[:cap]
	} else {
		right.UniqueTags = rightUnique
	}
	return left, right, nil
}

func scanCompareFacts(cx *galleryCtx, id int64, dst *relationCompareFacts) error {
	var w, h sql.NullInt64
	var addedAt sql.NullString
	var canonical, fileType string
	if err := cx.DB.Read.QueryRow(
		`SELECT COALESCE(width, 0), COALESCE(height, 0), file_size, ingested_at, canonical_path, file_type
		 FROM images WHERE id = ?`, id,
	).Scan(&w, &h, &dst.FileSize, &addedAt, &canonical, &fileType); err != nil {
		return err
	}
	if w.Valid {
		dst.ResolutionW = w.Int64
	}
	if h.Valid {
		dst.ResolutionH = h.Int64
	}
	if addedAt.Valid {
		// Drop the time-of-day so the table reads "2026-04-29" not the full
		// RFC3339 string the database emits.
		if i := strings.IndexByte(addedAt.String, 'T'); i > 0 {
			dst.AddedAt = addedAt.String[:i]
		} else {
			dst.AddedAt = addedAt.String
		}
	}
	if dot := strings.LastIndexByte(canonical, '.'); dot >= 0 {
		dst.Format = strings.ToLower(canonical[dot:])
	}
	dst.TagCount = countTags(cx, id)
	return nil
}

// loadTagDelta returns the names of every tag carried by exactly one
// of the two image ids. The HAVING COUNT(*) = 1 clause splits the join
// into "left only" vs "right only" by re-reading the per-row image_id.
// Rating tags are excluded because the table caller is comparing the
// images, not their ratings.
func loadTagDelta(cx *galleryCtx, leftID, rightID int64) (left []string, right []string, err error) {
	rows, err := cx.DB.Read.Query(`
		WITH delta AS (
			SELECT it.tag_id, MAX(it.image_id) AS owner_id
			FROM image_tags it
			LEFT JOIN tags t ON t.id = it.tag_id
			LEFT JOIN tag_categories tc ON tc.id = t.category_id
			WHERE it.image_id IN (?, ?)
			  AND (tc.name IS NULL OR tc.name != 'rating')
			GROUP BY it.tag_id
			HAVING COUNT(*) = 1
		)
		SELECT delta.owner_id, t.name
		FROM delta
		JOIN tags t ON t.id = delta.tag_id
		ORDER BY t.name
		LIMIT 200`, leftID, rightID,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var owner int64
		var name string
		if scanErr := rows.Scan(&owner, &name); scanErr != nil {
			return nil, nil, scanErr
		}
		if owner == leftID {
			left = append(left, name)
		} else if owner == rightID {
			right = append(right, name)
		}
	}
	return left, right, rows.Err()
}

func baseNameOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

func countTags(cx *galleryCtx, id int64) int {
	var n int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ?`, id).Scan(&n); err != nil {
		return 0
	}
	return n
}

// sessionDecidePost is the swipe page's decision endpoint. Form:
//   - a, b: image ids (canonical pair order from the queue)
//   - type: duplicate|alternate|version|derivative|not_related|skip
//   - left: image id the operator considers "left" after any W swap.
//     When omitted, "a" is left by default. Every Add* call below
//     receives (left, right) regardless of relation symmetry so the
//     directional semantic stays explicit at the call site.
//
// On success, redirects (HTMX) back to the session page so the next
// pair renders.
func (s *Server) sessionDecidePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.Active()
	if cx == nil || cx.RelationsSvc == nil {
		http.Error(w, "no gallery", http.StatusServiceUnavailable)
		return
	}
	a, ok := formInt64(w, r, "a")
	if !ok {
		return
	}
	b, ok := formInt64(w, r, "b")
	if !ok {
		return
	}
	decision := r.FormValue("type")
	left := a
	if raw := r.FormValue("left"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && (v == a || v == b) {
			left = v
		}
	}
	right := a
	if left == a {
		right = b
	}
	now := time.Now().UTC().Format(time.RFC3339)

	if decision == "skip" {
		if _, err := cx.DB.Write.Exec(
			`UPDATE potential_relation_pairs SET skipped_at = ? WHERE a_image_id = ? AND b_image_id = ?`,
			now, a, b,
		); err != nil {
			logx.Warnf("session skip: %v", err)
			http.Error(w, "skip", http.StatusInternalServerError)
			return
		}
		sessionRedirect(w, r)
		return
	}

	// Every service call takes (left, right): for duplicate the first
	// arg becomes original when a new group forms; for version/
	// derivative the first arg is parent/source; the two symmetric
	// types canonicalise internally so the call shape is uniform.
	var err error
	switch decision {
	case "duplicate":
		err = cx.RelationsSvc.AddDuplicate(left, right)
	case "alternate":
		err = cx.RelationsSvc.AddAlternate(left, right)
	case "version":
		err = cx.RelationsSvc.AddVersionEdge(left, right)
	case "derivative":
		err = cx.RelationsSvc.AddDerivativeEdge(left, right)
	case "not_related":
		err = cx.RelationsSvc.AddNotRelated(left, right)
	default:
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">Unknown decision.</div>`))
		return
	}
	if err != nil {
		writeRelationError(w, err)
		return
	}
	if _, err := cx.DB.Write.Exec(
		`DELETE FROM potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?`, a, b,
	); err != nil {
		logx.Warnf("session queue drop: %v", err)
	}
	if decision == "duplicate" && writeDuplicatePostDecideHeaders(w, cx, left, right) {
		return
	}
	sessionRedirect(w, r)
}

// writeDuplicatePostDecideHeaders fills the X-Relations-Post-Decision
// header set so the session template can pop a "Delete this duplicate
// from disk?" dialog instead of auto-advancing. Returns true when the
// headers were written (the caller skips the redirect) and false when
// the resolved state doesn't merit a prompt (no group, member missing
// a row, etc.) - the caller then falls through to the usual redirect.
func writeDuplicatePostDecideHeaders(w http.ResponseWriter, cx *galleryCtx, left, right int64) bool {
	// Find the dup group that now contains the pair. Both sides are
	// members; the non-original side is the one the dialog targets.
	var gid, original int64
	if err := cx.DB.Read.QueryRow(`
		SELECT g.id, g.original_image_id
		FROM dup_group_members ma
		JOIN dup_group_members mb ON ma.group_id = mb.group_id
		JOIN dup_groups g ON g.id = ma.group_id
		WHERE ma.image_id = ? AND mb.image_id = ?
		LIMIT 1`, left, right,
	).Scan(&gid, &original); err != nil {
		logx.Debugf("dup post-decide group lookup: %v", err)
		return false
	}
	nonOriginal := left
	if left == original {
		nonOriginal = right
	}
	var canonical string
	if err := cx.DB.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, nonOriginal).Scan(&canonical); err != nil {
		logx.Debugf("dup post-decide filename: %v", err)
		return false
	}
	var hasUnique int
	cx.DB.Read.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT it.tag_id
			FROM image_tags it
			LEFT JOIN tags t ON t.id = it.tag_id
			LEFT JOIN tag_categories tc ON tc.id = t.category_id
			WHERE it.image_id = ?
			  AND (tc.name IS NULL OR tc.name != 'rating')
			  AND NOT EXISTS (
			    SELECT 1 FROM image_tags it2 WHERE it2.image_id = ? AND it2.tag_id = it.tag_id
			  )
			LIMIT 1
		)`, nonOriginal, original,
	).Scan(&hasUnique)
	w.Header().Set("X-Relations-Post-Decision", "duplicate-cleanup")
	w.Header().Set("X-Relations-Duplicate-ID", strconv.FormatInt(nonOriginal, 10))
	w.Header().Set("X-Relations-Duplicate-OriginalID", strconv.FormatInt(original, 10))
	w.Header().Set("X-Relations-Duplicate-GroupID", strconv.FormatInt(gid, 10))
	w.Header().Set("X-Relations-Duplicate-Filename", baseNameOf(canonical))
	if hasUnique > 0 {
		w.Header().Set("X-Relations-Duplicate-HasUniqueTags", "1")
	} else {
		w.Header().Set("X-Relations-Duplicate-HasUniqueTags", "0")
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}

// sessionRedirect sends the operator back to /relations/session so
// the next pair renders. For HTMX requests, emits an HX-Redirect
// header so the swap reloads the whole page.
func sessionRedirect(w http.ResponseWriter, r *http.Request) {
	dest := fmt.Sprintf("/relations/session?order=%s", r.FormValue("order"))
	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
