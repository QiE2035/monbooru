package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/db"
)

// addRatingTag inserts an image_tags row linking imageID to the
// canonical rating tag name (one of the four levels). Used by the
// rating-ceiling tests to seed pairs whose visibility should depend on
// the cookie.
func addRatingTag(t *testing.T, database *db.DB, imageID int64, ratingName string) {
	t.Helper()
	var tagID int64
	err := database.Read.QueryRow(
		`SELECT id FROM tags
		 WHERE category_id = (SELECT id FROM tag_categories WHERE name = 'rating')
		   AND name = ?`,
		ratingName,
	).Scan(&tagID)
	if err != nil {
		t.Fatalf("lookup rating tag %q: %v", ratingName, err)
	}
	if _, err := database.Write.Exec(
		`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, created_at)
		 VALUES (?, ?, 0, 0, datetime('now'))`,
		imageID, tagID,
	); err != nil {
		t.Fatalf("attach rating tag %q to image %d: %v", ratingName, imageID, err)
	}
}

// queueRow drops a row into potential_relation_pairs with the
// canonicalised (lo, hi) pair shape the find-pairs job emits.
func queueRow(t *testing.T, database *db.DB, a, b int64, distance int) {
	t.Helper()
	lo, hi := a, b
	if hi < lo {
		lo, hi = hi, lo
	}
	if _, err := database.Write.Exec(
		`INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at)
		 VALUES (?, ?, ?, datetime('now'))`,
		lo, hi, distance,
	); err != nil {
		t.Fatalf("queue pair (%d, %d): %v", lo, hi, err)
	}
}

// A pair whose either side carries a rating tag strictly above the
// cookie ceiling must not surface in the session: loadNextPair returns
// the visible count, the raw queue total, and a pair the operator can
// actually act on under the ceiling.
func TestSessionLoadNextPair_RatingCeilingFiltersBothSides(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	if cx == nil {
		t.Fatal("active gallery missing")
	}
	a := insertTestImage(t, srv.db())
	b, _ := srv.db().Write.Exec(
		`INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		 VALUES ('/tmp/explicit.jpg', 'jpg', 2048, 'sha_explicit', datetime('now'))`)
	bID, _ := b.LastInsertId()
	c, _ := srv.db().Write.Exec(
		`INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		 VALUES ('/tmp/sensitive.jpg', 'jpg', 1024, 'sha_sensitive', datetime('now'))`)
	cID, _ := c.LastInsertId()

	addRatingTag(t, srv.db(), bID, "explicit")
	addRatingTag(t, srv.db(), cID, "sensitive")

	// Pair 1: untagged + sensitive → visible at every ceiling.
	queueRow(t, srv.db(), a, cID, 3)
	// Pair 2: untagged + explicit → hidden under any ceiling below explicit.
	queueRow(t, srv.db(), a, bID, 4)

	excludeIDs, err := ratingExcludeTagIDs(cx, "sensitive")
	if err != nil {
		t.Fatalf("ratingExcludeTagIDs: %v", err)
	}
	if len(excludeIDs) != 2 {
		t.Fatalf("excludeIDs len = %d, want 2 (questionable + explicit)", len(excludeIDs))
	}

	pair, visible, raw, err := loadNextPair(cx, "smallest_distance_first", excludeIDs)
	if err != nil {
		t.Fatalf("loadNextPair: %v", err)
	}
	if raw != 2 {
		t.Errorf("raw remaining = %d, want 2", raw)
	}
	if visible != 1 {
		t.Errorf("visible remaining = %d, want 1 (the sensitive pair only)", visible)
	}
	if pair == nil {
		t.Fatal("expected the sensitive pair to surface, got nil")
	}
	if pair.A.ID != a && pair.B.ID != a {
		t.Errorf("returned pair does not include the untagged image: A=%d B=%d", pair.A.ID, pair.B.ID)
	}
	if pair.A.ID == bID || pair.B.ID == bID {
		t.Errorf("returned pair includes the explicit-rated image: A=%d B=%d", pair.A.ID, pair.B.ID)
	}

	// Empty ceiling matches every pair.
	noFilter, _ := ratingExcludeTagIDs(cx, "")
	if len(noFilter) != 0 {
		t.Errorf("ratingExcludeTagIDs(empty) returned %d ids, want 0", len(noFilter))
	}
	_, visibleNo, rawNo, err := loadNextPair(cx, "smallest_distance_first", noFilter)
	if err != nil {
		t.Fatalf("loadNextPair no-filter: %v", err)
	}
	if visibleNo != 2 || rawNo != 2 {
		t.Errorf("no-filter counts: visible=%d raw=%d, want 2/2", visibleNo, rawNo)
	}
}

// The session page renders the hidden-by-ceiling helper line and a
// raise-ceiling form when every queue row is filtered out by the cookie.
func TestSessionPage_HiddenByCeilingPromptsRaise(t *testing.T) {
	srv := newTestServer(t)
	a := insertTestImage(t, srv.db())
	res, _ := srv.db().Write.Exec(
		`INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		 VALUES ('/tmp/over.jpg', 'jpg', 4096, 'sha_over', datetime('now'))`)
	overID, _ := res.LastInsertId()
	addRatingTag(t, srv.db(), overID, "explicit")
	queueRow(t, srv.db(), a, overID, 5)

	req := httptest.NewRequest("GET", "/relations/session", nil)
	req.AddCookie(&http.Cookie{Name: "monbooru_rating_ceiling", Value: "sensitive"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("session page expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No pairs at your") {
		t.Errorf("expected the ceiling-hidden empty-state heading, body: %s", body)
	}
	if !strings.Contains(body, "Raise ceiling to explicit") {
		t.Errorf("expected the raise-ceiling button, body: %s", body)
	}
	if !strings.Contains(body, "/internal/rating-ceiling?level=explicit") {
		t.Errorf("expected the raise-ceiling form action, body: %s", body)
	}
}
