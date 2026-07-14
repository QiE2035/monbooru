package gallery

import (
	"archive/zip"
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// solidPNG returns a tiny opaque PNG suitable for stuffing into test
// archives. Hand-rolled so the gallery package's tests don't need the
// playwright fixture machinery.
func solidPNG(t *testing.T, w, h int, rgb [3]uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	col := color.RGBA{rgb[0], rgb[1], rgb[2], 0xff}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, col)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func writeTestZip(t *testing.T, dir, name string, entries map[string][]byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	for k, v := range entries {
		w, err := zw.Create(k)
		if err != nil {
			t.Fatalf("zip create %q: %v", k, err)
		}
		if _, err := w.Write(v); err != nil {
			t.Fatalf("zip write %q: %v", k, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return path
}

func TestNaturalLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"page1.jpg", "page2.jpg", true},
		{"page2.jpg", "page10.jpg", true},
		{"page10.jpg", "page2.jpg", false},
		{"page001.jpg", "page2.jpg", true},
		{"a.jpg", "b.jpg", true},
		{"chapter1/01.jpg", "chapter1/10.jpg", true},
		{"chapter1/10.jpg", "chapter2/01.jpg", true},
	}
	for _, c := range cases {
		if got := naturalLess(c.a, c.b); got != c.want {
			t.Errorf("naturalLess(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestOpenManga_PageListNaturalSort(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{1, 2, 3})
	path := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"10.png": pic, "2.png": pic, "1.png": pic, "100.png": pic,
	})
	m, err := OpenManga(path)
	if err != nil {
		t.Fatalf("OpenManga: %v", err)
	}
	defer func() { _ = m.Close() }()
	want := []string{"1.png", "2.png", "10.png", "100.png"}
	if len(m.Pages) != len(want) {
		t.Fatalf("page count = %d, want %d", len(m.Pages), len(want))
	}
	for i, p := range m.Pages {
		if p.Path != want[i] {
			t.Errorf("page[%d] = %q, want %q", i, p.Path, want[i])
		}
	}
}

func TestOpenManga_FilterMacOSX(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{0, 0, 0})
	path := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"01.png":            pic,
		"__MACOSX/._01.png": []byte("garbage"),
		".DS_Store":         {},
		"Thumbs.db":         {},
		"chapter/02.png":    pic,
	})
	m, err := OpenManga(path)
	if err != nil {
		t.Fatalf("OpenManga: %v", err)
	}
	defer func() { _ = m.Close() }()
	if len(m.Pages) != 2 {
		var names []string
		for _, p := range m.Pages {
			names = append(names, p.Path)
		}
		t.Fatalf("got %d pages %v, want 2 (MacOSX/junk filtered)", len(m.Pages), names)
	}
}

func TestOpenManga_DeepSubfolders(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{0, 0, 0})
	path := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"chapter1/01.jpg": pic,
		"chapter1/02.jpg": pic,
		"chapter2/01.jpg": pic,
	})
	m, err := OpenManga(path)
	if err != nil {
		t.Fatalf("OpenManga: %v", err)
	}
	defer func() { _ = m.Close() }()
	if len(m.Pages) != 3 {
		t.Fatalf("got %d pages, want 3", len(m.Pages))
	}
	if m.Pages[0].Path != "chapter1/01.jpg" || m.Pages[2].Path != "chapter2/01.jpg" {
		t.Errorf("unexpected order: %v / %v", m.Pages[0].Path, m.Pages[2].Path)
	}
}

func TestOpenManga_EmptyArchiveRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeTestZip(t, dir, "empty.cbz", map[string][]byte{
		"README.txt": []byte("no pages here"),
	})
	_, err := OpenManga(path)
	if !errors.Is(err, ErrEmptyManga) {
		t.Fatalf("err = %v, want ErrEmptyManga", err)
	}
}

func TestOpenManga_CorruptArchiveRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.cbz")
	if err := os.WriteFile(path, []byte("PK\x03\x04 not a zip"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenManga(path); err == nil {
		t.Fatal("expected open failure on corrupt archive")
	}
}

func TestOpenManga_SinglePage(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{0, 0, 0})
	path := writeTestZip(t, dir, "m.cbz", map[string][]byte{"01.png": pic})
	m, err := OpenManga(path)
	if err != nil {
		t.Fatalf("OpenManga: %v", err)
	}
	defer func() { _ = m.Close() }()
	if len(m.Pages) != 1 {
		t.Fatalf("got %d pages, want 1", len(m.Pages))
	}
}

func TestExtractPage_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{42, 0, 0})
	path := writeTestZip(t, dir, "m.cbz", map[string][]byte{"01.png": pic})
	m, err := OpenManga(path)
	if err != nil {
		t.Fatalf("OpenManga: %v", err)
	}
	defer func() { _ = m.Close() }()

	cacheDir := t.TempDir()
	dst := filepath.Join(cacheDir, "page_0001.png")
	if err := m.ExtractPage(0, dst); err != nil {
		t.Fatalf("ExtractPage: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if !bytes.Equal(got, pic) {
		t.Errorf("extracted bytes differ from source")
	}
}

func TestCoverImage_DecodesPageOne(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 8, 4, [3]uint8{0, 0, 0})
	path := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"02.png": solidPNG(t, 16, 16, [3]uint8{0, 0, 0}),
		"01.png": pic,
	})
	m, err := OpenManga(path)
	if err != nil {
		t.Fatalf("OpenManga: %v", err)
	}
	defer func() { _ = m.Close() }()
	w, h, err := m.CoverDimensions()
	if err != nil {
		t.Fatalf("CoverDimensions: %v", err)
	}
	if w != 8 || h != 4 {
		t.Errorf("cover dims = %dx%d, want 8x4 (page 1, not page 2)", w, h)
	}
}
