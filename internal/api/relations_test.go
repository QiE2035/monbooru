package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestRelationsAddAndFetch(t *testing.T) {
	env := newTestEnv(t)
	mux := env.mux
	a := env.createTestImage(t, "a.png", 10, 10)
	b := env.createTestImage(t, "b.png", 11, 11) // different shape → different SHA

	body, _ := json.Marshal(map[string]any{
		"type": "duplicate",
		"a":    a,
		"b":    b,
	})
	req := httptest.NewRequest("POST", "/api/v1/relations", bytes.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /relations: got %d %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("GET", "/api/v1/images/"+strconv.FormatInt(a, 10)+"/relations", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET relations: got %d %s", w.Code, w.Body.String())
	}
	var resp relationsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.DuplicateGroup == nil {
		t.Fatal("expected duplicate_group in response")
	}
	if len(resp.DuplicateGroup.Members) != 2 {
		t.Fatalf("members = %d, want 2", len(resp.DuplicateGroup.Members))
	}
}

func TestRelationsAddConflict(t *testing.T) {
	env := newTestEnv(t)
	mux := env.mux
	a := env.createTestImage(t, "a.png", 10, 10)
	b := env.createTestImage(t, "b.png", 11, 11) // different shape → different SHA

	post := func(typ string) int {
		body, _ := json.Marshal(map[string]any{"type": typ, "a": a, "b": b})
		req := httptest.NewRequest("POST", "/api/v1/relations", bytes.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}
	if post("duplicate") != http.StatusCreated {
		t.Fatal("first POST should succeed")
	}
	if got := post("alternate"); got != http.StatusConflict {
		t.Fatalf("conflicting POST: got %d, want 409", got)
	}
}

func TestRelationsRemove(t *testing.T) {
	env := newTestEnv(t)
	mux := env.mux
	a := env.createTestImage(t, "a.png", 10, 10)
	b := env.createTestImage(t, "b.png", 11, 11) // different shape → different SHA

	body, _ := json.Marshal(map[string]any{"type": "duplicate", "a": a, "b": b})
	req := httptest.NewRequest("POST", "/api/v1/relations", bytes.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("setup add: %d", w.Code)
	}

	remBody, _ := json.Marshal(map[string]any{"type": "duplicate", "image_id": a})
	req = httptest.NewRequest("DELETE", "/api/v1/relations", bytes.NewReader(remBody))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: got %d %s", w.Code, w.Body.String())
	}
	// b should no longer be in a dup group.
	req = httptest.NewRequest("GET", "/api/v1/images/"+strconv.FormatInt(b, 10)+"/relations", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var resp relationsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.DuplicateGroup != nil {
		t.Fatalf("dup group survived after only-pair removal: %+v", resp.DuplicateGroup)
	}
}

func TestRelationsAddSelfRefused(t *testing.T) {
	env := newTestEnv(t)
	mux := env.mux
	a := env.createTestImage(t, "a.png", 10, 10)
	body, _ := json.Marshal(map[string]any{"type": "duplicate", "a": a, "b": a})
	req := httptest.NewRequest("POST", "/api/v1/relations", bytes.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("self-relation: got %d, want 400", w.Code)
	}
}

func TestImageGetIncludesPhashField(t *testing.T) {
	env := newTestEnv(t)
	mux := env.mux
	id := env.createTestImage(t, "phash.png", 32, 32)
	req := httptest.NewRequest("GET", "/api/v1/images/"+strconv.FormatInt(id, 10), nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get image: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"phash"`) {
		t.Errorf("image response missing phash field: %s", w.Body.String())
	}
}
