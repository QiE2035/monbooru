//go:build tagger

package tagger

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// solidPNG is a minimal RGB PNG used by the cbz frame test. Mirrors the
// gallery package's helper so the test stays self-contained.
func solidPNG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func makeCBZ(t *testing.T, dir, name string, pages [][]byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create cbz: %v", err)
	}
	zw := zip.NewWriter(f)
	for i, body := range pages {
		w, err := zw.Create(filepath.Join("p", fmt.Sprintf("%03d.png", i+1)))
		if err != nil {
			f.Close()
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write(body); err != nil {
			f.Close()
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		t.Fatalf("zip close: %v", err)
	}
	f.Close()
	return path
}

func TestFramesForTagging_CBZIteratesEveryPage(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 8, 8, color.RGBA{0, 0, 0, 255})
	cbz := makeCBZ(t, dir, "m.cbz", [][]byte{pic, pic, pic, pic, pic})

	cacheRoot := t.TempDir()
	paths, cleanup := framesForTagging(cbz, "cbz", cacheRoot, 42)
	defer cleanup()

	if len(paths) != 5 {
		t.Fatalf("got %d frame paths, want 5", len(paths))
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("frame %q not on disk: %v", p, err)
		}
	}
}

func TestFramesForTagging_CBZUsesCacheRoot(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 8, 8, color.RGBA{0, 0, 0, 255})
	cbz := makeCBZ(t, dir, "m.cbz", [][]byte{pic, pic})
	cacheRoot := t.TempDir()
	paths, cleanup := framesForTagging(cbz, "cbz", cacheRoot, 7)
	defer cleanup()
	if len(paths) != 2 {
		t.Fatalf("got %d frames, want 2", len(paths))
	}
	for _, p := range paths {
		// Every cached path lives under cacheRoot/<id>/page_*.
		if rel, err := filepath.Rel(cacheRoot, p); err != nil || rel == "" || rel[0] == '.' && rel[1] == '.' {
			t.Errorf("path %q is not under cacheRoot %q", p, cacheRoot)
		}
	}
}

func TestFramesForTagging_StaticImageUnchanged(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "x.png")
	if err := os.WriteFile(src, solidPNG(t, 4, 4, color.RGBA{0, 0, 0, 255}), 0o644); err != nil {
		t.Fatal(err)
	}
	paths, cleanup := framesForTagging(src, "png", "", 1)
	defer cleanup()
	if len(paths) != 1 || paths[0] != src {
		t.Errorf("static branch = %v, want [%q]", paths, src)
	}
}
