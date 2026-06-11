package api

import (
	"archive/zip"
	"bytes"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/gallery"
)

// createTestManga ingests a tiny one-page cbz under the test env's
// gallery and returns the new image id.
func (e *testEnv) createTestManga(t *testing.T, name string) int64 {
	t.Helper()
	pic := image.NewRGBA(image.Rect(0, 0, 8, 8))
	cbzPath := filepath.Join(e.galleryDir, name)
	f, err := os.Create(cbzPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("01.png")
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := png.Encode(w, pic); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	rec, _, err := gallery.Ingest(e.database, e.galleryDir, e.thumbDir, cbzPath, "cbz", "")
	if err != nil {
		t.Fatalf("ingest cbz: %v", err)
	}
	return rec.ID
}

func TestAPI_PostImages_AcceptsCBZUpload(t *testing.T) {
	env := newTestEnv(t)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 1; i <= 3; i++ {
		w, _ := zw.Create("page_" + strconv.Itoa(i) + ".png")
		pic := image.NewRGBA(image.Rect(0, 0, 4, 4))
		if err := png.Encode(w, pic); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, err := mw.CreateFormFile("file", "test.cbz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(fw, &buf); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/images", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	for _, frag := range []string{
		`"file_type":"cbz"`,
		`"page_count":3`,
	} {
		if !strings.Contains(rec.Body.String(), frag) {
			t.Errorf("response missing %q; body = %s", frag, rec.Body.String())
		}
	}
}

// The UI tag input accepts manga rows, so the API behaves the same:
// POST /api/v1/images/{id}/tags on a cbz id returns 200.
func TestAPI_AddTags_AcceptsMangaID(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestManga(t, "m.cbz")
	body := strings.NewReader(`{"tags":["manga_demo"]}`)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/images/"+strconv.FormatInt(id, 10)+"/tags", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("POST tags on manga = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAPI_RemoveTags_AcceptsMangaID(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestManga(t, "m.cbz")
	body := strings.NewReader(`{"tags":["1girl"]}`)
	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/images/"+strconv.FormatInt(id, 10)+"/tags", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("DELETE tags on manga = %d, want 200 (no-op for missing tag); body = %s", rec.Code, rec.Body.String())
	}
}

func TestAPI_GetImage_ReturnsMangaFields(t *testing.T) {
	env := newTestEnv(t)
	id := env.createTestManga(t, "m.cbz")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/"+strconv.FormatInt(id, 10), nil)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, frag := range []string{
		`"file_type":"cbz"`,
		`"page_count":1`,
		`"collection":""`,
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("response missing %q; body = %s", frag, body)
		}
	}
}
