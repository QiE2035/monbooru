package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/models"
)

func postBatchSource(t *testing.T, srv *Server, form url.Values) {
	t.Helper()
	form.Set("_csrf", srv.csrfToken("anon"))
	req := httptest.NewRequest("POST", "/internal/batch-source", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch-source: %d, %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !srv.jobs.IsRunning() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("batch-source job never drained")
}

// The add branch must rebind the scalar mirror to each row's oldest origin:
// filling the source column wherever it was blank pairs the new label with
// an older unlabeled row's url, a (source, url) combination no origin row
// holds.
func TestBatchSource_AddKeepsMirrorOnOldestOrigin(t *testing.T) {
	srv := newTestServer(t)
	urlOnly := seedImage(t, srv, "b1.png", 4, 4)
	bare := seedImage(t, srv, "b2.png", 5, 5)
	if err := gallery.AddSourceMembership(srv.db(), urlOnly, "", "", "https://x/legacy"); err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"scope": {"selection"},
		"site":  {"pixiv"},
		"ids":   {strconv.FormatInt(urlOnly, 10), strconv.FormatInt(bare, 10)},
	}
	postBatchSource(t, srv, form)

	var src, u string
	if err := srv.Active().DB.Read.QueryRow(`SELECT source, url FROM images WHERE id = ?`, urlOnly).Scan(&src, &u); err != nil {
		t.Fatal(err)
	}
	if src != "" || u != "https://x/legacy" {
		t.Errorf("url-only image mirror = (%q,%q), want the unlabeled oldest origin kept", src, u)
	}
	if err := srv.Active().DB.Read.QueryRow(`SELECT source, url FROM images WHERE id = ?`, bare).Scan(&src, &u); err != nil {
		t.Fatal(err)
	}
	if src != "pixiv" || u != "" {
		t.Errorf("bare image mirror = (%q,%q), want the fresh pixiv origin", src, u)
	}
	var n int
	if err := srv.Active().DB.Read.QueryRow(
		`SELECT COUNT(*) FROM image_sources WHERE image_id = ? AND site = 'pixiv'`, urlOnly).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pixiv origin rows on url-only image = %d, want 1", n)
	}
}

func TestBatchSource_RemoveCascadesAnnotationsAndRebinds(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "b3.png", 6, 6)
	if err := gallery.AddSourceMembership(srv.db(), id, "pixiv", "", "https://p/1"); err != nil {
		t.Fatal(err)
	}
	if err := gallery.AddSourceMembership(srv.db(), id, "gelbooru", "", "https://g/1"); err != nil {
		t.Fatal(err)
	}
	if err := gallery.ReplaceSourceAnnotations(srv.db(), id, "pixiv", "",
		[]models.Annotation{{X: 1, Y: 1, W: 2, H: 2, Body: "b"}}); err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"scope": {"selection"},
		"site":  {"pixiv"},
		"mode":  {"remove"},
		"ids":   {strconv.FormatInt(id, 10)},
	}
	postBatchSource(t, srv, form)

	var srcs, anns int
	if err := srv.Active().DB.Read.QueryRow(`SELECT COUNT(*) FROM image_sources WHERE image_id = ? AND site = 'pixiv'`, id).Scan(&srcs); err != nil {
		t.Fatal(err)
	}
	if err := srv.Active().DB.Read.QueryRow(`SELECT COUNT(*) FROM image_annotations WHERE image_id = ?`, id).Scan(&anns); err != nil {
		t.Fatal(err)
	}
	if srcs != 0 || anns != 0 {
		t.Errorf("after batch remove: pixiv rows=%d annotations=%d, want 0/0", srcs, anns)
	}
	var src, u string
	if err := srv.Active().DB.Read.QueryRow(`SELECT source, url FROM images WHERE id = ?`, id).Scan(&src, &u); err != nil {
		t.Fatal(err)
	}
	if src != "gelbooru" || u != "https://g/1" {
		t.Errorf("mirror = (%q,%q), want rebound to the surviving gelbooru origin", src, u)
	}
}

func TestBatchSourceFetch_EnqueuesPerURLAndSkipsURLLess(t *testing.T) {
	enqueued := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enqueued++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer stub.Close()

	srv := newTestServer(t)
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"
	withURL := seedImage(t, srv, "f1.png", 6, 6)
	alsoURL := seedImage(t, srv, "f2.png", 7, 7)
	noURL := seedImage(t, srv, "f3.png", 8, 8)
	for _, id := range []int64{withURL, alsoURL} {
		if err := gallery.AddSourceMembership(srv.db(), id, "danbooru", "", "https://d/1"); err != nil {
			t.Fatal(err)
		}
	}

	form := url.Values{
		"scope": {"selection"},
		"ids": {strconv.FormatInt(withURL, 10), strconv.FormatInt(alsoURL, 10),
			strconv.FormatInt(noURL, 10)},
	}
	form.Set("_csrf", srv.csrfToken("anon"))
	req := httptest.NewRequest("POST", "/internal/batch-source-fetch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch-source-fetch: %d, %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.jobs.IsRunning() {
		time.Sleep(20 * time.Millisecond)
	}
	if srv.jobs.IsRunning() {
		t.Fatal("batch fetch job never drained")
	}
	if enqueued != 2 {
		t.Errorf("peer received %d enqueues, want 2 (url-less image skipped)", enqueued)
	}
}
