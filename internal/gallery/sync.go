// Monbooru is a Linux-only deployment; path handling assumes forward slashes.
package gallery

import (
	"cmp"
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/tags"
)

// imageDecodeConfig is image.DecodeConfig spelled inline so the
// applyInPlaceEdit helper doesn't need a fresh import block.
var imageDecodeConfig = image.DecodeConfig

// SyncResult summarizes the outcome of a gallery sync.
type SyncResult struct {
	Added      int
	Removed    int
	Moved      int
	Duplicates int
}

// FolderNode represents a folder in the gallery tree.
type FolderNode struct {
	Path     string
	Name     string
	Count    int
	Depth    int
	Children []FolderNode
}

// SourceCounts holds the non-missing image counts feeding the sidebar's
// Source tree.
type SourceCounts struct {
	AI      int // a1111 + comfyui + combined
	A1111   int // standalone or combined
	Comfyui int // standalone or combined
	None    int
}

// SeriesCount is one row of the sidebar's Series section: a series
// label and the number of non-missing image rows tagged with it.
// Series spans every file type so operators can group images, videos,
// and archives under a shared label.
type SeriesCount struct {
	Series string
	Count  int
}

// SourceLabelCount is one row of the sidebar's Sources section: a
// per-image source label and the count of non-missing image rows
// carrying it. Mirrors SeriesCount; the column is partial-indexed on
// `source != ”` so the read hits it directly.
type SourceLabelCount struct {
	Source string
	Count  int
}

// SeriesCountsQuery returns the top series labels (by row count desc)
// across non-missing rows of any file type. Empty series strings are
// excluded - the index is partial on `series != ”` so the read hits
// it directly. Sorted Count desc, then alphabetical to make the
// sidebar deterministic.
func SeriesCountsQuery(database *db.DB, limit int) ([]SeriesCount, error) {
	return SeriesCountsUnderQuery(database, limit, nil)
}

// SourceLabelCountsQuery returns the top source labels (by row count
// desc) across non-missing rows. Empty source strings are excluded;
// idx_images_source is partial on `source != ”` so the seek skips
// untouched rows. Sorted Count desc, then alphabetical for a
// deterministic sidebar.
func SourceLabelCountsQuery(database *db.DB, limit int) ([]SourceLabelCount, error) {
	return SourceLabelCountsUnderQuery(database, limit, nil)
}

// SourceCountsQuery returns the source-tree counts for the given database.
func SourceCountsQuery(database *db.DB) (SourceCounts, error) {
	return SourceCountsUnderQuery(database, nil)
}

// syncFileInfo is one walk-result entry: the on-disk path plus the SHA
// either taken from the (size+mtime)-unchanged shortcut or freshly
// hashed during the walk.
type syncFileInfo struct {
	path     string
	sha256   string
	fileType string
	size     int64
	mtime    int64
}

// syncKnownEntry is one image_paths row preloaded for the unchanged-
// shortcut. The Phase 1 walker keys on (size, mtime); a hit avoids the
// re-hash on every untouched file.
type syncKnownEntry struct {
	size   int64
	sha256 string
	mtime  int64
}

// syncBySHARow is one images row preloaded for the reconcile lookup.
// One full scan beats N indexed SELECTs on a 25k-image library.
type syncBySHARow struct {
	id            int64
	canonicalPath string
	isMissing     int
}

// Sync runs the three-phase gallery sync (walk, reconcile, mark-missing).
// progress receives (processed, total, message) tuples shaped to match
// jobs.Manager.Update so the handler can forward the call verbatim.
// maxFileSizeMB <= 0 disables the per-file cap.
func Sync(ctx context.Context, database *db.DB, galleryPath, thumbnailsPath string, maxFileSizeMB int, progress func(processed, total int, message string)) (SyncResult, error) {
	var result SyncResult

	progress(0, 0, "Phase 1: scanning filesystem...")
	known, err := loadKnownPaths(database)
	if err != nil {
		return result, err
	}
	found, err := walkGalleryFiles(ctx, galleryPath, int64(maxFileSizeMB)*1024*1024, known)
	if err != nil {
		return result, err
	}

	total := len(found)
	progress(0, total, "Phase 2: reconciling...")

	foundPaths := make(map[string]struct{}, total)
	for _, fi := range found {
		foundPaths[fi.path] = struct{}{}
	}

	bySHA, err := loadImagesBySHA(database)
	if err != nil {
		return result, err
	}

	reactivated := 0
	for i, fi := range found {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		// Throttle progress emissions so Update's lock traffic stays
		// bounded on large libraries.
		if i%50 == 0 || i == total-1 {
			progress(i, total, "Phase 2: reconciling...")
		}
		reconcileFile(database, galleryPath, thumbnailsPath, fi, known, bySHA, &result, &reactivated)
	}

	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	toMark, err := selectImagesToMarkMissing(database, foundPaths)
	if err != nil {
		return result, err
	}
	removed, err := markImagesMissingChunked(ctx, database, toMark)
	result.Removed = removed
	if err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	// Drop non-canonical paths whose file the walk didn't find, so a move
	// or a deleted copy can't leave a phantom duplicate. Gated on a
	// non-empty walk so a not-yet-mounted gallery can't wipe live aliases;
	// existence comes from foundPaths, not a per-row stat.
	if len(found) > 0 {
		if err := pruneStaleAliasPaths(ctx, database, foundPaths); err != nil {
			return result, err
		}
	}

	// Recompute tag usage counts only when the reconcile touched something
	// that could change them. Duplicates alone never do, so an idle sync on
	// a large library skips this step.
	if result.Added > 0 || result.Removed > 0 || result.Moved > 0 || reactivated > 0 {
		progress(0, 0, "Recalculating tag counts...")
		tags.RecalcDB(database)
	}

	// Phase 3: report. Files gone from disk are flagged is_missing rather
	// than deleted, so "missing" reflects what sync actually did.
	progress(0, 0, fmt.Sprintf("Done: %d added, %d missing, %d moved, %d duplicates",
		result.Added, result.Removed, result.Moved, result.Duplicates))

	return result, nil
}

// loadKnownPaths preloads (path, size, sha256, mtime_unix) for every
// image_paths row, used by the walker's unchanged-shortcut.
func loadKnownPaths(database *db.DB) (map[string]syncKnownEntry, error) {
	known := map[string]syncKnownEntry{}
	rows, err := database.Read.Query(
		`SELECT ip.path, i.file_size, i.sha256, ip.mtime_unix FROM image_paths ip JOIN images i ON i.id = ip.image_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("preloading known paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p, sha string
		var sz, mt int64
		if err := rows.Scan(&p, &sz, &sha, &mt); err != nil {
			return nil, fmt.Errorf("scanning known paths: %w", err)
		}
		known[p] = syncKnownEntry{size: sz, sha256: sha, mtime: mt}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating known paths: %w", err)
	}
	return known, nil
}

// walkGalleryFiles walks galleryPath and returns one syncFileInfo per
// supported file. Hashes are taken from the known-shortcut when the
// (path, size, mtime) tuple matches; otherwise the file is hashed and
// ownership claimed.
func walkGalleryFiles(ctx context.Context, galleryPath string, maxBytes int64, known map[string]syncKnownEntry) ([]syncFileInfo, error) {
	var found []syncFileInfo
	err := filepath.WalkDir(galleryPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable dirs
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		ft, typeErr := DetectFileType(path)
		if typeErr != nil {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if maxBytes > 0 && info.Size() > maxBytes {
			return nil
		}
		mtimeUnix := info.ModTime().Unix()
		var hash string
		if k, ok := known[path]; ok && k.size == info.Size() && k.mtime != 0 && k.mtime == mtimeUnix {
			// Same path, size, and mtime: assume unchanged content. The
			// mtime gate catches the same-size in-place edit case the
			// size-only check missed; rows that predate the mtime column
			// (mtime=0) re-hash once and persist the real mtime back.
			hash = k.sha256
		} else {
			h, hashErr := HashFile(path)
			if hashErr != nil {
				logx.Warnf("hash failed for %q: %v", path, hashErr)
				return nil
			}
			hash = h
			// Only chown when we just hashed; files reused from `known`
			// were already claimed by a previous sync.
			ClaimOwnership(path)
		}
		found = append(found, syncFileInfo{path: path, sha256: hash, fileType: ft, size: info.Size(), mtime: mtimeUnix})
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("walking gallery: %w", err)
	}
	return found, nil
}

// loadImagesBySHA preloads (sha256 → id, canonical_path, is_missing) for
// every row in images. Phase 2 then looks up each walked file with one
// map hit instead of N indexed SELECTs.
func loadImagesBySHA(database *db.DB) (map[string]syncBySHARow, error) {
	bySHA := map[string]syncBySHARow{}
	rows, err := database.Read.Query(
		`SELECT id, sha256, canonical_path, is_missing FROM images`,
	)
	if err != nil {
		return nil, fmt.Errorf("preloading SHA index: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r syncBySHARow
		var sha string
		if err := rows.Scan(&r.id, &sha, &r.canonicalPath, &r.isMissing); err != nil {
			return nil, fmt.Errorf("scanning SHA index: %w", err)
		}
		bySHA[sha] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating SHA index: %w", err)
	}
	return bySHA, nil
}

// reconcileFile decides what to do with one walked file: in-place edit,
// SHA-match (same canonical, alias promotion, move, or duplicate), or
// brand-new ingest. Mutates result and maintains the known / bySHA
// maps so a later same-SHA walk entry falls into the right branch.
func reconcileFile(database *db.DB, galleryPath, thumbnailsPath string, fi syncFileInfo, known map[string]syncKnownEntry, bySHA map[string]syncBySHARow, result *SyncResult, reactivated *int) {
	// In-place edit: same path on disk, but the freshly hashed SHA
	// differs from what image_paths last saw. The mtime gate forced
	// a re-hash; apply the new SHA / size / dimensions / metadata to
	// the existing image row so tags survive the rewrite.
	if k, knownPath := known[fi.path]; knownPath && k.sha256 != fi.sha256 {
		if err := applyInPlaceEdit(database, galleryPath, thumbnailsPath, fi.path, fi.fileType, fi.sha256, fi.mtime, fi.size); err != nil {
			logx.Warnf("sync: in-place edit %q: %v", fi.path, err)
			return
		}
		// Refresh bySHA: the old sha row may be gone or repointed; the
		// new sha now anchors at this path so a later same-SHA file
		// falls into duplicate.
		var imgID int64
		if err := database.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, fi.sha256).Scan(&imgID); err == nil {
			bySHA[fi.sha256] = syncBySHARow{id: imgID, canonicalPath: fi.path, isMissing: 0}
		}
		delete(bySHA, k.sha256)
		known[fi.path] = syncKnownEntry{size: fi.size, sha256: fi.sha256, mtime: fi.mtime}
		return
	}

	row, ok := bySHA[fi.sha256]
	if !ok {
		reconcileNewFile(database, galleryPath, thumbnailsPath, fi, bySHA, result)
		return
	}
	reconcileExistingSHA(database, galleryPath, fi, row, known, result, reactivated)
}

// reconcileNewFile handles the new-SHA branch: a fresh ingest reusing
// the Phase-1 hash so Ingest doesn't hash twice.
func reconcileNewFile(database *db.DB, galleryPath, thumbnailsPath string, fi syncFileInfo, bySHA map[string]syncBySHARow, result *SyncResult) {
	img, _, ingestErr := ingestWithHash(database, galleryPath, thumbnailsPath, fi.path, fi.fileType, fi.sha256, "")
	if ingestErr != nil {
		logx.Warnf("ingest failed for %q: %v", fi.path, ingestErr)
		return
	}
	result.Added++
	if img != nil {
		bySHA[fi.sha256] = syncBySHARow{id: img.ID, canonicalPath: fi.path, isMissing: 0}
	}
}

// reconcileExistingSHA routes a SHA hit to one of the four sub-cases:
// same path (touch / reactivate), known alias (promote or reactivate),
// new path with vanished canonical (move), or new copy (duplicate).
func reconcileExistingSHA(database *db.DB, galleryPath string, fi syncFileInfo, row syncBySHARow, known map[string]syncKnownEntry, result *SyncResult, reactivated *int) {
	// Persist the freshly-observed mtime on the touched row so the next
	// sync's unchanged-shortcut can fire.
	if _, wErr := database.Write.Exec(
		`UPDATE image_paths SET mtime_unix = ? WHERE path = ?`, fi.mtime, fi.path,
	); wErr != nil {
		logx.Warnf("sync: persist mtime for %q: %v", fi.path, wErr)
	}

	if row.canonicalPath == fi.path {
		if row.isMissing == 1 {
			reactivateImage(database, row.id)
			*reactivated++
		}
		return
	}

	// image_paths.path is UNIQUE, so a known-path entry with a matching
	// SHA is unambiguously this image's alias.
	if k, knownAlias := known[fi.path]; knownAlias && k.sha256 == fi.sha256 {
		if _, canonErr := os.Stat(row.canonicalPath); canonErr != nil {
			promoteAliasToCanonical(database, galleryPath, fi.path, row)
			result.Moved++
		} else if row.isMissing == 1 {
			reactivateImage(database, row.id)
			*reactivated++
		}
		return
	}

	// New path for an existing SHA: a move if the canonical file is gone,
	// otherwise another copy / alias.
	if _, canonErr := os.Stat(row.canonicalPath); canonErr != nil {
		moveCanonical(database, galleryPath, fi.path, row.id)
		result.Moved++
		return
	}
	if _, wErr := database.Write.Exec(
		`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical, mtime_unix) VALUES (?, ?, 0, ?)`,
		row.id, fi.path, fi.mtime,
	); wErr != nil {
		logx.Warnf("sync: insert alias path %d: %v", row.id, wErr)
	}
	result.Duplicates++
}

// reactivateImage clears is_missing on a row that has just been
// re-observed on disk. Errors are logged - the sync still has Phase 3's
// mark-missing pass to land on, and a failed UPDATE here would be
// picked up by the next run.
func reactivateImage(database *db.DB, imageID int64) {
	if _, wErr := database.Write.Exec(`UPDATE images SET is_missing = 0 WHERE id = ?`, imageID); wErr != nil {
		logx.Warnf("sync: reactivate %d: %v", imageID, wErr)
	}
}

// promoteAliasToCanonical fires when an alias path's file is still on
// disk but the row's canonical_path is gone. The image row is repointed,
// the alias becomes canonical, and the vanished old canonical row is
// dropped so it can't resurface as a phantom duplicate.
func promoteAliasToCanonical(database *db.DB, galleryPath, newCanonical string, row syncBySHARow) {
	newFolder := FolderPath(galleryPath, newCanonical)
	if _, wErr := database.Write.Exec(
		`UPDATE images SET canonical_path = ?, folder_path = ?, is_missing = 0 WHERE id = ?`,
		newCanonical, newFolder, row.id,
	); wErr != nil {
		logx.Warnf("sync: promote alias %d: %v", row.id, wErr)
	}
	if _, wErr := database.Write.Exec(
		`UPDATE image_paths SET is_canonical = 1 WHERE image_id = ? AND path = ?`,
		row.id, newCanonical,
	); wErr != nil {
		logx.Warnf("sync: set canonical path %d: %v", row.id, wErr)
	}
	if _, wErr := database.Write.Exec(
		`DELETE FROM image_paths WHERE image_id = ? AND path = ?`,
		row.id, row.canonicalPath,
	); wErr != nil {
		logx.Warnf("sync: drop old canonical %d: %v", row.id, wErr)
	}
}

// moveCanonical points the image row at a new on-disk path when the
// previous canonical file has vanished. The vanished path is dropped
// from image_paths rather than kept as an alias, so it can't resurface
// as a phantom duplicate.
func moveCanonical(database *db.DB, galleryPath, newCanonical string, imageID int64) {
	newFolder := FolderPath(galleryPath, newCanonical)
	if _, wErr := database.Write.Exec(
		`UPDATE images SET canonical_path = ?, folder_path = ?, is_missing = 0 WHERE id = ?`,
		newCanonical, newFolder, imageID,
	); wErr != nil {
		logx.Warnf("sync: move %d: %v", imageID, wErr)
	}
	if _, wErr := database.Write.Exec(
		`DELETE FROM image_paths WHERE image_id = ? AND is_canonical = 1`,
		imageID,
	); wErr != nil {
		logx.Warnf("sync: drop old canonical %d: %v", imageID, wErr)
	}
	if _, wErr := database.Write.Exec(
		`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 1)
		 ON CONFLICT(path) DO UPDATE SET is_canonical = 1`,
		imageID, newCanonical,
	); wErr != nil {
		logx.Warnf("sync: install new canonical %d: %v", imageID, wErr)
	}
}

// selectImagesToMarkMissing returns the ids of non-missing rows whose
// canonical_path wasn't seen by Phase 1's walker.
func selectImagesToMarkMissing(database *db.DB, foundPaths map[string]struct{}) ([]int64, error) {
	rows, err := database.Read.Query(
		`SELECT id, canonical_path FROM images WHERE is_missing = 0`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying existing images: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var toMark []int64
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			// Skip rows that fail to scan rather than silently appending a
			// zero id. A zero id never matches a real row but the Removed
			// count would drift up by one per scan failure, hiding driver
			// issues.
			logx.Warnf("sync: scan existing image row: %v", err)
			continue
		}
		if _, seen := foundPaths[path]; !seen {
			toMark = append(toMark, id)
		}
	}
	if err := rows.Err(); err != nil {
		return toMark, fmt.Errorf("iterating existing images: %w", err)
	}
	return toMark, nil
}

// markImagesMissingChunked flips is_missing=1 for the supplied ids in
// 500-row chunks. Per-id UPDATEs through the single-writer pool used
// to dominate Phase 3 on libraries where many files had gone away.
// Returns the number of rows the database actually flipped: sums
// RowsAffected per chunk so a writer-contention error on a single
// chunk doesn't drift the user-visible "N missing" count out from
// under the operator. The first chunk-level error short-circuits the
// loop and surfaces alongside the partial total.
func markImagesMissingChunked(ctx context.Context, database *db.DB, ids []int64) (int, error) {
	marked := 0
	err := db.Chunked(ids, 500, func(chunk []int64) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		placeholders, args := db.InPlaceholders(chunk)
		res, wErr := database.Write.Exec(
			`UPDATE images SET is_missing = 1 WHERE id IN (`+placeholders+`)`, args...,
		)
		if wErr != nil {
			logx.Warnf("sync: mark missing chunk: %v", wErr)
			return fmt.Errorf("mark missing chunk: %w", wErr)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			marked += int(n)
		}
		return nil
	})
	return marked, err
}

// pruneStaleAliasPaths deletes non-canonical image_paths rows whose file
// is gone from disk, in 500-row chunks. foundPaths is the fast path: a
// path the walk observed is kept without a stat. A path it didn't observe
// is stat'd and removed only when genuinely absent, so a file merely
// skipped this pass (over the size cap, undetectable, transiently
// unreadable) keeps its row. Canonical rows are left to the is_missing pass.
func pruneStaleAliasPaths(ctx context.Context, database *db.DB, foundPaths map[string]struct{}) error {
	rows, err := database.Read.Query(`SELECT id, path FROM image_paths WHERE is_canonical = 0`)
	if err != nil {
		return fmt.Errorf("listing alias paths: %w", err)
	}
	var staleIDs []int64
	for rows.Next() {
		var id int64
		var path string
		if scanErr := rows.Scan(&id, &path); scanErr != nil {
			_ = rows.Close()
			return fmt.Errorf("scanning alias path: %w", scanErr)
		}
		if _, ok := foundPaths[path]; !ok {
			if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
				staleIDs = append(staleIDs, id)
			}
		}
	}
	if iterErr := rows.Err(); iterErr != nil {
		_ = rows.Close()
		return fmt.Errorf("iterating alias paths: %w", iterErr)
	}
	_ = rows.Close()

	return db.Chunked(staleIDs, 500, func(chunk []int64) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		placeholders, args := db.InPlaceholders(chunk)
		if _, wErr := database.Write.Exec(
			`DELETE FROM image_paths WHERE id IN (`+placeholders+`)`, args...,
		); wErr != nil {
			return fmt.Errorf("prune alias paths chunk: %w", wErr)
		}
		return nil
	})
}

// applyInPlaceEdit handles the case where sync re-hashed a known path
// and observed a different SHA. The image_id stays so user-curated tags
// survive; sha, size, dimensions, source_type, and side-table metadata
// (sd_metadata, comfyui_metadata) are refreshed to match the new bytes
// on disk, and the thumbnail is regenerated. The mtime gate at the top
// of the walk is what triggers entry; the corresponding image_paths
// row's mtime is updated here so the next sync's shortcut can fire.
func applyInPlaceEdit(database *db.DB, galleryPath, thumbnailsPath, path, fileType, newSHA string, newMtime, newSize int64) error {
	var imageID int64
	if err := database.Read.QueryRow(
		`SELECT image_id FROM image_paths WHERE path = ?`, path,
	).Scan(&imageID); err != nil {
		return fmt.Errorf("locate image for path %q: %w", path, err)
	}

	var imgWidth, imgHeight *int
	var pageCount *int
	if fileType == "cbz" {
		archive, openErr := OpenManga(path)
		if openErr == nil {
			if w, h, dimErr := archive.CoverDimensions(); dimErr == nil {
				imgWidth, imgHeight = &w, &h
			}
			pcVal := len(archive.Pages)
			pageCount = &pcVal
			_ = archive.Close()
		}
	} else if IsVideoType(fileType) {
		if w, h, ok := ProbeVideoDimensions(path); ok {
			imgWidth, imgHeight = &w, &h
		}
	} else {
		f, openErr := os.Open(path)
		if openErr == nil {
			if cfg2, _, decErr := imageDecodeConfig(f); decErr == nil {
				w, h := cfg2.Width, cfg2.Height
				imgWidth, imgHeight = &w, &h
			}
			_ = f.Close()
		}
	}

	tx, err := database.Write.Begin()
	if err != nil {
		return fmt.Errorf("begin in-place edit tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`UPDATE images SET sha256 = ?, file_size = ?, width = ?, height = ?, page_count = ? WHERE id = ?`,
		newSHA, newSize, toNullInt(imgWidth), toNullInt(imgHeight), toNullInt(pageCount), imageID,
	); err != nil {
		return fmt.Errorf("update images row: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE image_paths SET mtime_unix = ? WHERE path = ?`, newMtime, path,
	); err != nil {
		return fmt.Errorf("update image_paths mtime: %w", err)
	}
	// Drop the side-table metadata so the regenerated row reflects what
	// the new bytes actually carry. Re-extract is best-effort below.
	if _, err := tx.Exec(`DELETE FROM sd_metadata WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("clear sd_metadata: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM comfyui_metadata WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("clear comfyui_metadata: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit in-place edit: %w", err)
	}

	// A cbz whose bytes changed can have a different page count or order,
	// so the lazily-extracted raw page cache for the old contents is stale
	// (the reader serves an existing page file without revalidating it).
	// Drop the whole per-image cache; Generate below re-renders the page
	// thumbnails and the raw pages re-extract on the next read.
	if fileType == "cbz" {
		RemoveMangaCache(thumbnailsPath, imageID)
	}

	if err := Generate(path, thumbnailsPath, imageID, fileType); err != nil {
		logx.Warnf("in-place edit: thumbnail regen for %q: %v", path, err)
	} else if err := RecomputeAndStorePhash(context.Background(), database, imageID, thumbnailsPath); err != nil {
		logx.Warnf("in-place edit: phash recompute for %q: %v", path, err)
	}
	logx.Infof("sync: applied in-place edit to image id=%d at %q (sha %s)", imageID, path, newSHA)
	return nil
}

// FolderTree builds the folder tree from images. Each node's Count rolls
// up its own images plus every descendant's, so a parent with only
// subfolder content still shows a non-zero figure. Empty intermediate
// folders are included so the arborescence is complete.
func FolderTree(database *db.DB) ([]FolderNode, error) {
	rows, err := database.Read.Query(
		`SELECT COALESCE(folder_path, ''), COUNT(*) FROM images WHERE is_missing=0 GROUP BY folder_path ORDER BY folder_path`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	flat, err := scanFolderRows(rows)
	if err != nil {
		return nil, err
	}
	return buildFolderTree(flat), nil
}

// buildFolderTree turns a flat (path, count) list from the database
// into the rolled-up tree the sidebar renders.
func buildFolderTree(flat []folderCount) []FolderNode {
	// Add intermediate ancestor paths so the full arborescence shows.
	known := map[string]bool{"": true}
	for _, fc := range flat {
		known[fc.path] = true
	}
	var toAdd []folderCount
	for _, fc := range flat {
		if fc.path == "" {
			continue
		}
		segments := strings.Split(fc.path, "/")
		for i := 1; i < len(segments); i++ {
			ancestor := strings.Join(segments[:i], "/")
			if !known[ancestor] {
				known[ancestor] = true
				toAdd = append(toAdd, folderCount{path: ancestor, count: 0})
			}
		}
	}
	flat = append(flat, toAdd...)

	// Pointer-tree intermediate so parent-child wiring survives mutations.
	type pnode struct {
		FolderNode
		children []*pnode
	}

	rootP := &pnode{FolderNode: FolderNode{Path: "", Name: "(root)", Depth: 0}}
	pnodeMap := map[string]*pnode{"": rootP}

	// Sort lexicographically so parents always exist before children.
	slices.SortFunc(flat, func(a, b folderCount) int {
		return cmp.Compare(a.path, b.path)
	})

	for _, fc := range flat {
		if fc.path == "" {
			rootP.Count = fc.count
			continue
		}
		n := &pnode{FolderNode: FolderNode{
			Path:  fc.path,
			Name:  filepath.Base(fc.path),
			Count: fc.count,
			Depth: countSlashes(fc.path) + 1,
		}}
		pnodeMap[fc.path] = n

		parentPath := filepath.Dir(fc.path)
		if parentPath == "." {
			parentPath = ""
		}
		parent, ok := pnodeMap[parentPath]
		if !ok {
			parent = rootP
		}
		parent.children = append(parent.children, n)
	}

	// Post-order: roll descendant counts into each ancestor.
	var rollup func(p *pnode)
	rollup = func(p *pnode) {
		for _, c := range p.children {
			rollup(c)
			p.Count += c.Count
		}
	}
	rollup(rootP)

	// Pointer tree to value tree (deep copy).
	var toValue func(p *pnode) FolderNode
	toValue = func(p *pnode) FolderNode {
		n := p.FolderNode
		for _, c := range p.children {
			n.Children = append(n.Children, toValue(c))
		}
		return n
	}

	return []FolderNode{toValue(rootP)}
}

func countSlashes(s string) int {
	n := 0
	for _, c := range s {
		if c == '/' {
			n++
		}
	}
	return n
}
