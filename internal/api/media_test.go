package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func getMedia(t *testing.T, env *testEnv, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	return w
}

func TestServeImageFile(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "bytes.png", 12, 12)

	w := getMedia(t, env, fmt.Sprintf("/api/v1/images/%d/file", id))
	if w.Code != http.StatusOK {
		t.Fatalf("file: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.Len() == 0 {
		t.Error("file: expected non-empty body")
	}

	w = getMedia(t, env, "/api/v1/images/99999/file")
	if w.Code != http.StatusNotFound {
		t.Errorf("file missing id: expected 404, got %d", w.Code)
	}
}

func TestServeThumbnail(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "thumb.png", 12, 12)

	w := getMedia(t, env, fmt.Sprintf("/api/v1/images/%d/thumbnail", id))
	if w.Code != http.StatusOK {
		t.Fatalf("thumbnail: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.Len() == 0 {
		t.Error("thumbnail: expected non-empty body")
	}

	w = getMedia(t, env, "/api/v1/images/99999/thumbnail")
	if w.Code != http.StatusNotFound {
		t.Errorf("thumbnail missing id: expected 404, got %d", w.Code)
	}
}

func TestServeMangaPage(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestManga(t, "pages.cbz")

	w := getMedia(t, env, fmt.Sprintf("/api/v1/images/%d/page/1", id))
	if w.Code != http.StatusOK {
		t.Fatalf("page 1: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.Len() == 0 {
		t.Error("page 1: expected non-empty body")
	}

	w = getMedia(t, env, fmt.Sprintf("/api/v1/images/%d/page/1/thumb", id))
	if w.Code != http.StatusOK {
		t.Fatalf("page 1 thumb: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Page 0 is out of the 1-based range.
	w = getMedia(t, env, fmt.Sprintf("/api/v1/images/%d/page/0", id))
	if w.Code != http.StatusBadRequest {
		t.Errorf("page 0: expected 400, got %d", w.Code)
	}
}

// A non-cbz row has no pages; the per-page route must 404 rather than
// try to open it as an archive.
func TestServeMangaPage_NotManga(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestImage(t, "still.png", 10, 10)

	w := getMedia(t, env, fmt.Sprintf("/api/v1/images/%d/page/1", id))
	if w.Code != http.StatusNotFound {
		t.Errorf("page on non-manga: expected 404, got %d: %s", w.Code, w.Body.String())
	}
}
