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

// SeriesCountsQuery returns the top series labels (by row count desc)
// across non-missing rows of any file type. Empty series strings are
// excluded - the index is partial on `series != ''` so the read hits
// it directly. Sorted Count desc, then alphabetical to make the
// sidebar deterministic.
func SeriesCountsQuery(database *db.DB, limit int) ([]SeriesCount, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := database.Read.Query(
		`SELECT series, COUNT(*) c FROM images
		 WHERE is_missing = 0 AND series != ''
		 GROUP BY series ORDER BY c DESC, series ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesCount
	for rows.Next() {
		var sc SeriesCount
		if err := rows.Scan(&sc.Series, &sc.Count); err != nil {
			return out, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// SourceCountsQuery returns the source-tree counts for the given database.
func SourceCountsQuery(database *db.DB) (SourceCounts, error) {
	var out SourceCounts
	rows, err := database.Read.Query(
		`SELECT source_type, COUNT(*) FROM images WHERE is_missing = 0 GROUP BY source_type`,
	)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var src string
		var n int
		if err := rows.Scan(&src, &n); err != nil {
			return out, err
		}
		switch src {
		case "a1111":
			out.A1111 += n
			out.AI += n
		case "comfyui":
			out.Comfyui += n
			out.AI += n
		case "a1111,comfyui":
			out.A1111 += n
			out.Comfyui += n
			out.AI += n
		case "none", "":
			out.None += n
		}
	}
	return out, rows.Err()
}

// Sync runs the three-phase gallery sync (walk, reconcile, mark-missing).
// progress receives (processed, total, message) tuples shaped to match
// jobs.Manager.Update so the handler can forward the call verbatim.
// maxFileSizeMB <= 0 disables the per-file cap.
func Sync(ctx context.Context, database *db.DB, galleryPath, thumbnailsPath string, maxFileSizeMB int, progress func(processed, total int, message string)) (SyncResult, error) {
	var result SyncResult

	maxBytes := int64(maxFileSizeMB) * 1024 * 1024

	// Phase 1: walk filesystem and build path -> sha256.
	progress(0, 0, "Phase 1: scanning filesystem...")
	type fileInfo struct {
		path     string
		sha256   string
		fileType string
		size     int64
		mtime    int64
	}
	var found []fileInfo

	// Preload (path, size, sha256, mtime_unix) so the walk skips hashing
	// files whose size + mtime haven't changed since the last sync. Re-
	// hashing every file dominates idle-sync time on 10k+ libraries.
	type knownEntry struct {
		size   int64
		sha256 string
		mtime  int64
	}
	known := map[string]knownEntry{}
	krows, kerr := database.Read.Query(
		`SELECT ip.path, i.file_size, i.sha256, ip.mtime_unix FROM image_paths ip JOIN images i ON i.id = ip.image_id`,
	)
	if kerr != nil {
		return result, fmt.Errorf("preloading known paths: %w", kerr)
	}
	for krows.Next() {
		var p, sha string
		var sz, mt int64
		if err := krows.Scan(&p, &sz, &sha, &mt); err != nil {
			krows.Close()
			return result, fmt.Errorf("scanning known paths: %w", err)
		}
		known[p] = knownEntry{size: sz, sha256: sha, mtime: mt}
	}
	if err := krows.Err(); err != nil {
		krows.Close()
		return result, fmt.Errorf("iterating known paths: %w", err)
	}
	krows.Close()

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
			return nil // unsupported type, skip
		}

		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if maxBytes > 0 && info.Size() > maxBytes {
			return nil // too large
		}

		var hash string
		mtimeUnix := info.ModTime().Unix()
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

		found = append(found, fileInfo{path: path, sha256: hash, fileType: ft, size: info.Size(), mtime: mtimeUnix})
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, fmt.Errorf("walking gallery: %w", err)
	}

	total := len(found)
	progress(0, total, "Phase 2: reconciling...")

	foundPaths := map[string]struct{}{}
	for _, fi := range found {
		foundPaths[fi.path] = struct{}{}
	}

	// Pre-load every (sha256 → id, canonical_path, is_missing) row so the
	// per-file lookup below is a map hit instead of N indexed SELECTs.
	// One full scan beats 25k single-row queries.
	type bySHARow struct {
		id            int64
		canonicalPath string
		isMissing     int
	}
	bySHA := map[string]bySHARow{}
	srows, sErr := database.Read.Query(
		`SELECT id, sha256, canonical_path, is_missing FROM images`,
	)
	if sErr != nil {
		return result, fmt.Errorf("preloading SHA index: %w", sErr)
	}
	for srows.Next() {
		var r bySHARow
		var sha string
		if err := srows.Scan(&r.id, &sha, &r.canonicalPath, &r.isMissing); err != nil {
			srows.Close()
			return result, fmt.Errorf("scanning SHA index: %w", err)
		}
		bySHA[sha] = r
	}
	if err := srows.Err(); err != nil {
		srows.Close()
		return result, fmt.Errorf("iterating SHA index: %w", err)
	}
	srows.Close()

	// reactivated counts silent is_missing=0 updates that don't bump any
	// SyncResult counter. Needed to decide whether tag counts must be
	// recomputed at the end of the sync.
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

		// In-place edit: same path on disk, but the freshly hashed SHA
		// differs from what image_paths last saw. The mtime gate at the
		// top of the walk forced a re-hash; here we apply the new SHA,
		// size, dimensions, and metadata to the existing image row so
		// tags survive the rewrite. The audit's "delete row and resync"
		// recovery becomes unnecessary; the operator's edit lands cleanly.
		if k, knownPath := known[fi.path]; knownPath && k.sha256 != fi.sha256 {
			if err := applyInPlaceEdit(database, galleryPath, thumbnailsPath, fi.path, fi.fileType, fi.sha256, fi.mtime, fi.size); err != nil {
				logx.Warnf("sync: in-place edit %q: %v", fi.path, err)
				continue
			}
			// Refresh the bySHA map: the old sha row may be gone (if the
			// path was its canonical and unique alias) or have its
			// canonical_path repointed; either way the new sha now points
			// at this path so a later same-SHA file falls into duplicate.
			var imgID int64
			if err := database.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, fi.sha256).Scan(&imgID); err == nil {
				bySHA[fi.sha256] = bySHARow{id: imgID, canonicalPath: fi.path, isMissing: 0}
			}
			delete(bySHA, k.sha256)
			known[fi.path] = knownEntry{size: fi.size, sha256: fi.sha256, mtime: fi.mtime}
			continue
		}

		row, ok := bySHA[fi.sha256]
		if ok {
			imgID := row.id
			canonPath := row.canonicalPath
			isMissing := row.isMissing

			// Persist the freshly-observed mtime on the touched row so the
			// next sync's unchanged-shortcut can fire. Cheap UPDATE keyed
			// on the UNIQUE path index.
			if _, wErr := database.Write.Exec(
				`UPDATE image_paths SET mtime_unix = ? WHERE path = ?`, fi.mtime, fi.path,
			); wErr != nil {
				logx.Warnf("sync: persist mtime for %q: %v", fi.path, wErr)
			}

			if canonPath == fi.path {
				if isMissing == 1 {
					if _, wErr := database.Write.Exec(`UPDATE images SET is_missing = 0 WHERE id = ?`, imgID); wErr != nil {
						logx.Warnf("sync: reactivate %d: %v", imgID, wErr)
					}
					reactivated++
				}
				continue
			}

			// image_paths.path is UNIQUE, so a known-path entry with a
			// matching SHA is unambiguously this image's alias.
			if k, knownAlias := known[fi.path]; knownAlias && k.sha256 == fi.sha256 {
				_, canonErr := os.Stat(canonPath)
				if canonErr != nil {
					// Canonical is gone; promote this alias to canonical.
					newFolder := FolderPath(galleryPath, fi.path)
					if _, wErr := database.Write.Exec(
						`UPDATE images SET canonical_path = ?, folder_path = ?, is_missing = 0 WHERE id = ?`,
						fi.path, newFolder, imgID,
					); wErr != nil {
						logx.Warnf("sync: promote alias %d: %v", imgID, wErr)
					}
					if _, wErr := database.Write.Exec(
						`UPDATE image_paths SET is_canonical = 1 WHERE image_id = ? AND path = ?`,
						imgID, fi.path,
					); wErr != nil {
						logx.Warnf("sync: set canonical path %d: %v", imgID, wErr)
					}
					if _, wErr := database.Write.Exec(
						`UPDATE image_paths SET is_canonical = 0 WHERE image_id = ? AND path = ?`,
						imgID, canonPath,
					); wErr != nil {
						logx.Warnf("sync: clear old canonical %d: %v", imgID, wErr)
					}
					result.Moved++
				} else if isMissing == 1 {
					if _, wErr := database.Write.Exec(`UPDATE images SET is_missing = 0 WHERE id = ?`, imgID); wErr != nil {
						logx.Warnf("sync: reactivate %d: %v", imgID, wErr)
					}
					reactivated++
				}
				continue
			}

			// New path for an existing SHA: a move if the canonical file
			// is gone, otherwise another copy/alias.
			_, canonErr := os.Stat(canonPath)
			if canonErr != nil {
				// Demote the previous canonical to alias and upsert the
				// new one so the prior path stays in image_paths instead
				// of being rewritten in place.
				newFolder := FolderPath(galleryPath, fi.path)
				if _, wErr := database.Write.Exec(
					`UPDATE images SET canonical_path = ?, folder_path = ?, is_missing = 0 WHERE id = ?`,
					fi.path, newFolder, imgID,
				); wErr != nil {
					logx.Warnf("sync: move %d: %v", imgID, wErr)
				}
				if _, wErr := database.Write.Exec(
					`UPDATE image_paths SET is_canonical = 0 WHERE image_id = ? AND is_canonical = 1`,
					imgID,
				); wErr != nil {
					logx.Warnf("sync: demote old canonical %d: %v", imgID, wErr)
				}
				if _, wErr := database.Write.Exec(
					`INSERT INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 1)
					 ON CONFLICT(path) DO UPDATE SET is_canonical = 1`,
					imgID, fi.path,
				); wErr != nil {
					logx.Warnf("sync: install new canonical %d: %v", imgID, wErr)
				}
				result.Moved++
			} else {
				// Canonical still exists; record this path as a duplicate.
				if _, wErr := database.Write.Exec(
					`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 0)`,
					imgID, fi.path,
				); wErr != nil {
					logx.Warnf("sync: insert alias path %d: %v", imgID, wErr)
				}
				result.Duplicates++
			}
		} else {
			// New file; reuse the Phase-1 hash so Ingest doesn't hash
			// twice on a fresh dump of 25k images.
			img, _, ingestErr := ingestWithHash(database, galleryPath, thumbnailsPath, fi.path, fi.fileType, fi.sha256, "")
			if ingestErr != nil {
				logx.Warnf("ingest failed for %q: %v", fi.path, ingestErr)
				continue
			}
			result.Added++
			// Record the new row in bySHA so a later same-SHA file in the
			// same walk falls into the duplicate branch.
			if img != nil {
				bySHA[fi.sha256] = bySHARow{id: img.ID, canonicalPath: fi.path, isMissing: 0}
			}
		}
	}

	// Honour cancellation between phases so the watcher's "sync running"
	// pause releases promptly on cancel.
	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	rows, err := database.Read.Query(
		`SELECT id, canonical_path FROM images WHERE is_missing = 0`,
	)
	if err != nil {
		return result, fmt.Errorf("querying existing images: %w", err)
	}
	var toMark []int64
	for rows.Next() {
		var id int64
		var path string
		// Skip rows that fail to scan rather than silently appending a zero id.
		// A zero id never matches a real row but the Removed count would still
		// drift up by one per scan failure, hiding driver issues.
		if err := rows.Scan(&id, &path); err != nil {
			logx.Warnf("sync: scan existing image row: %v", err)
			continue
		}
		if _, seen := foundPaths[path]; !seen {
			toMark = append(toMark, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("iterating existing images: %w", err)
	}

	// Chunked IN-list UPDATE: per-id UPDATEs through the single-writer
	// pool dominated Phase 3 on libraries where many files had gone away.
	const chunkSize = 500
	for start := 0; start < len(toMark); start += chunkSize {
		if ctx.Err() != nil {
			result.Removed = start
			return result, ctx.Err()
		}
		end := start + chunkSize
		if end > len(toMark) {
			end = len(toMark)
		}
		chunk := toMark[start:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		if _, wErr := database.Write.Exec(
			`UPDATE images SET is_missing = 1 WHERE id IN (`+placeholders+`)`, args...,
		); wErr != nil {
			logx.Warnf("sync: mark missing chunk: %v", wErr)
		}
	}
	result.Removed = len(toMark)

	if ctx.Err() != nil {
		return result, ctx.Err()
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
			archive.Close()
		}
	} else if !IsVideoType(fileType) {
		f, openErr := os.Open(path)
		if openErr == nil {
			if cfg2, _, decErr := imageDecodeConfig(f); decErr == nil {
				w, h := cfg2.Width, cfg2.Height
				imgWidth, imgHeight = &w, &h
			}
			f.Close()
		}
	}

	tx, err := database.Write.Begin()
	if err != nil {
		return fmt.Errorf("begin in-place edit tx: %w", err)
	}
	defer tx.Rollback()

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

	if err := Generate(path, thumbnailsPath, imageID, fileType); err != nil {
		logx.Warnf("in-place edit: thumbnail regen for %q: %v", path, err)
	}
	logx.Infof("sync: applied in-place edit to image id=%d at %q (sha %s)", imageID, path, newSHA)
	return nil
}

// DeleteEmptyFolderIfEmpty removes folderPath (relative to gallery root)
// when it's empty, then walks up the parent chain and removes any ancestors
// that become empty too. Stops at the gallery root.
func DeleteEmptyFolderIfEmpty(galleryPath, folderPath string) {
	if folderPath == "" {
		return
	}
	root := galleryPath
	cur := folderPath
	for cur != "" && cur != "." {
		absPath := filepath.Join(root, cur)
		entries, err := os.ReadDir(absPath)
		if err != nil {
			return
		}
		if len(entries) != 0 {
			return
		}
		if removeErr := os.Remove(absPath); removeErr != nil {
			logx.Warnf("removing empty folder %q: %v", absPath, removeErr)
			return
		}
		cur = filepath.Dir(cur)
		if cur == "." {
			cur = ""
		}
	}
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
	defer rows.Close()

	type folderCount struct {
		path  string
		count int
	}
	var flat []folderCount

	for rows.Next() {
		var fc folderCount
		if err := rows.Scan(&fc.path, &fc.count); err != nil {
			return nil, fmt.Errorf("scanning folder row: %w", err)
		}
		flat = append(flat, fc)
	}

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

	return []FolderNode{toValue(rootP)}, nil
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
