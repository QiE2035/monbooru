package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// awaitJobsDrain busy-waits until the per-request job has finished.
// Mirrors the loop in TestDeleteSearch_BulkDeleteReconcilesUsage.
func awaitJobsDrain(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !srv.jobs.IsRunning() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("background job never drained")
}

// seedRatedPair seeds one general-rated and one explicit-rated image
// and invalidates the caches so the test's first read sees the new
// rows.
func seedRatedPair(t *testing.T, srv *Server) (safe, explicit int64) {
	t.Helper()
	cx := srv.Active()
	safe = seedImage(t, srv, "safe.png", 10, 10)
	explicit = seedImage(t, srv, "explicit.png", 11, 11)
	if err := cx.TagSvc.AddTagToImage(safe, ratingTagIDWeb(t, cx.DB, "general"), false, nil); err != nil {
		t.Fatal(err)
	}
	if err := cx.TagSvc.AddTagToImage(explicit, ratingTagIDWeb(t, cx.DB, "explicit"), false, nil); err != nil {
		t.Fatal(err)
	}
	cx.InvalidateCaches()
	return safe, explicit
}

// postCeilingRequest posts form against path with ceiling=sensitive
// and returns the recorder.
func postCeilingRequest(t *testing.T, srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	csrf := srv.csrfToken("anon")
	form.Set("_csrf", csrf)
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(&http.Cookie{Name: "monbooru_rating_ceiling", Value: "sensitive"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// TestDeleteSearch_HonoursCeiling: deleting "all current search"
// with ceiling=sensitive must skip rows the operator can't see
// (rating:explicit).
func TestDeleteSearch_HonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	safe, explicit := seedRatedPair(t, srv)

	w := postCeilingRequest(t, srv, "/internal/delete-search", url.Values{"q": {""}})
	if w.Code != http.StatusAccepted {
		t.Fatalf("delete-search expected 202, got %d: %s", w.Code, w.Body.String())
	}
	awaitJobsDrain(t, srv)

	var safeAlive, explicitAlive int
	srv.db().Read.QueryRow(`SELECT COUNT(*) FROM images WHERE id = ?`, safe).Scan(&safeAlive)
	srv.db().Read.QueryRow(`SELECT COUNT(*) FROM images WHERE id = ?`, explicit).Scan(&explicitAlive)
	if safeAlive != 0 {
		t.Errorf("safe image %d should have been deleted; still present", safe)
	}
	if explicitAlive != 1 {
		t.Errorf("explicit image %d should have survived under ceiling=sensitive; got %d rows", explicit, explicitAlive)
	}
}

// TestBatchTag_ScopeSearch_HonoursCeiling: tagging "current search"
// must skip ceiling-hidden rows.
func TestBatchTag_ScopeSearch_HonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	safe, explicit := seedRatedPair(t, srv)

	w := postCeilingRequest(t, srv, "/internal/batch-tag", url.Values{
		"scope": {"search"}, "q": {""}, "op": {"add"}, "tags": {"mark_under_ceiling"},
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch-tag expected 202, got %d: %s", w.Code, w.Body.String())
	}
	awaitJobsDrain(t, srv)

	carriesMark := func(id int64) bool {
		var n int
		srv.db().Read.QueryRow(`
			SELECT COUNT(*) FROM image_tags it
			JOIN tags t ON t.id = it.tag_id
			WHERE it.image_id = ? AND t.name = 'mark_under_ceiling'`, id,
		).Scan(&n)
		return n == 1
	}
	if !carriesMark(safe) {
		t.Errorf("safe image %d should have gained the mark", safe)
	}
	if carriesMark(explicit) {
		t.Errorf("explicit image %d should NOT have gained the mark under ceiling=sensitive", explicit)
	}
}

// TestBatchMove_ScopeSearch_HonoursCeiling: the explicit image's
// folder stays untouched under ceiling=sensitive.
func TestBatchMove_ScopeSearch_HonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	safe, explicit := seedRatedPair(t, srv)

	target := "moved"
	w := postCeilingRequest(t, srv, "/internal/batch-move", url.Values{
		"scope": {"search"}, "q": {""}, "folder": {target},
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch-move expected 202, got %d: %s", w.Code, w.Body.String())
	}
	awaitJobsDrain(t, srv)

	folderOf := func(id int64) string {
		var p string
		srv.db().Read.QueryRow(`SELECT folder_path FROM images WHERE id = ?`, id).Scan(&p)
		return p
	}
	if got := folderOf(safe); got != target {
		t.Errorf("safe image %d folder = %q, want %q", safe, got, target)
	}
	if got := folderOf(explicit); got == target {
		t.Errorf("explicit image %d should not have moved under ceiling=sensitive; folder = %q", explicit, got)
	}
}

// TestMarkedWalkerDeleteAll_HonoursCeiling pins the relations
// "Delete all duplicate images" bulk action against the ceiling.
// Two duplicate groups, one with an explicit-rated non-original
// member: the SFW operator clicking Delete-all must only delete
// the clean group's non-original, leaving the explicit row in
// place because they can't see it on the walker.
func TestMarkedWalkerDeleteAll_HonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	rel := cx.RelationsSvc

	dim := 7
	mkImg := func(name string) int64 {
		dim++
		return seedImage(t, srv, name, dim, dim)
	}

	safeOrig, safeDup := mkImg("safe_orig.png"), mkImg("safe_dup.png")
	taintOrig, taintDup := mkImg("taint_orig.png"), mkImg("taint_dup.png")
	if err := cx.TagSvc.AddTagToImage(taintDup, ratingTagIDWeb(t, cx.DB, "explicit"), false, nil); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddDuplicate(safeOrig, safeDup); err != nil {
		t.Fatal(err)
	}
	if err := rel.AddDuplicate(taintOrig, taintDup); err != nil {
		t.Fatal(err)
	}

	w := postCeilingRequest(t, srv, "/relations/duplicates/marked/delete-all", url.Values{})
	if w.Code != http.StatusOK {
		t.Fatalf("delete-all expected 200, got %d: %s", w.Code, w.Body.String())
	}
	awaitJobsDrain(t, srv)

	alive := func(id int64) int {
		var n int
		srv.db().Read.QueryRow(`SELECT COUNT(*) FROM images WHERE id = ?`, id).Scan(&n)
		return n
	}
	if alive(safeDup) != 0 {
		t.Errorf("safe duplicate %d should have been deleted", safeDup)
	}
	if alive(taintDup) != 1 {
		t.Errorf("explicit duplicate %d should have survived (ceiling=sensitive hides it from the walker)", taintDup)
	}
	if alive(safeOrig) != 1 || alive(taintOrig) != 1 {
		t.Errorf("originals must always survive: safeOrig=%d taintOrig=%d", alive(safeOrig), alive(taintOrig))
	}
}

// TestFileDuplicatesRemoveAll_HonoursCeiling pins the relations
// "Delete all duplicate files" bulk action against the ceiling. One
// SFW image and one explicit image each get a non-canonical alias
// path; SFW operator clicking Delete-all must only remove the SFW
// alias.
func TestFileDuplicatesRemoveAll_HonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()

	dim := 9
	mkImg := func(name string) int64 {
		dim++
		return seedImage(t, srv, name, dim, dim)
	}

	safe := mkImg("safe.png")
	explicit := mkImg("explicit.png")
	if err := cx.TagSvc.AddTagToImage(explicit, ratingTagIDWeb(t, cx.DB, "explicit"), false, nil); err != nil {
		t.Fatal(err)
	}
	// Drop a non-canonical alias on each image so the bulk-remove has
	// something to operate on. Paths must be absolute inside the
	// gallery root for unlinkUnderGallery's safety gate to accept
	// them; the underlying file doesn't need to exist (os.Remove
	// tolerates ENOENT in that helper).
	addAlias := func(imgID int64, name string) int64 {
		t.Helper()
		fullPath := filepath.Join(cx.GalleryPath, name)
		res, err := srv.db().Write.Exec(
			`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, imgID, fullPath,
		)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	safeAlias := addAlias(safe, "safe_alias.png")
	explicitAlias := addAlias(explicit, "explicit_alias.png")

	w := postCeilingRequest(t, srv, "/relations/file-duplicates/remove", url.Values{"all": {"true"}})
	if w.Code != http.StatusOK {
		t.Fatalf("file-duplicates remove all expected 200, got %d: %s", w.Code, w.Body.String())
	}
	awaitJobsDrain(t, srv)

	pathAlive := func(id int64) int {
		var n int
		srv.db().Read.QueryRow(`SELECT COUNT(*) FROM image_paths WHERE id = ?`, id).Scan(&n)
		return n
	}
	if pathAlive(safeAlias) != 0 {
		t.Errorf("safe alias %d should have been deleted", safeAlias)
	}
	if pathAlive(explicitAlias) != 1 {
		t.Errorf("explicit alias %d should have survived under ceiling=sensitive", explicitAlias)
	}
}

// TestBatchInbox_ScopeSearch_HonoursCeiling covers the shared
// resolveBatchScope code path. Inbox / favorite / collection / strip
// all share that resolver, so one assertion covers the family.
func TestBatchInbox_ScopeSearch_HonoursCeiling(t *testing.T) {
	srv := newTestServer(t)
	safe, explicit := seedRatedPair(t, srv)
	for _, id := range []int64{safe, explicit} {
		var v int
		srv.db().Read.QueryRow(`SELECT is_inbox FROM images WHERE id = ?`, id).Scan(&v)
		if v != 1 {
			t.Fatalf("seedImage %d expected is_inbox=1, got %d", id, v)
		}
	}

	w := postCeilingRequest(t, srv, "/internal/batch-inbox", url.Values{
		"scope": {"search"}, "q": {""},
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch-inbox expected 202, got %d: %s", w.Code, w.Body.String())
	}
	awaitJobsDrain(t, srv)

	inboxOf := func(id int64) int {
		var v int
		srv.db().Read.QueryRow(`SELECT is_inbox FROM images WHERE id = ?`, id).Scan(&v)
		return v
	}
	if got := inboxOf(safe); got != 0 {
		t.Errorf("safe image %d should have been toggled out of inbox; is_inbox=%d", safe, got)
	}
	if got := inboxOf(explicit); got != 1 {
		t.Errorf("explicit image %d should have stayed in inbox under ceiling=sensitive; is_inbox=%d", explicit, got)
	}
}
