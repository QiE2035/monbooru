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
	if body := rec.Body.String(); !strings.Contains(body, "Fetching tags") || !strings.Contains(body, "hx-get") {
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
