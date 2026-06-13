package web

import (
	"image/color"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/gallery"
)

// promoteCanonical on a gone-file alias must report the refusal as a flash
// (HX-Trigger) and leave the operator on the detail page, not navigate to a
// bare error body, and must not change the canonical.
func TestPromoteCanonical_MissingFileFlashesForHTMX(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	imgPath := filepath.Join(cx.GalleryPath, "pc.png")
	if err := os.WriteFile(imgPath, tinyPNG(t, 8, 8, color.RGBA{1, 2, 3, 255}), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, _, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, imgPath, "png", "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	gone := filepath.Join(cx.GalleryPath, "gone.png")
	if _, err := cx.DB.Write.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`, rec.ID, gone,
	); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"path": {gone}, "_csrf": {srv.csrfToken("anon")}}
	req := httptest.NewRequest("POST", "/images/"+strconv.FormatInt(rec.ID, 10)+"/canonical-path", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (htmx flash, no navigation); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("HX-Trigger"), "monbooru:flash") {
		t.Errorf("HX-Trigger = %q, want a monbooru:flash", w.Header().Get("HX-Trigger"))
	}
	var canon string
	if err := cx.DB.Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, rec.ID).Scan(&canon); err != nil {
		t.Fatal(err)
	}
	if canon != imgPath {
		t.Errorf("canonical_path = %q, want unchanged %q", canon, imgPath)
	}
}
