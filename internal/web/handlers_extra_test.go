package web

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
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

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/models"
)

func TestMoveImage_RejectsAbsolutePath(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "mv.png", 10, 10)

	form := url.Values{"_csrf": {srv.csrfToken("anon")}, "folder": {"/etc/passwd"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/move", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for absolute folder path, got %d: %s", w.Code, w.Body.String())
	}
	// Canonical path must still be at the root.
	cx := srv.Active()
	var canonPath string
	_ = cx.DB.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&canonPath)
	if !strings.HasSuffix(canonPath, "mv.png") || strings.Contains(canonPath, "passwd") {
		t.Errorf("image moved to an unexpected path: %s", canonPath)
	}
}

func TestMoveImage_RejectsTraversal(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "mv2.png", 10, 10)

	form := url.Values{"_csrf": {srv.csrfToken("anon")}, "folder": {"../escape"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/move", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for traversal, got %d: %s", w.Code, w.Body.String())
	}
}

func TestMoveImage_HappyPath(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "shot.png", 10, 10)

	form := url.Values{"_csrf": {srv.csrfToken("anon")}, "folder": {"archive/2026"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/move", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for HX move, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("HX-Redirect") == "" {
		t.Error("HX move should set HX-Redirect")
	}
	cx := srv.Active()
	var canonPath, folderPath string
	_ = cx.DB.Read.QueryRow(`SELECT canonical_path, folder_path FROM images WHERE id = ?`, id).Scan(&canonPath, &folderPath)
	if folderPath != "archive/2026" {
		t.Errorf("folder_path = %q, want archive/2026", folderPath)
	}
	want := filepath.Join(cx.GalleryPath, "archive", "2026", "shot.png")
	if canonPath != want {
		t.Errorf("canonical_path = %q, want %q", canonPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("file not at new path: %v", err)
	}
}

func TestImageByHashRedirectsToCurrentID(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "hash.png", 10, 10)
	var sha string
	if err := srv.Active().DB.Read.QueryRow(`SELECT sha256 FROM images WHERE id = ?`, id).Scan(&sha); err != nil {
		t.Fatalf("read sha: %v", err)
	}

	req := httptest.NewRequest("GET", "/i/"+sha, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if got := w.Header().Get("Location"); got != fmt.Sprintf("/images/%d", id) {
		t.Errorf("Location = %q, want /images/%d", got, id)
	}

	req = httptest.NewRequest("GET", "/i/deadbeef", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown hash status = %d, want 404", w.Code)
	}
}

func TestImageByHashSwitchesToImageGallery(t *testing.T) {
	srv := newMultiGalleryServer(t)

	// Seed an image into the non-active "stock" gallery, then return to "default".
	if err := srv.SwitchGallery("stock"); err != nil {
		t.Fatalf("switch to stock: %v", err)
	}
	id := seedImage(t, srv, "elsewhere.png", 10, 10)
	var sha string
	if err := srv.Active().DB.Read.QueryRow(`SELECT sha256 FROM images WHERE id = ?`, id).Scan(&sha); err != nil {
		t.Fatalf("read sha: %v", err)
	}
	if err := srv.SwitchGallery("default"); err != nil {
		t.Fatalf("switch to default: %v", err)
	}

	req := httptest.NewRequest("GET", "/i/"+sha, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != fmt.Sprintf("/images/%d", id) {
		t.Errorf("Location = %q, want /images/%d", got, id)
	}
	if srv.activeName != "stock" {
		t.Errorf("activeName = %q, want stock (view should switch to the image's gallery)", srv.activeName)
	}
}

// The move flash echoes the operator-supplied folder name, and the client
// renders it via innerHTML, so the trigger text must arrive HTML-escaped.
func TestMoveImage_FlashEscapesFolderName(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "esc.png", 10, 10)

	form := url.Values{"_csrf": {srv.csrfToken("anon")}, "folder": {"<b>x</b>"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/move", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var trig struct {
		Flash struct {
			Text string `json:"text"`
		} `json:"monbooru:flash"`
	}
	if err := json.Unmarshal([]byte(w.Header().Get("HX-Trigger")), &trig); err != nil {
		t.Fatalf("HX-Trigger not valid JSON: %v (%q)", err, w.Header().Get("HX-Trigger"))
	}
	if strings.Contains(trig.Flash.Text, "<b>") {
		t.Errorf("flash text carries raw markup: %q", trig.Flash.Text)
	}
	if !strings.Contains(trig.Flash.Text, "&lt;b&gt;") {
		t.Errorf("flash text not HTML-escaped: %q", trig.Flash.Text)
	}
}

// Folders / source autocomplete share the case-insensitivity of the
// matching `folder:` / `source:` search filters. The prefix-range
// seek must agree so a user typing a lowercase prefix surfaces a
// capitalised folder name.
func TestFoldersSuggest_CaseInsensitivePrefix(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	sub := filepath.Join(cx.GalleryPath, "Characters")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, "f.png")
	img := image.NewRGBA(image.Rect(0, 0, 9, 9))
	f, _ := os.Create(p)
	_ = png.Encode(f, img)
	_ = f.Close()
	if _, _, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, p, "png", ""); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"ch", "Ch", "CHAR"} {
		req := httptest.NewRequest("GET", "/internal/folders/suggest?prefix="+prefix, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("folders suggest prefix=%s: %d", prefix, w.Code)
		}
		if !strings.Contains(w.Body.String(), "Characters") {
			t.Errorf("prefix=%s should surface Characters folder, got %s", prefix, w.Body.String())
		}
	}
}

func TestSourceSuggest_CaseInsensitivePrefix(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "src.png", 7, 7)
	if _, err := srv.Active().DB.Write.Exec(
		`UPDATE images SET source = 'Pixiv' WHERE id = ?`, id,
	); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"pix", "Pix", "PIXIV"} {
		req := httptest.NewRequest("GET", "/internal/source/suggest?prefix="+prefix, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("source suggest prefix=%s: %d", prefix, w.Code)
		}
		if !strings.Contains(w.Body.String(), `data-series="Pixiv"`) {
			t.Errorf("prefix=%s should surface Pixiv source, got %s", prefix, w.Body.String())
		}
	}
}

func TestFoldersSuggest_PrefixFilter(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	// Seed three folder paths via direct file drops + ingest. Each image
	// needs a distinct SHA-256, so use the loop index to vary one pixel.
	for i, folder := range []string{"2024/jan", "2024/feb", "2025/mar"} {
		sub := filepath.Join(cx.GalleryPath, folder)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		name := strings.ReplaceAll(folder, "/", "_") + ".png"
		p := filepath.Join(sub, name)
		img := image.NewRGBA(image.Rect(0, 0, 12+i*3, 12+i))
		for px := 0; px < len(img.Pix); px += 4 {
			img.Pix[px] = byte(i * 50)
		}
		f, _ := os.Create(p)
		_ = png.Encode(f, img)
		_ = f.Close()
		if _, _, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, p, "png", ""); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest("GET", "/internal/folders/suggest?prefix=2024", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "2024/jan") || !strings.Contains(body, "2024/feb") {
		t.Errorf("expected both 2024 folders in response, got %s", body)
	}
	if strings.Contains(body, "2025/mar") {
		t.Errorf("2025/mar must not match prefix=2024, got %s", body)
	}
}

// Sidebar folder/series links carry the q value through html/template's
// href-context URL autoescaper, which re-percent-encodes anything it
// doesn't recognise as a trusted URL. The links must therefore render
// the percent-escapes exactly once so a click actually submits the
// quoted filter, not a double-encoded literal that matches nothing.
func TestSidebarFolderLink_SinglePercentEncoded(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	sub := filepath.Join(cx.GalleryPath, "api-test")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, "f.png")
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	f, _ := os.Create(p)
	_ = png.Encode(f, img)
	_ = f.Close()
	if _, _, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, p, "png", ""); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/internal/sidebar-browse", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	body := w.Body.String()

	// Single-encoded form: folder:"api-test" → folder%3A%22api-test%22.
	if !strings.Contains(body, `href="/?q=folder%3A%22api-test%22"`) {
		t.Errorf("folder href not single-encoded; want /?q=folder%%3A%%22api-test%%22\nbody: %s", body)
	}
	// Double-encoded (% → %25) means html/template re-escaped the value.
	if strings.Contains(body, "folder%253A") || strings.Contains(body, "%2522") {
		t.Errorf("folder href is double-encoded\nbody: %s", body)
	}

	// Follow the link the way a browser would: the q parameter decodes
	// once to folder:"api-test", which the gallery search must match.
	req2 := httptest.NewRequest("GET", `/?q=folder%3A%22api-test%22`, nil)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("gallery returned %d", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), `<span id="result-count" class="result-count">1</span>`) {
		t.Errorf("folder:\"api-test\" should return 1 match, body excerpt:\n%s", resultCountSlice(t, w2.Body.String()))
	}
}

// Collection link on the detail page mirrors the sidebar's folder
// link path: emitted via urlQ inside an href= attribute. The same
// single-encoded contract applies so a click submits the quoted
// collection filter intact.
func TestDetailCollectionLink_SinglePercentEncoded(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "s.png", 9, 9)
	if err := gallery.SetHomeCollection(srv.db(), id, "touhou series", nil); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", fmt.Sprintf("/images/%d", id), nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	// `+` in an HTML attribute may render as `&#43;` after html/template
	// HTML-escapes the value; the browser still decodes it to `+` before
	// submitting the URL. Accept either form.
	want := []string{
		`href="/?q=collection%3A%22touhou+series%22"`,
		`href="/?q=collection%3A%22touhou&#43;series%22"`,
	}
	ok := false
	for _, w := range want {
		if strings.Contains(body, w) {
			ok = true
			break
		}
	}
	if !ok {
		t.Errorf("collection href not single-encoded; want one of %v\nbody slice: %s", want, seriesLinkSlice(t, body))
	}
	if strings.Contains(body, "collection%253A") {
		t.Errorf("collection href is double-encoded\nbody slice: %s", seriesLinkSlice(t, body))
	}

	req2 := httptest.NewRequest("GET", `/?q=collection%3A%22touhou+series%22`, nil)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if !strings.Contains(w2.Body.String(), `<span id="result-count" class="result-count">1</span>`) {
		t.Errorf("collection:\"touhou series\" should return 1 match\n%s", resultCountSlice(t, w2.Body.String()))
	}
}

func resultCountSlice(t *testing.T, body string) string {
	t.Helper()
	idx := strings.Index(body, `id="gallery-status"`)
	if idx < 0 {
		return body[:min(len(body), 200)]
	}
	end := strings.Index(body[idx:], "</div>")
	if end < 0 {
		return body[idx:min(len(body), idx+200)]
	}
	return body[idx : idx+end+6]
}

func seriesLinkSlice(t *testing.T, body string) string {
	t.Helper()
	idx := strings.Index(body, `Series</th>`)
	if idx < 0 {
		return body[:min(len(body), 200)]
	}
	end := idx + 400
	if end > len(body) {
		end = len(body)
	}
	return body[idx:end]
}

// system: cheat-sheet branch: typing the bare prefix should surface every
// real filter keyword and tag the rows with the dim "system" category
// label. The trailing colon on each row is what the JS keys off to keep
// the cursor parked for the value the user is about to type.
func TestSearchSuggest_System_TopLevel(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=system:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-tag-name="fav:"`,
		`data-tag-name="source:"`,
		`data-tag-name="date:"`,
		`data-tag-name="rating:"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("system: top-level dropdown missing %q\nbody: %s", want, body)
		}
	}
	// The count column belongs to tag rows only; system rows must not carry it.
	if strings.Contains(body, `class="suggest-count"`) {
		t.Errorf("system: rows must not render .suggest-count, got: %s", body)
	}
}

func TestSearchSuggest_System_TopLevelPrefixFilter(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=system:fa", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-tag-name="fav:"`) {
		t.Errorf("expected fav: row when prefix=fa, body: %s", body)
	}
	if strings.Contains(body, `data-tag-name="date:"`) {
		t.Errorf("date: must not match prefix=fa, body: %s", body)
	}
}

func TestSearchSuggest_System_Level2_Operators(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=system:date:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-tag-name="date:&gt;"`,
		`data-tag-name="date:&lt;"`,
		`data-tag-name="date:..`,
		`data-tag-name="date:&gt;="`,
		`data-tag-name="date:&lt;="`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("date: level-2 dropdown missing %q\nbody: %s", want, body)
		}
	}
}

func TestSearchSuggest_System_Level2_Values(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=system:ai:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-tag-name="ai:a1111"`,
		`data-tag-name="ai:comfyui"`,
		`data-tag-name="ai:none"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ai: level-2 dropdown missing %q\nbody: %s", want, body)
		}
	}
}

// cat: is data-driven from tag_categories; the cheat-sheet must list both
// builtin (e.g. character) and custom rows that the operator created.
func TestSearchSuggest_System_Level2_Categories(t *testing.T) {
	srv := newTestServer(t)
	if _, err := srv.tagSvc().CreateCategory("custom_pal", "#aabbcc"); err != nil {
		t.Fatalf("seed custom category: %v", err)
	}
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=system:cat:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-tag-name="cat:character"`,
		`data-tag-name="cat:custom_pal"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("cat: level-2 dropdown missing %q\nbody: %s", want, body)
		}
	}
}

// The description span carries the cheat-sheet label; rows without a
// description (rating values, fav:true, etc.) omit the span entirely.
func TestSearchSuggest_System_DescriptionPresence(t *testing.T) {
	srv := newTestServer(t)

	// System row with a description: span must be present.
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=system:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	withDesc := w.Body.String()
	if !strings.Contains(withDesc, `<span class="suggest-description">favorite images</span>`) {
		t.Errorf("expected description span on system row, got: %s", withDesc)
	}

	// Level-2 row without a description (rating values are bare): no
	// description span on those rows.
	req2 := httptest.NewRequest("GET", "/internal/search/suggest?q=system:rating:", nil)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	bare := w2.Body.String()
	if strings.Contains(bare, `class="suggest-description"`) {
		t.Errorf("rows without a description must not render the description span, got: %s", bare)
	}
}

// Cheat-sheet rows carry a short English label right of the name so the
// dropdown reads as a discoverable reference.
func TestSearchSuggest_System_TopLevel_DescriptionColumn(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=system:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`<span class="suggest-description">favorite images</span>`,
		`<span class="suggest-description">image width</span>`,
		`<span class="suggest-description">ingestion date</span>`,
		// Category rows wear a generic label.
		`<span class="suggest-description">tag category</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("level-1 dropdown missing description %q\nbody: %s", want, body)
		}
	}
}

func TestSearchSuggest_System_Level2_OperatorDescriptions(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=system:date:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{
		`<span class="suggest-description">after</span>`,
		`<span class="suggest-description">before</span>`,
		`<span class="suggest-description">range</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("date: level-2 dropdown missing description %q\nbody: %s", want, body)
		}
	}
}

func TestSearchSuggest_System_Level2_AIDescriptions(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=system:ai:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{
		`<span class="suggest-description">A1111 / Forge</span>`,
		`<span class="suggest-description">ComfyUI</span>`,
		`<span class="suggest-description">no metadata</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ai: level-2 dropdown missing description %q\nbody: %s", want, body)
		}
	}
}

// Real categories should surface in the level-1 cheat-sheet alongside
// filter keywords so the user can discover the `<category>:<tag>` form.
func TestSearchSuggest_System_TopLevel_IncludesCategories(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=system:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-tag-name="character:"`,
		`data-tag-name="artist:"`,
		`data-tag-name="copyright:"`,
		`data-tag-name="general:"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("level-1 dropdown missing category %q\nbody: %s", want, body)
		}
	}
	// `rating` is both a filter keyword and a builtin category. Only the
	// filter-keyword row should appear; the category copy is folded in.
	if got := strings.Count(body, `data-tag-name="rating:"`); got != 1 {
		t.Errorf("expected exactly one rating: row, got %d\nbody: %s", got, body)
	}
}

// Bare filter key (no `system:` prefix) is the natural state after
// picking a row from the cheat-sheet. The dropdown must surface the
// level-2 hint there too, otherwise the user has to type a throwaway
// character to wake autocomplete back up.
func TestSearchSuggest_BareFilterKey_SurfacesValueHint(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=fav:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-tag-name="fav:true"`,
		`data-tag-name="fav:false"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bare fav: dropdown missing %q\nbody: %s", want, body)
		}
	}
}

// inbox: should land in the level-1 cheat-sheet (system:) the same way
// fav: does, since both are boolean filter keywords driven off a column
// on images.
func TestSearchSuggest_System_TopLevel_Inbox(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=system:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-tag-name="inbox:"`) {
		t.Errorf("system: top-level dropdown missing inbox: row\nbody: %s", body)
	}
}

// Bare inbox: must surface the level-2 true/false expansion, mirroring
// the fav: bare-key behaviour.
func TestSearchSuggest_BareInboxKey_SurfacesValueHint(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=inbox:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-tag-name="inbox:true"`,
		`data-tag-name="inbox:false"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bare inbox: dropdown missing %q\nbody: %s", want, body)
		}
	}
}

func TestSearchSuggest_BareDateKey_SurfacesOperators(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=date:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-tag-name="date:&gt;"`,
		`data-tag-name="date:&lt;"`,
		`data-tag-name="date:..`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bare date: dropdown missing %q\nbody: %s", want, body)
		}
	}
}

func TestSearchSuggest_BareCatKey_SurfacesCategories(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=cat:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-tag-name="cat:character"`) {
		t.Errorf("bare cat: dropdown missing cat:character\nbody: %s", body)
	}
}

// system:<category>: drills into tags in that category, mirroring what
// the existing `<category>:<prefix>` autocomplete returns. These rows
// are real tag data, not static cheat-sheet hints, so they wear a
// usage count instead of the dim "system" label.
func TestSearchSuggest_System_Level2_RealCategoryDrillIn(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv.db())
	cats, err := srv.tagSvc().ListCategories()
	if err != nil {
		t.Fatalf("list categories: %v", err)
	}
	var charCatID int64
	for _, c := range cats {
		if c.Name == "character" {
			charCatID = c.ID
			break
		}
	}
	if charCatID == 0 {
		t.Fatal("character category missing from seed")
	}
	tag, err := srv.tagSvc().GetOrCreateTag("bocchi_the_rock", charCatID)
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := srv.tagSvc().AddTagToImage(id, tag.ID, false, nil); err != nil {
		t.Fatalf("attach tag: %v", err)
	}

	req := httptest.NewRequest("GET", "/internal/search/suggest?q=system:character:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-tag-name="character:bocchi_the_rock"`) {
		t.Errorf("expected character:bocchi_the_rock in level-2 dropdown, got: %s", body)
	}
	// Real-data rows render a usage count column, not the dim system label.
	if !strings.Contains(body, `class="suggest-count"`) {
		t.Errorf("expected suggest-count column on real category drill-in\nbody: %s", body)
	}
}

// TestSavedSearch_RoundtripsSortAndSeed: a save from the random-sort
// gallery must persist sort+order+seed and the sidebar entry must
// reopen at the same view.
func TestSavedSearch_RoundtripsSortAndSeed(t *testing.T) {
	srv := newTestServer(t)
	csrf := srv.csrfToken("anon")

	form := url.Values{
		"_csrf": {csrf}, "name": {"rand_42"}, "query": {"bulk"},
		"sort": {"random"}, "order": {"asc"}, "seed": {"42"},
	}
	req := httptest.NewRequest("POST", "/search/saved", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create saved search expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var got struct {
		Sort, Order, Seed string
	}
	if err := srv.db().Read.QueryRow(
		`SELECT sort, sort_order, seed FROM saved_searches WHERE name='rand_42'`,
	).Scan(&got.Sort, &got.Order, &got.Seed); err != nil {
		t.Fatalf("saved search columns missing: %v", err)
	}
	if got.Sort != "random" || got.Order != "asc" || got.Seed != "42" {
		t.Errorf("sort/order/seed = %+v, want {random asc 42}", got)
	}

	ss := models.SavedSearch{Query: "bulk", Sort: "random", Order: "asc", Seed: "42"}
	want := "/?q=bulk&sort=random&order=asc&seed=42"
	if got := ss.HRef(); got != want {
		t.Errorf("HRef = %q, want %q", got, want)
	}
}

func TestSavedSearch_CreateAndDelete(t *testing.T) {
	srv := newTestServer(t)
	csrf := srv.csrfToken("anon")

	// Create (HTMX form → 200 with flash-ok body).
	form := url.Values{"_csrf": {csrf}, "name": {"my_cats"}, "query": {"cat"}}
	req := httptest.NewRequest("POST", "/search/saved", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create saved search expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var id int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM saved_searches WHERE name='my_cats'`).Scan(&id); err != nil {
		t.Fatalf("saved search not persisted: %v", err)
	}

	// Delete.
	delReq := httptest.NewRequest("DELETE", fmt.Sprintf("/search/saved/%d", id), nil)
	delReq.Header.Set("X-CSRF-Token", csrf)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, delReq)
	if w2.Code != http.StatusOK {
		t.Errorf("delete saved search expected 200, got %d", w2.Code)
	}
	var count int
	_ = srv.db().Read.QueryRow(`SELECT COUNT(*) FROM saved_searches`).Scan(&count)
	if count != 0 {
		t.Errorf("saved_searches should be empty after delete, got %d", count)
	}
}

func TestCreateCategory_Post(t *testing.T) {
	srv := newTestServer(t)
	csrf := srv.csrfToken("anon")

	form := url.Values{"_csrf": {csrf}, "name": {"mood"}, "color": {"#abcdef"}}
	req := httptest.NewRequest("POST", "/tags/categories", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	// HTMX path returns 204 + HX-Redirect to /categories.
	if w.Code != http.StatusNoContent {
		t.Fatalf("create category (HTMX) expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("HX-Redirect"); loc != "/categories" {
		t.Errorf("HX-Redirect = %q, want /categories", loc)
	}
	var id int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM tag_categories WHERE name='mood'`).Scan(&id); err != nil {
		t.Fatalf("category not persisted: %v", err)
	}
}

func TestJobDismissPost_ClearsDoneSummary(t *testing.T) {
	srv := newTestServer(t)
	// Stage a completed job.
	if err := srv.jobs.Start("sync"); err != nil {
		t.Fatal(err)
	}
	srv.jobs.Complete("done")

	csrf := srv.csrfToken("anon")
	req := httptest.NewRequest("POST", "/internal/job/dismiss", strings.NewReader("_csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("dismiss expected 204, got %d", w.Code)
	}
	if state := srv.jobs.Get(); state != nil {
		t.Errorf("state should be nil after dismiss, got %+v", state)
	}
}

func TestJobCancelPost_CancelsRunning(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.jobs.Start("delete"); err != nil {
		t.Fatal(err)
	}
	ctx := srv.jobs.Context()
	csrf := srv.csrfToken("anon")
	req := httptest.NewRequest("POST", "/internal/job/cancel", strings.NewReader("_csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("cancel expected 204, got %d", w.Code)
	}
	if ctx.Err() == nil {
		t.Error("Cancel should have fired the job's context")
	}
	srv.jobs.Complete("cancelled test")
}

func TestReExtract_ReplacesExistingMetadata(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()

	// Stage an image and fabricate an SD metadata row with a stale prompt.
	id := seedImage(t, srv, "reext.png", 10, 10)
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO sd_metadata (image_id, prompt, raw_params, generation_hash)
		 VALUES (?, 'stale_prompt', 'stale_params', 'stale_hash12')`, id,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := cx.DB.Write.Exec(`UPDATE images SET source_type = 'a1111' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	// Fire the re-extract handler.
	csrf := srv.csrfToken("anon")
	req := httptest.NewRequest("POST", "/settings/maintenance/re-extract-metadata", strings.NewReader("_csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("re-extract expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Wait for the background job to drain. Poll the manager state with a
	// real time budget so the test isn't racing the goroutine.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !srv.jobs.IsRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if srv.jobs.IsRunning() {
		t.Fatal("re-extract job never drained")
	}
	// The plain PNG has no real SD metadata so re-extraction should drop the
	// stale row and flip source_type back to "none".
	var count int
	_ = cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM sd_metadata WHERE image_id = ?`, id).Scan(&count)
	if count != 0 {
		t.Errorf("re-extract should have cleared the stale sd_metadata row for a plain PNG, count = %d", count)
	}
	var sourceType string
	_ = cx.DB.Read.QueryRow(`SELECT source_type FROM images WHERE id = ?`, id).Scan(&sourceType)
	if sourceType != "none" {
		t.Errorf("source_type after re-extract = %q, want 'none'", sourceType)
	}
}

func TestGallerySwitch_RejectedWhileJobRunning(t *testing.T) {
	srv := newMultiGalleryServer(t)
	// Hold the job manager lock.
	if err := srv.jobs.Start("sync"); err != nil {
		t.Fatal(err)
	}
	defer srv.jobs.Complete("test")

	csrf := srv.csrfToken("anon")
	form := url.Values{"_csrf": {csrf}, "name": {"stock"}}
	req := httptest.NewRequest("POST", "/internal/gallery/switch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// Handler returns 400 with the "a job is running" message.
	if w.Code != http.StatusBadRequest {
		t.Errorf("switch while job running expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "job is running") {
		t.Errorf("expected 'job is running' in body, got: %s", w.Body.String())
	}
	if srv.activeName != "default" {
		t.Errorf("activeName should not have changed, got %q", srv.activeName)
	}
}

func TestGallerySwitch_UnknownGalleryRejected(t *testing.T) {
	srv := newMultiGalleryServer(t)
	csrf := srv.csrfToken("anon")
	form := url.Values{"_csrf": {csrf}, "name": {"does-not-exist"}}
	req := httptest.NewRequest("POST", "/internal/gallery/switch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code < 400 || w.Code >= 500 {
		t.Errorf("unknown gallery switch expected 4xx, got %d: %s", w.Code, w.Body.String())
	}
}

// imageTagCategory returns the category name of a tag attached to id whose
// name equals want, or "" if no such row exists. Used by the colon tests to
// confirm the parser kept the literal name instead of splitting it.
func imageTagCategory(t *testing.T, srv *Server, id int64, want string) string {
	t.Helper()
	cx := srv.Active()
	var cat string
	err := cx.DB.Read.QueryRow(
		`SELECT tc.name FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE it.image_id = ? AND t.name = ?`, id, want).Scan(&cat)
	if err != nil {
		return ""
	}
	return cat
}

// TestAddTagToImage_PartialDup_SurfacesDupList covers the mixed-submit
// branch: some tokens are new, some already on the image. The user
// should see both the success line and the "already on image" note,
// otherwise they're left diffing the under-image list against their
// pasted input to find which tokens were no-ops.
func TestAddTagToImage_PartialDup_SurfacesDupList(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "partial_dup.png", 10, 10)

	csrf := srv.csrfToken("anon")
	post := func(tag string) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{"_csrf": {csrf}, "tag": {tag}}
		req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/tags", id), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}

	// Seed: the image already carries `existing_one`.
	if w := post("existing_one"); w.Code != http.StatusOK {
		t.Fatalf("seed add: expected 200, got %d", w.Code)
	}

	// Mixed submit: one new + one duplicate.
	w := post("brand_new existing_one")
	if w.Code != http.StatusOK {
		t.Fatalf("mixed add: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "already on image: existing_one") {
		t.Errorf("response missing 'already on image' note for the dup token; body=%s", body)
	}
}

// TestAddTagToImage_PartialReject_ShowsWarnFlash pins the mixed-outcome
// flash routing: when some tokens land and some are rejected (e.g.
// malformed input alongside good tags), the response must surface
// BOTH lists in one orange .flash-warn flash, not just the rejects in
// red. The user reads the flash to know which of their tokens went in
// and which to retry.
func TestAddTagToImage_PartialReject_ShowsWarnFlash(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "partial_reject.png", 10, 10)

	csrf := srv.csrfToken("anon")
	// `general:` is a known category with an empty tag-name token, which
	// parseTagInput rejects via the "empty tag name after category
	// prefix" path. `brand_new` is a clean new tag.
	form := url.Values{"_csrf": {csrf}, "tag": {"general: brand_new"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/tags", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="flash flash-warn"`) {
		t.Errorf("mixed outcome should render a flash-warn; body=%s", body)
	}
	if !strings.Contains(body, "added: brand_new") {
		t.Errorf("warn flash should list the added token; body=%s", body)
	}
	if !strings.Contains(body, "rejected:") {
		t.Errorf("warn flash should list the rejected token; body=%s", body)
	}
	if strings.Contains(body, `class="flash flash-err"`) {
		t.Errorf("mixed outcome should NOT render an err flash; body=%s", body)
	}
	if strings.Contains(body, `class="flash flash-ok"`) {
		t.Errorf("mixed outcome should NOT render an ok flash; body=%s", body)
	}
}

func TestAddTagToImage_ColonFallbackLiteral(t *testing.T) {
	// `nier` is not a category, so the token must fall through to a literal
	// tag-name insert and land whole in general - not be rejected as an
	// unknown category.
	srv := newTestServer(t)
	id := seedImage(t, srv, "colon_literal.png", 10, 10)

	csrf := srv.csrfToken("anon")
	form := url.Values{"_csrf": {csrf}, "tag": {"nier:automata"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/tags", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if cat := imageTagCategory(t, srv, id, "nier:automata"); cat != "general" {
		t.Errorf("nier:automata category = %q, want general", cat)
	}
}

// TestGalleryHandler_RandomSeedFitsInt32 pins the auto-generated random
// seed range. SQLite's `(i.id * seed) & 2147483647` ordering coerces to
// REAL when the product overflows int64, and the low bits then track id
// monotonically; reproducible with 19-digit auto-seeds. A 32-bit seed
// keeps the product in int64 for any plausible image id.
func TestGalleryHandler_RandomSeedFitsInt32(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest("GET", "/?sort=random", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect for sort=random with no seed, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location %q: %v", loc, err)
	}
	seedStr := parsed.Query().Get("seed")
	if seedStr == "" {
		t.Fatalf("Location missing seed: %q", loc)
	}
	seed, err := strconv.ParseInt(seedStr, 10, 64)
	if err != nil {
		t.Fatalf("parse seed %q: %v", seedStr, err)
	}
	if seed <= 0 || seed > (1<<32)-1 {
		t.Errorf("seed %d outside [1, 2^32-1]; multiplication can overflow int64", seed)
	}
	if seed&1 == 0 {
		t.Errorf("seed %d is even; (id*seed) mod 2^31 is not a permutation", seed)
	}
}

// `?page=0` and `?page=-1` must redirect to `?page=1` so the URL
// agrees with the rendered pager, the same way past-end values
// redirect to the actual last page.
func TestGalleryHandler_NonPositivePageRedirects(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, "p0.png", 10, 10)

	for _, raw := range []string{"0", "-1"} {
		req := httptest.NewRequest("GET", "/?page="+raw, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("page=%s expected 303 redirect, got %d", raw, w.Code)
		}
		loc := w.Header().Get("Location")
		parsed, err := url.Parse(loc)
		if err != nil {
			t.Fatalf("parse Location %q: %v", loc, err)
		}
		if got := parsed.Query().Get("page"); got != "1" {
			t.Errorf("page=%s redirected to page=%q, want 1", raw, got)
		}
	}
}

// TestCreateSavedSearch_RejectsDuplicateName pins the no-overwrite
// promise: a second save under an existing name surfaces an error
// instead of silently clobbering the previous saved search's query.
func TestCreateSavedSearch_RejectsDuplicateName(t *testing.T) {
	srv := newTestServer(t)
	csrf := srv.csrfToken("anon")

	post := func(name, query string) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{"_csrf": {csrf}, "name": {name}, "query": {query}}
		req := httptest.NewRequest("POST", "/search/saved", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}

	if w := post("girls", "1girl"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "flash-ok") {
		t.Fatalf("first save: code=%d body=%q", w.Code, w.Body.String())
	}

	w := post("girls", "blue_eyes")
	if !strings.Contains(w.Body.String(), "flash-err") {
		t.Errorf("duplicate save should surface flash-err, got %q", w.Body.String())
	}

	cx := srv.Active()
	var stored string
	if err := cx.DB.Read.QueryRow(`SELECT query FROM saved_searches WHERE name = 'girls'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "1girl" {
		t.Errorf("original query should survive duplicate-name attempt, got %q", stored)
	}
}

// TestRenameTag_HTMXCollisionSurfacesError pins the rename dialog's
// "name already exists" round trip: an HTMX rename to a colliding name
// returns 200 with a flash payload htmx swaps into #rename-error,
// instead of a bare 400 the dialog can't see.
func TestRenameTag_HTMXCollisionSurfacesError(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	var generalID int64
	_ = cx.DB.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&generalID)
	first, _ := cx.TagSvc.GetOrCreateTag("first", generalID)
	if _, err := cx.TagSvc.GetOrCreateTag("second", generalID); err != nil {
		t.Fatal(err)
	}

	csrf := srv.csrfToken("anon")
	form := url.Values{"_csrf": {csrf}, "name": {"second"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/tags/%d/rename", first.ID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (so htmx swaps the flash), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "flash-err") {
		t.Errorf("response missing flash-err: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "second") {
		t.Errorf("flash should name the colliding name: %s", w.Body.String())
	}
	var stillName string
	_ = cx.DB.Read.QueryRow(`SELECT name FROM tags WHERE id = ?`, first.ID).Scan(&stillName)
	if stillName != "first" {
		t.Errorf("collision should leave tag untouched, got name %q", stillName)
	}
}

// TestMergeTags_HTMXSuccessRedirectsToAlias pins the post-merge
// destination: a successful alias / repoint lands the user on
// /tags?origin=alias&q=<src> so the freshly-aliased row is in scope,
// mirroring the create-alias dialog's redirect.
func TestMergeTags_HTMXSuccessRedirectsToAlias(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	var generalID int64
	_ = cx.DB.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&generalID)
	src, _ := cx.TagSvc.GetOrCreateTag("alias_src", generalID)
	dst, _ := cx.TagSvc.GetOrCreateTag("alias_dst", generalID)

	csrf := srv.csrfToken("anon")
	form := url.Values{
		"_csrf":        {csrf},
		"alias_id":     {fmt.Sprintf("%d", src.ID)},
		"canonical_id": {fmt.Sprintf("%d", dst.ID)},
	}
	req := httptest.NewRequest("POST", "/tags/merge", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204 on HTMX merge, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("HX-Redirect"); got != "/tags?origin=alias&q=alias_src" {
		t.Errorf("HX-Redirect = %q, want /tags?origin=alias&q=alias_src", got)
	}
	if w.Header().Get("HX-Refresh") != "" {
		t.Errorf("HX-Refresh = %q, want empty (HX-Redirect handles nav)", w.Header().Get("HX-Refresh"))
	}
}

// TestRenameTag_HTMXSuccessRefreshes pins the success branch: the
// handler emits HX-Refresh so the client reloads the current URL,
// preserving the user's active /tags filter (q, sort, origin, page)
// instead of dropping them by redirecting to /tags.
func TestRenameTag_HTMXSuccessRefreshes(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	var generalID int64
	_ = cx.DB.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&generalID)
	tag, _ := cx.TagSvc.GetOrCreateTag("renameme", generalID)

	csrf := srv.csrfToken("anon")
	form := url.Values{"_csrf": {csrf}, "name": {"renamed"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/tags/%d/rename", tag.ID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204 on HTMX rename, got %d", w.Code)
	}
	if w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("HX-Refresh = %q, want true", w.Header().Get("HX-Refresh"))
	}
	if got := w.Header().Get("HX-Redirect"); got != "" {
		t.Errorf("HX-Redirect = %q, want empty (HX-Refresh handles reload)", got)
	}
}

func TestAddTagToImage_RealCategoryPrefixStillSplits(t *testing.T) {
	// `artist` IS a built-in category, so `artist:john_doe` must still
	// be split into an artist-category `john_doe` tag - otherwise the
	// detail page loses category-qualified input entirely.
	srv := newTestServer(t)
	id := seedImage(t, srv, "colon_artist.png", 10, 10)

	csrf := srv.csrfToken("anon")
	form := url.Values{"_csrf": {csrf}, "tag": {"artist:john_doe"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/tags", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if cat := imageTagCategory(t, srv, id, "john_doe"); cat != "artist" {
		t.Errorf("john_doe category = %q, want artist", cat)
	}
	if cat := imageTagCategory(t, srv, id, "artist:john_doe"); cat != "" {
		t.Errorf("literal artist:john_doe must not be stored, got category %q", cat)
	}
}

// TestUpdateExternal_HTMXBadURLSurfacesFlash pins the dialog UX: a
// non-http URL submitted from the detail-page #external-url-dialog
// must come back as a 200 + flash-err fragment so htmx swaps the
// message into the slot and the dialog stays open with the typed
// value intact, instead of navigating the browser to a stripped
// text/plain error page.
func TestUpdateExternal_HTMXBadURLSurfacesFlash(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ext.png", 10, 10)

	csrf := srv.csrfToken("anon")
	form := url.Values{"_csrf": {csrf}, "url": {"ftp://example.com"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/external", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (htmx swap target), got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html so htmx swaps as a fragment", got)
	}
	if !strings.Contains(w.Body.String(), `class="flash flash-err"`) {
		t.Errorf("body does not carry flash-err fragment: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "url must start with") {
		t.Errorf("body missing the validation message: %s", w.Body.String())
	}
}

// TestUpdateExternal_HTMXSuccessRefreshes pins the success branch:
// a valid URL save under HX-Request emits HX-Refresh so the detail
// page reloads with the new value rendered.
func TestUpdateExternal_HTMXSuccessRefreshes(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ext_ok.png", 10, 10)

	csrf := srv.csrfToken("anon")
	form := url.Values{"_csrf": {csrf}, "url": {"https://example.com/art"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/external", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("HX-Refresh = %q, want true", w.Header().Get("HX-Refresh"))
	}
	var stored string
	_ = srv.Active().DB.Read.QueryRow(`SELECT url FROM images WHERE id = ?`, id).Scan(&stored)
	if stored != "https://example.com/art" {
		t.Errorf("images.url = %q, want https://example.com/art", stored)
	}
}

// TestVacuumDBPost_RunsToCompletion: the maintenance handler kicks
// off a vacuum job in a goroutine, returns the "started" flash
// immediately, and the job worker runs VACUUM + wal_checkpoint to
// completion. Pinning the handler shape would catch a future
// regression that drops the goroutine wrap or inverts the WAL
// checkpoint order.
func TestVacuumDBPost_RunsToCompletion(t *testing.T) {
	srv := newTestServer(t)
	csrf := srv.csrfToken("anon")

	// Seed a few images so VACUUM has actual pages to rewrite.
	for i := 0; i < 4; i++ {
		seedImage(t, srv, fmt.Sprintf("vac%d.png", i), 6+i, 6+i)
	}

	form := url.Values{"_csrf": {csrf}}
	req := httptest.NewRequest("POST", "/settings/maintenance/vacuum-db", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("vacuum handler expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Vacuum started") {
		t.Errorf("body missing 'Vacuum started' flash: %q", w.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !srv.jobs.IsRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if srv.jobs.IsRunning() {
		t.Fatal("vacuum job never drained")
	}
	state := srv.jobs.Get()
	if state == nil {
		t.Fatal("expected job state after vacuum drain")
	}
	if !strings.Contains(state.Summary, "Vacuumed") {
		t.Errorf("job summary = %q, want 'Vacuumed (...)'", state.Summary)
	}
}

// TestDeleteSearch_BulkDeleteReconcilesUsage: the bulk-delete
// background path must drop image rows, cascade image_tags, reconcile
// tags.usage_count to the post-delete reality, and clear thumbnail
// files. The Playwright cancel test owns the cancellation branch;
// this test pins the happy-path commit invariants without
// the 3000-image fixture cost.
func TestDeleteSearch_BulkDeleteReconcilesUsage(t *testing.T) {
	srv := newTestServer(t)
	csrf := srv.csrfToken("anon")

	// Seed several images, all sharing one tag plus one image-private
	// tag so the recalc has both the "shared survives" and the "tag
	// drops to zero" cases on the same run.
	const n = 6
	var ids []int64
	for i := 0; i < n; i++ {
		ids = append(ids, seedImage(t, srv, fmt.Sprintf("bd%d.png", i), 4+i, 4+i))
	}
	var general int64
	_ = srv.db().Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&general)
	res, err := srv.db().Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 0)`, "shared", general,
	)
	if err != nil {
		t.Fatal(err)
	}
	sharedID, _ := res.LastInsertId()
	for _, id := range ids {
		if err := srv.tagSvc().AddTagToImage(id, sharedID, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Bump usage_count of an unrelated tag so the recalc must leave
	// untouched tags untouched.
	res, err = srv.db().Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 99)`, "untouched", general,
	)
	if err != nil {
		t.Fatal(err)
	}
	untouchedID, _ := res.LastInsertId()

	form := url.Values{"_csrf": {csrf}, "q": {""}}
	req := httptest.NewRequest("POST", "/internal/delete-search", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("delete-search expected 202, got %d: %s", w.Code, w.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !srv.jobs.IsRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if srv.jobs.IsRunning() {
		t.Fatal("delete job never drained")
	}

	var imgCount int
	_ = srv.db().Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&imgCount)
	if imgCount != 0 {
		t.Errorf("images count after bulk delete = %d, want 0", imgCount)
	}
	var sharedUsage, untouchedUsage int
	_ = srv.db().Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, sharedID).Scan(&sharedUsage)
	_ = srv.db().Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, untouchedID).Scan(&untouchedUsage)
	if sharedUsage != 0 {
		t.Errorf("shared tag usage_count = %d after wiping every carrier, want 0", sharedUsage)
	}
	if untouchedUsage != 99 {
		t.Errorf("untouched tag usage_count = %d, want 99 (unchanged)", untouchedUsage)
	}
}

// TestRemoveUserTagsFromImageHandler_DropsManualOnly: a DELETE on
// /images/{id}/user-tags must clear every is_auto=0 row for the
// image while leaving auto-tagged rows intact, with usage_count
// reconciled on each affected tag.
func TestRemoveUserTagsFromImageHandler_DropsManualOnly(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ut.png", 10, 10)

	insertTag := func(name string) int64 {
		t.Helper()
		var general int64
		_ = srv.db().Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&general)
		res, err := srv.db().Write.Exec(
			`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 1)`, name, general,
		)
		if err != nil {
			t.Fatal(err)
		}
		tagID, _ := res.LastInsertId()
		return tagID
	}
	manualID := insertTag("manual_a")
	autoID := insertTag("auto_b")
	if _, err := srv.db().Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 0, NULL)`,
		id, manualID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.db().Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 1, ?)`,
		id, autoID, "tagger-A",
	); err != nil {
		t.Fatal(err)
	}

	csrf := srv.csrfToken("anon")
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/images/%d/user-tags", id), nil)
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE /user-tags expected 200, got %d", w.Code)
	}

	var manualLeft, autoLeft int
	_ = srv.db().Read.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ? AND is_auto = 0`, id).Scan(&manualLeft)
	_ = srv.db().Read.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ? AND is_auto = 1`, id).Scan(&autoLeft)
	if manualLeft != 0 {
		t.Errorf("user-tags should be 0 after delete, got %d", manualLeft)
	}
	if autoLeft != 1 {
		t.Errorf("auto-tags should remain 1 (left alone), got %d", autoLeft)
	}
	var manualUsage, autoUsage int
	_ = srv.db().Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, manualID).Scan(&manualUsage)
	_ = srv.db().Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, autoID).Scan(&autoUsage)
	if manualUsage != 0 {
		t.Errorf("manual_a usage_count = %d after user-tags delete, want 0", manualUsage)
	}
	if autoUsage != 1 {
		t.Errorf("auto_b usage_count = %d after user-tags delete, want 1 (unchanged)", autoUsage)
	}
}

// TestRemoveAutoTagsFromImageHandler_RespectsTaggerFilter: the
// optional `taggers=` query param must narrow the delete to the
// named taggers, leaving other tagger rows alone. An empty filter
// removes every auto row.
func TestRemoveAutoTagsFromImageHandler_RespectsTaggerFilter(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "at.png", 10, 10)

	insertAuto := func(name, taggerName string) int64 {
		t.Helper()
		var general int64
		_ = srv.db().Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&general)
		res, err := srv.db().Write.Exec(
			`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 1)`, name, general,
		)
		if err != nil {
			t.Fatal(err)
		}
		tagID, _ := res.LastInsertId()
		if _, err := srv.db().Write.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto, tagger_name) VALUES (?, ?, 1, ?)`,
			id, tagID, taggerName,
		); err != nil {
			t.Fatal(err)
		}
		return tagID
	}
	aID := insertAuto("auto_from_a", "tagger-A")
	bID := insertAuto("auto_from_b", "tagger-B")
	_ = aID

	csrf := srv.csrfToken("anon")
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/images/%d/auto-tags?taggers=tagger-A", id), nil)
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE /auto-tags?taggers=tagger-A expected 200, got %d", w.Code)
	}

	var leftA, leftB int
	_ = srv.db().Read.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ? AND tag_id = ?`, id, aID).Scan(&leftA)
	_ = srv.db().Read.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ? AND tag_id = ?`, id, bID).Scan(&leftB)
	if leftA != 0 {
		t.Errorf("tagger-A row should be removed, got %d", leftA)
	}
	if leftB != 1 {
		t.Errorf("tagger-B row should survive (out of scope), got %d", leftB)
	}
}

// TestServeImageFile_RejectsTraversal: the handler-side
// path-traversal defense at GET /images/{id}/file must refuse a
// canonical_path that resolves outside the active gallery root, even
// when the row exists in the DB. Mirrors the gallery.PathInside test
// in the gallery package.
func TestServeImageFile_RejectsTraversal(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "served.png", 10, 10)

	req := httptest.NewRequest("GET", fmt.Sprintf("/images/%d/file", id), nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("happy-path serve expected 200, got %d", w.Code)
	}

	// Poison the row's canonical_path to a sibling directory and assert
	// the handler refuses to follow it. The cx.GalleryPath stays the
	// active gallery root, so the PathInside check fires.
	outside := filepath.Join(t.TempDir(), "evil.txt")
	if err := os.WriteFile(outside, []byte("leaked"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Active().DB.Write.Exec(
		`UPDATE images SET canonical_path = ? WHERE id = ?`, outside, id,
	); err != nil {
		t.Fatal(err)
	}

	req2 := httptest.NewRequest("GET", fmt.Sprintf("/images/%d/file", id), nil)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("traversal serve expected 404, got %d (body: %q)", w2.Code, w2.Body.String())
	}
}

// TestServeThumbnail_InvalidatesOnIDReuse pins that a thumbnail re-served
// at the same URL after the prior image was deleted and a new one ingested
// at the reused INTEGER PRIMARY KEY id no longer rides a cached If-None-
// Match response. The handler sets Cache-Control: no-cache + an mtime-
// bearing ETag, so the conditional GET sees a fresh tag and returns 200.
func TestServeThumbnail_InvalidatesOnIDReuse(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "first.png", 8, 8)

	thumbURL := fmt.Sprintf("/thumbnails/%s/%d.jpg", srv.activeName, id)

	req := httptest.NewRequest("GET", thumbURL, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("initial thumbnail serve expected 200, got %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q, want it to include no-cache", cc)
	}
	firstETag := w.Header().Get("ETag")
	if firstETag == "" {
		t.Fatal("ETag missing on initial thumbnail serve")
	}

	// Record the first thumbnail's mtime so we can force the rewrite's mtime
	// to a known, strictly-later value below — the ETag is keyed on
	// info.ModTime().Unix() (one-second resolution).
	cx := srv.Active()
	firstThumbPath := gallery.ThumbnailPath(cx.ThumbnailsPath, id)
	firstInfo, err := os.Stat(firstThumbPath)
	if err != nil {
		t.Fatalf("stat first thumbnail: %v", err)
	}

	// Delete the image (this also unlinks the thumbnail file) then re-ingest
	// a new image with different content. SQLite hands back the same id (no
	// AUTOINCREMENT on images.id and, with this row being the only one, the
	// table is empty after the delete so the next insert reuses id 1), so the
	// URL is identical but the bytes behind it must not be served from a
	// If-None-Match=304 fast path.
	if _, err := gallery.DeleteImage(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, id,
		func(int64) error { return nil }, nil,
	); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	newID := seedImage(t, srv, "second.png", 12, 12)
	if newID != id {
		t.Fatalf("test premise broken: id was not reused (got %d, want %d); "+
			"the only image was deleted so the re-ingest must reuse the id", newID, id)
	}

	// Force the rewritten thumbnail's mtime two seconds past the first so the
	// ETag's mtime component deterministically differs, replacing the old
	// 1100 ms sleep that hoped the rewrite landed in a later integer second.
	newThumbPath := gallery.ThumbnailPath(cx.ThumbnailsPath, newID)
	forcedMtime := firstInfo.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(newThumbPath, forcedMtime, forcedMtime); err != nil {
		t.Fatalf("chtimes new thumbnail: %v", err)
	}

	req2 := httptest.NewRequest("GET", thumbURL, nil)
	req2.Header.Set("If-None-Match", firstETag)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Code == http.StatusNotModified {
		t.Fatalf("re-ingest at reused id should not 304; the cached ETag must be stale")
	}
	if w2.Code != http.StatusOK {
		t.Fatalf("re-served thumbnail expected 200, got %d", w2.Code)
	}
	if newETag := w2.Header().Get("ETag"); newETag == "" || newETag == firstETag {
		t.Errorf("ETag must change on id-reuse rewrite: old=%q new=%q", firstETag, newETag)
	}
}

// TestUpdateExternal_AbsentFieldsLeaveOthersAlone: each detail-page
// dialog ships only its own field, so a caller that posts only
// `source=foo` (no series, no url) must leave collection and url
// unchanged. Absent != empty - empty clears the field, truly absent
// leaves it alone.
func TestUpdateExternal_AbsentFieldsLeaveOthersAlone(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ext_isolation.png", 10, 10)
	csrf := srv.csrfToken("anon")

	seedAll := url.Values{
		"_csrf":  {csrf},
		"source": {"danbooru"},
		"url":    {"https://example.com/art"},
	}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/external", id), strings.NewReader(seedAll.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code/100 != 2 && w.Code != http.StatusSeeOther {
		t.Fatalf("seed external fields: %d %s", w.Code, w.Body.String())
	}
	// Collection lives on its own endpoint; seed it so the source-only
	// update below can be shown to leave it untouched.
	seedOrder := 3
	if err := gallery.SetHomeCollection(srv.Active().DB, id, "my set", &seedOrder); err != nil {
		t.Fatal(err)
	}

	sourceOnly := url.Values{"_csrf": {csrf}, "source": {"updated"}}
	req2 := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/external", id), strings.NewReader(sourceOnly.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("X-CSRF-Token", csrf)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Code/100 != 2 && w2.Code != http.StatusSeeOther {
		t.Fatalf("update source only: %d %s", w2.Code, w2.Body.String())
	}

	var src, urlVal, collection string
	var order sql.NullInt64
	if err := srv.Active().DB.Read.QueryRow(
		`SELECT source, url, series, series_order FROM images WHERE id = ?`, id,
	).Scan(&src, &urlVal, &collection, &order); err != nil {
		t.Fatal(err)
	}
	if src != "updated" {
		t.Errorf("source = %q, want updated", src)
	}
	if urlVal != "https://example.com/art" {
		t.Errorf("url = %q, want unchanged https://example.com/art", urlVal)
	}
	if collection != "my set" {
		t.Errorf("collection = %q, want unchanged 'my set'", collection)
	}
	if !order.Valid || order.Int64 != 3 {
		t.Errorf("collection_order = %v (valid=%v), want unchanged 3", order.Int64, order.Valid)
	}
}

// setCollection requires a non-empty label; an order with no label is
// rejected before any membership is written.
func TestSetCollection_RejectsEmptyLabel(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ext_so.png", 10, 10)
	csrf := srv.csrfToken("anon")

	form := url.Values{"_csrf": {csrf}, "collection": {""}, "collection_order": {"5"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/collections/set", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (htmx swap target), got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "collection label required") {
		t.Errorf("body missing the validation message: %s", w.Body.String())
	}
	if cols, _ := gallery.CollectionsForImage(srv.Active().DB, id); len(cols) != 0 {
		t.Errorf("no membership should be created, got %#v", cols)
	}
}

// Removing the home collection clears the series mirror and its order.
func TestRemoveCollection_ClearsHome(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ext_clear.png", 10, 10)
	csrf := srv.csrfToken("anon")

	order := 5
	if err := gallery.SetHomeCollection(srv.Active().DB, id, "My Set", &order); err != nil {
		t.Fatal(err)
	}

	clr := url.Values{"_csrf": {csrf}, "collection": {"My Set"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/collections/remove", id), strings.NewReader(clr.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code/100 != 2 && w.Code != http.StatusSeeOther {
		t.Fatalf("remove collection: %d %s", w.Code, w.Body.String())
	}

	var collection string
	var ord sql.NullInt64
	if err := srv.Active().DB.Read.QueryRow(
		`SELECT series, series_order FROM images WHERE id = ?`, id,
	).Scan(&collection, &ord); err != nil {
		t.Fatal(err)
	}
	if collection != "" {
		t.Errorf("collection = %q, want empty", collection)
	}
	if ord.Valid {
		t.Errorf("collection_order = %d (valid=true), want NULL", ord.Int64)
	}
	if cols, _ := gallery.CollectionsForImage(srv.Active().DB, id); len(cols) != 0 {
		t.Errorf("memberships = %#v, want none", cols)
	}
}

// Zero and negative integers fall outside the 1-based position model.
func TestSetCollection_RejectsZeroOrNegativeOrder(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "ext_neg.png", 10, 10)
	csrf := srv.csrfToken("anon")

	for _, raw := range []string{"0", "-3"} {
		form := url.Values{"_csrf": {csrf}, "collection": {"My Set"}, "collection_order": {raw}}
		req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/collections/set", id), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("collection_order=%q expected 200, got %d: %s", raw, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "1 or higher") {
			t.Errorf("collection_order=%q body missing validation message: %s", raw, w.Body.String())
		}
	}
	if cols, _ := gallery.CollectionsForImage(srv.Active().DB, id); len(cols) != 0 {
		t.Errorf("no membership should be created, got %#v", cols)
	}
}

// ratingTagIDWeb is the web-package mirror of search.ratingTagID. The
// rating rows are seeded by the schema bootstrap so the lookup always
// succeeds.
func ratingTagIDWeb(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := database.Read.QueryRow(
		`SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.name = ?`, name,
	).Scan(&id); err != nil {
		t.Fatalf("rating tag %q not seeded: %v", name, err)
	}
	return id
}

// TestAddTagToImage_CategoryPrefixOnlyRejected pins parseTagInput's
// fix for the silent-drop that swallowed `general:` (known category +
// empty name). The token must surface as a rejected flash so the user
// sees the malformed input, matching how `:` and overlong names already
// behave. Other valid tokens in the same submit still get applied.
func TestAddTagToImage_CategoryPrefixOnlyRejected(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "prefix_only.png", 10, 10)

	csrf := srv.csrfToken("anon")
	form := url.Values{"_csrf": {csrf}, "tag": {"general: 1girl"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/tags", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "empty tag name after category prefix") {
		t.Errorf("rejected flash missing the prefix-only diagnostic; body: %s", body)
	}
	if !strings.Contains(body, "general:") {
		t.Errorf("flash should echo the offending token `general:`; body: %s", body)
	}
	// The valid 1girl token must still land.
	if cat := imageTagCategory(t, srv, id, "1girl"); cat != "general" {
		t.Errorf("1girl category = %q, want general (token after rejected prefix should still apply)", cat)
	}
}

// TestTagsPage_AliasRowExposesCategorySelect pins the spec §5.6
// editable-cell behaviour for alias rows: the Category cell is the
// same editable <select> non-alias rows use, riding the same
// PATCH /tags/{id}/category path.
func TestTagsPage_AliasRowExposesCategorySelect(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	var generalID int64
	_ = cx.DB.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&generalID)
	canon, _ := cx.TagSvc.GetOrCreateTag("alias_canon", generalID)
	src, _ := cx.TagSvc.GetOrCreateTag("alias_src", generalID)
	if err := cx.TagSvc.MergeTags(src.ID, canon.ID); err != nil {
		t.Fatalf("MergeTags: %v", err)
	}

	req := httptest.NewRequest("GET", "/tags?q=alias_src&show_zero=1&origin=alias", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /tags expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	idx := strings.Index(body, `data-name="alias_src"`)
	if idx < 0 {
		t.Fatalf("alias row not in body: %s", body[:min(len(body), 600)])
	}
	rowStart := strings.LastIndex(body[:idx], "<tr")
	rowEnd := strings.Index(body[idx:], "</tr>")
	row := body[rowStart : idx+rowEnd]
	if !strings.Contains(row, `<select class="cat-select"`) {
		t.Errorf("alias row missing the editable Category <select>; row was: %s", row)
	}
	// The select must hx-patch the same route non-alias rows use.
	if !strings.Contains(row, `hx-patch="/tags/`) {
		t.Errorf("alias row's select missing hx-patch wiring; row was: %s", row)
	}
}

// TestTagsPage_ClampsPastTheEndPage mirrors the gallery clamp: a stale
// ?page=N URL past the actual page count must clamp to the last valid
// page so the header count and the table content stay aligned.
func TestTagsPage_ClampsPastTheEndPage(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/tags?page=999", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /tags?page=999 expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "No tags found.") {
		t.Errorf("clamp failed: body still renders empty-state message: %s", body[:min(len(body), 600)])
	}
	// The seeded test gallery carries the four canonical rating tags
	// at usage 0, so show_zero=Show + page=999 must clamp to page 1
	// and surface at least one row.
	if !strings.Contains(body, `data-name="general"`) && !strings.Contains(body, `data-name="explicit"`) {
		t.Errorf("clamp didn't surface any rating row; body: %s", body[:min(len(body), 600)])
	}
}

// TestTagsPage_CategoryPrefixRedirectsToCatFilter: a `?q=character:`
// token (category prefix only, no tag-name suffix) surfaces the
// category-only filter instead of returning "No tags found".
func TestTagsPage_CategoryPrefixRedirectsToCatFilter(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/tags?q=character:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "cat=") || strings.Contains(loc, "q=character%3A") {
		t.Errorf("redirect Location = %q; want cat=<id> with q dropped", loc)
	}
}

// TestTagsPage_RatingRowOmitsImmutableActions pins the UI gating that
// matches spec §5.9: rating rows must not surface Rename / Alias→ /
// Delete buttons because the server uniformly rejects those operations.
// Implications stays visible because rating tags are valid edge sides.
func TestTagsPage_RatingRowOmitsImmutableActions(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/tags?q=explicit&show_zero=1", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /tags expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-name="explicit"`) {
		t.Fatalf("rating row for `explicit` not in /tags?q=explicit response: %s", body[:min(len(body), 600)])
	}
	for _, forbidden := range []string{
		`btn-rename-trigger" title="Rename this tag"`,
		`btn-merge-trigger"`,
		`<select class="cat-select"`,
	} {
		// Each appears around `data-name="explicit"` in the trigger
		// button block, so an unguarded scan of the body would hit even
		// for unrelated tags. Slice the explicit row's cell to scope the
		// assertion.
		idx := strings.Index(body, `data-name="explicit"`)
		if idx < 0 {
			break
		}
		// Walk backwards to the row's <tr; the row spans <tr ...> through
		// </tr>. Cheap split here is fine for a test.
		rowStart := strings.LastIndex(body[:idx], "<tr")
		rowEnd := strings.Index(body[idx:], "</tr>")
		if rowStart < 0 || rowEnd < 0 {
			t.Fatalf("could not isolate the explicit row in body")
		}
		row := body[rowStart : idx+rowEnd]
		if strings.Contains(row, forbidden) {
			t.Errorf("rating row should not contain %q; row was: %s", forbidden, row)
		}
	}
	// Implications and Delete stay available on the rating row.
	// Implications because rating tags are valid edge sides per §5.6.1;
	// Delete because the rating-tag branch of DeleteTag strips
	// image_tags rows but leaves the immutable catalog row in place.
	idx := strings.Index(body, `data-name="explicit"`)
	rowStart := strings.LastIndex(body[:idx], "<tr")
	rowEnd := strings.Index(body[idx:], "</tr>")
	row := body[rowStart : idx+rowEnd]
	if !strings.Contains(row, `btn-implications-trigger"`) {
		t.Errorf("rating row missing the Implications trigger; row was: %s", row)
	}
	if !strings.Contains(row, `btn-delete-tag"`) {
		t.Errorf("rating row missing the Delete trigger; row was: %s", row)
	}
}

// The detail page section listing duplicate filesystem paths sharing one
// SHA-256 must header itself "Duplicates" so the label doesn't shadow
// the unrelated tag-alias concept. The CSS class follows so the two
// surfaces don't drift.
func TestDetailPage_DuplicateFilePathsHeaderReadsDuplicates(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "dup.png", 10, 10)
	cx := srv.Active()
	// The panel only lists duplicates whose file is present, so the alias
	// needs to exist on disk.
	dupPath := filepath.Join(cx.GalleryPath, "dup-alt.png")
	if err := os.WriteFile(dupPath, []byte("dup"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`,
		id, dupPath,
	); err != nil {
		t.Fatalf("insert duplicate path: %v", err)
	}

	// Duplicates moved into the Related entries panel; it ships in the
	// lazy partial rather than the initial detail render.
	req := httptest.NewRequest("GET", fmt.Sprintf("/internal/images/%d/related-entries", id), nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("related-entries GET: %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<h3>Duplicates</h3>`) {
		t.Errorf("duplicate-paths section header should read Duplicates; not found in body")
	}
	if strings.Contains(body, `<h3>Aliases</h3>`) {
		t.Errorf("legacy <h3>Aliases</h3> still present in detail body")
	}
	if !strings.Contains(body, `duplicates-section`) {
		t.Errorf("duplicates-section CSS class missing")
	}
}

// TestDuplicatesPanel_HidesGoneFileAlias: a non-canonical path whose file
// is gone is move/copy history, not a live duplicate, so it must not
// render in the detail page's Duplicates panel - even before a sync prunes
// the row.
func TestDuplicatesPanel_HidesGoneFileAlias(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "live.png", 10, 10)
	cx := srv.Active()
	ghost := filepath.Join(cx.GalleryPath, "moved-away.png") // never written to disk
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`,
		id, ghost,
	); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", fmt.Sprintf("/internal/images/%d/related-entries", id), nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("related-entries GET: %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "moved-away.png") {
		t.Errorf("gone-file alias rendered in Duplicates panel; body should omit it")
	}
	if strings.Contains(body, `<h3>Duplicates</h3>`) {
		t.Errorf("Duplicates panel shown with only a phantom alias; should be hidden")
	}
}

// TestPromoteCanonical_RefusesMissingFile: setting canonical to an alias
// whose file is gone must be rejected so the image isn't repointed at a
// nonexistent path (which would make it unservable).
func TestPromoteCanonical_RefusesMissingFile(t *testing.T) {
	srv := newTestServer(t)
	id := seedImage(t, srv, "real.png", 10, 10)
	cx := srv.Active()
	ghost := filepath.Join(cx.GalleryPath, "ghost.png") // never written to disk
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`,
		id, ghost,
	); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"_csrf": {srv.csrfToken("anon")}, "path": {ghost}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/canonical-path", id), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("promote missing-file path: status = %d, want 400", w.Code)
	}

	var canonPath string
	if err := cx.DB.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&canonPath); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(canonPath, "real.png") {
		t.Errorf("canonical_path = %q, want it left at real.png", canonPath)
	}
}

// TestResetSkippedPost pins the new "Reset skipped" button: skipped_at
// gets cleared so previously-skipped pairs return to the head of the
// queue, while open (skipped_at IS NULL) rows are untouched.
func TestResetSkippedPost(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	ids := make([]int64, 8)
	for i := range ids {
		// Unique dimensions per image so each gets a distinct sha256 and
		// the ingest dedup doesn't collapse them onto one row.
		ids[i] = seedImage(t, srv, fmt.Sprintf("rs%d.png", i+1), 8+i, 8)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?,?,3,?), (?,?,4,?)`,
		ids[0], ids[1], now, ids[2], ids[3], now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at, skipped_at) VALUES (?,?,5,?,?), (?,?,6,?,?)`,
		ids[4], ids[5], now, now, ids[6], ids[7], now, now,
	); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"_csrf": {srv.csrfToken("anon")}}
	req := httptest.NewRequest("POST", "/relations/reset-skipped", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Reset 2 skipped pair(s)") {
		t.Errorf("flash should report the count; got: %s", body)
	}

	var skipped, open int
	_ = cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM potential_relation_pairs WHERE skipped_at IS NOT NULL`).Scan(&skipped)
	_ = cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM potential_relation_pairs WHERE skipped_at IS NULL`).Scan(&open)
	if skipped != 0 {
		t.Errorf("after reset, skipped count = %d, want 0", skipped)
	}
	if open != 4 {
		t.Errorf("after reset, open count = %d, want 4 (2 original + 2 reset)", open)
	}
}
