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

func TestAnnotationHandlers_AddEditRemove(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ann.png", 100, 80)
	post := func(form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		form.Set("_csrf", srv.csrfToken("anon"))
		req := httptest.NewRequest("POST", "/images/"+strconv.FormatInt(id, 10)+"/annotations/set", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}

	if w := post(url.Values{"x": {"5"}, "y": {"6"}, "w": {"10"}, "h": {"12"}, "body": {"hello"}}); w.Code != http.StatusNoContent {
		t.Fatalf("add: %d %s", w.Code, w.Body.String())
	}
	anns, _ := gallery.AnnotationsForImage(srv.db(), id)
	if len(anns) != 1 || !anns[0].Manual || anns[0].Body != "hello" {
		t.Fatalf("want one manual box, got %+v", anns)
	}
	annID := anns[0].ID

	if w := post(url.Values{"id": {strconv.FormatInt(annID, 10)}, "x": {"1"}, "y": {"2"}, "w": {"3"}, "h": {"4"}, "body": {"edited"}}); w.Code != http.StatusNoContent {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	if anns, _ = gallery.AnnotationsForImage(srv.db(), id); anns[0].Body != "edited" || anns[0].X != 1 {
		t.Fatalf("edit did not apply: %+v", anns)
	}

	// A box whose x sits at the width has zero area after clamping; a negative
	// coord is invalid. Neither is accepted (HTMX errors flash at 200), so the
	// box count stays at the one edited box.
	post(url.Values{"x": {"200"}, "y": {"0"}, "w": {"10"}, "h": {"10"}})
	post(url.Values{"x": {"-1"}, "y": {"0"}, "w": {"10"}, "h": {"10"}})
	if anns, _ := gallery.AnnotationsForImage(srv.db(), id); len(anns) != 1 {
		t.Errorf("invalid boxes must be rejected; want 1 box, got %d", len(anns))
	}

	rem := url.Values{"id": {strconv.FormatInt(annID, 10)}, "_csrf": {srv.csrfToken("anon")}}
	req := httptest.NewRequest("POST", "/images/"+strconv.FormatInt(id, 10)+"/annotations/remove", strings.NewReader(rem.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	if anns, _ := gallery.AnnotationsForImage(srv.db(), id); len(anns) != 0 {
		t.Fatalf("want 0 after remove, got %d", len(anns))
	}
}

func TestAnnotationHandler_DimensionlessAccepts(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "dim.png", 10, 10)
	if _, err := srv.db().Write.Exec(`UPDATE images SET width = NULL, height = NULL WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"x": {"3"}, "y": {"4"}, "w": {"5"}, "h": {"6"}, "body": {"no-dims"}, "_csrf": {srv.csrfToken("anon")}}
	req := httptest.NewRequest("POST", "/images/"+strconv.FormatInt(id, 10)+"/annotations/set", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("dimensionless add: %d %s", w.Code, w.Body.String())
	}
	if anns, _ := gallery.AnnotationsForImage(srv.db(), id); len(anns) != 1 {
		t.Fatalf("a dimensionless image should still accept the box, got %d", len(anns))
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

func TestSetSourceOriginal_SetAndRemove(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "orig1.png", 6, 6)
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
	original := func() string {
		t.Helper()
		var o string
		if err := srv.Active().DB.Read.QueryRow(
			`SELECT original FROM image_sources WHERE image_id = ? AND site = 'danbooru'`, id).Scan(&o); err != nil {
			t.Fatal(err)
		}
		return o
	}

	base := "/images/" + strconv.FormatInt(id, 10)
	if w := post(base+"/original/set", url.Values{"site": {"danbooru"}, "original": {"https://pixiv/artworks/1"}}); w.Code != http.StatusNoContent {
		t.Fatalf("set: %d %s", w.Code, w.Body.String())
	}
	if got := original(); got != "https://pixiv/artworks/1" {
		t.Errorf("original = %q, want set", got)
	}
	if w := post(base+"/original/remove", url.Values{"site": {"danbooru"}}); w.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
	if got := original(); got != "" {
		t.Errorf("original after remove = %q, want empty (origin kept)", got)
	}

	// The per-source original is deliberately freeform (unlike the http(s)-only
	// image-level field): a multi-line, non-URL value is accepted verbatim so
	// the operator can edit whatever a booru's enrich stored.
	freeform := "some artist\nhttps://pixiv/artworks/2"
	if w := post(base+"/original/set", url.Values{"site": {"danbooru"}, "original": {freeform}}); w.Code != http.StatusNoContent {
		t.Fatalf("freeform set: %d %s", w.Code, w.Body.String())
	}
	if got := original(); got != freeform {
		t.Errorf("freeform original = %q, want it stored verbatim", got)
	}
}

// The image-level original source is an operator field, http(s)-validated,
// cleared by an empty submit; setting a source never touches it.
func TestSetImageOriginalSource_SetAndClear(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "orig2.png", 6, 6)
	post := func(form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		form.Set("_csrf", srv.csrfToken("anon"))
		req := httptest.NewRequest("POST", "/images/"+strconv.FormatInt(id, 10)+"/original-source", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}
	stored := func() string {
		t.Helper()
		var o string
		if err := srv.Active().DB.Read.QueryRow(`SELECT original_source FROM images WHERE id = ?`, id).Scan(&o); err != nil {
			t.Fatal(err)
		}
		return o
	}

	if w := post(url.Values{"original": {"not-a-url"}}); !strings.Contains(w.Body.String(), "http://") {
		t.Fatalf("non-http original should flash a validation error: %d %s", w.Code, w.Body.String())
	}
	if got := stored(); got != "" {
		t.Errorf("original_source after rejected input = %q, want empty", got)
	}
	if w := post(url.Values{"original": {"https://pixiv/artworks/1"}}); w.Code != http.StatusNoContent {
		t.Fatalf("set: %d %s", w.Code, w.Body.String())
	}
	if got := stored(); got != "https://pixiv/artworks/1" {
		t.Errorf("original_source = %q, want set", got)
	}
	if w := post(url.Values{"original": {""}}); w.Code != http.StatusNoContent {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	if got := stored(); got != "" {
		t.Errorf("original_source after clear = %q, want empty", got)
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
	if body := w.Body.String(); !strings.Contains(body, "Fetching data via monloader") || !strings.Contains(body, "fetch-status") {
		t.Errorf("expected the polling pending pill, got %q", body)
	}

	// A dead peer surfaces inline instead of recording a pending state.
	stub.Close()
	w = post("https://d/2")
	if body := w.Body.String(); !strings.Contains(body, "could not reach monloader") {
		t.Errorf("expected the unreachable error inline, got %q", body)
	}
}

// lookupImage sends both hashes for the unified "all" lookup (md5 hashed on
// demand, stored sha256) or the sha256 alone for a targeted "ptr" one; a 409
// from monloader (PTR off) degrades inline instead of recording a pending
// fetch.
func TestLookupImage_EnqueuesHashOnMonloader(t *testing.T) {
	var gotPath, gotBody string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b := make([]byte, 1024)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.WriteHeader(http.StatusAccepted)
	}))
	defer stub.Close()

	srv := newTestServer(t)
	id := seedImage(t, srv, "lookup1.png", 6, 6)
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	post := func(backend string) *httptest.ResponseRecorder {
		form := url.Values{"backend": {backend}, "_csrf": {srv.csrfToken("anon")}}
		req := httptest.NewRequest("POST", "/images/"+strconv.FormatInt(id, 10)+"/lookup", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}

	var canonPath, sha string
	if err := srv.db().Read.QueryRow(
		`SELECT canonical_path, sha256 FROM images WHERE id = ?`, id).Scan(&canonPath, &sha); err != nil {
		t.Fatal(err)
	}
	md5, err := gallery.Md5File(canonPath)
	if err != nil {
		t.Fatal(err)
	}

	w := post("all")
	if gotPath != "/api/v1/lookup" {
		t.Fatalf("peer path = %q, want /api/v1/lookup", gotPath)
	}
	for _, want := range []string{`"backend":"all"`, `"md5":"` + md5 + `"`, `"sha256":"` + sha + `"`, `"gallery":"default"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("all payload %q missing %q", gotBody, want)
		}
	}
	if body := w.Body.String(); !strings.Contains(body, "Fetching data via monloader") {
		t.Errorf("expected the polling pending pill, got %q", body)
	}

	post("ptr")
	if !strings.Contains(gotBody, `"backend":"ptr"`) || !strings.Contains(gotBody, `"sha256":"`+sha+`"`) {
		t.Errorf("ptr payload %q missing backend/sha256", gotBody)
	}

	post("booru")
	if !strings.Contains(gotBody, `"backend":"booru"`) || !strings.Contains(gotBody, `"md5":"`+md5+`"`) {
		t.Errorf("booru payload %q missing backend/md5", gotBody)
	}

	// PTR off on monloader: the 409 degrades inline, no pending state recorded.
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer refuse.Close()
	srv.clearFetchStatus(srv.activeName, id)
	srv.cfg.Monloader.APIURL = refuse.URL
	w = post("ptr")
	if body := w.Body.String(); !strings.Contains(body, "PTR lookup is unavailable") {
		t.Errorf("expected the ptr-unavailable message inline, got %q", body)
	}
	if _, ok := srv.loadFetchStatus(srv.activeName, id); ok {
		t.Error("a refused lookup must not record a pending fetch")
	}
}
