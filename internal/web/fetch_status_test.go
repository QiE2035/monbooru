package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The detail-page poll re-emits the "fetching" pill while a source fetch is in
// flight and, once the enrich records "ok", tells htmx to reload so the applied
// tags render. The terminal outcome is consumed so a later poll stops.
func TestFetchStatusHandler_PendingThenReloadOnOk(t *testing.T) {
	srv := newTestServer(t)
	g := srv.activeName

	srv.recordFetchStatus(g, 7, "pending", "")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/internal/images/7/fetch-status?n=0", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pending status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("HX-Refresh"); got != "" {
		t.Errorf("pending set HX-Refresh=%q, want empty", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Fetching data") || !strings.Contains(body, "hx-get") {
		t.Errorf("pending body = %q, want the polling pill", body)
	}

	srv.recordFetchStatus(g, 7, "ok", "")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/internal/images/7/fetch-status?n=1", nil))
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Errorf("ok HX-Refresh = %q, want true", rec.Header().Get("HX-Refresh"))
	}
	if !strings.Contains(rec.Header().Get("HX-Trigger"), "monbooru:flash") {
		t.Errorf("ok should emit a monbooru:flash trigger for the reload, got %q", rec.Header().Get("HX-Trigger"))
	}
	if _, ok := srv.loadFetchStatus(g, 7); ok {
		t.Error("ok outcome should be cleared once the poll consumes it")
	}
}

// A fetch that fails on monloader before it can enrich (an unsupported URL, a
// timeout) is reported as a queue error code; the poll maps it to a plain
// inline error and stops polling, never spinning to the poll cap.
func TestFetchStatusHandler_SurfacesReportedFailure(t *testing.T) {
	srv := newTestServer(t)
	g := srv.activeName

	srv.recordFetchStatus(g, 11, "unsupported_url", "no suitable extractor found")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/internal/images/11/fetch-status?n=3", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "flash-err") || !strings.Contains(body, "fetch this source URL") {
		t.Errorf("reported failure body = %q, want a mapped inline error", body)
	}
	if strings.Contains(body, "hx-get") {
		t.Errorf("reported failure should stop polling, got %q", body)
	}
	if _, ok := srv.loadFetchStatus(g, 11); ok {
		t.Error("reported failure should be cleared after the poll consumes it")
	}
}

// A lookup miss renders monloader's per-source trail as a list plus the
// hashes recorded at enqueue time, as a warn (a result, not an error).
func TestFetchStatusHandler_HashNotFoundListsTrailAndHashes(t *testing.T) {
	srv := newTestServer(t)
	g := srv.activeName

	srv.recordFetchLookup(g, 12, "md5 0123abcd")
	srv.recordFetchStatus(g, 12, "hash_not_found", "Public Tag Repository: no match; danbooru: no match")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/internal/images/12/fetch-status?n=1", nil))
	body := rec.Body.String()
	for _, want := range []string{"flash-warn", "No online source found",
		"<div>Public Tag Repository: no match</div>", "<li>danbooru: no match</li>", "Searched md5 0123abcd",
		"compressed or re-encoded", "similarity lookup service",
		`id="fetch-status"`, "hx-swap-oob"} {
		if !strings.Contains(body, want) {
			t.Errorf("miss body = %q, missing %q", body, want)
		}
	}
	if strings.Contains(body, "<li>Public Tag Repository") {
		t.Errorf("miss body = %q, the PTR line should sit apart from the online list", body)
	}
	if strings.Contains(body, "hx-get") {
		t.Errorf("miss body should stop polling, got %q", body)
	}

	// An old monloader reporting no message still gets the plain sentence.
	srv.recordFetchStatus(g, 12, "hash_not_found", "")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/internal/images/12/fetch-status?n=1", nil))
	if body := rec.Body.String(); !strings.Contains(body, "No source found; no tags found.") {
		t.Errorf("empty-trail body = %q, want the fallback sentence", body)
	}
}

// The "set up a similarity lookup service" hint only renders when the trail
// shows no similarity service ran: one that answered is already set up, while
// a credential skip still counts as not set up.
func TestFetchStatusHandler_MissHintOnlyWithoutSimilarity(t *testing.T) {
	srv := newTestServer(t)
	g := srv.activeName

	srv.recordFetchStatus(g, 13, "hash_not_found", "danbooru: no match; iqdb: no match")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/internal/images/13/fetch-status?n=1", nil))
	if body := rec.Body.String(); strings.Contains(body, "similarity lookup service") {
		t.Errorf("miss body = %q, want no hint when iqdb answered", body)
	}

	srv.recordFetchStatus(g, 13, "hash_not_found", "danbooru: no match; saucenao: skipped, needs api key")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/internal/images/13/fetch-status?n=1", nil))
	if body := rec.Body.String(); !strings.Contains(body, "similarity lookup service") {
		t.Errorf("miss body = %q, want the hint when the only similarity entry is a skip", body)
	}
}

// A PTR match with a full online miss renders both parts: the PTR line with
// the reload hint (tags already landed), and the online trail with monloader's
// closest candidates turned into host-labeled links.
func TestFetchStatusHandler_PTRMatchAndCandidateLinks(t *testing.T) {
	srv := newTestServer(t)
	g := srv.activeName

	srv.recordFetchLookup(g, 14, "md5 0123abcd, sha256 4567ef")
	srv.recordFetchStatus(g, 14, "hash_not_found",
		"Public Tag Repository: match, tags applied; danbooru: no match; "+
			"iqdb: no match, closest candidates: https://danbooru.donmai.us/posts/1 (62%), https://yande.re/post/show/2 (58%)")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/internal/images/14/fetch-status?n=1", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"<div>Public Tag Repository: match, tags applied (reload the page to see them)</div>",
		"No online source found",
		"<li>danbooru: no match</li>",
		`<a href="https://danbooru.donmai.us/posts/1" target="_blank" rel="noopener">danbooru.donmai.us</a> (62%)`,
		`<a href="https://yande.re/post/show/2" target="_blank" rel="noopener">yande.re</a> (58%)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %q, missing %q", body, want)
		}
	}
	if strings.Contains(body, "similarity lookup service") {
		t.Errorf("body = %q, want no set-it-up hint when iqdb answered", body)
	}
	if got := rec.Header().Get("HX-Refresh"); got != "" {
		t.Errorf("HX-Refresh = %q, want empty (the reload would wipe the trail)", got)
	}
}

// A recorded hash mismatch surfaces as an inline error and stops the poll,
// instead of reloading or leaving the pill spinning.
func TestFetchStatusHandler_SurfacesMismatch(t *testing.T) {
	srv := newTestServer(t)
	g := srv.activeName

	srv.recordFetchStatus(g, 9, "mismatch", "the source no longer serves this file (hash mismatch); no tags applied")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/internal/images/9/fetch-status?n=2", nil))
	if got := rec.Header().Get("HX-Refresh"); got != "" {
		t.Errorf("mismatch set HX-Refresh=%q, want empty (no reload)", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "flash-err") || !strings.Contains(body, "hash mismatch") {
		t.Errorf("mismatch body = %q, want an inline error flash", body)
	}
	if strings.Contains(body, "hx-get") {
		t.Errorf("mismatch body should stop polling, got %q", body)
	}
	if _, ok := srv.loadFetchStatus(g, 9); ok {
		t.Error("mismatch outcome should be cleared after the poll consumes it")
	}
}

// With nothing in flight the poll clears the slot and stops: an empty body
// with no re-arm attribute.
func TestFetchStatusHandler_NotInFlightStopsPolling(t *testing.T) {
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/internal/images/12/fetch-status?n=1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); strings.Contains(body, "hx-get") || strings.TrimSpace(body) != "" {
		t.Errorf("not-in-flight poll should return an empty non-polling body, got %q", body)
	}
}

// A fetch that never resolves stops nagging at the poll cap.
func TestFetchStatusHandler_PollCapStopsPolling(t *testing.T) {
	srv := newTestServer(t)
	srv.recordFetchStatus(srv.activeName, 12, "pending", "")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/internal/images/12/fetch-status?n=30", nil))
	body := rr.Body.String()
	if strings.Contains(body, "hx-get") {
		t.Errorf("poll at the cap must not re-arm, got %q", body)
	}
	if !strings.Contains(body, "Still fetching") {
		t.Errorf("poll cap should surface the still-fetching warning, got %q", body)
	}
}
