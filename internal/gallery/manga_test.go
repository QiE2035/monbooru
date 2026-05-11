package gallery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureMangaPage_HitAndMiss(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{0, 0, 0})
	cbz := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"01.png": pic, "02.png": pic, "03.png": pic,
	})
	thumbDir := t.TempDir()

	page2, err := EnsureMangaPage(thumbDir, cbz, 42, 2)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	st1, err := os.Stat(page2)
	if err != nil {
		t.Fatalf("stat after extract: %v", err)
	}
	// Roll the mtime backwards so the next call's TouchCacheFile is
	// observable; otherwise the resolution of the filesystem clock
	// would let two os.Chtimes calls collapse to identical mtimes.
	past := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(page2, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	page2Again, err := EnsureMangaPage(thumbDir, cbz, 42, 2)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if page2Again != page2 {
		t.Errorf("second call returned %q, want %q", page2Again, page2)
	}
	st2, err := os.Stat(page2Again)
	if err != nil {
		t.Fatalf("stat after touch: %v", err)
	}
	if !st2.ModTime().After(st1.ModTime()) {
		t.Errorf("mtime after hit did not advance")
	}
}

func TestEnsureMangaPage_OutOfRange(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{0, 0, 0})
	cbz := writeTestZip(t, dir, "m.cbz", map[string][]byte{"01.png": pic})
	thumbDir := t.TempDir()
	if _, err := EnsureMangaPage(thumbDir, cbz, 1, 2); err == nil {
		t.Error("expected error for page 2 of a 1-page archive")
	}
}

func TestEnsureMangaPageThumb_GeneratesAndCaches(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 64, 32, [3]uint8{0, 0, 0})
	cbz := writeTestZip(t, dir, "m.cbz", map[string][]byte{"01.png": pic})
	thumbDir := t.TempDir()

	thumb1, err := EnsureMangaPageThumb(thumbDir, cbz, 7, 1)
	if err != nil {
		t.Fatalf("thumb generate: %v", err)
	}
	if _, err := os.Stat(thumb1); err != nil {
		t.Fatalf("thumb stat: %v", err)
	}
	thumb2, err := EnsureMangaPageThumb(thumbDir, cbz, 7, 1)
	if err != nil {
		t.Fatalf("thumb hit: %v", err)
	}
	if thumb1 != thumb2 {
		t.Errorf("hit returned different path %q vs %q", thumb1, thumb2)
	}
}

func TestRemoveMangaCache_DropsDirectory(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{0, 0, 0})
	cbz := writeTestZip(t, dir, "m.cbz", map[string][]byte{"01.png": pic})
	thumbDir := t.TempDir()
	if _, err := EnsureMangaPage(thumbDir, cbz, 99, 1); err != nil {
		t.Fatalf("extract: %v", err)
	}
	imageDir := MangaImageDir(thumbDir, 99)
	if _, err := os.Stat(imageDir); err != nil {
		t.Fatalf("image dir missing: %v", err)
	}
	RemoveMangaCache(thumbDir, 99)
	if _, err := os.Stat(imageDir); !os.IsNotExist(err) {
		t.Errorf("image dir still exists after RemoveMangaCache: %v", err)
	}
}

func TestMangaCacheReclaimer_EvictsStale(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 4, 4, [3]uint8{0, 0, 0})
	cbz := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"01.png": pic, "02.png": pic,
	})
	thumbDir := t.TempDir()
	page1, err := EnsureMangaPage(thumbDir, cbz, 5, 1)
	if err != nil {
		t.Fatalf("extract page 1: %v", err)
	}
	page2, err := EnsureMangaPage(thumbDir, cbz, 5, 2)
	if err != nil {
		t.Fatalf("extract page 2: %v", err)
	}
	// page 1 stays warm; page 2 ages out.
	old := time.Now().Add(-2 * MangaPageCacheTTL)
	if err := os.Chtimes(page2, old, old); err != nil {
		t.Fatal(err)
	}
	r := NewMangaCacheReclaimer(MangaCacheDir(thumbDir))
	r.sweepOnce()
	if _, err := os.Stat(page1); err != nil {
		t.Errorf("warm page evicted: %v", err)
	}
	if _, err := os.Stat(page2); !os.IsNotExist(err) {
		t.Errorf("stale page survived reclaim: %v", err)
	}
}

func TestMangaCacheReclaimer_StartStop(t *testing.T) {
	r := NewMangaCacheReclaimer(filepath.Join(t.TempDir(), "manga"))
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	r.Start(ctx) // idempotent
	cancel()
	r.Stop()
	r.Stop() // idempotent
}

// Generate on a cbz writes the cover plus a per-page _thumb.jpg for
// every entry, so the first /pages render is a static-file serve.
func TestGenerate_CBZ_PrecomputesEveryPageThumbnail(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 32, 32, [3]uint8{12, 34, 56})
	cbz := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"01.png": pic, "02.png": pic, "03.png": pic,
	})
	thumbDir := t.TempDir()
	if err := Generate(cbz, thumbDir, 77, "cbz"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := os.Stat(ThumbnailPath(thumbDir, 77)); err != nil {
		t.Errorf("cover thumb missing: %v", err)
	}
	imgDir := MangaImageDir(thumbDir, 77)
	for n := 1; n <= 3; n++ {
		if _, err := os.Stat(MangaPageThumbPath(imgDir, n)); err != nil {
			t.Errorf("page %d thumb missing: %v", n, err)
		}
	}
}

// Page thumbnails are exempt from idle reclaim. Only the raw page-byte
// files (`page_NNNN.<ext>`) get unlinked once their mtime ages past the
// TTL; the precomputed _thumb.jpg companions stay forever.
func TestMangaCacheReclaimer_PreservesPageThumbnails(t *testing.T) {
	dir := t.TempDir()
	pic := solidPNG(t, 16, 16, [3]uint8{0, 0, 0})
	cbz := writeTestZip(t, dir, "m.cbz", map[string][]byte{
		"01.png": pic, "02.png": pic,
	})
	thumbDir := t.TempDir()
	if err := Generate(cbz, thumbDir, 5, "cbz"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	rawPage, err := EnsureMangaPage(thumbDir, cbz, 5, 1)
	if err != nil {
		t.Fatalf("ensure raw page: %v", err)
	}
	thumb := MangaPageThumbPath(MangaImageDir(thumbDir, 5), 1)
	old := time.Now().Add(-2 * MangaPageCacheTTL)
	if err := os.Chtimes(rawPage, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(thumb, old, old); err != nil {
		t.Fatal(err)
	}
	r := NewMangaCacheReclaimer(MangaCacheDir(thumbDir))
	r.sweepOnce()
	if _, err := os.Stat(rawPage); !os.IsNotExist(err) {
		t.Errorf("stale raw page survived reclaim: %v", err)
	}
	if _, err := os.Stat(thumb); err != nil {
		t.Errorf("page thumb evicted by reclaim - precompute would never warm again: %v", err)
	}
}
