package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// insertRawImage writes a synthetic images row with a deterministic
// SHA-256 derived from path. Useful when the test only cares about
// relation rows pointing at distinct image ids, not actual on-disk
// state.
func insertRawImage(t *testing.T, srv *Server, path, sha string) int64 {
	t.Helper()
	if sha == "" {
		sum := sha256.Sum256([]byte(path))
		sha = hex.EncodeToString(sum[:])
	}
	res, err := srv.db().Write.Exec(
		`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, source_type, origin)
		 VALUES (?, ?, '', 'png', 1024, 'image', 'test')`,
		sha, path,
	)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// reviewAgainPost happy paths: dissolve a 2-member dup / alt group and
// requeue the pair into potential_relation_pairs. Mirrored across the
// two kinds the handler dispatches.
func TestReviewAgainPost_DupGroup(t *testing.T) {
	srv := newTestServer(t)
	a := insertRawImage(t, srv, "a.png", "")
	b := insertRawImage(t, srv, "b.png", "")
	if err := srv.Active().RelationsSvc.AddDuplicate(a, b); err != nil {
		t.Fatal(err)
	}
	var gid int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM dup_groups LIMIT 1`).Scan(&gid); err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"_csrf":    {srv.csrfToken("anon")},
		"type":     {"review-again"},
		"kind":     {"duplicate"},
		"group_id": {strconv.FormatInt(gid, 10)},
	}
	req := httptest.NewRequest("POST", "/relations/remove", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	var n int
	if err := srv.db().Read.QueryRow(`SELECT COUNT(*) FROM dup_groups`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("dup_groups remaining = %d, want 0", n)
	}
	var queued int
	if err := srv.db().Read.QueryRow(`SELECT COUNT(*) FROM potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?`, a, b).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Errorf("potential_relation_pairs entries = %d, want 1", queued)
	}
}

func TestReviewAgainPost_AltGroup(t *testing.T) {
	srv := newTestServer(t)
	a := insertRawImage(t, srv, "a.png", "")
	b := insertRawImage(t, srv, "b.png", "")
	if err := srv.Active().RelationsSvc.AddAlternate(a, b); err != nil {
		t.Fatal(err)
	}
	var gid int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM alt_groups LIMIT 1`).Scan(&gid); err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"_csrf":    {srv.csrfToken("anon")},
		"type":     {"review-again"},
		"kind":     {"alternate"},
		"group_id": {strconv.FormatInt(gid, 10)},
	}
	req := httptest.NewRequest("POST", "/relations/remove", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	var n int
	if err := srv.db().Read.QueryRow(`SELECT COUNT(*) FROM alt_groups`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("alt_groups remaining = %d, want 0", n)
	}
	var queued int
	if err := srv.db().Read.QueryRow(`SELECT COUNT(*) FROM potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?`, a, b).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Errorf("potential_relation_pairs entries = %d, want 1", queued)
	}
}

// Unknown kind surfaces a flash err rather than a silent success.
func TestReviewAgainPost_UnknownKind(t *testing.T) {
	srv := newTestServer(t)
	form := url.Values{
		"_csrf":    {srv.csrfToken("anon")},
		"type":     {"review-again"},
		"kind":     {"nonsense"},
		"group_id": {"1"},
	}
	req := httptest.NewRequest("POST", "/relations/remove", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Unknown review-again kind") {
		t.Errorf("body = %s", w.Body.String())
	}
}
