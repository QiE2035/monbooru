package gallery

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/jobs"
)

// startWatcher spawns a Watcher against a fresh DB and gallery dir.
// Helper local to this test file because the existing tests don't have
// one yet and exercising fsnotify needs a goroutine + cleanup contract.
func startWatcher(t *testing.T) (*Watcher, *db.DB, string) {
	t.Helper()
	tmp := t.TempDir()
	galleryDir := filepath.Join(tmp, "gallery")
	thumbDir := filepath.Join(tmp, "thumbs")
	for _, d := range []string{galleryDir, thumbDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	database, err := db.Open(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	w, err := NewWatcher("default", galleryDir, thumbDir, 0, database, jobs.NewManager())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx) //nolint:errcheck
	return w, database, galleryDir
}

// pollForRow waits for a single non-missing image row matching predicate
// to appear; fails the test on timeout. Used because the watcher's
// 500 ms debounce + write-extension makes the ingest asynchronous.
func pollForRow(t *testing.T, database *db.DB, predicate string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var n int
		if err := database.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE ` + predicate).Scan(&n); err == nil && n == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("row matching %s never appeared", predicate)
}

// TestWatcher_DebounceExtendsOnWrite simulates the slow-write race that
// breaks cbz ingest: a writer opens a zip, flushes header bytes, sleeps
// long enough for the create-only debounce window to expire, then
// flushes the central directory and closes. Without write-side
// debounce extension, the ingest fires on the half-written file and
// archive parsing fails. With the extension, the writer's last flush
// resets the timer and the ingest sees the complete archive.
func TestWatcher_DebounceExtendsOnWrite(t *testing.T) {
	_, database, galleryDir := startWatcher(t)

	// Build a complete cbz in memory, then split the bytes into two
	// halves and write them with a 700 ms gap (longer than the 500 ms
	// debounce). On write-extension the watcher waits for the second
	// chunk before ingesting; without it, the create-only timer fires
	// after 500 ms on the half-file and OpenManga fails.
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	out, _ := zw.Create("01.png")
	out.Write(solidPNG(t, 8, 8, [3]uint8{55, 66, 77}))
	zw.Close()
	full := zbuf.Bytes()
	mid := len(full) / 2

	dst := filepath.Join(galleryDir, "slow.cbz")
	f, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(full[:mid]); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	if _, err := f.Write(full[mid:]); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	pollForRow(t, database, `file_type = 'cbz' AND canonical_path LIKE '%slow.cbz'`, 5*time.Second)
}
