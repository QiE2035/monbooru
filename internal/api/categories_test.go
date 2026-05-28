package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func getCategories(t *testing.T, env *testEnv) []map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/categories", nil)
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET categories: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out []map[string]any
	json.NewDecoder(w.Body).Decode(&out)
	return out
}

// categoryID returns the id of the category with the given name.
func categoryID(t *testing.T, env *testEnv, name string) int64 {
	t.Helper()
	var id int64
	if err := env.database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatalf("lookup category %q: %v", name, err)
	}
	return id
}

func TestListCategories(t *testing.T) {
	env := newTestEnv(t)
	cats := getCategories(t, env)
	if len(cats) == 0 {
		t.Fatal("expected built-in categories")
	}
	var general map[string]any
	for _, c := range cats {
		if c["name"] == "general" {
			general = c
		}
	}
	if general == nil {
		t.Fatal("general category missing")
	}
	if general["is_builtin"] != true {
		t.Errorf("general is_builtin = %v, want true", general["is_builtin"])
	}
}

func TestCreateCategory(t *testing.T) {
	env := newTestEnv(t)
	resp := apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "mood", "color": "#abc"}, http.StatusCreated)
	if resp["name"] != "mood" {
		t.Errorf("name = %v, want mood", resp["name"])
	}
	if resp["color"] != "#abc" {
		t.Errorf("color = %v, want #abc", resp["color"])
	}
	if resp["is_builtin"] != false {
		t.Errorf("is_builtin = %v, want false", resp["is_builtin"])
	}
}

func TestCreateCategory_DefaultColor(t *testing.T) {
	env := newTestEnv(t)
	resp := apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "vibe"}, http.StatusCreated)
	if resp["color"] != "#888888" {
		t.Errorf("color = %v, want #888888", resp["color"])
	}
}

func TestCreateCategory_Duplicate(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "general"}, http.StatusConflict)
}

func TestCreateCategory_Reserved(t *testing.T) {
	env := newTestEnv(t)
	// "source" is a search-filter keyword, refused as a category name.
	apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "source"}, http.StatusBadRequest)
}

func TestCreateCategory_InvalidName(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "Bad Name!"}, http.StatusBadRequest)
}

func TestPatchCategory_Recolor(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "tone"}, http.StatusCreated)
	id := int64(created["id"].(float64))
	resp := apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/categories/%d", id), map[string]any{"color": "#123456"}, http.StatusOK)
	if resp["color"] != "#123456" {
		t.Errorf("color = %v, want #123456", resp["color"])
	}
}

func TestPatchCategory_Rename(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "oldcat"}, http.StatusCreated)
	id := int64(created["id"].(float64))
	resp := apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/categories/%d", id), map[string]any{"name": "newcat"}, http.StatusOK)
	if resp["name"] != "newcat" {
		t.Errorf("name = %v, want newcat", resp["name"])
	}
}

func TestPatchCategory_BuiltinRenameRejected(t *testing.T) {
	env := newTestEnv(t)
	id := categoryID(t, env, "general")
	apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/categories/%d", id), map[string]any{"name": "renamed_general"}, http.StatusBadRequest)
}

func TestPatchCategory_BuiltinRecolorOK(t *testing.T) {
	env := newTestEnv(t)
	id := categoryID(t, env, "general")
	apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/categories/%d", id), map[string]any{"color": "#020202"}, http.StatusOK)
}

func TestPatchCategory_BadColor(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "hue"}, http.StatusCreated)
	id := int64(created["id"].(float64))
	apiJSON(t, env, "PATCH", fmt.Sprintf("/api/v1/categories/%d", id), map[string]any{"color": "notacolor"}, http.StatusBadRequest)
}

func TestPatchCategory_NotFound(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "PATCH", "/api/v1/categories/99999", map[string]any{"name": "x"}, http.StatusNotFound)
}

func TestDeleteCategory_MoveReparentsTags(t *testing.T) {
	env := newTestEnv(t)
	created := apiJSON(t, env, "POST", "/api/v1/categories", map[string]any{"name": "doomedcat"}, http.StatusCreated)
	catID := int64(created["id"].(float64))
	// A tag living in the doomed category.
	apiJSON(t, env, "POST", "/api/v1/tags", map[string]any{"name": "orphan", "category": "doomedcat"}, http.StatusCreated)

	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/categories/%d", catID), map[string]any{"action": "move"}, http.StatusNoContent)

	// The tag now lives in general (the default move target).
	var movedTo int64
	if err := env.database.Read.QueryRow(`SELECT category_id FROM tags WHERE name = 'orphan'`).Scan(&movedTo); err != nil {
		t.Fatal(err)
	}
	if movedTo != categoryID(t, env, "general") {
		t.Errorf("orphan tag category = %d, want general", movedTo)
	}
}

func TestDeleteCategory_BuiltinRejected(t *testing.T) {
	env := newTestEnv(t)
	id := categoryID(t, env, "general")
	apiJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/categories/%d", id), nil, http.StatusBadRequest)
}

func TestDeleteCategory_NotFound(t *testing.T) {
	env := newTestEnv(t)
	apiJSON(t, env, "DELETE", "/api/v1/categories/99999", nil, http.StatusNotFound)
}
