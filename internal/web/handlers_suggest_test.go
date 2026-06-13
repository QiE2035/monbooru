package web

import (
	"fmt"
	"image/color"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/gallery"
)

func TestFoldersSuggest_CaseBoundary(t *testing.T) {
	srv := newTestServer(t)
	cx := srv.Active()
	folders := []string{"Zoo/lions", "zebra/herd", "Foo/Bar", "fooBaz", "Az/one", "aZ/two"}
	for i, fp := range folders {
		p := filepath.Join(cx.GalleryPath, fmt.Sprintf("f%d.png", i))
		if err := os.WriteFile(p, tinyPNG(t, 8, 8, color.RGBA{byte(i + 1), 0, 0, 255}), 0o644); err != nil {
			t.Fatal(err)
		}
		rec, _, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, p, "png", "")
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if _, err := cx.DB.Write.Exec(`UPDATE images SET folder_path=? WHERE id=?`, fp, rec.ID); err != nil {
			t.Fatal(err)
		}
	}
	body := func(prefix string) string {
		req := httptest.NewRequest("GET", "/internal/folders/suggest?prefix="+url.QueryEscape(prefix), nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w.Body.String()
	}
	for _, tc := range []struct{ prefix, want string }{
		{"zoo", "Zoo/lions"},
		{"ZEBRA", "zebra/herd"},
		{"foo", "Foo/Bar"},
		{"foo", "fooBaz"},
		{"az", "Az/one"},
		{"AZ", "aZ/two"},
		{"Z", "zebra/herd"},
		{"z", "Zoo/lions"},
	} {
		if got := body(tc.prefix); !strings.Contains(got, tc.want) {
			t.Errorf("prefix %q: %q missing from suggestions:\n%s", tc.prefix, tc.want, got)
		}
	}
}
