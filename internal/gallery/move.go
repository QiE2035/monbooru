package gallery

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/logx"
)

// MoveImageResult summarises a successful move so the caller can clean up the
// old parent directory and invalidate caches without re-querying.
type MoveImageResult struct {
	OldCanonicalPath string
	OldFolderPath    string
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

	if newFolder == oldFolder {
		return &MoveImageResult{
			OldCanonicalPath: oldCanonical,
			OldFolderPath:    oldFolder,
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
		tx.Rollback()
		return nil, fmt.Errorf("update images row: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE image_paths SET path = ? WHERE image_id = ? AND is_canonical = 1`,
		newPath, id,
	); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("update image_paths row: %w", err)
	}
	if err := os.Rename(oldCanonical, newPath); err != nil {
		tx.Rollback()
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
		OldCanonicalPath: oldCanonical,
		OldFolderPath:    oldFolder,
		NewCanonicalPath: newPath,
		NewFolderPath:    newFolder,
	}, nil
}
