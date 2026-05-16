package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
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
	pair, remaining, err := loadNextPair(cx, order)
	if err != nil {
		logx.Warnf("session next pair: %v", err)
		http.Error(w, "load pair", http.StatusInternalServerError)
		return
	}
	s.renderTemplate(w, "relations_session.html", sessionPageData{
		baseData:      s.base(r, "relations", "Session - Monbooru"),
		Pair:          pair,
		Remaining:     remaining,
		Order:         order,
		ActiveGallery: s.activeName,
	})
}

type sessionPageData struct {
	baseData
	Pair          *sessionPairView
	Remaining     int
	Order         string
	ActiveGallery string
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
// covering query against the queue table.
func loadNextPair(cx *galleryCtx, order string) (*sessionPairView, int, error) {
	var remaining int
	if err := cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM potential_relation_pairs`).Scan(&remaining); err != nil {
		return nil, 0, err
	}
	if remaining == 0 {
		return nil, 0, nil
	}
	q := `
		SELECT p.a_image_id, p.b_image_id, p.distance,
		       ia.canonical_path, COALESCE(ia.width, 0), COALESCE(ia.height, 0), ia.file_size,
		       ib.canonical_path, COALESCE(ib.width, 0), COALESCE(ib.height, 0), ib.file_size
		FROM potential_relation_pairs p
		JOIN images ia ON ia.id = p.a_image_id
		JOIN images ib ON ib.id = p.b_image_id
		` + orderClauseForMode(order) + `
		LIMIT 1`
	var aPath, bPath string
	var aW, aH, bW, bH sql.NullInt64
	view := sessionPairView{Order: order, Remaining: remaining}
	if err := cx.DB.Read.QueryRow(q).Scan(
		&view.A.ID, &view.B.ID, &view.Distance,
		&aPath, &aW, &aH, &view.A.FileSize,
		&bPath, &bW, &bH, &view.B.FileSize,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, remaining, nil
		}
		return nil, remaining, err
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
	return &view, remaining, nil
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
	sessionRedirect(w, r)
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
