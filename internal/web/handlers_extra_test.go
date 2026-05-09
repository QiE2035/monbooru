package web

import (
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

	"github.com/leqwin/monbooru/internal/gallery"
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
	cx.DB.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&canonPath)
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
	cx.DB.Read.QueryRow(`SELECT canonical_path, folder_path FROM images WHERE id = ?`, id).Scan(&canonPath, &folderPath)
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
		f.Close()
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
		`class="suggest-category">system<`,
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

// A small dot separator sits between the description and the dim
// "system" column so the cheat-sheet reads as `name  description · system`.
// Rows without a description (rating values, fav:true, etc.) and tag
// rows skip the separator.
func TestSearchSuggest_System_DescriptionSeparator(t *testing.T) {
	srv := newTestServer(t)

	// System row with a description: separator must be present.
	req := httptest.NewRequest("GET", "/internal/search/suggest?q=system:", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	withDesc := w.Body.String()
	if !strings.Contains(withDesc, `<span class="suggest-description">favorite images</span><span class="suggest-sep">·</span>`) {
		t.Errorf("expected description+separator pair on system row, got: %s", withDesc)
	}

	// Level-2 row without a description (rating values are bare): no
	// separator and no description span.
	req2 := httptest.NewRequest("GET", "/internal/search/suggest?q=system:rating:", nil)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	bare := w2.Body.String()
	if strings.Contains(bare, `class="suggest-sep"`) {
		t.Errorf("rows without a description must not render the separator, got: %s", bare)
	}
}

// Cheat-sheet rows carry a short English label between the name and the
// dim "system" column so the dropdown reads as a discoverable reference.
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
	srv.db().Read.QueryRow(`SELECT COUNT(*) FROM saved_searches`).Scan(&count)
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
	cx.DB.Read.QueryRow(`SELECT COUNT(*) FROM sd_metadata WHERE image_id = ?`, id).Scan(&count)
	if count != 0 {
		t.Errorf("re-extract should have cleared the stale sd_metadata row for a plain PNG, count = %d", count)
	}
	var sourceType string
	cx.DB.Read.QueryRow(`SELECT source_type FROM images WHERE id = ?`, id).Scan(&sourceType)
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
// monotonically; the audit reproduced this with 19-digit auto-seeds. A
// 32-bit seed keeps the product in int64 for any plausible image id.
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
	cx.DB.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&generalID)
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
	cx.DB.Read.QueryRow(`SELECT name FROM tags WHERE id = ?`, first.ID).Scan(&stillName)
	if stillName != "first" {
		t.Errorf("collision should leave tag untouched, got name %q", stillName)
	}
}

// TestMergeTags_HTMXSuccessRefreshes mirrors the rename refresh
// guarantee: a successful alias / repoint posts back HX-Refresh so
// the dialog's parent /tags page reloads at the same URL, keeping
// the user's q / sort / origin / page filter in scope.
func TestMergeTags_HTMXSuccessRefreshes(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	var generalID int64
	cx.DB.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&generalID)
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
	if w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("HX-Refresh = %q, want true", w.Header().Get("HX-Refresh"))
	}
	if got := w.Header().Get("HX-Redirect"); got != "" {
		t.Errorf("HX-Redirect = %q, want empty (HX-Refresh handles reload)", got)
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
	cx.DB.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&generalID)
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
	srv.Active().DB.Read.QueryRow(`SELECT url FROM images WHERE id = ?`, id).Scan(&stored)
	if stored != "https://example.com/art" {
		t.Errorf("images.url = %q, want https://example.com/art", stored)
	}
}

// TestUploadInboxLink_DropsCountUnderCeiling pins the same fix as
// the gallery toolbar tooltip: the InboxCount cache is ceiling-blind,
// so a "View inbox (39)" link mis-promises when a ceiling hides
// higher-rated rows from the click result. Drop the parens when a
// ceiling is active so the link doesn't masquerade as the post-click
// match count.
func TestUploadInboxLink_DropsCountUnderCeiling(t *testing.T) {
	srv := newTestServer(t)
	seedImage(t, srv, "in.png", 10, 10)

	req := httptest.NewRequest("GET", "/upload", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /upload expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `>✱ View inbox (1)</a>`) {
		t.Errorf("no-ceiling link should carry the count; body slice: %s", uploadInboxLink(t, body))
	}

	req = httptest.NewRequest("GET", "/upload", nil)
	req.AddCookie(&http.Cookie{Name: "monbooru_rating_ceiling", Value: "sensitive"})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /upload with ceiling expected 200, got %d", w.Code)
	}
	body = w.Body.String()
	if !strings.Contains(body, `>✱ View inbox</a>`) {
		t.Errorf("ceiling-active link should drop the count; body slice: %s", uploadInboxLink(t, body))
	}
}

func uploadInboxLink(t *testing.T, body string) string {
	t.Helper()
	idx := strings.Index(body, `id="upload-inbox-link"`)
	if idx < 0 {
		return "(no upload-inbox-link)"
	}
	end := strings.Index(body[idx:], "</a>")
	if end < 0 {
		return body[idx:min(len(body), idx+200)]
	}
	return body[idx : idx+end+4]
}

// TestGalleryInboxTooltip_DropsCountUnderCeiling pins F011: the
// InboxCount cache deliberately ignores the cookie-applied rating
// ceiling, so a tooltip "Inbox (39)" would lie when the ceiling
// hides higher-rated rows from the click result. Drop the parens
// when a ceiling is active so the badge doesn't masquerade as the
// post-click match count.
func TestGalleryInboxTooltip_DropsCountUnderCeiling(t *testing.T) {
	srv := newTestServer(t)
	// Seed one inbox image so InboxCount > 0 in both branches.
	seedImage(t, srv, "in.png", 10, 10)

	// No cookie → ActiveRating = "explicit" → count parenthesised.
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET / expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `title="Inbox (1) (I)"`) {
		t.Errorf("no-ceiling tooltip should carry the count; body slice: %s", inboxTooltip(t, body))
	}

	// With ceiling=sensitive set, the badge must drop the count.
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "monbooru_rating_ceiling", Value: "sensitive"})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET / with ceiling expected 200, got %d", w.Code)
	}
	body = w.Body.String()
	if !strings.Contains(body, `title="Inbox (I)"`) {
		t.Errorf("ceiling-active tooltip should drop the count; body slice: %s", inboxTooltip(t, body))
	}
}

func inboxTooltip(t *testing.T, body string) string {
	t.Helper()
	idx := strings.Index(body, `id="inbox-filter-btn"`)
	if idx < 0 {
		return "(no inbox-filter-btn)"
	}
	end := strings.Index(body[idx:], ">")
	if end < 0 {
		return body[idx:min(len(body), idx+200)]
	}
	return body[idx : idx+end+1]
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

// TestTagsPage_AliasRowOmitsCategorySelect pins the spec §5.6 read-only
// claim for alias rows: the Category cell is read-only because the
// alias's category is set at creation and ChangeTagCategory wouldn't
// know which side to move. Render a colored span instead of the
// editable <select>.
func TestTagsPage_AliasRowOmitsCategorySelect(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	var generalID int64
	cx.DB.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&generalID)
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
	if strings.Contains(row, `<select class="cat-select"`) {
		t.Errorf("alias row still renders an editable Category <select>; row was: %s", row)
	}
	// The cell should still surface the alias's own category by name.
	if !strings.Contains(row, `>general</span>`) {
		t.Errorf("alias row missing the static Category span; row was: %s", row)
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
