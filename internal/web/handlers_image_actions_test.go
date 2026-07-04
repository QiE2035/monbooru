package web

import (
	"image/color"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/models"
)

// promoteCanonical on a gone-file alias must report the refusal as a flash
// (HX-Trigger) and leave the operator on the detail page, not navigate to a
// bare error body, and must not change the canonical.
func TestPromoteCanonical_MissingFileFlashesForHTMX(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	imgPath := filepath.Join(cx.GalleryPath, "pc.png")
	if err := os.WriteFile(imgPath, tinyPNG(t, 8, 8, color.RGBA{1, 2, 3, 255}), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, _, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, imgPath, "png", "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	gone := filepath.Join(cx.GalleryPath, "gone.png")
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, rec.ID, gone,
	); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"path": {gone}, "_csrf": {srv.csrfToken("anon")}}
	req := httptest.NewRequest("POST", "/images/"+strconv.FormatInt(rec.ID, 10)+"/canonical-path", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (htmx flash, no navigation); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("HX-Trigger"), "monbooru:flash") {
		t.Errorf("HX-Trigger = %q, want a monbooru:flash", w.Header().Get("HX-Trigger"))
	}
	var canon string
	if err := cx.DB.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, rec.ID).Scan(&canon); err != nil {
		t.Fatal(err)
	}
	if canon != imgPath {
		t.Errorf("canonical_path = %q, want unchanged %q", canon, imgPath)
	}
}

// A note POST against a nonexistent image must not flash success.
func TestSetNote_MissingImage404s(t *testing.T) {
	srv := newTestServer(t)

	form := url.Values{"note": {"hello"}, "_csrf": {srv.csrfToken("anon")}}
	req := httptest.NewRequest("POST", "/images/999999/note", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestSetNote_PersistsAndFlashes(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "note1.png", 6, 6)

	form := url.Values{"note": {"keep this"}, "_csrf": {srv.csrfToken("anon")}}
	req := httptest.NewRequest("POST", "/images/"+strconv.FormatInt(id, 10)+"/note", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	var note string
	if err := srv.Active().DB.Read.QueryRow(`SELECT note FROM images WHERE id = ?`, id).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note != "keep this" {
		t.Errorf("note = %q, want %q", note, "keep this")
	}
}

func TestSetSourceCommentary_SetAndRemove(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "comm1.png", 6, 6)
	if err := gallery.AddSourceMembership(srv.db(), id, "danbooru", "", "https://d/1"); err != nil {
		t.Fatal(err)
	}
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		form.Set("_csrf", srv.csrfToken("anon"))
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}
	commentary := func() string {
		t.Helper()
		var c string
		if err := srv.Active().DB.Read.QueryRow(
			`SELECT commentary FROM image_sources WHERE image_id = ? AND site = 'danbooru'`, id).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}

	base := "/images/" + strconv.FormatInt(id, 10)
	if w := post(base+"/commentary/set", url.Values{"site": {"danbooru"}, "commentary": {"artist words"}}); w.Code != http.StatusNoContent {
		t.Fatalf("set: %d %s", w.Code, w.Body.String())
	}
	if got := commentary(); got != "artist words" {
		t.Errorf("commentary = %q, want set", got)
	}
	if w := post(base+"/commentary/remove", url.Values{"site": {"danbooru"}}); w.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	if got := commentary(); got != "" {
		t.Errorf("commentary after remove = %q, want empty (origin kept)", got)
	}
}

func TestRemoveSource_HandlerCascadesAnnotations(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "remsrc.png", 6, 6)
	if err := gallery.AddSourceMembership(srv.db(), id, "danbooru", "", "https://d/1"); err != nil {
		t.Fatal(err)
	}
	if err := gallery.ReplaceSourceAnnotations(srv.db(), id, "danbooru", "",
		[]models.Annotation{{X: 1, Y: 1, W: 2, H: 2, Body: "b"}}); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"site": {"danbooru"}, "_csrf": {srv.csrfToken("anon")}}
	req := httptest.NewRequest("POST", "/images/"+strconv.FormatInt(id, 10)+"/sources/remove", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	var n int
	if err := srv.Active().DB.Read.QueryRow(`SELECT COUNT(*) FROM image_annotations WHERE image_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("annotations after source remove = %d, want 0", n)
	}
}

func TestFetchSource_PendingPillAndUnreachable(t *testing.T) {
	accepted := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepted++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer stub.Close()

	srv := newTestServer(t)
	id := seedImage(t, srv, "fetch1.png", 6, 6)
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	post := func(u string) *httptest.ResponseRecorder {
		form := url.Values{"url": {u}, "_csrf": {srv.csrfToken("anon")}}
		req := httptest.NewRequest("POST", "/images/"+strconv.FormatInt(id, 10)+"/sources/fetch", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}

	w := post("https://d/1")
	if accepted != 1 {
		t.Fatalf("peer accepted %d enqueues, want 1", accepted)
	}
	if body := w.Body.String(); !strings.Contains(body, "Fetching tags from source") || !strings.Contains(body, "fetch-status") {
		t.Errorf("expected the polling pending pill, got %q", body)
	}

	// A dead peer surfaces inline instead of recording a pending state.
	stub.Close()
	w = post("https://d/2")
	if body := w.Body.String(); !strings.Contains(body, "could not reach monloader") {
		t.Errorf("expected the unreachable error inline, got %q", body)
	}
}
