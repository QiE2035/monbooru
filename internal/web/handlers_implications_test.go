package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leqwin/monbooru/internal/models"
)

// formPOST builds a form-urlencoded POST request, sets the CSRF
// header so the middleware accepts the call, and runs it through the
// server's handler. Returns the recorder for assertion.
func formPOST(t *testing.T, srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	form.Set("_csrf", srv.csrfToken("anon"))
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// drainJob spins until the job manager reports the run finished (Running
// false and FinishedAt non-nil), or the deadline expires. The 5-second
// cap is generous enough for the chunked propagation against the
// 600-image seed below; a real CI box settles in <1 s.
func drainJob(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := srv.jobs.Get()
		if !st.Running && st.FinishedAt != nil {
			return
		}
		if !st.Running && st.JobType == "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not finish within 5s")
}

// TestAddImplicationPost_DeclaresAndDispatches pins the happy path of
// the dialog submit: a single tag input declares a parent->child edge,
// the propagation job fires, and post-job the implied row lands on
// every image carrying the parent (with is_implied=1).
func TestAddImplicationPost_DeclaresAndDispatches(t *testing.T) {
	srv := newTestServer(t)
	// Seed two images carrying tag "anime". Different dimensions so
	// gallery.Ingest's sha-dedup gives both distinct rows.
	idA := seedImage(t, srv, "a.png", 11, 11)
	idB := seedImage(t, srv, "b.png", 12, 12)
	parent, err := srv.tagSvc().GetOrCreateTag("anime", srv.Active().GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.tagSvc().AddTagToImage(idA, parent.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := srv.tagSvc().AddTagToImage(idB, parent.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"implied_id": {"japanese"},
	}
	w := formPOST(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/implications", form)
	if w.Code != http.StatusNoContent {
		t.Fatalf("declare: code=%d body=%s", w.Code, w.Body.String())
	}
	drainJob(t, srv)

	// Implied tag exists, and every carrier got the implied row with
	// is_implied=1.
	var impliedTagID int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM tags WHERE name = ?`, "japanese").Scan(&impliedTagID); err != nil {
		t.Fatalf("look up implied tag: %v", err)
	}
	var n int
	if err := srv.db().Read.QueryRow(
		`SELECT COUNT(*) FROM image_tags WHERE tag_id = ? AND is_implied = 1`, impliedTagID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("implied rows = %d, want 2", n)
	}
}

// TestAddImplicationPost_CycleRejected pins the cycle-rejection flash
// shape so the parser-side input validation stays wired up.
func TestAddImplicationPost_CycleRejected(t *testing.T) {
	srv := newTestServer(t)
	parent, err := srv.tagSvc().GetOrCreateTag("p", srv.Active().GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := srv.tagSvc().GetOrCreateTag("c", srv.Active().GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.tagSvc().AddImplication(parent.ID, child.ID); err != nil {
		t.Fatal(err)
	}
	// Now declare c -> p; would close a cycle.
	form := url.Values{"implied_id": {"p"}}
	w := formPOST(t, srv, "/tags/"+strconv.FormatInt(child.ID, 10)+"/implications", form)
	body := w.Body.String()
	if !strings.Contains(body, "cycle") {
		t.Errorf("expected cycle rejection in body, got %s", body)
	}
}

// TestDeleteImplicationPost_RemovesAndPropagates declares an
// implication, lets the add propagate, then deletes it and confirms
// the remove sweep clears the implied rows from every carrier.
func TestDeleteImplicationPost_RemovesAndPropagates(t *testing.T) {
	srv := newTestServer(t)
	idA := seedImage(t, srv, "a.png", 10, 10)
	parent, err := srv.tagSvc().GetOrCreateTag("anime", srv.Active().GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.tagSvc().AddTagToImage(idA, parent.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	addForm := url.Values{"implied_id": {"japanese"}}
	w := formPOST(t, srv, "/tags/"+strconv.FormatInt(parent.ID, 10)+"/implications", addForm)
	if w.Code != http.StatusNoContent {
		t.Fatalf("declare: code=%d body=%s", w.Code, w.Body.String())
	}
	drainJob(t, srv)

	var impliedTagID int64
	if err := srv.db().Read.QueryRow(`SELECT id FROM tags WHERE name = ?`, "japanese").Scan(&impliedTagID); err != nil {
		t.Fatal(err)
	}
	delReq := httptest.NewRequest("DELETE", fmt.Sprintf("/tags/%d/implications/%d", parent.ID, impliedTagID), nil)
	delReq.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	delW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(delW, delReq)
	if delW.Code != http.StatusNoContent {
		t.Fatalf("delete: code=%d body=%s", delW.Code, delW.Body.String())
	}
	drainJob(t, srv)

	var n int
	if err := srv.db().Read.QueryRow(
		`SELECT COUNT(*) FROM image_tags WHERE tag_id = ? AND is_implied = 1`, impliedTagID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("implied rows after remove = %d, want 0", n)
	}
}

// TestRunImplicationPropagation_ChunkedAcrossBoundary seeds enough
// carriers to span the 500-row chunk boundary and verifies the
// post-job state: implied rows present everywhere, usage_count on the
// implied tag matches the carrier count, and the job summary names
// the documented "applied to N image(s)" shape.
func TestRunImplicationPropagation_ChunkedAcrossBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 600+ images; skipping in short mode")
	}
	srv := newTestServer(t)
	parent, err := srv.tagSvc().GetOrCreateTag("parent", srv.Active().GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	const carriers = 600
	// Direct DB inserts: gallery.Ingest dedupes by SHA-256, and 600
	// PNGs of varying dimensions collapse to a handful of unique
	// hashes when their content stays mostly zeros. Real ingest is
	// already exercised by sibling tests; here the contract under
	// test is the chunked propagation loop, not the ingest pipeline.
	for i := 0; i < carriers; i++ {
		sha := fmt.Sprintf("%064x", i+1)
		path := fmt.Sprintf("img%03d.png", i)
		res, err := srv.db().Write.Exec(
			`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, source_type, origin)
			 VALUES (?, ?, '', 'png', 1024, 'image', 'test')`,
			sha, path,
		)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if err := srv.tagSvc().AddTagToImage(id, parent.ID, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	implied, err := srv.tagSvc().GetOrCreateTag("implied", srv.Active().GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.tagSvc().AddImplication(parent.ID, implied.ID); err != nil {
		t.Fatal(err)
	}
	srv.startImplicationPropagation(parent.ID, implied.ID, "add")
	drainJob(t, srv)

	var rowCount int
	if err := srv.db().Read.QueryRow(
		`SELECT COUNT(*) FROM image_tags WHERE tag_id = ? AND is_implied = 1`, implied.ID,
	).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != carriers {
		t.Errorf("implied rows = %d, want %d", rowCount, carriers)
	}

	var usage int
	if err := srv.db().Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, implied.ID).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if usage != carriers {
		t.Errorf("usage_count = %d, want %d", usage, carriers)
	}

	st := srv.jobs.Get()
	summary := st.Summary
	if summary == "" {
		summary = st.Message
	}
	if !strings.Contains(summary, fmt.Sprintf("%d image", carriers)) {
		t.Errorf("job summary = %q", summary)
	}
	if st.JobType != models.JobTypeTag {
		t.Errorf("job type = %q", st.JobType)
	}
}

// implSummaryRe matches the documented partial-progress summary the
// implication-propagation chunk loop emits when cancelled mid-run:
// "applying implication cancelled (N/total)". Pinned as a shape (any N)
// because the precise chunk boundary the cancel lands on is timing-
// dependent; the branch and the format are what the test guards.
var implSummaryRe = regexp.MustCompile(`^applying implication cancelled \((\d+)/(\d+)\)$`)

// TestRunImplicationPropagation_CancelledMidRun pins the chunk-loop's
// ctx.Err() branch and the documented "cancelled (N/total)" summary that
// TestRunImplicationPropagation_ChunkedAcrossBoundary (which only asserts
// the completed post-job state) never reaches. A flip of that branch to
// jobs.Fail, or a lost processed count, would regress silently otherwise.
//
// Determinism note: cancelling before the worker starts is NOT viable —
// runImplicationPropagation's first step is a QueryContext read of the
// carriers, and database/sql fails any read on an already-cancelled
// context, which would take the Fail path instead of the chunk-loop
// branch. So the worker runs in a goroutine, and the test cancels the
// moment the read has finished and the chunk loop has begun (observed via
// the "0/N…" progress message chunkedJob emits before its first ctx.Err()
// check). With more than one chunk, the post-chunk-1 ctx.Err() boundary
// then deterministically catches the cancel. The poll is a deadline-
// bounded runtime.Gosched busy-wait, not a fixed time.Sleep settle.
func TestRunImplicationPropagation_CancelledMidRun(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds many images to span chunk boundaries; skipping in short mode")
	}
	srv := newTestServer(t)
	parent, err := srv.tagSvc().GetOrCreateTag("parent", srv.Active().GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	// > one 500-row chunk so there is a second ctx.Err() boundary after the
	// first (slow) chunk: the cancel, issued the instant the read completes,
	// lands either before chunk 1 (processed 0) or during it (caught at the
	// chunk-2 boundary, processed 500) — never after the whole loop drains,
	// since chunk 1's per-image transactions dwarf the cancel latency.
	// Direct inserts (the propagation loop, not ingest, is under test).
	const carriers = 600
	for i := 0; i < carriers; i++ {
		res, err := srv.db().Write.Exec(
			`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size, source_type, origin)
			 VALUES (?, ?, '', 'png', 1024, 'image', 'test')`,
			fmt.Sprintf("%064x", i+1), fmt.Sprintf("c%05d.png", i),
		)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if err := srv.tagSvc().AddTagToImage(id, parent.ID, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	implied, err := srv.tagSvc().GetOrCreateTag("implied", srv.Active().GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.tagSvc().AddImplication(parent.ID, implied.ID); err != nil {
		t.Fatal(err)
	}

	// Reserve the job slot ourselves (mirroring startImplicationPropagation)
	// and run the worker directly so we own the cancel timing.
	if err := srv.jobs.Start(models.JobTypeTag); err != nil {
		t.Fatalf("reserve job: %v", err)
	}
	done := make(chan struct{})
	go func() {
		srv.runImplicationPropagation(parent.ID, implied.ID, "add")
		close(done)
	}()

	// Wait until the read has finished and the chunk loop is live: chunkedJob
	// sets Total and emits "applying implication 0/5000…" before its first
	// ctx.Err() check. A deadline-bounded Gosched busy-wait, no sleep.
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := srv.jobs.Get()
		if st.Total == carriers && strings.Contains(st.Message, "applying implication") {
			break
		}
		if !st.Running { // worker already returned (unexpected this early)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("chunk loop did not start within 5s")
		}
		runtime.Gosched()
	}
	srv.jobs.Cancel()

	// Drain to completion (deadline-bounded Gosched wait, no sleep).
	drainDeadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-done:
			goto drained
		default:
		}
		if time.Now().After(drainDeadline) {
			t.Fatal("worker did not finish within 5s of cancel")
		}
		runtime.Gosched()
	}
drained:

	st := srv.jobs.Get()
	summary := st.Summary
	if summary == "" {
		summary = st.Message
	}
	m := implSummaryRe.FindStringSubmatch(summary)
	if m == nil {
		t.Fatalf("cancel summary = %q, want shape %q", summary, implSummaryRe.String())
	}
	if st.Error != "" {
		t.Errorf("cancelled run must Complete with a summary, not Fail: error=%q", st.Error)
	}
	// N must be a partial count: a whole-multiple-of-500 boundary strictly
	// below the total (the loop was interrupted, not run to completion).
	processed, _ := strconv.Atoi(m[1])
	total, _ := strconv.Atoi(m[2])
	if total != carriers {
		t.Errorf("summary total = %d, want %d", total, carriers)
	}
	if processed < 0 || processed >= carriers {
		t.Errorf("cancelled processed = %d, want a partial count in [0,%d)", processed, carriers)
	}
	if processed%chunkSizeImplication != 0 {
		t.Errorf("cancelled processed = %d, want a 500-row chunk boundary", processed)
	}
}

// chunkSizeImplication mirrors the const chunkSize inside
// runImplicationPropagation; the cancel always lands on a multiple of it.
const chunkSizeImplication = 500

// TestImplicationsDialogHandler_NonHTMXRedirects pins the HX-Request
// gate: a direct (non-htmx) GET of the dialog URL redirects to the
// tag's row on /tags instead of serving the chrome-less fragment,
// matching duplicatesListHandler. An htmx GET still renders the body.
func TestImplicationsDialogHandler_NonHTMXRedirects(t *testing.T) {
	srv := newTestServer(t)
	tag, err := srv.tagSvc().GetOrCreateTag("anime", srv.Active().GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/tags/%d/implications", tag.ID)

	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("non-htmx GET: code=%d, want %d", w.Code, http.StatusSeeOther)
	}
	if loc := w.Header().Get("Location"); loc != fmt.Sprintf("/tags#tag-row-%d", tag.ID) {
		t.Errorf("redirect Location = %q, want /tags#tag-row-%d", loc, tag.ID)
	}

	hxReq := httptest.NewRequest("GET", path, nil)
	hxReq.Header.Set("HX-Request", "true")
	hxW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(hxW, hxReq)
	if hxW.Code != http.StatusOK {
		t.Fatalf("htmx GET: code=%d, want 200", hxW.Code)
	}
	if !strings.Contains(hxW.Body.String(), "Tags implied by") {
		t.Errorf("htmx GET body missing dialog header")
	}
}
