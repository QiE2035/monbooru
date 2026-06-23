package web

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leqwin/monbooru/internal/gallery"
)

// seedManga writes a small cbz with the supplied per-page images into
// the active gallery and ingests it. Returns the new image id.
func seedManga(t *testing.T, srv *Server, name string, pages [][]byte) int64 {
	t.Helper()
	cx := srv.Active()
	if cx == nil {
		t.Fatal("no active gallery")
	}
	cbzPath := filepath.Join(cx.GalleryPath, name)
	f, err := os.Create(cbzPath)
	if err != nil {
		t.Fatalf("create cbz: %v", err)
	}
	zw := zip.NewWriter(f)
	for i, body := range pages {
		w, err := zw.Create(fmt.Sprintf("page_%03d.png", i+1))
		if err != nil {
			_ = f.Close()
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write(body); err != nil {
			_ = f.Close()
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		t.Fatalf("zip close: %v", err)
	}
	_ = f.Close()
	rec, _, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, cbzPath, "cbz", "")
	if err != nil {
		t.Fatalf("Ingest cbz: %v", err)
	}
	return rec.ID
}

func tinyPNG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestReader_RendersAtPage1(t *testing.T) {
	srv := newTestServer(t)
	pages := [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{0, 0, 0, 255}),
		tinyPNG(t, 8, 8, color.RGBA{0, 0, 0, 255}),
		tinyPNG(t, 8, 8, color.RGBA{0, 0, 0, 255}),
	}
	id := seedManga(t, srv, "m.cbz", pages)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/images/"+strconv.FormatInt(id, 10)+"/read?page=1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="reader-img"`) {
		t.Errorf("reader-img not rendered")
	}
	if !strings.Contains(body, "1 / 3") {
		t.Errorf("page counter not rendered: %q", snippet(body, "reader-counter"))
	}
}

func TestReader_OutOfRangePageClamped(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "m.cbz", [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{0, 0, 0, 255}),
		tinyPNG(t, 8, 8, color.RGBA{0, 0, 0, 255}),
	})
	h := srv.Handler()

	// Each out-of-range page redirects to the clamped value (URL
	// coherence: bookmark to `?page=999` shouldn't disagree with what
	// the reader actually renders).
	cases := map[string]string{
		"0":   "1",
		"-3":  "1",
		"999": "2",
	}
	for raw, want := range cases {
		req := httptest.NewRequest("GET", "/images/"+strconv.FormatInt(id, 10)+"/read?page="+raw, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("page=%s: status %d, want 303", raw, w.Code)
		}
		loc := w.Header().Get("Location")
		if !strings.Contains(loc, "page="+want) {
			t.Errorf("page=%s redirected to %q, want page=%s", raw, loc, want)
		}
	}
}

func TestReader_404OnNonManga(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "regular.png", 8, 8)
	h := srv.Handler()
	req := httptest.NewRequest("GET", "/images/"+strconv.FormatInt(id, 10)+"/read?page=1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("non-manga reader = %d, want 404", w.Code)
	}
}

func TestPagesGrid_Renders(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "m.cbz", [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{0, 0, 0, 255}),
		tinyPNG(t, 8, 8, color.RGBA{0, 0, 0, 255}),
		tinyPNG(t, 8, 8, color.RGBA{0, 0, 0, 255}),
	})
	h := srv.Handler()
	req := httptest.NewRequest("GET", "/images/"+strconv.FormatInt(id, 10)+"/pages", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, frag := range []string{
		"manga-pages-grid",
		"/page/1/thumb",
		"/page/3/thumb",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("missing %q in pages grid", frag)
		}
	}
}

func TestServeMangaPage_404OnRange(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "m.cbz", [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{0, 0, 0, 255}),
	})
	h := srv.Handler()
	req := httptest.NewRequest("GET", "/images/"+strconv.FormatInt(id, 10)+"/page/2", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("page 2 of 1-page archive = %d, want 404", w.Code)
	}
}

func TestServeMangaPage_HitServesBytes(t *testing.T) {
	srv := newTestServer(t)
	pic := tinyPNG(t, 16, 16, color.RGBA{77, 77, 77, 255})
	id := seedManga(t, srv, "m.cbz", [][]byte{pic})
	h := srv.Handler()
	req := httptest.NewRequest("GET", "/images/"+strconv.FormatInt(id, 10)+"/page/1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), pic) {
		t.Errorf("served bytes do not match the source page")
	}
}

func TestServeMangaPage_RejectsTraversal(t *testing.T) {
	srv := newTestServer(t)
	pic := tinyPNG(t, 16, 16, color.RGBA{77, 77, 77, 255})
	id := seedManga(t, srv, "m.cbz", [][]byte{pic})
	// Drift canonical_path outside the gallery root; the page route must
	// 404 instead of extracting from an out-of-tree archive.
	outside := filepath.Join(t.TempDir(), "evil.cbz")
	cx := srv.Active()
	if _, err := cx.DB.Write.Exec(`UPDATE images SET canonical_path = ? WHERE id = ?`, outside, id); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/images/"+strconv.FormatInt(id, 10)+"/page/1", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestSetCollection_Adds(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "m.cbz", [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{0, 0, 0, 255}),
	})
	h := srv.Handler()
	body := strings.NewReader("collection=Naruto&_csrf=" + srv.csrfToken("anon"))
	req := httptest.NewRequest("POST", "/images/"+strconv.FormatInt(id, 10)+"/collections/set", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("set collection status = %d, body = %s", w.Code, w.Body.String())
	}
	cx := srv.Active()
	var got string
	if err := cx.DB.Read.QueryRow(`SELECT series FROM images WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "Naruto" {
		t.Errorf("home collection (DB col 'series') = %q, want %q", got, "Naruto")
	}
	cols, err := gallery.CollectionsForImage(cx.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 || cols[0].Name != "Naruto" {
		t.Errorf("memberships = %#v, want one Naruto", cols)
	}
}

// Adding several memberships keeps the first as the home mirror;
// removing the home promotes the next, and removing the last clears it.
func TestCollectionMembership_PromoteHomeOnRemove(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "multi.png", 7, 7)
	d := srv.Active().DB
	if err := gallery.AddCollectionMembership(d, id, "First", nil); err != nil {
		t.Fatal(err)
	}
	if err := gallery.AddCollectionMembership(d, id, "Second", nil); err != nil {
		t.Fatal(err)
	}
	home := func() string {
		var s string
		if err := d.Read.QueryRow(`SELECT series FROM images WHERE id = ?`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if home() != "First" {
		t.Errorf("home = %q, want First", home())
	}
	if cols, _ := gallery.CollectionsForImage(d, id); len(cols) != 2 {
		t.Fatalf("memberships = %d, want 2", len(cols))
	}
	if err := gallery.RemoveCollectionMembership(d, id, "First"); err != nil {
		t.Fatal(err)
	}
	if home() != "Second" {
		t.Errorf("after removing home, series = %q, want Second", home())
	}
	if err := gallery.RemoveCollectionMembership(d, id, "Second"); err != nil {
		t.Fatal(err)
	}
	if home() != "" {
		t.Errorf("after removing all, series = %q, want empty", home())
	}
}

// snippet returns the substring of s containing needle plus 60 char
// trailing context, or the empty string. Helps surface why a render
// assertion failed without dumping the entire HTML body.
func snippet(s, needle string) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return ""
	}
	end := i + 120
	if end > len(s) {
		end = len(s)
	}
	return s[i:end]
}

// Collection management spans every file type: a regular image can
// carry both a collection label and an integer order, surfaced via
// the same /external endpoint and collection: filter that the manga
// path uses.
func TestSetCollection_NonMangaWithOrder(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "non-manga.png", 16, 16)
	h := srv.Handler()

	body := strings.NewReader("collection=Vol1&collection_order=7&_csrf=" + srv.csrfToken("anon"))
	req := httptest.NewRequest("POST", "/images/"+strconv.FormatInt(id, 10)+"/collections/set", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("update collection status = %d, body = %s", w.Code, w.Body.String())
	}

	var collectionGot string
	var orderGot sql.NullInt64
	if err := srv.Active().DB.Read.QueryRow(
		`SELECT series, series_order FROM images WHERE id = ?`, id,
	).Scan(&collectionGot, &orderGot); err != nil {
		t.Fatal(err)
	}
	if collectionGot != "Vol1" {
		t.Errorf("collection = %q, want Vol1", collectionGot)
	}
	if !orderGot.Valid || orderGot.Int64 != 7 {
		t.Errorf("collection_order = %v (valid=%v), want 7", orderGot.Int64, orderGot.Valid)
	}
}

// /internal/collection/suggest returns existing labels matching the
// typed prefix - used by the detail-page edit dialog and the batch
// dialogs. The DB column is still named `series` for schema stability.
func TestCollectionSuggest_PrefixMatch(t *testing.T) {
	srv := newTestServer(t)
	id1 := seedImage(t, srv, "a.png", 4, 4)
	id2 := seedImage(t, srv, "b.png", 5, 5)
	if err := gallery.SetHomeCollection(srv.Active().DB, id1, "Naruto", nil); err != nil {
		t.Fatal(err)
	}
	if err := gallery.SetHomeCollection(srv.Active().DB, id2, "Bleach", nil); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/internal/collection/suggest?prefix=Nar", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("collection suggest: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `data-series="Naruto"`) {
		t.Errorf("Naruto missing from suggest: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `data-series="Bleach"`) {
		t.Errorf("Bleach should not match prefix Nar: %s", w.Body.String())
	}
}

// The matching `collection:` search filter is NOCASE; the suggest
// must agree so a user typing a lowercase prefix sees a capitalised
// label and vice versa.
func TestCollectionSuggest_CaseInsensitivePrefix(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "a.png", 4, 4)
	if err := gallery.SetHomeCollection(srv.Active().DB, id, "Cat Series", nil); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"cat", "Cat", "CAT"} {
		req := httptest.NewRequest("GET", "/internal/collection/suggest?prefix="+prefix, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("collection suggest prefix=%s: %d", prefix, w.Code)
		}
		if !strings.Contains(w.Body.String(), `data-series="Cat Series"`) {
			t.Errorf("prefix=%s should surface Cat Series, got %s", prefix, w.Body.String())
		}
	}
}

// batch-collection adds the label across every image in the selection in
// chunked transactions. Fresh rows (no prior collection) adopt it as the
// home mirror with a NULL order - order is set per-image on the detail page.
func TestBatchCollection_AssignsLabel(t *testing.T) {
	srv := newTestServer(t)
	var ids []int64
	for i := 0; i < 3; i++ {
		// Vary dimensions so each PNG hashes distinct - else the
		// SHA-256 dedup collapses identical blanks into one row and
		// the batch UPDATEs land on a single id.
		ids = append(ids, seedImage(t, srv, fmt.Sprintf("p%d.png", i), 4+i, 4+i))
	}
	form := url.Values{
		"_csrf":      {srv.csrfToken("anon")},
		"scope":      {"selection"},
		"collection": {"Bundle"},
	}
	for _, id := range ids {
		form.Add("ids", strconv.FormatInt(id, 10))
	}
	req := httptest.NewRequest("POST", "/internal/batch-collection", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch-collection: %d, %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !srv.jobs.IsRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if srv.jobs.IsRunning() {
		t.Fatal("batch-collection job never drained")
	}
	for i, id := range ids {
		var collection string
		var order sql.NullInt64
		if err := srv.Active().DB.Read.QueryRow(
			`SELECT series, series_order FROM images WHERE id = ?`, id,
		).Scan(&collection, &order); err != nil {
			t.Fatal(err)
		}
		if collection != "Bundle" {
			t.Errorf("ids[%d]: collection = %q, want Bundle", i, collection)
		}
		if order.Valid {
			t.Errorf("ids[%d]: collection_order should remain NULL, got %d", i, order.Int64)
		}
	}
}

// Batch add is additive: an image already in a collection keeps it and
// gains the new one; its home (the series mirror) is left untouched.
func TestBatchCollection_AddsKeepingExisting(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "existing.png", 9, 9)
	if err := gallery.SetHomeCollection(srv.Active().DB, id, "Original", nil); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"_csrf":      {srv.csrfToken("anon")},
		"scope":      {"selection"},
		"collection": {"Added"},
		"ids":        {strconv.FormatInt(id, 10)},
	}
	req := httptest.NewRequest("POST", "/internal/batch-collection", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch-collection: %d, %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !srv.jobs.IsRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var home string
	if err := srv.Active().DB.Read.QueryRow(`SELECT series FROM images WHERE id = ?`, id).Scan(&home); err != nil {
		t.Fatal(err)
	}
	if home != "Original" {
		t.Errorf("home = %q, want Original (unchanged)", home)
	}
	names := map[string]bool{}
	cols, err := gallery.CollectionsForImage(srv.Active().DB, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cols {
		names[c.Name] = true
	}
	if !names["Original"] || !names["Added"] {
		t.Errorf("memberships = %#v, want both Original and Added", cols)
	}
}

// Batch remove drops the named membership; when it was the home, another
// membership is promoted into the mirror.
func TestBatchCollection_RemoveMode(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "rm.png", 11, 11)
	d := srv.Active().DB
	if err := gallery.SetHomeCollection(d, id, "Drop", nil); err != nil {
		t.Fatal(err)
	}
	if err := gallery.AddCollectionMembership(d, id, "Keep", nil); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"_csrf":      {srv.csrfToken("anon")},
		"scope":      {"selection"},
		"collection": {"Drop"},
		"mode":       {"remove"},
		"ids":        {strconv.FormatInt(id, 10)},
	}
	req := httptest.NewRequest("POST", "/internal/batch-collection", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch-collection remove: %d, %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !srv.jobs.IsRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cols, err := gallery.CollectionsForImage(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 || cols[0].Name != "Keep" {
		t.Errorf("memberships = %#v, want only Keep", cols)
	}
	var home string
	if err := d.Read.QueryRow(`SELECT series FROM images WHERE id = ?`, id).Scan(&home); err != nil {
		t.Fatal(err)
	}
	if home != "Keep" {
		t.Errorf("home = %q, want Keep (promoted)", home)
	}
}

// The detail-page Read and Pages anchors must forward every back_*
// param (back_q, back_sort, back_order, back_page, back_seed) so the
// gallery context survives the click-through. Both ride the same
// precomputed BackQS / BackKVQS fragment.
func TestDetailPage_MangaActionAnchorsForwardAllBackParams(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "ctx.cbz", [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{1, 2, 3, 255}),
		tinyPNG(t, 8, 8, color.RGBA{4, 5, 6, 255}),
	})

	url := fmt.Sprintf(
		"/images/%d?back_q=foo&back_sort=newest&back_order=desc&back_page=2&back_seed=42",
		id,
	)
	req := httptest.NewRequest("GET", url, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("detail GET: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Both anchors must carry every back_* fragment, not just back_q.
	// back_page is intentionally overridden by the handler to the page
	// that actually contains the current image; we just assert it's
	// present, not equal to the input.
	for _, must := range []string{
		`btn-read`,
		`back_q=foo`,
		`back_sort=newest`,
		`back_order=desc`,
		`back_page=`,
		`back_seed=42`,
	} {
		if !strings.Contains(body, must) {
			t.Errorf("detail body missing %q", must)
		}
	}
	// And the Pages anchor specifically must carry the encoded fragment,
	// not just back_q. Spot-check that the manga-actions section
	// contains the full set, and that no `?page=4%26...`-style
	// double-encoding has crept in.
	mangaSec := body[strings.Index(body, "manga-actions"):]
	for _, must := range []string{`/pages?`, `back_seed=42`, `back_sort=newest`} {
		if !strings.Contains(mangaSec, must) {
			t.Errorf("manga-actions section missing %q; section was %s", must, mangaSec[:min(len(mangaSec), 600)])
		}
	}
	if strings.Contains(mangaSec, "%26") || strings.Contains(mangaSec, "%3d") {
		t.Errorf("manga-actions href URL-encoded a separator - browser will reparse the trailing back_* as the page value: %s", mangaSec[:min(len(mangaSec), 600)])
	}
}

// The reader's chevron and bottom-bar nav anchors must not URL-encode
// the back_* tail when it follows `?page=N`. BackKVQS is typed as
// template.URL so html/template inserts it verbatim instead of
// treating the value as a query VALUE and percent-encoding the `&`
// separators (which the browser would then deliver as the literal
// `page` value, defaulting the next render back to page 1).
func TestReaderNavHrefs_DoNotURLEncodeBackParams(t *testing.T) {
	srv := newTestServer(t)
	id := seedManga(t, srv, "nav.cbz", [][]byte{
		tinyPNG(t, 8, 8, color.RGBA{1, 1, 1, 255}),
		tinyPNG(t, 8, 8, color.RGBA{2, 2, 2, 255}),
		tinyPNG(t, 8, 8, color.RGBA{3, 3, 3, 255}),
	})
	url := fmt.Sprintf(
		"/images/%d/read?page=2&back_q=foo&back_sort=newest&back_order=desc&back_seed=42&from=pages",
		id,
	)
	req := httptest.NewRequest("GET", url, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reader GET: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, banned := range []string{"%26back_q", "%26back_sort", "%26back_order", "%26back_seed", "%26from", "%3D"} {
		if strings.Contains(body, banned) {
			t.Errorf("reader href encoded a separator (%q) - chevron click would land on page=1", banned)
		}
	}
	// The next-page chevron href must keep `?page=N` followed by an
	// HTML-decoded `&` (rendered as `&amp;`), not `%26`. The browser
	// then parses `back_q` etc as separate query parameters.
	if !strings.Contains(body, `?page=3&amp;`) || !strings.Contains(body, `back_q=foo`) {
		extract := body[max(0, strings.Index(body, "reader-side-next")-50):min(len(body), strings.Index(body, "reader-side-next")+250)]
		t.Errorf("next-chevron href missing `?page=3&amp;…back_q=foo`. extract: %s", extract)
	}
}
