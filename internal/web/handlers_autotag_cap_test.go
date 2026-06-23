//go:build tagger

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageDummyTagger writes a model.onnx + tags.csv pair under a fresh
// model_path so tagger.IsAvailable(cfg) reports true. The files only need
// to exist for the over-cap path, which returns before any model is ever
// loaded — the cap rejection fires ahead of jobs.Start and RunWithTaggers.
func stageDummyTagger(t *testing.T, srv *Server) {
	t.Helper()
	modelRoot := filepath.Join(t.TempDir(), "models")
	taggerDir := filepath.Join(modelRoot, "wd14")
	if err := os.MkdirAll(taggerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taggerDir, "model.onnx"), []byte("dummy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taggerDir, "tags.csv"), []byte("tag_id,name,category\n0,sample,0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// IsAvailable / DiscoverTaggers read cfg.Paths.ModelPath live, so the
	// post-construction override takes effect immediately.
	srv.cfg.Paths.ModelPath = modelRoot
}

// bulkSeedVisibleImages inserts n visible (is_missing defaults 0) image
// rows in a single statement via WITH RECURSIVE so a 50k+ seed stays sub-
// second. sha256 is unique per row; canonical_path/file_type/file_size are
// the remaining NOT NULL columns without defaults.
func bulkSeedVisibleImages(t *testing.T, srv *Server, n int) {
	t.Helper()
	_, err := srv.db().Write.Exec(`
		WITH RECURSIVE seq(n) AS (
			SELECT 1
			UNION ALL
			SELECT n + 1 FROM seq WHERE n < ?
		)
		INSERT INTO images (sha256, canonical_path, file_type, file_size)
		SELECT printf('cap-%08d', n), printf('/cap/%08d.png', n), 'png', 1024
		FROM seq`, n)
	if err != nil {
		t.Fatalf("bulk seed %d images: %v", n, err)
	}
}

// TestAutotagTrigger_SearchScopeOverCap pins the autotagSearchScopeCap
// guard: with more than the cap visible and scope=search, the trigger
// must surface the "narrow the query" flash and start NO job — a flipped
// comparison or a dropped errAutotagOverCap sentinel would otherwise let
// an unbounded result set through. Tagger-build only: in the !tagger
// build tagger.IsAvailable is hard-false and the handler 503s before the
// cap check is ever reached, so this branch is unreachable there without
// a production-code change.
func TestAutotagTrigger_SearchScopeOverCap(t *testing.T) {
	srv := newTestServer(t)
	stageDummyTagger(t, srv)

	// One past the cap. The empty query matches every visible row.
	bulkSeedVisibleImages(t, srv, autotagSearchScopeCap+1)

	form := url.Values{"scope": {"search"}, "q": {""}, "_csrf": {srv.csrfToken("anon")}}
	req := httptest.NewRequest("POST", "/internal/autotag", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("over-cap htmx response expected 200 (inline flash), got %d: %s", w.Code, w.Body.String())
	}
	wantMsg := "Search matches more than 50000 images; narrow the query and re-run."
	if !strings.Contains(w.Body.String(), wantMsg) {
		t.Errorf("over-cap flash missing %q; body=%s", wantMsg, w.Body.String())
	}
	// The cap rejection precedes jobs.Start, so no job state must exist.
	if st := srv.jobs.Get(); st != nil {
		t.Errorf("over-cap path must not start a job; job state = %+v", st)
	}
	if srv.jobs.IsRunning() {
		t.Error("over-cap path must not leave a job running")
	}
}
