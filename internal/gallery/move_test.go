package gallery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMoveImage_IntoSubfolder(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "original.png")

	_, _, err := Ingest(database, galleryDir, env.thumbnailsPath, srcPath, "png", "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM images`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	res, err := MoveImage(database, galleryDir, id, "2026/april")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if res.NewFolderPath != "2026/april" {
		t.Errorf("new folder = %q, want 2026/april", res.NewFolderPath)
	}

	var canonPath, folderPath string
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path FROM images WHERE id = ?`, id,
	).Scan(&canonPath, &folderPath); err != nil {
		t.Fatal(err)
	}
	if folderPath != "2026/april" {
		t.Errorf("folder_path = %q, want 2026/april", folderPath)
	}
	wantPath := filepath.Join(galleryDir, "2026", "april", "original.png")
	if canonPath != wantPath {
		t.Errorf("canonical_path = %q, want %q", canonPath, wantPath)
	}

	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("file not at new path: %v", err)
	}
	if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
		t.Errorf("file still at old path (err=%v)", err)
	}

	var aliasPath string
	if err := database.Read.QueryRow(
		`SELECT path FROM image_paths WHERE image_id = ? AND is_canonical = 1`, id,
	).Scan(&aliasPath); err != nil {
		t.Fatal(err)
	}
	if aliasPath != wantPath {
		t.Errorf("canonical image_paths row = %q, want %q", aliasPath, wantPath)
	}
}

func TestMoveImage_FilenameCollisionAutosuffixes(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFileSize(t, galleryDir, "pic.png", 10, 10)
	if _, _, err := Ingest(database, galleryDir, env.thumbnailsPath, srcPath, "png", ""); err != nil {
		t.Fatalf("ingest src: %v", err)
	}
	// Pre-seed an existing distinct file at the destination with the same
	// filename so UniqueDestPath must take the `_1` branch.
	dstDir := filepath.Join(galleryDir, "dst")
	os.MkdirAll(dstDir, 0o755)
	createTestPNGFileSize(t, dstDir, "pic.png", 11, 10)

	var id int64
	database.Read.QueryRow(`SELECT id FROM images ORDER BY id LIMIT 1`).Scan(&id)

	res, err := MoveImage(database, galleryDir, id, "dst")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	want := filepath.Join(dstDir, "pic_1.png")
	if res.NewCanonicalPath != want {
		t.Errorf("new canonical = %q, want %q", res.NewCanonicalPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("file not at auto-suffixed path: %v", err)
	}
}

func TestMoveImage_RenameFailureRollsBackTx(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permissions")
	}
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "rollme.png")
	if _, _, err := Ingest(database, galleryDir, env.thumbnailsPath, srcPath, "png", ""); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var id int64
	database.Read.QueryRow(`SELECT id FROM images`).Scan(&id)

	// Pre-create the destination dir without write permission so the
	// rename inside MoveImage fails; the in-flight tx must roll back and
	// leave canonical_path / image_paths.path pointing at the original.
	dstDir := filepath.Join(galleryDir, "ro")
	if err := os.MkdirAll(dstDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dstDir, 0o755) })

	if _, err := MoveImage(database, galleryDir, id, "ro"); err == nil {
		t.Fatal("expected MoveImage to fail when destination is not writable")
	}

	var canonPath, folderPath string
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path FROM images WHERE id = ?`, id,
	).Scan(&canonPath, &folderPath); err != nil {
		t.Fatal(err)
	}
	if canonPath != srcPath {
		t.Errorf("canonical_path = %q, want %q (rename failure should roll back)", canonPath, srcPath)
	}
	if folderPath != "" {
		t.Errorf("folder_path = %q, want empty", folderPath)
	}
	var pathRow string
	if err := database.Read.QueryRow(
		`SELECT path FROM image_paths WHERE image_id = ? AND is_canonical = 1`, id,
	).Scan(&pathRow); err != nil {
		t.Fatal(err)
	}
	if pathRow != srcPath {
		t.Errorf("image_paths.path = %q, want %q", pathRow, srcPath)
	}
	if _, err := os.Stat(srcPath); err != nil {
		t.Errorf("source file gone after failed move: %v", err)
	}
}

// When the computed destination path already lives in image_paths
// against a different image (a stale alias whose file is gone), the
// move fails with a clean diagnostic that points at the colliding
// image, not a raw UNIQUE constraint error.
func TestMoveImage_RejectsAliasPathCollision(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "src.png")
	if _, _, err := Ingest(database, galleryDir, env.thumbnailsPath, srcPath, "png", ""); err != nil {
		t.Fatalf("ingest src: %v", err)
	}
	var srcID int64
	database.Read.QueryRow(`SELECT id FROM images ORDER BY id LIMIT 1`).Scan(&srcID)

	// Seed a stale alias row on a different image whose path is exactly
	// where MoveImage would land (`<galleryDir>/dst/src.png`).
	dstDir := filepath.Join(galleryDir, "dst")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	otherPath := createTestPNGFileSize(t, galleryDir, "other.png", 11, 11)
	if _, _, err := Ingest(database, galleryDir, env.thumbnailsPath, otherPath, "png", ""); err != nil {
		t.Fatal(err)
	}
	var otherID int64
	database.Read.QueryRow(`SELECT id FROM images WHERE id != ?`, srcID).Scan(&otherID)
	if _, err := database.Write.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`,
		otherID, filepath.Join(dstDir, "src.png"),
	); err != nil {
		t.Fatalf("seed stale alias: %v", err)
	}

	_, err := MoveImage(database, galleryDir, srcID, "dst")
	if err == nil {
		t.Fatal("expected move to fail on alias collision")
	}
	if !strings.Contains(err.Error(), "collides with an existing alias") {
		t.Errorf("error %v should call out the alias collision shape", err)
	}
}

func TestMoveImage_SameFolderIsNoop(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	sub := filepath.Join(galleryDir, "here")
	os.MkdirAll(sub, 0o755)
	srcPath := createTestPNGFile(t, sub, "x.png")
	if _, _, err := Ingest(database, galleryDir, env.thumbnailsPath, srcPath, "png", ""); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var id int64
	database.Read.QueryRow(`SELECT id FROM images`).Scan(&id)

	res, err := MoveImage(database, galleryDir, id, "here")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if res.NewCanonicalPath != srcPath {
		t.Errorf("new canonical = %q, want unchanged %q", res.NewCanonicalPath, srcPath)
	}
	if _, err := os.Stat(srcPath); err != nil {
		t.Errorf("file moved unexpectedly: %v", err)
	}
}
