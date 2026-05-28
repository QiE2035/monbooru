package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListGalleries(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "g.png", 10, 10)
	body, _ := json.Marshal(map[string]any{"tags": []string{"a_tag"}})
	addReq := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/images/%d/tags", id), bytes.NewReader(body))
	addReq.Header.Set("Content-Type", "application/json")
	env.mux.ServeHTTP(httptest.NewRecorder(), addReq)

	req := httptest.NewRequest("GET", "/api/v1/galleries", nil)
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out []map[string]any
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 gallery, got %d", len(out))
	}
	g := out[0]
	if g["name"] != env.cfg.DefaultGallery {
		t.Errorf("name = %v, want %q", g["name"], env.cfg.DefaultGallery)
	}
	if g["active"] != true {
		t.Errorf("active = %v, want true", g["active"])
	}
	if g["images"] != float64(1) {
		t.Errorf("images = %v, want 1", g["images"])
	}
	if tags, _ := g["tags"].(float64); tags < 1 {
		t.Errorf("tags = %v, want >= 1", g["tags"])
	}
}
