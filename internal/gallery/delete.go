package gallery

import (
	"fmt"
	"os"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/logx"
)

// DeleteImageResult holds metadata about a deleted image for post-delete cleanup.
type DeleteImageResult struct {
	CanonicalPath string
	FolderPath    string
	IsMissing     bool
}

// DeleteImage removes one image from the database, then cleans up files
// on disk. Two callbacks are injected (rather than direct package imports)
// to avoid internal/gallery → internal/tags / internal/relations cycles:
//   - removeAllTags clears the image_tags rows for id and prunes any
//     zero-usage tag that the image alone was carrying.
//   - onImageDelete (may be nil) fixes up relations-graph state that the
//     FK CASCADE can't reach - specifically dup_groups.original_image_id,
//     which is NOT NULL with no CASCADE so the parent DELETE would fail
//     while the image is still wearing the original badge.
//
// galleryPath gates the canonical-path unlink behind PathInside so a
// row whose canonical_path drifted outside the gallery root (a hand-
// edited DB, a renamed mount) can't trick the handler into removing
// arbitrary filesystem paths; sibling unlink paths in handlers_image_
// actions.go and handlers_maintenance.go already carry the same gate.
func DeleteImage(database *db.DB, galleryPath, thumbnailsPath string, id int64, removeAllTags func(int64) error, onImageDelete func(int64) error) (*DeleteImageResult, error) {
	var canonPath, folderPath, fileType string
	var isMissing int
	if err := database.Read.QueryRow(
		`SELECT canonical_path, folder_path, is_missing, file_type FROM images WHERE id = ?`, id,
	).Scan(&canonPath, &folderPath, &isMissing, &fileType); err != nil {
		return nil, fmt.Errorf("image not found: %w", err)
	}

	// removeAllTags prunes zero-usage tags scoped to this image's own tag
	// set, so we don't need a follow-up unscoped prune that could touch
	// unrelated rows. Surface the error rather than logging-and-continuing:
	// a partial removal would let the FK cascade clear image_tags while
	// leaving tags.usage_count drifting until the next RecalcCount.
	if err := removeAllTags(id); err != nil {
		return nil, fmt.Errorf("remove tags for image %d: %w", id, err)
	}

	if onImageDelete != nil {
		if err := onImageDelete(id); err != nil {
			return nil, fmt.Errorf("relations cleanup for image %d: %w", id, err)
		}
	}

	if _, err := database.Write.Exec(`DELETE FROM images WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("delete image row: %w", err)
	}

	os.Remove(ThumbnailPath(thumbnailsPath, id))
	os.Remove(HoverPath(thumbnailsPath, id))
	// Manga cache directory only exists for cbz rows. Skipping the
	// RemoveAll for static images cuts a per-image syscall in the bulk
	// delete and prune-missing hot paths.
	if fileType == "cbz" {
		RemoveMangaCache(thumbnailsPath, id)
	}

	result := &DeleteImageResult{
		CanonicalPath: canonPath,
		FolderPath:    folderPath,
		IsMissing:     isMissing == 1,
	}

	if !result.IsMissing && canonPath != "" {
		if galleryPath != "" && !PathInside(galleryPath, canonPath) {
			logx.Warnf("delete image %d: refusing to unlink %q outside gallery root %q", id, canonPath, galleryPath)
		} else if err := os.Remove(canonPath); err != nil && !os.IsNotExist(err) {
			logx.Warnf("delete image file %q: %v", canonPath, err)
		}
	}

	return result, nil
}
