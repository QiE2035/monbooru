package web

import (
	"net/http"
	"net/http/httptest"
	"strconv"
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

	ceiling := &Ceiling{level: "sensitive", cx: cx}
	excludeIDs := ceiling.ExcludedTagIDs()
	if len(excludeIDs) != 2 {
		t.Fatalf("excludeIDs len = %d, want 2 (questionable + explicit)", len(excludeIDs))
	}

	pair, visible, raw, err := loadNextPair(cx, "smallest_distance_first", ceiling, 0, 0)
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
	none := &Ceiling{level: "", cx: cx}
	if got := none.ExcludedTagIDs(); len(got) != 0 {
		t.Errorf("ExcludedTagIDs(empty) returned %d ids, want 0", len(got))
	}
	_, visibleNo, rawNo, err := loadNextPair(cx, "smallest_distance_first", none, 0, 0)
	if err != nil {
		t.Fatalf("loadNextPair no-filter: %v", err)
	}
	if visibleNo != 2 || rawNo != 2 {
		t.Errorf("no-filter counts: visible=%d raw=%d, want 2/2", visibleNo, rawNo)
	}
}

// pairHas reports whether the loaded pair is exactly {x, y} in either
// orientation.
func pairHas(p *sessionPairView, x, y int64) bool {
	if p == nil {
		return false
	}
	return (p.A.ID == x && p.B.ID == y) || (p.A.ID == y && p.B.ID == x)
}

// review-again reopens the session on one exact pair: loadNextPair must
// surface the pinned pair even when another pair sorts ahead of it, and
// fall back to the ordered pick when the pin is not in the queue.
func TestSessionLoadNextPair_PinsRequestedPair(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	if cx == nil {
		t.Fatal("active gallery missing")
	}
	a := insertRawImage(t, srv, "pin_a.png", "")
	b := insertRawImage(t, srv, "pin_b.png", "")
	c := insertRawImage(t, srv, "pin_c.png", "")
	// (a,c) at distance 0 sorts ahead of (a,b) at distance 5.
	queueRow(t, srv.db(), a, c, 0)
	queueRow(t, srv.db(), a, b, 5)
	none := &Ceiling{level: "", cx: cx}

	top, _, _, err := loadNextPair(cx, "smallest_distance_first", none, 0, 0)
	if err != nil {
		t.Fatalf("loadNextPair unpinned: %v", err)
	}
	if !pairHas(top, a, c) {
		t.Fatalf("unpinned pick should be (a,c), got A=%d B=%d", top.A.ID, top.B.ID)
	}

	pinned, _, _, err := loadNextPair(cx, "smallest_distance_first", none, a, b)
	if err != nil {
		t.Fatalf("loadNextPair pinned: %v", err)
	}
	if !pairHas(pinned, a, b) {
		t.Fatalf("pinned pick should be (a,b) despite sorting later, got A=%d B=%d", pinned.A.ID, pinned.B.ID)
	}

	// A pin that isn't queued falls back to the ordered pick.
	fallback, _, _, err := loadNextPair(cx, "smallest_distance_first", none, a, 999999)
	if err != nil {
		t.Fatalf("loadNextPair fallback: %v", err)
	}
	if !pairHas(fallback, a, c) {
		t.Fatalf("missing pin should fall back to (a,c), got A=%d B=%d", fallback.A.ID, fallback.B.ID)
	}
}

// A pair whose sides are mp4/webm must surface the file_type on the
// session-pair data attributes so the compare-slider JS mounts <video>
// elements rather than <img>. Without these attributes the slider
// renders the raw video bytes through <img> and shows broken-image
// icons.
func TestSessionPage_VideoPairExposesFileType(t *testing.T) {
	srv := newTestServer(t)
	resA, _ := srv.db().Write.Exec(
		`INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		 VALUES ('/tmp/clip_a.mp4', 'mp4', 4096, 'sha_clip_a', datetime('now'))`)
	aID, _ := resA.LastInsertId()
	resB, _ := srv.db().Write.Exec(
		`INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		 VALUES ('/tmp/clip_b.webm', 'webm', 2048, 'sha_clip_b', datetime('now'))`)
	bID, _ := resB.LastInsertId()
	queueRow(t, srv.db(), aID, bID, 3)

	req := httptest.NewRequest("GET", "/relations/session", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("session page expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-a-type="mp4"`) {
		t.Errorf("expected data-a-type=\"mp4\" on .session-pair, body did not contain it")
	}
	if !strings.Contains(body, `data-b-type="webm"`) {
		t.Errorf("expected data-b-type=\"webm\" on .session-pair, body did not contain it")
	}
	if !strings.Contains(body, `id="compare-slot-left"`) || !strings.Contains(body, `id="compare-slot-right"`) {
		t.Errorf("expected compare-slot-left / compare-slot-right containers in the dialog")
	}
	// The old <img id="compare-img-left"> elements are gone now that
	// the JS mounts the right media element per slot at open time.
	if strings.Contains(body, `id="compare-img-left"`) || strings.Contains(body, `id="compare-img-right"`) {
		t.Errorf("legacy <img id=\"compare-img-{left,right}\"> elements still in the dialog")
	}
}

// Session-cell previews serve the real image bytes (or video element for
// mp4/webm sides) rather than the 360px jpg thumbnail. The thumbnail
// path is reserved for the archive case where there is no single image
// to display.
func TestSessionPage_CellMediaSrc(t *testing.T) {
	srv := newTestServer(t)
	exec := func(path, ftype, sha string) int64 {
		t.Helper()
		res, err := srv.db().Write.Exec(
			`INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
			 VALUES (?, ?, 1024, ?, datetime('now'))`,
			path, ftype, sha,
		)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	staticID := exec("/tmp/cell_static.png", "png", "sha_cell_static")
	videoID := exec("/tmp/cell_video.mp4", "mp4", "sha_cell_video")
	queueRow(t, srv.db(), staticID, videoID, 3)

	req := httptest.NewRequest("GET", "/relations/session", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("session page expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<img src="/images/`+strconv.FormatInt(staticID, 10)+`/file"`) {
		t.Errorf("expected static-side <img> wired to /images/%d/file, body: %s", staticID, body)
	}
	if !strings.Contains(body, `<video class="session-cell-img" src="/images/`+strconv.FormatInt(videoID, 10)+`/file"`) {
		t.Errorf("expected mp4-side <video> wired to /images/%d/file, body: %s", videoID, body)
	}
	// The cell <img>/<video> must not fall back to the thumbnail endpoint
	// for static or video sides - the whole point of the change is to
	// drop the 360px stretch.
	if strings.Contains(body, `src="/thumbnails/`+srv.activeName+`/`+strconv.FormatInt(staticID, 10)+`.jpg" alt="image `+strconv.FormatInt(staticID, 10)+`" class="session-cell-img"`) {
		t.Errorf("static cell still serves the thumbnail jpg, body: %s", body)
	}
	if strings.Contains(body, `src="/thumbnails/`+srv.activeName+`/`+strconv.FormatInt(videoID, 10)+`.jpg" alt="image `+strconv.FormatInt(videoID, 10)+`" class="session-cell-img"`) {
		t.Errorf("video cell still serves the thumbnail jpg, body: %s", body)
	}
}

// Archive (cbz/zip) sides have no single viewable image, so the cell
// keeps the existing cover-thumbnail src rather than pointing at the
// archive bytes the browser cannot render.
func TestSessionPage_CellMediaArchiveKeepsThumbnail(t *testing.T) {
	srv := newTestServer(t)
	exec := func(path, ftype, sha string) int64 {
		t.Helper()
		res, err := srv.db().Write.Exec(
			`INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
			 VALUES (?, ?, 1024, ?, datetime('now'))`,
			path, ftype, sha,
		)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	zipA := exec("/tmp/cell_zip_a.cbz", "cbz", "sha_zip_a")
	zipB := exec("/tmp/cell_zip_b.cbz", "cbz", "sha_zip_b")
	queueRow(t, srv.db(), zipA, zipB, 4)

	req := httptest.NewRequest("GET", "/relations/session", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("session page expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, id := range []int64{zipA, zipB} {
		want := `src="/thumbnails/` + srv.activeName + `/` + strconv.FormatInt(id, 10) + `.jpg"`
		if !strings.Contains(body, want) {
			t.Errorf("expected archive cell to keep thumbnail src %q, body: %s", want, body)
		}
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

// A pair whose two images share a collection stays out of the session
// by default (the collection is the relation); opting the collection in
// through collection_find_relations resurfaces it, and any disabled
// shared collection keeps the pair hidden even if another is enabled.
func TestSessionLoadNextPair_SameCollectionHiddenUntilOptIn(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	if cx == nil {
		t.Fatal("active gallery missing")
	}
	a := insertTestImage(t, srv.db())
	res, err := srv.db().Write.Exec(
		`INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		 VALUES ('/tmp/album-b.jpg', 'jpg', 2048, 'sha_album_b', datetime('now'))`)
	if err != nil {
		t.Fatalf("insert second image: %v", err)
	}
	bID, _ := res.LastInsertId()
	for _, id := range []int64{a, bID} {
		if _, err := srv.db().Write.Exec(
			`INSERT INTO image_collections (image_id, name) VALUES (?, 'Album')`, id); err != nil {
			t.Fatalf("add membership: %v", err)
		}
	}
	queueRow(t, srv.db(), a, bID, 2)

	none := &Ceiling{level: "", cx: cx}
	pair, visible, raw, err := loadNextPair(cx, "smallest_distance_first", none, 0, 0)
	if err != nil {
		t.Fatalf("loadNextPair: %v", err)
	}
	if pair != nil || visible != 0 || raw != 0 {
		t.Fatalf("same-collection pair surfaced by default: pair=%v visible=%d raw=%d", pair, visible, raw)
	}
	if c := loadRelationsCounts(cx, none); c.QueueOpen != 0 {
		t.Fatalf("hub QueueOpen = %d, want 0 while the collection is opted out", c.QueueOpen)
	}

	// Opt the collection in (lower-cased: the flag is NOCASE like the label).
	if _, err := srv.db().Write.Exec(
		`INSERT INTO collection_find_relations (name) VALUES ('album')`); err != nil {
		t.Fatalf("opt in: %v", err)
	}
	pair, visible, raw, err = loadNextPair(cx, "smallest_distance_first", none, 0, 0)
	if err != nil {
		t.Fatalf("loadNextPair after opt-in: %v", err)
	}
	if !pairHas(pair, a, bID) || visible != 1 || raw != 1 {
		t.Fatalf("opted-in pair missing: pair=%v visible=%d raw=%d", pair, visible, raw)
	}
	if c := loadRelationsCounts(cx, none); c.QueueOpen != 1 {
		t.Fatalf("hub QueueOpen = %d, want 1 after opt-in", c.QueueOpen)
	}

	// A second, still-disabled shared collection hides the pair again.
	for _, id := range []int64{a, bID} {
		if _, err := srv.db().Write.Exec(
			`INSERT INTO image_collections (image_id, name) VALUES (?, 'Other')`, id); err != nil {
			t.Fatalf("add second membership: %v", err)
		}
	}
	pair, visible, raw, err = loadNextPair(cx, "smallest_distance_first", none, 0, 0)
	if err != nil {
		t.Fatalf("loadNextPair with mixed collections: %v", err)
	}
	if pair != nil || visible != 0 || raw != 0 {
		t.Fatalf("pair sharing a disabled collection surfaced: pair=%v visible=%d raw=%d", pair, visible, raw)
	}
}
