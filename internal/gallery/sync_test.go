package gallery

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leqwin/monbooru/internal/db"
)

// syncEnv is the per-test bundle of paths and limits used by the refactored
// gallery signatures.
type syncEnv struct {
	galleryPath    string
	thumbnailsPath string
	maxFileSizeMB  int
}

func setupSyncTest(t *testing.T) (*db.DB, *syncEnv, string) {
	t.Helper()
	tmpDir := t.TempDir()
	galleryDir := filepath.Join(tmpDir, "gallery")
	os.MkdirAll(galleryDir, 0755)
	thumbDir := filepath.Join(tmpDir, "thumbs")
	os.MkdirAll(thumbDir, 0755)

	database, err := db.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	env := &syncEnv{
		galleryPath:    galleryDir,
		thumbnailsPath: thumbDir,
		maxFileSizeMB:  100,
	}
	return database, env, galleryDir
}

func (e *syncEnv) sync(t *testing.T, database *db.DB) SyncResult {
	t.Helper()
	r, err := Sync(context.Background(), database, e.galleryPath, e.thumbnailsPath, e.maxFileSizeMB, func(int, int, string) {})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func createTestPNGFile(t *testing.T, dir, name string) string {
	t.Helper()
	return createTestPNGFileSize(t, dir, name, 10, 10)
}

func createTestPNGFileSize(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	png.Encode(f, img)
	return path
}

func TestSync_NewFile(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFile(t, galleryDir, "test.png")

	result := env.sync(t, database)
	if result.Added != 1 {
		t.Errorf("Added = %d, want 1", result.Added)
	}
}

func TestIngest_RecordsOrigin(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	for i, tc := range []struct {
		name   string
		origin string
		want   string
	}{
		{"default", "", "ingest"},
		{"upload", "upload", "upload"},
		{"url", "https://example.com/pic", "https://example.com/pic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Distinct dimensions per subtest so each file has its own
			// SHA-256 and the ingest insert branch runs every time.
			path := createTestPNGFileSize(t, galleryDir, tc.name+".png", 10+i, 10+i)
			rec, _, err := Ingest(database, galleryDir, env.thumbnailsPath, path, "png", tc.origin)
			if err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			var got string
			if err := database.Read.QueryRow(`SELECT origin FROM images WHERE id = ?`, rec.ID).Scan(&got); err != nil {
				t.Fatalf("scan origin: %v", err)
			}
			if got != tc.want {
				t.Errorf("origin = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIngest_ReactivationRestoresUsageCount pins the visible-only
// invariant: a missing-then-reappeared image must lift its tags back
// into usage_count, otherwise the next unrelated mutation that
// triggers RecalcIDs would silently agree with a different number.
func TestIngest_ReactivationRestoresUsageCount(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	path := createTestPNGFile(t, galleryDir, "reappear.png")
	rec, _, err := Ingest(database, galleryDir, env.thumbnailsPath, path, "png", "")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// Attach a tag and simulate the watcher's mark-missing path: drop
	// the file's row from visible, decrement the tag's usage_count.
	var generalID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	var tagID int64
	if err := database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('reappear_tag', ?, 1) RETURNING id`,
		generalID,
	).Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id, is_auto, is_implied) VALUES (?, ?, 0, 0)`, rec.ID, tagID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`UPDATE images SET is_missing = 1 WHERE id = ?`, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`UPDATE tags SET usage_count = 0 WHERE id = ?`, tagID); err != nil {
		t.Fatal(err)
	}

	// Re-ingest the same content; the duplicate-by-SHA branch should
	// reactivate and lift the tag back into the visible count.
	if _, _, err := Ingest(database, galleryDir, env.thumbnailsPath, path, "png", ""); err != nil {
		t.Fatalf("re-Ingest: %v", err)
	}
	var got int
	if err := database.Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, tagID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("usage_count after reactivation = %d, want 1", got)
	}
}

// TestIngest_PromotesAliasWhenCanonicalGone simulates a watcher-observed
// mv: a file is ingested, removed from disk, and the same content is
// re-ingested at a new path. The new path must become canonical so the
// folder filter (which keys off canonical_path) sees the file at its
// real location.
func TestIngest_PromotesAliasWhenCanonicalGone(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	oldPath := createTestPNGFile(t, galleryDir, "before.png")
	rec, _, err := Ingest(database, galleryDir, env.thumbnailsPath, oldPath, "png", "")
	if err != nil {
		t.Fatalf("initial Ingest: %v", err)
	}

	// fsnotify on Linux reports IN_MOVED_FROM as a Rename event without
	// firing a Remove, so the watcher never marks the file missing. To
	// model the duplicate-on-new-path branch, copy the bytes to the new
	// location and remove the original before re-ingest.
	movedDir := filepath.Join(galleryDir, "relocated")
	if err := os.MkdirAll(movedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(movedDir, "before.png")
	if err := os.WriteFile(newPath, bytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(oldPath); err != nil {
		t.Fatal(err)
	}

	if _, isDup, err := Ingest(database, galleryDir, env.thumbnailsPath, newPath, "png", ""); err != nil {
		t.Fatalf("re-Ingest: %v", err)
	} else if isDup {
		t.Errorf("expected promotion (not isDup) when canonical was gone")
	}

	var canonPath, folderPath string
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path, is_missing FROM images WHERE id = ?`, rec.ID,
	).Scan(&canonPath, &folderPath, &isMissing); err != nil {
		t.Fatal(err)
	}
	if canonPath != newPath {
		t.Errorf("canonical_path = %q, want %q", canonPath, newPath)
	}
	if folderPath != "relocated" {
		t.Errorf("folder_path = %q, want %q", folderPath, "relocated")
	}
	if isMissing != 0 {
		t.Errorf("is_missing = %d, want 0", isMissing)
	}

	var newRow, oldRow int
	database.Read.QueryRow(`SELECT is_canonical FROM image_paths WHERE path = ?`, newPath).Scan(&newRow)
	database.Read.QueryRow(`SELECT is_canonical FROM image_paths WHERE path = ?`, oldPath).Scan(&oldRow)
	if newRow != 1 {
		t.Errorf("new path is_canonical = %d, want 1", newRow)
	}
	if oldRow != 0 {
		t.Errorf("old path is_canonical = %d, want 0 (or row gone)", oldRow)
	}
}

func TestSync_NoChange(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	createTestPNGFile(t, galleryDir, "test.png")

	env.sync(t, database)
	result := env.sync(t, database)
	if result.Added != 0 || result.Removed != 0 {
		t.Errorf("expected no changes, got Added=%d Removed=%d", result.Added, result.Removed)
	}
}

// TestSync_MovePreservesPriorPathAsAlias: the previous canonical
// entry must be demoted to an alias so the move history isn't
// silently overwritten. Counts both image_paths rows after the move
// and asserts the prior path is still in the table.
func TestSync_MovePreservesPriorPathAsAlias(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	subDir := filepath.Join(galleryDir, "moved")
	os.MkdirAll(subDir, 0755)

	original := createTestPNGFile(t, galleryDir, "wander.png")
	env.sync(t, database)

	// Move the file to the new folder; sync must record the new path
	// as canonical and keep the old one as a non-canonical alias.
	newPath := filepath.Join(subDir, "wander.png")
	if err := os.Rename(original, newPath); err != nil {
		t.Fatal(err)
	}
	env.sync(t, database)

	rows, err := database.Read.Query(
		`SELECT path, is_canonical FROM image_paths ORDER BY is_canonical DESC, path`,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var paths []string
	canonicals := 0
	for rows.Next() {
		var p string
		var ic int
		if err := rows.Scan(&p, &ic); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
		if ic == 1 {
			canonicals++
		}
	}
	if len(paths) != 2 {
		t.Errorf("image_paths rows = %d, want 2 (canonical + alias); got paths = %v", len(paths), paths)
	}
	if canonicals != 1 {
		t.Errorf("canonical rows = %d, want exactly 1; paths = %v", canonicals, paths)
	}
}

// TestSync_DetectsSameSizeInPlaceEdit: a same-size in-place rewrite
// must surface (re-hash) on the next sync via the mtime gate, not
// silently keep the prior SHA. The unchanged-shortcut keeps idle
// syncs cheap on libraries that haven't seen edits, but a touch +
// same-size overwrite must invalidate it.
func TestSync_DetectsSameSizeInPlaceEdit(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFile(t, galleryDir, "edit.png")

	env.sync(t, database)
	var origSHA string
	database.Read.QueryRow(`SELECT sha256 FROM images`).Scan(&origSHA)

	// Stat the seeded file, build a new payload of the same byte length
	// (different content), and write it back with a fresh mtime so the
	// (size, mtime) parity gate trips.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	overwrite := make([]byte, info.Size())
	for i := range overwrite {
		overwrite[i] = byte((i * 7) ^ 0x5a)
	}
	if err := os.WriteFile(path, overwrite, 0o644); err != nil {
		t.Fatal(err)
	}
	// Force a distinguishable mtime even on coarse-grained filesystems.
	future := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	env.sync(t, database)

	var nextSHA string
	database.Read.QueryRow(`SELECT sha256 FROM images`).Scan(&nextSHA)
	if nextSHA == origSHA {
		// Either the row stayed pinned to the old SHA, or re-ingest
		// landed in a state where SHA isn't refreshed in-row. Both
		// leave the catalog out of sync with disk: surface as fail.
		t.Errorf("post-edit SHA still %q; expected the row to be re-hashed", nextSHA)
	}
}

// TestSync_InPlaceCbzEditClearsPageCache: rewriting a cbz in place must
// drop the stale per-page reader cache. The reader serves an extracted
// page file without revalidating it against the archive, so a leftover
// raw page from the old contents would be served until the idle TTL.
func TestSync_InPlaceCbzEditClearsPageCache(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := writeTestZip(t, galleryDir, "book.cbz", map[string][]byte{
		"001.png": solidPNG(t, 20, 20, [3]uint8{10, 20, 30}),
		"002.png": solidPNG(t, 20, 20, [3]uint8{40, 50, 60}),
	})

	env.sync(t, database)
	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM images`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	// Simulate a reader having extracted a raw page into the lazy cache.
	stale := filepath.Join(MangaImageDir(env.thumbnailsPath, id), "page_0001.png")
	if err := os.WriteFile(stale, []byte("stale page bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rewrite the archive with different page bytes (new SHA) and a fresh
	// mtime so sync takes the in-place-edit branch.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	writeTestZip(t, galleryDir, "book.cbz", map[string][]byte{
		"001.png": solidPNG(t, 30, 30, [3]uint8{200, 100, 0}),
		"002.png": solidPNG(t, 30, 30, [3]uint8{0, 100, 200}),
		"003.png": solidPNG(t, 30, 30, [3]uint8{100, 0, 100}),
	})
	future := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	env.sync(t, database)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale raw page cache survived the in-place edit (err=%v)", err)
	}
}

func TestSync_FileDeleted(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFile(t, galleryDir, "test.png")

	env.sync(t, database)
	os.Remove(path)
	result := env.sync(t, database)
	if result.Removed != 1 {
		t.Errorf("Removed = %d, want 1", result.Removed)
	}

	var isMissing int
	database.Read.QueryRow(`SELECT is_missing FROM images`).Scan(&isMissing)
	if isMissing != 1 {
		t.Error("image not marked as missing")
	}
}

func TestSync_Duplicate(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	subDir := filepath.Join(galleryDir, "sub")
	os.MkdirAll(subDir, 0755)

	original := createTestPNGFile(t, galleryDir, "original.png")
	content, _ := os.ReadFile(original)
	os.WriteFile(filepath.Join(subDir, "copy.png"), content, 0644)

	result := env.sync(t, database)
	if result.Added != 1 {
		t.Errorf("Added = %d, want 1", result.Added)
	}
	if result.Duplicates != 1 {
		t.Errorf("Duplicates = %d, want 1", result.Duplicates)
	}
}

// TestSync_DuplicateAliasRecordsMtime: the alias path inserted for a
// byte-identical copy must carry the file's mtime. Left at 0 it never
// satisfies the (size, mtime) unchanged-shortcut, so the copy is
// re-hashed on every later sync.
func TestSync_DuplicateAliasRecordsMtime(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	subDir := filepath.Join(galleryDir, "sub")
	os.MkdirAll(subDir, 0755)

	original := createTestPNGFile(t, galleryDir, "original.png")
	content, _ := os.ReadFile(original)
	copyPath := filepath.Join(subDir, "copy.png")
	os.WriteFile(copyPath, content, 0644)

	env.sync(t, database)

	info, err := os.Stat(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	var mtime int64
	if err := database.Read.QueryRow(
		`SELECT mtime_unix FROM image_paths WHERE path = ?`, copyPath,
	).Scan(&mtime); err != nil {
		t.Fatal(err)
	}
	if mtime != info.ModTime().Unix() {
		t.Errorf("alias mtime_unix = %d, want %d (file mtime)", mtime, info.ModTime().Unix())
	}
}

func TestSync_SkipsLargeFile(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	// Build a >1 MiB PNG so a 1 MiB cap leaves it out. Random pixel data
	// defeats deflate so the encoded size exceeds the cap.
	big := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
	for i := 0; i < len(big.Pix); i++ {
		big.Pix[i] = byte(i * 131 % 251)
	}
	bigPath := filepath.Join(galleryDir, "too_big.png")
	bf, err := os.Create(bigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(bf, big); err != nil {
		bf.Close()
		t.Fatal(err)
	}
	bf.Close()
	if fi, err := os.Stat(bigPath); err != nil || fi.Size() <= 1024*1024 {
		t.Skipf("big PNG unexpectedly compressed below 1 MiB (%d bytes); cannot exercise cap", fi.Size())
	}

	// Also drop a tiny file that should always ingest regardless of cap.
	createTestPNGFileSize(t, galleryDir, "small.png", 10, 10)

	// 1 MiB cap → big is skipped, small ingests.
	env.maxFileSizeMB = 1
	r1 := env.sync(t, database)
	if r1.Added != 1 {
		t.Errorf("with cap=1 MiB expected 1 added (small), got %d", r1.Added)
	}
	// Raise the cap; big now ingests, small stays.
	env.maxFileSizeMB = 100
	r2 := env.sync(t, database)
	if r2.Added != 1 {
		t.Errorf("after raising cap expected 1 new add (big), got %d", r2.Added)
	}
}

func TestFolderPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		galleryPath string
		filePath    string
		want        string
	}{
		{"root", "/gallery", "/gallery/image.png", ""},
		{"nested", "/gallery", "/gallery/2024/jan/x.png", "2024/jan"},
		{"one level", "/gallery", "/gallery/sub/image.png", "sub"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FolderPath(tt.galleryPath, tt.filePath)
			if got != tt.want {
				t.Errorf("FolderPath(%q, %q) = %q, want %q", tt.galleryPath, tt.filePath, got, tt.want)
			}
		})
	}
}

func TestSync_FileMoved(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "original.png")

	r1 := env.sync(t, database)
	if r1.Added != 1 {
		t.Fatalf("initial sync Added=%d, want 1", r1.Added)
	}

	subDir := filepath.Join(galleryDir, "sub")
	os.MkdirAll(subDir, 0755)
	dstPath := filepath.Join(subDir, "original.png")
	os.Rename(srcPath, dstPath)

	r2 := env.sync(t, database)
	if r2.Moved != 1 {
		t.Errorf("Moved = %d, want 1", r2.Moved)
	}
}

func TestFolderTree_Empty(t *testing.T) {
	database, _, _ := setupSyncTest(t)

	nodes, err := FolderTree(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 root node, got %d", len(nodes))
	}
	if nodes[0].Name != "(root)" {
		t.Errorf("root name = %q", nodes[0].Name)
	}
	if nodes[0].Count != 0 {
		t.Errorf("root count = %d, want 0", nodes[0].Count)
	}
}

func TestFolderTree_WithImages(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	// Root image (10x10)
	createTestPNGFileSize(t, galleryDir, "root.png", 10, 10)

	// Sub-folder image (distinct size to ensure different SHA-256)
	subDir := filepath.Join(galleryDir, "sub")
	os.MkdirAll(subDir, 0755)
	createTestPNGFileSize(t, subDir, "sub.png", 11, 10)

	env.sync(t, database)

	nodes, err := FolderTree(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 {
		t.Fatal("expected nodes")
	}
	root := nodes[0]
	if root.Count < 2 {
		t.Errorf("root total count = %d, want >= 2", root.Count)
	}
	if len(root.Children) < 1 {
		t.Errorf("expected at least 1 child folder, got %d", len(root.Children))
	}
	// Check sub folder node
	found := false
	for _, child := range root.Children {
		if child.Name == "sub" {
			found = true
			if child.Count != 1 {
				t.Errorf("sub folder count = %d, want 1", child.Count)
			}
			if child.Depth != 1 {
				t.Errorf("sub folder depth = %d, want 1", child.Depth)
			}
		}
	}
	if !found {
		t.Error("sub folder not found in FolderTree")
	}
}

func TestFolderTree_RecursiveCount(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	// parent/ has zero direct images but two subfolders with one each, so
	// parent should roll up to 2 even though nothing sits at parent/ itself.
	parentDir := filepath.Join(galleryDir, "parent")
	subA := filepath.Join(parentDir, "a")
	subB := filepath.Join(parentDir, "b")
	os.MkdirAll(subA, 0755)
	os.MkdirAll(subB, 0755)
	createTestPNGFileSize(t, subA, "a.png", 10, 10)
	createTestPNGFileSize(t, subB, "b.png", 11, 10)

	env.sync(t, database)

	nodes, err := FolderTree(database)
	if err != nil {
		t.Fatal(err)
	}
	root := nodes[0]
	if root.Count != 2 {
		t.Errorf("root count = %d, want 2", root.Count)
	}
	var parent *FolderNode
	for i := range root.Children {
		if root.Children[i].Name == "parent" {
			parent = &root.Children[i]
		}
	}
	if parent == nil {
		t.Fatal("parent folder not found in tree")
	}
	if parent.Count != 2 {
		t.Errorf("parent count = %d, want 2 (recursive)", parent.Count)
	}
	for _, c := range parent.Children {
		if c.Count != 1 {
			t.Errorf("%s count = %d, want 1", c.Name, c.Count)
		}
	}
}

func TestCountSlashes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		s    string
		want int
	}{
		{"empty", "", 0},
		{"one slash", "a/b", 1},
		{"two slashes", "a/b/c", 2},
		{"no slashes", "no_slashes", 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := countSlashes(tt.s); got != tt.want {
				t.Errorf("countSlashes(%q) = %d, want %d", tt.s, got, tt.want)
			}
		})
	}
}

func TestSync_ContextCanceled(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	// Seed enough files that Phase 2's per-file loop notices the cancelled
	// context before it finishes scanning everything.
	for i := 0; i < 32; i++ {
		createTestPNGFileSize(t, galleryDir, fmt.Sprintf("ctx_%02d.png", i), 10+i, 10)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Sync(ctx, database, env.galleryPath, env.thumbnailsPath, env.maxFileSizeMB, func(int, int, string) {})
	if err == nil {
		t.Fatal("Sync with a pre-cancelled ctx should surface the cancellation, got nil err")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestWatcher_IngestsFile(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	w, err := NewWatcher("", env.galleryPath, env.thumbnailsPath, env.maxFileSizeMB, database, nil)
	if err != nil {
		if strings.Contains(err.Error(), "too many open files") {
			t.Skip("skipping: system file descriptor limit reached")
		}
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)

	// Drop a file; poll the DB for arrival instead of assuming a fixed
	// debounce + IO budget.
	createTestPNGFile(t, galleryDir, "new.png")
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		database.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&count)
		if count == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	var count int
	database.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&count)
	t.Errorf("watcher did not ingest within 8 s; count = %d", count)
}

// TestWatcher_DecrementsTagsOnMissing pins that mark-missing decrements
// the usage_count of every tag the image carried. Tag rows persist at
// zero usage so user-declared aliases and implications survive a removed
// image; explicit deletion via the Tags page is the only way to drop them.
func TestWatcher_DecrementsTagsOnMissing(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	w, err := NewWatcher("", env.galleryPath, env.thumbnailsPath, env.maxFileSizeMB, database, nil)
	if err != nil {
		if strings.Contains(err.Error(), "too many open files") {
			t.Skip("skipping: system file descriptor limit reached")
		}
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)

	path := createTestPNGFile(t, galleryDir, "tagged.png")
	deadline := time.Now().Add(8 * time.Second)
	var imgID int64
	for time.Now().Before(deadline) {
		if err := database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path = ?`, path).Scan(&imgID); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if imgID == 0 {
		t.Fatal("watcher did not ingest the file")
	}

	// "shared" is seeded at usage=2 so the decrement leaves it positive;
	// "solo" is seeded at usage=1 so the decrement drives it to zero. Both
	// rows must survive: zero-usage tags persist after mark-missing.
	var generalID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	var sharedID, soloID int64
	if err := database.Write.QueryRow(`INSERT INTO tags (name, category_id, usage_count) VALUES ('shared', ?, 2) RETURNING id`, generalID).Scan(&sharedID); err != nil {
		t.Fatal(err)
	}
	if err := database.Write.QueryRow(`INSERT INTO tags (name, category_id, usage_count) VALUES ('solo', ?, 1) RETURNING id`, generalID).Scan(&soloID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0), (?, ?, 0)`, imgID, sharedID, imgID, soloID); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		var isMissing int
		database.Read.QueryRow(`SELECT is_missing FROM images WHERE id = ?`, imgID).Scan(&isMissing)
		if isMissing == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	var sharedCount int
	if err := database.Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, sharedID).Scan(&sharedCount); err != nil {
		t.Fatalf("shared tag should still exist with decremented count: %v", err)
	}
	if sharedCount != 1 {
		t.Errorf("shared usage_count = %d, want 1 (was 2 before mark-missing)", sharedCount)
	}
	var soloCount int
	if err := database.Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, soloID).Scan(&soloCount); err != nil {
		t.Fatalf("solo tag should persist at zero usage: %v", err)
	}
	if soloCount != 0 {
		t.Errorf("solo usage_count = %d, want 0", soloCount)
	}
}

// TestWatcher_AliasPathRemovalDoesNotMarkMissing pins that deleting a
// non-canonical duplicate does not flip the parent image to is_missing or
// decrement its tag counts. The mark-missing fallback must restrict to
// canonical image_paths rows.
func TestWatcher_AliasPathRemovalDoesNotMarkMissing(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)

	w, err := NewWatcher("", env.galleryPath, env.thumbnailsPath, env.maxFileSizeMB, database, nil)
	if err != nil {
		if strings.Contains(err.Error(), "too many open files") {
			t.Skip("skipping: system file descriptor limit reached")
		}
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go w.Run(ctx)

	canonicalPath := createTestPNGFile(t, galleryDir, "canonical.png")
	deadline := time.Now().Add(8 * time.Second)
	var imgID int64
	for time.Now().Before(deadline) {
		if err := database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path = ?`, canonicalPath).Scan(&imgID); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if imgID == 0 {
		t.Fatal("watcher did not ingest the canonical file")
	}

	var generalID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	var tagID int64
	if err := database.Write.QueryRow(`INSERT INTO tags (name, category_id, usage_count) VALUES ('alias_check', ?, 1) RETURNING id`, generalID).Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, imgID, tagID); err != nil {
		t.Fatal(err)
	}

	// Drop a duplicate file in the watched tree and let the watcher ingest
	// it; the second copy should land as a non-canonical image_paths row
	// against the same image id.
	aliasPath := createTestPNGFile(t, galleryDir, "duplicate.png")
	deadline = time.Now().Add(8 * time.Second)
	var aliasID int64
	for time.Now().Before(deadline) {
		if err := database.Read.QueryRow(
			`SELECT id FROM image_paths WHERE path = ? AND image_id = ? AND is_canonical = 0`,
			aliasPath, imgID,
		).Scan(&aliasID); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if aliasID == 0 {
		t.Fatal("watcher did not register the duplicate as a non-canonical alias")
	}

	if err := os.Remove(aliasPath); err != nil {
		t.Fatal(err)
	}

	// Give the debounce + mark-missing path time to fire (or wrongly fire).
	// A negative assertion needs a settle window; mirror the positive
	// test's 8-second budget so a slow CI doesn't false-pass.
	time.Sleep(2 * time.Second)

	var isMissing int
	if err := database.Read.QueryRow(`SELECT is_missing FROM images WHERE id = ?`, imgID).Scan(&isMissing); err != nil {
		t.Fatal(err)
	}
	if isMissing != 0 {
		t.Errorf("image marked missing after alias-path removal; is_missing = %d, want 0", isMissing)
	}

	var tagCount int
	if err := database.Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, tagID).Scan(&tagCount); err != nil {
		t.Fatal(err)
	}
	if tagCount != 1 {
		t.Errorf("tag usage_count = %d, want 1 (alias removal must not touch tag counts)", tagCount)
	}
}
