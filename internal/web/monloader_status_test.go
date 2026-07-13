package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leqwin/monbooru/internal/config"
)

func TestMonloaderStatusLight(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"version":"v1.2.3"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer stub.Close()

	srv := newTestServer(t)
	tok, _ := config.GenerateToken("monloader (paired)", config.AllScopes)
	tok.Paired = "monloader"
	srv.cfg.Auth.Tokens = []config.Token{tok}
	srv.cfg.Monloader.APIURL = stub.URL
	rr := httptest.NewRecorder()
	srv.monloaderStatusHandler(rr, httptest.NewRequest("GET", "/internal/monloader-status", nil))
	if body := rr.Body.String(); !strings.Contains(body, "connected to ") || !strings.Contains(body, ">monloader</a>") || !strings.Contains(body, "v1.2.3") {
		t.Errorf("light should show connected + linked monloader + version, got %q", body)
	}

	// Unpaired: the light stops polling and clears.
	srv.cfg.Auth.Tokens = nil
	rr2 := httptest.NewRecorder()
	srv.monloaderStatusHandler(rr2, httptest.NewRequest("GET", "/internal/monloader-status", nil))
	if strings.Contains(rr2.Body.String(), "hx-get") {
		t.Errorf("unpaired light must not poll, got %q", rr2.Body.String())
	}
}

// A full page seeds its footer light from the last cached probe, so the light
// shows its known state at once instead of flickering to "checking" (and
// re-probing) on every navigation.
func TestMonloaderLightSeedsCachedStatus(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"version":"v1.2.3"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer stub.Close()

	srv := newTestServer(t)
	tok, _ := config.GenerateToken("monloader (paired)", config.AllScopes)
	tok.Paired = "monloader"
	srv.cfg.Auth.Tokens = []config.Token{tok}
	srv.cfg.Monloader.APIURL = stub.URL

	page := func() string {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		return w.Body.String()
	}

	// Cold cache: the initial render seeds the "checking" shell.
	if !strings.Contains(page(), "footer-dot-checking") {
		t.Fatal("cold cache should seed the checking shell")
	}

	// The poll warms the cache; the next page seeds the connected state.
	srv.monloaderStatusHandler(httptest.NewRecorder(), httptest.NewRequest("GET", "/internal/monloader-status", nil))
	if body := page(); !strings.Contains(body, "footer-dot-ok") {
		t.Errorf("warm cache should seed the connected light, got %q", body)
	}
}

// The footer light links the word "monloader" to the monloader queue, using the
// configured web url and falling back to the api url; the poll endpoint that
// re-renders the light carries the link so it survives the swap.
func TestMonloaderFooterLink(t *testing.T) {
	srv := newTestServer(t)
	tok, _ := config.GenerateToken("monloader (paired)", config.AllScopes)
	tok.Paired = "monloader"
	srv.cfg.Auth.Tokens = []config.Token{tok}

	page := func() string {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		return w.Body.String()
	}
	poll := func() string {
		w := httptest.NewRecorder()
		srv.monloaderStatusHandler(w, httptest.NewRequest("GET", "/internal/monloader-status", nil))
		return w.Body.String()
	}

	srv.cfg.Server.MonloaderURL = "http://localhost:8081/"
	link := `<a href="http://localhost:8081/queue" target="_blank" rel="noopener">monloader</a>`
	if !strings.Contains(page(), link) {
		t.Error("full-page footer should link the monloader word to the queue")
	}
	if !strings.Contains(poll(), link) {
		t.Error("poll endpoint should carry the footer link")
	}

	srv.cfg.Server.MonloaderURL = ""
	srv.cfg.Monloader.APIURL = "http://monloader:8081"
	if !strings.Contains(poll(), `<a href="http://monloader:8081/queue" target="_blank" rel="noopener">monloader</a>`) {
		t.Error("footer link should fall back to the api url")
	}

	srv.cfg.Monloader.APIURL = ""
	if out := poll(); strings.Contains(out, "<a ") {
		t.Errorf("footer word should be plain when no monloader url is set, got %q", out)
	}
}

// The footer-light probe also reads monloader's PTR capability, so the
// PTR-backed lookup controls can gate on the cached flag without a poll of
// their own.
func TestMonloaderProbeReadsPTRCapability(t *testing.T) {
	enabled := false
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"version":"v1"}`))
		case "/api/v1/ptr/status":
			if enabled {
				_, _ = w.Write([]byte(`{"enabled":true,"state":"ready"}`))
			} else {
				_, _ = w.Write([]byte(`{"enabled":false,"state":"disabled"}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer stub.Close()

	srv := newTestServer(t)
	tok, _ := config.GenerateToken("monloader (paired)", config.AllScopes)
	tok.Paired = "monloader"
	srv.cfg.Auth.Tokens = []config.Token{tok}
	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	srv.monloaderStatusHandler(httptest.NewRecorder(), httptest.NewRequest("GET", "/internal/monloader-status", nil))
	if _, _, ptr, _ := srv.monloaderStatusSeed(); ptr {
		t.Error("disabled PTR must seed false")
	}

	enabled = true
	srv.monloaderCheckedAt = time.Time{} // expire the cache so the next poll re-probes
	srv.monloaderStatusHandler(httptest.NewRecorder(), httptest.NewRequest("GET", "/internal/monloader-status", nil))
	if _, _, ptr, _ := srv.monloaderStatusSeed(); !ptr {
		t.Error("enabled PTR must seed true after the poll re-probes")
	}
}

// EnqueueMetadataFetch is the one outbound call both fetch entry points share:
// it must hit monloader's metadata endpoint with the bearer token and the
// (image, gallery, url) payload, and fail cleanly when unconfigured or refused.
func TestEnqueueMetadataFetch(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b := make([]byte, 512)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.WriteHeader(http.StatusAccepted)
	}))
	defer stub.Close()

	srv := newTestServer(t)
	if err := srv.EnqueueMetadataFetch(t.Context(), 7, "default", "https://d/1"); err == nil {
		t.Error("unconfigured enqueue should fail")
	}

	srv.cfg.Monloader.APIURL = stub.URL
	srv.cfg.Monloader.APIToken = "peer-secret"
	if err := srv.EnqueueMetadataFetch(t.Context(), 7, "default", "https://d/1"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if gotPath != "/api/v1/metadata" || gotAuth != "Bearer peer-secret" {
		t.Errorf("request = %s auth=%q, want /api/v1/metadata with the bearer", gotPath, gotAuth)
	}
	for _, want := range []string{`"image_id":7`, `"gallery":"default"`, `"url":"https://d/1"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("payload %q missing %q", gotBody, want)
		}
	}

	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer refuse.Close()
	srv.cfg.Monloader.APIURL = refuse.URL
	if err := srv.EnqueueMetadataFetch(t.Context(), 7, "default", "https://d/1"); err == nil {
		t.Error("a refused enqueue should surface an error")
	}
}
