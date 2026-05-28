package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTag creates a tag via the API and returns its id.
func newTag(t *testing.T, env *testEnv, name string) int64 {
	t.Helper()
	resp := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": name}, http.StatusCreated)
	return int64(resp["id"].(float64))
}

// getImplications fetches the implication list for a parent tag.
func getImplications(t *testing.T, env *testEnv, parentID int64, wantStatus int) []map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/tags/%d/implications", parentID), nil)
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("GET implications: expected %d, got %d: %s", wantStatus, w.Code, w.Body.String())
	}
	var out []map[string]any
	if w.Body.Len() > 0 {
		json.NewDecoder(w.Body).Decode(&out)
	}
	return out
}

func TestAddAndListImplications(t *testing.T) {
	env := newTestEnv(t)
	parent := newTag(t, env, "cat")
	child := newTag(t, env, "animal")

	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", parent),
		map[string]any{"implied_id": child}, http.StatusCreated)

	// Re-declaring the same edge is an idempotent no-op (200, not 201).
	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", parent),
		map[string]any{"implied_id": child}, http.StatusOK)

	imps := getImplications(t, env, parent, http.StatusOK)
	if len(imps) != 1 {
		t.Fatalf("expected 1 implication, got %d", len(imps))
	}
	if int64(imps[0]["implied_id"].(float64)) != child {
		t.Errorf("implied_id = %v, want %d", imps[0]["implied_id"], child)
	}
	if imps[0]["implied_name"] != "animal" {
		t.Errorf("implied_name = %v, want animal", imps[0]["implied_name"])
	}
}

func TestAddImplication_MissingImplied(t *testing.T) {
	env := newTestEnv(t)
	parent := newTag(t, env, "cat")
	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", parent),
		map[string]any{}, http.StatusBadRequest)
}

func TestAddImplication_Self(t *testing.T) {
	env := newTestEnv(t)
	parent := newTag(t, env, "cat")
	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", parent),
		map[string]any{"implied_id": parent}, http.StatusBadRequest)
}

func TestAddImplication_Cycle(t *testing.T) {
	env := newTestEnv(t)
	a := newTag(t, env, "cat")
	b := newTag(t, env, "animal")

	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", a),
		map[string]any{"implied_id": b}, http.StatusCreated)
	// b -> a would close a cycle.
	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", b),
		map[string]any{"implied_id": a}, http.StatusConflict)
}

func TestListImplications_ParentNotFound(t *testing.T) {
	env := newTestEnv(t)
	getImplications(t, env, 99999, http.StatusNotFound)
}

func TestRemoveImplication(t *testing.T) {
	env := newTestEnv(t)
	parent := newTag(t, env, "cat")
	child := newTag(t, env, "animal")
	apiJSON(t, env, "POST", fmt.Sprintf("/api/v1/tags/%d/implications", parent),
		map[string]any{"implied_id": child}, http.StatusCreated)

	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/tags/%d/implications/%d", parent, child), nil, http.StatusNoContent)

	if imps := getImplications(t, env, parent, http.StatusOK); len(imps) != 0 {
		t.Errorf("expected 0 implications after delete, got %d", len(imps))
	}
	// Removing a non-existent edge is a 404.
	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/tags/%d/implications/%d", parent, child), nil, http.StatusNotFound)
}
