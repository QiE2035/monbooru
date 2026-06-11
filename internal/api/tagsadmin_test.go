package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// apiJSON issues a method+path request with a JSON body and asserts the
// status, returning the decoded response object (empty for 204).
func apiJSON(t *testing.T, env *testEnv, method, path string, body map[string]any, wantStatus int) map[string]any {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("%s %s: expected %d, got %d: %s", method, path, wantStatus, w.Code, w.Body.String())
	}
	var resp map[string]any
	if w.Body.Len() > 0 {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
	}
	return resp
}

func TestCreateTag(t *testing.T) {
	env := newTestEnv(t)
	resp := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "newtag", "category": "artist"}, http.StatusCreated)
	if resp["name"] != "newtag" {
		t.Errorf("name = %v, want newtag", resp["name"])
	}
	if resp["category"] != "artist" {
		t.Errorf("category = %v, want artist", resp["category"])
	}
}

func TestCreateTag_DefaultsToGeneral(t *testing.T) {
	env := newTestEnv(t)
	resp := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "plaintag"}, http.StatusCreated)
	if resp["category"] != "general" {
		t.Errorf("category = %v, want general", resp["category"])
	}
}

func TestCreateTag_UnknownCategory(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "x", "category": "nope_xyz"}, http.StatusBadRequest)
}

func TestCreateTag_MissingName(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{}, http.StatusBadRequest)
}

func TestPatchTag_Rename(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "before"}, http.StatusCreated)
	id := int64(created["id"].(float64))

	resp := apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/tags/%d", id), map[string]any{"name": "after"}, http.StatusOK)
	if resp["name"] != "after" {
		t.Errorf("name = %v, want after", resp["name"])
	}
}

func TestPatchTag_ChangeCategory(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "movable"}, http.StatusCreated)
	id := int64(created["id"].(float64))

	resp := apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/tags/%d", id), map[string]any{"category": "character"}, http.StatusOK)
	if resp["category"] != "character" {
		t.Errorf("category = %v, want character", resp["category"])
	}
}

func TestPatchTag_NoFields(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "x"}, http.StatusCreated)
	id := int64(created["id"].(float64))
	apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/tags/%d", id), map[string]any{}, http.StatusBadRequest)
}

func TestPatchTag_NotFound(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "PATCH", "/api/v1/tags/99999", map[string]any{"name": "x"}, http.StatusNotFound)
}

func TestDeleteTag(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "doomed"}, http.StatusCreated)
	id := int64(created["id"].(float64))

	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/tags/%d", id), nil, http.StatusNoContent)
	// A second delete finds nothing.
	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/tags/%d", id), nil, http.StatusNotFound)
}

func TestCreateAlias(t *testing.T) {
	env := newTestEnv(t)
	canon := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "canonical"}, http.StatusCreated)
	canonID := int64(canon["id"].(float64))

	resp := apiJSON(t, env, "POST", "/api/v1/tags/aliases", map[string]any{"name": "synonym", "canonical_id": canonID}, http.StatusCreated)
	if resp["is_alias"] != true {
		t.Errorf("is_alias = %v, want true", resp["is_alias"])
	}
	if resp["name"] != "synonym" {
		t.Errorf("name = %v, want synonym", resp["name"])
	}
}

func TestCreateAlias_MissingCanonical(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "POST", "/api/v1/tags/aliases", map[string]any{"name": "synonym"}, http.StatusBadRequest)
}

func TestMergeTags(t *testing.T) {
	env := newTestEnv(t)
	a := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "from_tag"}, http.StatusCreated)
	b := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "into_tag"}, http.StatusCreated)
	aID := int64(a["id"].(float64))
	bID := int64(b["id"].(float64))

	resp := apiJSON(t, env, "POST", "/api/v1/tags/merge", map[string]any{"alias_id": aID, "canonical_id": bID}, http.StatusOK)
	if int64(resp["id"].(float64)) != bID {
		t.Errorf("merge returned id %v, want canonical %d", resp["id"], bID)
	}
	if resp["is_alias"] != false {
		t.Errorf("canonical is_alias = %v, want false", resp["is_alias"])
	}

	// The source tag is now an alias pointing at the canonical.
	var isAlias int
	var canonID int64
	if err := env.database.Read.QueryRow(
		`SELECT is_alias, COALESCE(canonical_tag_id, 0) FROM tags WHERE id = ?`, aID,
	).Scan(&isAlias, &canonID); err != nil {
		t.Fatal(err)
	}
	if isAlias != 1 || canonID != bID {
		t.Errorf("source tag is_alias=%d canonical=%d, want 1 and %d", isAlias, canonID, bID)
	}
}

func TestMergeTags_SelfRejected(t *testing.T) {
	env := newTestEnv(t)
	a := apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "lonely"}, http.StatusCreated)
	aID := int64(a["id"].(float64))
	apiJSON(t, env, "POST", "/api/v1/tags/merge", map[string]any{"alias_id": aID, "canonical_id": aID}, http.StatusBadRequest)
}
