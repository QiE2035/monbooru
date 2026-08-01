package gallery

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
)

// MoveImageResult reports the new location of a moved image so the caller
// can render it and invalidate caches without re-querying.
type MoveImageResult struct {
	NewCanonicalPath string
	NewFolderPath    string
}

// MoveImage relocates the canonical file of image id into targetFolder
// (relative to galleryPath). Filename collisions auto-suffix via
// UniqueDestPath, matching the upload and API paths. Callers that hold a
// watcher should gate this under a job type the watcher suppresses,
// otherwise the resulting CREATE/REMOVE events race with the DB update.
func MoveImage(database *db.DB, galleryPath string, id int64, targetFolder string) (*MoveImageResult, error) {
	var oldCanonical, oldFolder string
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path, is_missing FROM images WHERE id = ?`, id,
	).Scan(&oldCanonical, &oldFolder, &isMissing); err != nil {
		return nil, fmt.Errorf("image %d not found: %w", id, err)
	}
	if isMissing == 1 {
		return nil, fmt.Errorf("image %d is missing from disk", id)
	}
	// Refuse a source whose canonical_path drifted outside the gallery
	// root, mirroring DeleteImage; the destination is already root-bounded
	// by ResolveSubdir.
	if galleryPath != "" && !PathInside(galleryPath, oldCanonical) {
		return nil, fmt.Errorf("refusing to move %q outside gallery root %q", oldCanonical, galleryPath)
	}

	destDir, err := ResolveSubdir(galleryPath, targetFolder)
	if err != nil {
		return nil, err
	}
	newFolder, err := filepath.Rel(galleryPath, destDir)
	if err != nil {
		return nil, fmt.Errorf("resolve folder: %w", err)
	}
	if newFolder == "." {
		newFolder = ""
	}
	// folder_path is stored "/"-separated on every platform.
	newFolder = filepath.ToSlash(newFolder)

	if newFolder == oldFolder {
		return &MoveImageResult{
			NewCanonicalPath: oldCanonical,
			NewFolderPath:    oldFolder,
		}, nil
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("create destination folder: %w", err)
	}

	newPath := UniqueDestPath(destDir, filepath.Base(oldCanonical))

	// UniqueDestPath only checks the filesystem, not image_paths. A stale
	// alias row for a different image (file long gone but row never
	// pruned) would otherwise trip the UNIQUE constraint on path mid-tx
	// with no useful diagnostic. Surface the collision up front so the
	// caller can suggest "prune duplicate paths" from the Settings
	// maintenance page.
	var collidingImage int64
	if err := database.Read.QueryRow(
		`SELECT image_id FROM image_paths WHERE path = ? AND image_id != ?`,
		newPath, id,
	).Scan(&collidingImage); err == nil {
		return nil, fmt.Errorf("destination collides with an existing alias on image %d", collidingImage)
	}

	// Rename inside the open tx so a rename failure rolls the row updates
	// back automatically. The watcher suppresses events while the move job
	// runs, so the brief window where on-disk newPath exists but the tx
	// has not committed yet does not race with concurrent ingest.
	tx, err := database.Write.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin move tx: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`,
		newPath, newFolder, id,
	); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("update images row: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE image_paths SET path = ? WHERE image_id = ? AND is_canonical = 1`,
		newPath, id,
	); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("update image_paths row: %w", err)
	}
	if err := os.Rename(oldCanonical, newPath); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("rename file: %w", err)
	}
	if err := tx.Commit(); err != nil {
		// Commit failure is rare (SQLite COMMIT is essentially an fsync).
		// Reverse the rename so disk and DB stay consistent; if that
		// fails the library is wedged and the operator needs a manual sync.
		if rnErr := os.Rename(newPath, oldCanonical); rnErr != nil {
			logx.Errorf("move: reverse rename for %d after commit fail: %v (original: %v)", id, rnErr, err)
		}
		return nil, fmt.Errorf("commit move tx: %w", err)
	}

	return &MoveImageResult{
		NewCanonicalPath: newPath,
		NewFolderPath:    newFolder,
	}, nil
}

// RenameImage renames the canonical file of image id in place: the
// folder stays, the basename becomes newName with the original
// extension preserved. Collisions auto-suffix via uniqueRenamePath and
// the same watcher-suppression caveat as MoveImage applies.
func RenameImage(database *db.DB, galleryPath string, id int64, newName string) (*MoveImageResult, error) {
	newName = strings.TrimSpace(newName)
	if newName == "" || newName != filepath.Base(newName) || newName == "." || newName == ".." {
		return nil, fmt.Errorf("invalid file name %q", newName)
	}
	var oldCanonical, oldFolder string
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path, is_missing FROM images WHERE id = ?`, id,
	).Scan(&oldCanonical, &oldFolder, &isMissing); err != nil {
		return nil, fmt.Errorf("image %d not found: %w", id, err)
	}
	if isMissing == 1 {
		return nil, fmt.Errorf("image %d is missing from disk", id)
	}
	if galleryPath != "" && !PathInside(galleryPath, oldCanonical) {
		return nil, fmt.Errorf("refusing to rename %q outside gallery root %q", oldCanonical, galleryPath)
	}

	if ext := filepath.Ext(oldCanonical); !strings.EqualFold(filepath.Ext(newName), ext) {
		newName += ext
	}
	if newName == filepath.Base(oldCanonical) {
		return &MoveImageResult{
			NewCanonicalPath: oldCanonical,
			NewFolderPath:    oldFolder,
		}, nil
	}

	newPath := uniqueRenamePath(filepath.Dir(oldCanonical), newName)

	// Same alias-collision pre-check as MoveImage: a stale image_paths row
	// would trip the UNIQUE constraint mid-tx with no useful diagnostic.
	var collidingImage int64
	if err := database.Read.QueryRow(
		`SELECT image_id FROM image_paths WHERE path = ? AND image_id != ?`,
		newPath, id,
	).Scan(&collidingImage); err == nil {
		return nil, fmt.Errorf("destination collides with an existing alias on image %d", collidingImage)
	}

	tx, err := database.Write.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin rename tx: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE images SET canonical_path = ? WHERE id = ?`, newPath, id,
	); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("update images row: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE image_paths SET path = ? WHERE image_id = ? AND is_canonical = 1`,
		newPath, id,
	); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("update image_paths row: %w", err)
	}
	if err := os.Rename(oldCanonical, newPath); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("rename file: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if rnErr := os.Rename(newPath, oldCanonical); rnErr != nil {
			logx.Errorf("rename: reverse rename for %d after commit fail: %v (original: %v)", id, rnErr, err)
		}
		return nil, fmt.Errorf("commit rename tx: %w", err)
	}

	return &MoveImageResult{
		NewCanonicalPath: newPath,
		NewFolderPath:    oldFolder,
	}, nil
}

// uniqueRenamePath returns dir/filename if free, else appends a
// zero-padded counter to the stem (name01.png, name02.png, ...) so
// rename collisions read like the batch rename's numbered sequence
// instead of UniqueDestPath's `_N` upload suffixes.
func uniqueRenamePath(dir, filename string) string {
	dst := filepath.Join(dir, filename)
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return dst
	}
	stem := strings.TrimSuffix(filename, filepath.Ext(filename))
	ext := filepath.Ext(filename)
	for i := 1; ; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s%02d%s", stem, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}
