package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	if body := rr.Body.String(); !strings.Contains(body, "connected to monloader") || !strings.Contains(body, "v1.2.3") {
		t.Errorf("light should show connected + version, got %q", body)
	}

	// Unpaired: the light stops polling and clears.
	srv.cfg.Auth.Tokens = nil
	rr2 := httptest.NewRecorder()
	srv.monloaderStatusHandler(rr2, httptest.NewRequest("GET", "/internal/monloader-status", nil))
	if strings.Contains(rr2.Body.String(), "hx-get") {
		t.Errorf("unpaired light must not poll, got %q", rr2.Body.String())
	}
}
