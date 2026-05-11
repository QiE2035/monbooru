package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/search"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWithDegraded(t, false)
}

// newTestServerWithDegraded builds a Server over a single in-memory gallery.
// When degraded=true the gallery_path points at a non-existent directory so
// the startup probe flips the context's Degraded flag.
func newTestServerWithDegraded(t *testing.T, degraded bool) *Server {
	t.Helper()
	dir := t.TempDir()
	galleryDir := filepath.Join(dir, "gallery")
	if degraded {
		galleryDir = filepath.Join(dir, "nonexistent_gallery")
	} else {
		os.MkdirAll(galleryDir, 0o755)
	}

	cfg := config.Default()
	cfg.Paths.DataPath = filepath.Join(dir, "data")
	cfg.Galleries[0].GalleryPath = galleryDir
	cfg.Galleries[0].DBPath = filepath.Join(cfg.Paths.DataPath, "default", "monbooru.db")
	cfg.Galleries[0].ThumbnailsPath = filepath.Join(cfg.Paths.DataPath, "default", "thumbnails")
	cfg.Gallery.WatchEnabled = false

	srv, err := NewServer(cfg, filepath.Join(dir, "monbooru.toml"), jobs.NewManager())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func TestServerStartsAndServesStatic(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/static/main.css", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for /static/main.css, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct == "" {
		t.Error("expected Content-Type header for CSS")
	}
}

func TestCustomCSS_NotFoundWhenUnset(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/custom.css", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("default config: GET /custom.css expected 404, got %d", w.Code)
	}
}

func TestCustomCSS_ServesConfiguredFile(t *testing.T) {
	srv := newTestServer(t)
	cssPath := filepath.Join(t.TempDir(), "custom.css")
	body := `:root { --bg: #112233; }`
	if err := os.WriteFile(cssPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.cfg.Server.CustomCSS = cssPath

	req := httptest.NewRequest("GET", "/custom.css", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /custom.css expected 200, got %d", w.Code)
	}
	if got := w.Body.String(); got != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}

// TestCustomCSSPathAllowed pins A-F009: the path scope check must
// accept paths under config dir / /config / /data and reject every
// other absolute or relative path - including the operator footgun
// /etc/passwd.
func TestCustomCSSPathAllowed(t *testing.T) {
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "monbooru.toml")

	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(configDir, "custom.css"), true},
		{filepath.Join(configDir, "sub", "ok.css"), true},
		{"/config/custom.css", true},
		{"/data/custom.css", true},
		{"/etc/passwd", false},
		{"/proc/self/environ", false},
		{"/tmp/leak.css", false},
	}
	for _, tc := range cases {
		got := customCSSPathAllowed(tc.path, configPath)
		if got != tc.want {
			t.Errorf("customCSSPathAllowed(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// Walks every full-layout page so a handler that hand-copies baseData
// fields into a map and forgets CustomCSS fails loudly here, instead of
// silently dropping the override link on whichever page it forgot.
func TestCustomCSS_LinkRenderedWhenConfigured(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Server.CustomCSS = "/some/path/custom.css"

	pages := []string{"/", "/tags", "/categories", "/settings", "/help", "/upload"}
	for _, page := range pages {
		req := httptest.NewRequest("GET", page, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("GET %s expected 200, got %d", page, w.Code)
			continue
		}
		body := w.Body.String()
		mainIdx := strings.Index(body, `href="/static/main.css"`)
		customIdx := strings.Index(body, `href="/custom.css"`)
		if mainIdx < 0 {
			t.Errorf("%s: main.css link missing", page)
			continue
		}
		if customIdx < 0 {
			t.Errorf("%s: /custom.css link missing when CustomCSS configured", page)
			continue
		}
		if customIdx < mainIdx {
			t.Errorf("%s: custom.css link must follow main.css; main=%d custom=%d",
				page, mainIdx, customIdx)
		}
	}
}

// TestSettingsStatsSection asserts the Settings → Stats section renders
// the active gallery in the per-gallery DB table and the process-memory
// rows. A handler that drops the Stats data map key surfaces here as a
// missing gallery name in the rendered body.
func TestSettingsStatsSection(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/settings", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	checks := []string{
		`href="#stats"`,
		`id="stats"`,
		`Process memory`,
		`Database size per gallery`,
		`Goroutines`,
		// The seeded test gallery's name must appear in the DB-size row.
		srv.cfg.Galleries[0].Name,
	}
	for _, c := range checks {
		if !strings.Contains(body, c) {
			t.Errorf("settings page missing stats marker: %q", c)
		}
	}
}

// TestGatherStats_MountLabelsAreCompact pins that a row's label set
// collapses to one entry per gallery on that filesystem, not one entry per
// (gallery × {db, images, thumbnails}). Two galleries on the same temp
// dir should produce a single row with both names; the row must not list
// "default db, default images, default thumbnails, stock db, ...".
func TestGatherStats_MountLabelsAreCompact(t *testing.T) {
	srv := newMultiGalleryServer(t)
	stats := srv.gatherStats()
	if len(stats.Mounts) == 0 {
		t.Skip("filesystem stats unavailable on this platform")
	}
	for _, m := range stats.Mounts {
		seen := map[string]int{}
		for _, l := range m.Labels {
			seen[l]++
			if seen[l] > 1 {
				t.Errorf("mount labels duplicate gallery name: %v", m.Labels)
			}
		}
		for _, l := range m.Labels {
			if strings.ContainsAny(l, " ") {
				t.Errorf("mount label %q includes a kind suffix; should be just the gallery name", l)
			}
		}
	}
}

// TestPageLoadIndicator_RenderedOnFullLayoutPages mirrors the CustomCSS
// walk: every full-layout handler must thread RequestStart through to the
// footer template func so a hand-copy handler that forgets the field shows
// up here instead of silently rendering "page loaded in 0 ms".
func TestPageLoadIndicator_RenderedOnFullLayoutPages(t *testing.T) {
	srv := newTestServer(t)

	pages := []string{"/", "/tags", "/categories", "/settings", "/help", "/upload"}
	for _, page := range pages {
		req := httptest.NewRequest("GET", page, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("GET %s expected 200, got %d", page, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), "page loaded in ") {
			t.Errorf("%s: footer page-load indicator missing", page)
		}
	}
}

func TestCustomCSS_LinkOmittedByDefault(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), `href="/custom.css"`) {
		t.Error("layout should not include /custom.css link when CustomCSS is unset")
	}
}

func TestLoginPageRendersWithoutAuth(t *testing.T) {
	srv := newTestServer(t)
	// Auth disabled by default → /login now renders an informational
	// notice rather than 303'ing so a user who bookmarked the page still
	// sees an explanation.
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 from /login when auth disabled, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Password authentication is disabled") {
		t.Errorf("expected disabled notice, got:\n%s", w.Body.String())
	}
}

func TestGalleryReturns200(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET / expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
}

func TestGalleryContainsExpectedElements(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	checks := []string{
		`id="search-input"`,
		`id="gallery-grid"`,
		`id="batch-bar"`,
		`MONBOORU`,
		`/static/main.css`,
		`/static/htmx.min.js`,
	}
	for _, s := range checks {
		if !strings.Contains(body, s) {
			t.Errorf("gallery page missing expected element: %q", s)
		}
	}
}

func TestGalleryHTMXPartialReturnsGrid(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/?q=test&sort=newest", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", "gallery-grid")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("HTMX partial expected 200, got %d", w.Code)
	}
	// Content-Type must be text/html so HTMX's swap logic accepts it;
	// a JSON default would silently break grid updates.
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("HTMX partial Content-Type = %q, want text/html…", ct)
	}
	body := w.Body.String()
	// Should return partial, not full page.
	if strings.Contains(body, "<html") {
		t.Error("HTMX partial response should not contain <html>")
	}
	if !strings.Contains(body, "thumb-grid") {
		t.Error("HTMX partial should contain thumb-grid")
	}
	// Partials include the OOB sidebar swap so filtering updates tag
	// counts without an extra round trip (spec §12.3).
	if !strings.Contains(body, "sidebar-inner") {
		t.Error("HTMX partial should carry the OOB sidebar swap")
	}
	// Footer timer rides an OOB span so "page loaded in N ms" reflects
	// the search request, not the original full-page render time.
	if !strings.Contains(body, `id="page-load-ms" hx-swap-oob="true"`) {
		t.Error("HTMX partial should carry the OOB page-load-ms swap")
	}
}

func TestGalleryEmptyFolderDialogRendered(t *testing.T) {
	// Empty folders are deleted automatically without a dialog prompt;
	// verify the page still loads cleanly when no `empty_folder` param is
	// supplied.
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/?q=", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestGallerySearchParamsPreserved(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/?q=mycategory&sort=newest&order=asc&page=1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "mycategory") {
		t.Error("search query should appear in rendered page")
	}
	if !strings.Contains(body, `value="newest"`) || !strings.Contains(body, "selected") {
		t.Error("sort option should be selected")
	}
}

func TestCSRFRejectsUnauthenticatedPost(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("POST", "/internal/sync", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 CSRF rejection, got %d", w.Code)
	}
}

func TestSessionMiddlewareRedirectsWhenAuthEnabled(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Auth.EnablePassword = true
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("expected redirect to /login when auth enabled, got %d", w.Code)
	}
}

func TestAllPagesReturn200(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	pages := []string{"/", "/tags", "/categories", "/settings", "/help"}
	for _, page := range pages {
		req := httptest.NewRequest("GET", page, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s expected 200, got %d\nbody: %s", page, w.Code, w.Body.String())
		}
	}
}

func TestJobStatusPartialReturns200(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/internal/job/status", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("job status expected 200, got %d", w.Code)
	}
}

// TestJobStatusHandler_RunningMarkup covers the template branch that
// Playwright cannot reliably reach: racing a running indicator across the
// 2 s HTMX poll produces flaky tests. Here we stage the manager in
// "running" state directly and assert the template emits job-running plus
// a × cancel button for cancellable types.
func TestJobStatusHandler_RunningMarkup(t *testing.T) {
	srv := newTestServer(t)
	// Stage the manager in running re-extract state.
	if err := srv.jobs.Start("re-extract"); err != nil {
		t.Fatalf("jobs.Start: %v", err)
	}
	defer srv.jobs.Complete("done")
	srv.jobs.Update(3, 10, "reading metadata")

	req := httptest.NewRequest("GET", "/internal/job/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("job status expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="job-running`,           // running class flips on the wrapper
		`data-job-type="re-extract"`,   // job type surfaces for UI hooks
		`<button class="job-dismiss"`,  // × cancel button
		`reading metadata`,             // progress message rendered
	} {
		if !strings.Contains(body, want) {
			t.Errorf("running job partial missing %q\nbody: %s", want, body)
		}
	}
}

// TestJobStatusHandler_NoCancelForWatcher pins the complementary branch:
// the transient "watcher" pseudo-job is surfaced as a summary, not as a
// cancellable running job.
func TestJobStatusHandler_NoCancelForWatcher(t *testing.T) {
	srv := newTestServer(t)
	srv.jobs.SetWatcherMessage("added foo.png")

	req := httptest.NewRequest("GET", "/internal/job/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, `class="job-running`) {
		t.Error("watcher pseudo-event must not render as a running job")
	}
	if !strings.Contains(body, "added foo.png") {
		t.Errorf("expected watcher summary in body, got: %s", body)
	}
}

// insertTestImage inserts a minimal image row and returns its ID.
func insertTestImage(t *testing.T, database *db.DB) int64 {
	t.Helper()
	res, err := database.Write.Exec(`
		INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		VALUES ('/tmp/test.jpg', 'jpg', 1024, 'abc123', datetime('now'))`)
	if err != nil {
		t.Fatalf("insert test image: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// TestDetailPage_BackPageReflectsCachedPositionWhenWarm pins the
// detail-page back-link override: when the adjacency cache holds the
// match list for the back_* search context, the detail handler resolves
// the current image's page from its index in the cached list and uses
// that for the "← Back" link, regardless of the back_page param the URL
// arrived with. This is what lets Escape land on the page that
// contains the current image after the user has walked prev/next
// across page boundaries.
func TestDetailPage_BackPageReflectsCachedPositionWhenWarm(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5
	id := insertTestImage(t, srv.db())

	// Seed the cache so the current id sits at index 12 (page 3 with
	// page_size 5). Other ids are sentinel values; the handler only
	// uses positions, not row data.
	cacheKey := search.BuildAdjacencyCacheKey(srv.activeName, "general:tag", "newest", "desc", 0, "")
	ids := make([]int64, 30)
	for i := range ids {
		ids[i] = int64(900_000 + i)
	}
	ids[12] = id
	search.AdjacencyCacheClear()
	search.AdjacencyCacheSet(cacheKey, ids)

	// User arrived on the detail page from page 1 of the search but,
	// after walking prev/next, the current image actually lives on
	// page 3 (index 12 / page_size 5 = floor 2 + 1 = 3).
	url := fmt.Sprintf(
		"/images/%d?back_q=%s&back_sort=newest&back_order=desc&back_page=1",
		id, "general%3Atag",
	)
	req := httptest.NewRequest("GET", url, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("detail expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "&page=3") {
		t.Errorf("back link should carry &page=3 from the cached index; body lacked it")
	}
	if strings.Contains(body, "&page=1#img-") {
		t.Errorf("back link still anchored at the original back_page=1")
	}
}

// TestDetailPage_BackPageRankQueryOnCacheMiss pins the cold-path
// fallback: when no cached match list is available, the handler runs
// a COUNT-rank against the back_q's WHERE shape and uses that to land
// the back link on the page that actually contains the image, even
// across detail loads that bypassed the gallery render entirely.
func TestDetailPage_BackPageRankQueryOnCacheMiss(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.UI.PageSize = 5

	// Insert a handful of images so a back_q query (here just the
	// implicit visible filter) produces a multi-page result. Newest
	// sort orders by ingested_at DESC, then id DESC; ingested_at is
	// set to datetime('now') for every row so id tie-breaks. A row
	// inserted later has a larger id and ranks before earlier rows.
	var ids []int64
	for i := 0; i < 12; i++ {
		res, err := srv.db().Write.Exec(
			`INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
			 VALUES (?, 'jpg', 1024, ?, datetime('now'))`,
			fmt.Sprintf("/tmp/rank_%d.jpg", i),
			fmt.Sprintf("rank_sha_%d", i),
		)
		if err != nil {
			t.Fatalf("insert rank fixture: %v", err)
		}
		newID, _ := res.LastInsertId()
		ids = append(ids, newID)
	}
	target := ids[3] // 4th inserted (zero-indexed rank 8 in DESC by id); page 2 with page_size 5.
	search.AdjacencyCacheClear()

	url := fmt.Sprintf(
		"/images/%d?back_q=&back_sort=newest&back_order=desc&back_page=1",
		target,
	)
	req := httptest.NewRequest("GET", url, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("detail expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "&page=2") {
		t.Errorf("cache-miss rank query should land back link on page 2; body lacked &page=2")
	}
	if strings.Contains(body, "&page=1#img-") {
		t.Errorf("back link still anchored at the original back_page=1")
	}
}

func TestDetailPageReturns200(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv.db())
	h := srv.Handler()

	req := httptest.NewRequest("GET", fmt.Sprintf("/images/%d", id), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("detail page expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
}

func TestDetailPageContainsMetadata(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv.db())
	h := srv.Handler()

	req := httptest.NewRequest("GET", fmt.Sprintf("/images/%d", id), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	checks := []string{"detail-topbar", "detail-media", "meta-table", "danger-zone",
		// Browse sections on the sidebar are lazy-loaded from this endpoint.
		`/internal/sidebar-browse`,
		// Search bar (trimmed copy of the gallery header) lives on detail too.
		`id="gallery-header"`, `id="search-form"`, `id="search-input"`,
	}
	for _, sel := range checks {
		if !strings.Contains(body, sel) {
			t.Errorf("detail page missing element %q", sel)
		}
	}
}

func TestSidebarBrowseReturnsBrowseSections(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/internal/sidebar-browse", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Folder tree + source filter + saved searches markup; per-page tag
	// groups must not leak in.
	for _, sel := range []string{"folder-tree-section", "source-filter-section"} {
		if !strings.Contains(body, sel) {
			t.Errorf("sidebar-browse missing %q\nbody: %s", sel, body)
		}
	}
	if strings.Contains(body, `id="tag-groups"`) {
		t.Error("sidebar-browse should not render the per-page tag-groups block")
	}
}

func TestDetailPageReturns404ForMissingImage(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/images/99999", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("missing image expected 404, got %d", w.Code)
	}
}

func TestDegradedModeBannerShown(t *testing.T) {
	srv := newTestServerWithDegraded(t, true)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "degraded-banner") {
		t.Error("degraded mode: expected degraded-banner in page")
	}
	if strings.Contains(body, `action="/internal/sync"`) {
		t.Error("degraded mode: sync button should be hidden")
	}
}

func TestDegradedModeSyncBlocked(t *testing.T) {
	srv := newTestServerWithDegraded(t, true)
	h := srv.Handler()

	req := httptest.NewRequest("POST", "/internal/sync", nil)
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("degraded mode sync expected 503, got %d", w.Code)
	}
}

func TestMissingImageBannerShown(t *testing.T) {
	srv := newTestServer(t)
	// Insert a missing image
	res, err := srv.db().Write.Exec(`
		INSERT INTO images (canonical_path, file_type, file_size, sha256, is_missing, ingested_at)
		VALUES ('/nonexistent/file.jpg', 'jpg', 1024, 'deadbeef', 1, datetime('now'))`)
	if err != nil {
		t.Fatalf("insert missing image: %v", err)
	}
	id, _ := res.LastInsertId()

	h := srv.Handler()
	req := httptest.NewRequest("GET", fmt.Sprintf("/images/%d", id), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("missing image detail expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "missing-banner") {
		t.Error("missing image: expected missing-banner in detail page")
	}
	if !strings.Contains(body, "no longer present on disk") {
		t.Error("missing image: expected missing file message in banner")
	}
}

func TestToggleFavoriteReturnsButton(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv.db())
	h := srv.Handler()

	// Auth is disabled in test server so session ID is always "anon".
	postReq := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/favorite", id), nil)
	postReq.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))

	postW := httptest.NewRecorder()
	h.ServeHTTP(postW, postReq)

	if postW.Code != http.StatusOK {
		t.Errorf("toggle favorite expected 200, got %d\nbody: %s", postW.Code, postW.Body.String())
	}
	body := postW.Body.String()
	if !strings.Contains(body, "fav-btn") {
		t.Errorf("toggle favorite response missing fav-btn, got: %s", body)
	}
}

// Toggling inbox state flips is_inbox via the same RETURNING-update shape
// as the favorite toggle, returns the inline button HTML for the new
// state, and drops the cached InboxCount so the toolbar count refreshes
// on the next render.
func TestToggleInboxReturnsButtonAndInvalidatesCount(t *testing.T) {
	srv := newTestServer(t)
	// insertTestImage creates a row with default is_inbox = 1 (the column
	// default applies when the INSERT omits it).
	id := insertTestImage(t, srv.db())
	cx := srv.Active()
	if cx == nil {
		t.Fatal("active gallery missing")
	}

	pre, err := cx.InboxCount()
	if err != nil {
		t.Fatalf("InboxCount pre: %v", err)
	}
	if pre != 1 {
		t.Errorf("expected 1 inbox image before toggle, got %d", pre)
	}

	h := srv.Handler()
	postReq := httptest.NewRequest("POST", fmt.Sprintf("/images/%d/inbox", id), nil)
	postReq.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	postW := httptest.NewRecorder()
	h.ServeHTTP(postW, postReq)

	if postW.Code != http.StatusOK {
		t.Errorf("toggle inbox expected 200, got %d\nbody: %s", postW.Code, postW.Body.String())
	}
	body := postW.Body.String()
	if !strings.Contains(body, "inbox-btn") {
		t.Errorf("toggle inbox response missing inbox-btn, got: %s", body)
	}
	if !strings.Contains(body, "Archived") {
		t.Errorf("toggle inbox response should label the new state Archived, got: %s", body)
	}

	post, err := cx.InboxCount()
	if err != nil {
		t.Fatalf("InboxCount post: %v", err)
	}
	if post != 0 {
		t.Errorf("expected 0 inbox images after toggle, got %d (cache may not have invalidated)", post)
	}
}

func TestDeleteImage(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv.db())
	h := srv.Handler()

	req := httptest.NewRequest("DELETE", fmt.Sprintf("/images/%d", id), nil)
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("delete image expected 200, got %d", w.Code)
	}
	// Verify image is gone
	var count int
	srv.db().Read.QueryRow(`SELECT COUNT(*) FROM images WHERE id = ?`, id).Scan(&count)
	if count != 0 {
		t.Error("image should be deleted from DB")
	}
	// Without back_* params there's no referring search context, so the
	// redirect still falls through to the gallery root.
	if got := w.Header().Get("HX-Redirect"); got != "/" {
		t.Errorf("no back-context redirect = %q, want /", got)
	}
}

// insertTwoDistinctImages inserts two image rows (different shas + paths,
// older then newer) and returns their IDs as (older, newer).
func insertTwoDistinctImages(t *testing.T, database *db.DB) (older, newer int64) {
	t.Helper()
	r1, err := database.Write.Exec(`
		INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		VALUES ('/tmp/older.jpg', 'jpg', 1024, 'sha_older', '2024-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert older: %v", err)
	}
	older, _ = r1.LastInsertId()
	r2, err := database.Write.Exec(`
		INSERT INTO images (canonical_path, file_type, file_size, sha256, ingested_at)
		VALUES ('/tmp/newer.jpg', 'jpg', 1024, 'sha_newer', '2025-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert newer: %v", err)
	}
	newer, _ = r2.LastInsertId()
	return
}

func TestDeleteImage_RedirectsToNextInSearch(t *testing.T) {
	srv := newTestServer(t)
	older, newer := insertTwoDistinctImages(t, srv.db())
	h := srv.Handler()

	// Delete the newer image with a referring-search context. Under
	// newest-desc, the next image after the deleted one is the older one,
	// so the redirect should land on its detail page with back_* preserved.
	url := fmt.Sprintf("/images/%d?back_q=&back_sort=newest&back_order=desc", newer)
	req := httptest.NewRequest("DELETE", url, nil)
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("delete expected 200, got %d", w.Code)
	}
	got := w.Header().Get("HX-Redirect")
	wantPrefix := fmt.Sprintf("/images/%d", older)
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("redirect = %q, want prefix %q", got, wantPrefix)
	}
	if !strings.Contains(got, "back_sort=newest") || !strings.Contains(got, "back_order=desc") {
		t.Errorf("redirect %q should carry back_sort/back_order", got)
	}
}

func TestDeleteImage_FallsBackToGalleryOnLastImage(t *testing.T) {
	srv := newTestServer(t)
	id := insertTestImage(t, srv.db())
	h := srv.Handler()

	// Single image + back_* context: no next, no prev → fall back to gallery.
	url := fmt.Sprintf("/images/%d?back_q=&back_sort=newest&back_order=desc", id)
	req := httptest.NewRequest("DELETE", url, nil)
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("delete expected 200, got %d", w.Code)
	}
	got := w.Header().Get("HX-Redirect")
	if strings.HasPrefix(got, "/images/") {
		t.Errorf("last-image redirect = %q, want gallery URL", got)
	}
}

func TestUploadPageReturns200(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/upload", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /upload expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Upload Images") {
		t.Error("upload page missing expected heading")
	}
}

func TestSettingsGeneralPost(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	body := "_csrf=" + srv.csrfToken("anon") +
		"&watch_enabled=on&max_file_size_mb=200&page_size=60"
	req := httptest.NewRequest("POST", "/settings/general", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("settings general POST expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Saved") {
		t.Error("expected 'Saved' flash message")
	}
	if !srv.cfg.Gallery.WatchEnabled {
		t.Error("WatchEnabled should be true after save")
	}
	if srv.cfg.Gallery.MaxFileSizeMB != 200 {
		t.Errorf("MaxFileSizeMB = %d, want 200", srv.cfg.Gallery.MaxFileSizeMB)
	}
	if srv.cfg.UI.PageSize != 60 {
		t.Errorf("PageSize = %d, want 60", srv.cfg.UI.PageSize)
	}
}

// TestSettingsTagger_RejectsBadName pins the allowlist guard on every
// per-tagger settings endpoint. A name that escapes [A-Za-z0-9_-]+
// would otherwise land in cfg.Tagger.Taggers verbatim and persist to
// TOML, with subsequent runs joining model_path/<name>/model.onnx
// against a value the lookup can never resolve.
func TestSettingsTagger_RejectsBadName(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	csrf := srv.csrfToken("anon")
	form := "_csrf=" + csrf

	cases := []struct {
		method string
		path   string
	}{
		{"POST", "/settings/tagger/foo$bar/enable"},
		{"POST", "/settings/tagger/foo$bar/disable"},
		{"GET", "/settings/tagger/foo$bar/thresholds"},
		{"POST", "/settings/tagger/foo$bar/thresholds"},
		{"POST", "/settings/tagger/foo$bar/thresholds/reset"},
		{"GET", "/settings/tagger/foo$bar/galleries"},
		{"POST", "/settings/tagger/foo$bar/galleries"},
		{"POST", "/settings/tagger/foo$bar/delete"},
	}
	for _, tc := range cases {
		var body string
		if tc.method == "POST" {
			body = form
		}
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(body))
		if tc.method == "POST" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s %s: expected 400 for bad tagger name, got %d", tc.method, tc.path, w.Code)
		}
	}

	// The bad name must not have leaked into cfg.Tagger.Taggers via the
	// enable/disable handlers, which append a new instance when the name
	// isn't already configured.
	for _, ti := range srv.cfg.Tagger.Taggers {
		if ti.Name == "foo$bar" {
			t.Errorf("rejected tagger name persisted to config: %+v", ti)
		}
	}
}

func TestCSRFTokenValidation(t *testing.T) {
	srv := newTestServer(t)
	sess := "test-session-id"
	token := srv.csrfToken(sess)
	if !srv.validateCSRF(sess, token) {
		t.Error("validateCSRF should accept valid token")
	}
	if srv.validateCSRF(sess, "wrong-token") {
		t.Error("validateCSRF should reject invalid token")
	}
	if srv.validateCSRF("other-session", token) {
		t.Error("validateCSRF should reject token for different session")
	}
}

func TestCSRFTokensAreServerScoped(t *testing.T) {
	srvA := newTestServer(t)
	srvB := newTestServer(t)
	tok := srvA.csrfToken("anon")
	if srvB.validateCSRF("anon", tok) {
		t.Error("tokens issued by one Server must not validate against another")
	}
}

func TestSessionExpiry(t *testing.T) {
	store := NewSessionStore()
	id, err := store.NewSession(0) // 0 days = expires immediately
	if err != nil {
		t.Fatal(err)
	}
	// Session with 0-day lifetime should already be expired
	if _, ok := store.GetSession(id); ok {
		t.Error("session with 0-day lifetime should be expired")
	}

	// Create a valid session
	id2, _ := store.NewSession(1) // 1 day
	if _, ok := store.GetSession(id2); !ok {
		t.Error("session with 1-day lifetime should be valid")
	}

	// Test SweepExpired
	store.SweepExpired()
	if _, ok := store.GetSession(id2); !ok {
		t.Error("non-expired session should survive sweep")
	}
}

func TestPruneMissingImages(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	// Insert a missing image
	srv.db().Write.Exec(`
		INSERT INTO images (canonical_path, file_type, file_size, sha256, is_missing, ingested_at)
		VALUES ('/nonexistent/file.jpg', 'jpg', 1024, 'prune_test_hash', 1, datetime('now'))`)

	body := "_csrf=" + srv.csrfToken("anon")
	req := httptest.NewRequest("POST", "/settings/maintenance/prune-missing", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("prune missing expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Removed") {
		t.Error("expected 'Removed' in response")
	}

	// Verify pruned
	var count int
	srv.db().Read.QueryRow(`SELECT COUNT(*) FROM images WHERE sha256 = 'prune_test_hash'`).Scan(&count)
	if count != 0 {
		t.Error("missing image should have been pruned")
	}
}

// TestPruneOrphanedThumbnails_AsJob pins the new shape: the request
// returns immediately with "Thumbnail prune started.", the actual
// sweep happens in a goroutine, and the orphan file is gone once the
// job manager reports done.
func TestPruneOrphanedThumbnails_AsJob(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()

	orphan := filepath.Join(cx.ThumbnailsPath, "777777.jpg")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := "_csrf=" + srv.csrfToken("anon")
	req := httptest.NewRequest("POST", "/settings/maintenance/prune-orphaned-thumbnails", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "started") {
		t.Errorf("expected immediate 'started' flash, got: %s", w.Body.String())
	}

	// Wait for the goroutine to finish - 2s ceiling is more than enough
	// for the in-memory test fixture.
	deadline := time.Now().Add(2 * time.Second)
	for {
		state := srv.jobs.Get()
		if state != nil && !state.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prune-thumbs job did not finish within 2s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphan thumbnail should have been removed")
	}
}

// newMultiGalleryServer builds a Server with two galleries so the switch,
// add, and topbar-dialog paths are exercised. Watchers stay off for tests.
func newMultiGalleryServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	g1 := filepath.Join(dir, "g1")
	g2 := filepath.Join(dir, "g2")
	os.MkdirAll(g1, 0o755)
	os.MkdirAll(g2, 0o755)

	cfg := config.Default()
	cfg.Paths.DataPath = filepath.Join(dir, "data")
	cfg.Gallery.WatchEnabled = false
	cfg.Galleries = []config.Gallery{
		{
			Name: "default", GalleryPath: g1,
			DBPath:         filepath.Join(cfg.Paths.DataPath, "default", "monbooru.db"),
			ThumbnailsPath: filepath.Join(cfg.Paths.DataPath, "default", "thumbnails"),
		},
		{
			Name: "stock", GalleryPath: g2,
			DBPath:         filepath.Join(cfg.Paths.DataPath, "stock", "monbooru.db"),
			ThumbnailsPath: filepath.Join(cfg.Paths.DataPath, "stock", "thumbnails"),
		},
	}
	cfg.DefaultGallery = "default"

	srv, err := NewServer(cfg, filepath.Join(dir, "monbooru.toml"), jobs.NewManager())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func TestGallerySwitcherButtonShownWithMultipleGalleries(t *testing.T) {
	srv := newMultiGalleryServer(t)
	h := srv.Handler()

	for _, path := range []string{"/", "/upload", "/categories"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		body := w.Body.String()
		if !strings.Contains(body, `id="gallery-switch-btn"`) {
			t.Errorf("%s: layout should render the gallery switcher button with 2+ galleries", path)
		}
		if !strings.Contains(body, `id="gallery-switch-dialog"`) {
			t.Errorf("%s: layout should render the gallery switch dialog with 2+ galleries", path)
		}
	}
}

func TestGallerySwitchChangesActive(t *testing.T) {
	srv := newMultiGalleryServer(t)
	h := srv.Handler()

	body := "_csrf=" + srv.csrfToken("anon") + "&name=stock"
	req := httptest.NewRequest("POST", "/internal/gallery/switch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("switch expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("HX-Refresh") != "true" {
		t.Error("switch should respond with HX-Refresh: true")
	}
	if srv.activeName != "stock" {
		t.Errorf("activeName = %q, want stock", srv.activeName)
	}
}

func TestGallerySwitch_RedirectsHomeFromSearch(t *testing.T) {
	srv := newMultiGalleryServer(t)
	h := srv.Handler()

	body := "_csrf=" + srv.csrfToken("anon") + "&name=stock"
	req := httptest.NewRequest("POST", "/internal/gallery/switch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://localhost/?q=cat")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("switch expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("HX-Redirect"); got != "/" {
		t.Errorf("HX-Redirect = %q, want /", got)
	}
	if w.Header().Get("HX-Refresh") != "" {
		t.Error("search-bearing URL should redirect, not refresh in place")
	}
}

func TestGallerySwitcherHiddenWithSingleGallery(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), `id="gallery-switch-btn"`) {
		t.Error("gallery switcher button should be hidden when only one gallery is configured")
	}
}
