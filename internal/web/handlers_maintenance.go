package web

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	meta "github.com/leqwin/monbooru/internal/metadata"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/tagger"
)

// pruneMissingImagesPost queues the missing-row sweep as a background
// `delete` job so a concurrent submit lands on the shared "A job is
// already running" path the other long maintenance handlers use, and
// progress flows through the same status bar.
func (s *Server) pruneMissingImagesPost(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db().Read.Query(`SELECT id FROM images WHERE is_missing = 1`)
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if scanErr := rows.Scan(&id); scanErr != nil {
			rows.Close()
			w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(scanErr.Error()) + `</div>`))
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	if iterErr := rows.Err(); iterErr != nil {
		w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(iterErr.Error()) + `</div>`))
		return
	}
	if len(ids) == 0 {
		w.Write([]byte(`<div class="flash flash-ok">Removed 0 missing image(s).</div>`))
		return
	}

	if err := s.jobs.Start(models.JobTypeDelete); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	thumbnailsPath := s.thumbnailsPath()
	tagSvc := s.tagSvc()
	active := s.Active()
	go func() {
		ctx := s.jobs.Context()
		total := len(ids)
		s.jobs.Update(0, total, fmt.Sprintf("pruning 0/%d…", total))
		done := 0
		removed := 0
		affectedTags, processed, cancelled, err := tagSvc.ChunkedDeleteWithTagRecalc(
			ctx, ids, "", nil,
			func(tx *sql.Tx, placeholders string, args []any) error {
				res, err := tx.Exec(`DELETE FROM images WHERE id IN (`+placeholders+`)`, args...)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n > 0 {
					removed += int(n)
				}
				return nil
			},
			func(chunk []int64) {
				for _, id := range chunk {
					os.Remove(gallery.ThumbnailPath(thumbnailsPath, id))
					os.Remove(gallery.HoverPath(thumbnailsPath, id))
					gallery.RemoveMangaCache(thumbnailsPath, id)
				}
				done += len(chunk)
				s.jobs.Update(done, total, fmt.Sprintf("pruning %d/%d…", done, total))
			},
		)
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		if len(affectedTags) > 0 {
			s.jobs.Update(processed, total, "reconciling tag counts…")
			if err := tagSvc.RecalcIDs(affectedTags); err != nil {
				logx.Warnf("prune-missing recalc IDs: %v", err)
			}
		}
		if removed > 0 && active != nil {
			active.InvalidateCaches()
		}
		if cancelled {
			s.jobs.Complete(fmt.Sprintf("prune cancelled (%d/%d removed)", removed, total))
			return
		}
		s.jobs.Complete(fmt.Sprintf("Removed %d missing image(s).", removed))
	}()
	w.Write([]byte(`<div class="flash flash-ok">Prune started.</div>`))
}

// pruneOrphanedThumbnailsPost queues the orphan sweep as a background
// `prune-thumbs` job so the request returns immediately and progress
// surfaces through the same /internal/job/status poll as the other
// long maintenance buttons. The body is shared with scheduledRemoveOrphans
// via runOrphanSweep.
func (s *Server) pruneOrphanedThumbnailsPost(w http.ResponseWriter, r *http.Request) {
	cx := s.Active()
	if cx == nil {
		w.Write([]byte(`<div class="flash flash-err">No active gallery.</div>`))
		return
	}
	if err := s.jobs.Start(models.JobTypePruneThumbs); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	go func() {
		ctx := s.jobs.Context()
		removed, processed, total, err := s.runOrphanSweep(ctx, cx)
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		if ctx.Err() != nil {
			s.jobs.Complete(fmt.Sprintf("orphan sweep cancelled (%d/%d scanned, %d removed)", processed, total, removed))
			return
		}
		s.jobs.Complete(fmt.Sprintf("Removed %d orphaned thumbnail(s).", removed))
	}()
	w.Write([]byte(`<div class="flash flash-ok">Thumbnail prune started.</div>`))
}

func (s *Server) recalcTagsPost(w http.ResponseWriter, r *http.Request) {
	updated, err := s.tagSvc().RecalcCount()
	s.Active().InvalidateCaches()
	if err != nil {
		w.Write([]byte(fmt.Sprintf(
			`<div class="flash flash-err">Recalc partially completed (%d updated): %s</div>`,
			updated, html.EscapeString(err.Error()),
		)))
		return
	}
	w.Write([]byte(fmt.Sprintf(
		`<div class="flash flash-ok">Recalculated %d tag count(s).</div>`,
		updated,
	)))
}

func (s *Server) mergeGeneralTagsPost(w http.ResponseWriter, r *http.Request) {
	merged, err := s.tagSvc().MergeGeneralIntoCategorized()
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	s.Active().InvalidateCaches()
	w.Write([]byte(fmt.Sprintf(
		`<div class="flash flash-ok">Merged %d general tag(s) into categorized counterparts.</div>`,
		merged,
	)))
}

func (s *Server) duplicatesListHandler(w http.ResponseWriter, r *http.Request) {
	// The endpoint is an htmx target on the Settings page; non-htmx
	// callers (refresh, paste, bookmark) get redirected to the Settings
	// page so the URL produces a useful page either way rather than a
	// naked <table> fragment.
	if !isHTMXRequest(r) {
		http.Redirect(w, r, "/settings#maintenance", http.StatusSeeOther)
		return
	}
	rows, err := s.db().Read.Query(`
		SELECT i.id, i.canonical_path, ip.id as path_id, ip.path
		FROM images i
		JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 0
		ORDER BY i.id, ip.id
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type aliasRow struct {
		ImageID       int64
		CanonicalPath string
		PathID        int64
		AliasPath     string
	}
	var aliases []aliasRow
	for rows.Next() {
		var a aliasRow
		if err := rows.Scan(&a.ImageID, &a.CanonicalPath, &a.PathID, &a.AliasPath); err != nil {
			logx.Warnf("duplicates list scan: %v", err)
			continue
		}
		aliases = append(aliases, a)
	}

	s.renderTemplate(w, "partials/duplicates_list.html", map[string]any{
		"Aliases": aliases,
	})
}

func (s *Server) removeDuplicatesPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}

	// Remove a specific subset when the form carries path_id values (one per
	// listed row), or every non-canonical row when the form carries
	// `all=true`. Refusing to fall through unless one of the two is set
	// keeps a stray POST with just a CSRF token from wiping the whole
	// library of alias files at once.
	selected := r.Form["path_id"]
	allFlag := r.FormValue("all") == "true"
	if len(selected) == 0 && !allFlag {
		w.Write([]byte(`<div class="flash flash-err">No duplicate paths selected.</div>`))
		return
	}

	var (
		rows *sql.Rows
		err  error
	)
	if allFlag {
		rows, err = s.db().Read.Query(`
			SELECT ip.id, ip.path
			FROM image_paths ip
			WHERE ip.is_canonical = 0
		`)
	} else {
		// Build an IN (?,?,...) query restricted to the supplied path_ids
		// that still aren't canonical - callers can't use this endpoint to
		// remove the canonical path for an image.
		placeholders := strings.Repeat("?,", len(selected))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, 0, len(selected))
		for _, s := range selected {
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				args = append(args, id)
			}
		}
		if len(args) == 0 {
			w.Write([]byte(`<div class="flash flash-err">No valid path_ids in request.</div>`))
			return
		}
		rows, err = s.db().Read.Query(
			`SELECT ip.id, ip.path FROM image_paths ip
			 WHERE ip.is_canonical = 0 AND ip.id IN (`+placeholders+`)`,
			args...,
		)
	}
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}

	type pathRow struct {
		ID   int64
		Path string
	}
	var paths []pathRow
	for rows.Next() {
		var p pathRow
		if err := rows.Scan(&p.ID, &p.Path); err != nil {
			logx.Warnf("remove duplicates scan: %v", err)
			continue
		}
		paths = append(paths, p)
	}
	rows.Close()

	removed := 0
	galleryRoot := s.galleryPath()
	for _, p := range paths {
		if _, err := s.db().Write.Exec(`DELETE FROM image_paths WHERE id = ?`, p.ID); err != nil {
			logx.Warnf("remove duplicate %d: %v", p.ID, err)
			continue
		}
		if p.Path != "" {
			// See unlinkUnderGallery: defense-in-depth so a stray
			// out-of-root path can't make this handler unlink files
			// outside the active gallery.
			if err := unlinkUnderGallery(galleryRoot, p.Path); err != nil {
				logx.Warnf("remove duplicate %q: %v", p.Path, err)
			}
		}
		removed++
	}
	w.Write([]byte(fmt.Sprintf(`<div class="flash flash-ok">Removed %d duplicate path(s).</div>`, removed)))
}

func (s *Server) rebuildThumbnailsPost(w http.ResponseWriter, r *http.Request) {
	if err := s.startRebuildThumbsJob(s.Active()); err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	w.Write([]byte(`<div class="flash flash-ok">Thumbnail rebuild started.</div>`))
}

// startRebuildThumbsJob queues a rebuild-thumbs job against the supplied
// gallery context, reading images and writing thumbnails from that gallery's
// own DB + thumbnails dir. Reused by the manual handler (active gallery) and
// the post-import hook (imported non-active gallery).
func (s *Server) startRebuildThumbsJob(cx *galleryCtx) error {
	if cx == nil || cx.DB == nil {
		return fmt.Errorf("no gallery context")
	}
	type imgRow struct {
		ID       int64
		Path     string
		FileType string
	}
	rows, err := cx.DB.Read.Query(
		`SELECT id, canonical_path, file_type FROM images WHERE is_missing = 0`)
	if err != nil {
		return err
	}
	var imgs []imgRow
	for rows.Next() {
		var img imgRow
		if err := rows.Scan(&img.ID, &img.Path, &img.FileType); err != nil {
			rows.Close()
			return err
		}
		imgs = append(imgs, img)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if err := s.jobs.Start(models.JobTypeRebuildThumbs); err != nil {
		return fmt.Errorf("a job is already running")
	}
	thumbnailsPath := cx.ThumbnailsPath
	galleryName := cx.Name
	go func() {
		ctx := s.jobs.Context()
		processed := 0
		total := len(imgs)
		for _, img := range imgs {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("[%s] rebuild cancelled (%d/%d rebuilt)", galleryName, processed, total))
				return
			}
			s.jobs.Update(processed, total, fmt.Sprintf("[%s] rebuilding %d/%d", galleryName, processed, total))
			if err := gallery.Generate(img.Path, thumbnailsPath, img.ID, img.FileType); err != nil {
				logx.Warnf("rebuild thumbnail for %d: %v", img.ID, err)
			}
			processed++
		}
		s.jobs.Complete(fmt.Sprintf("[%s] rebuilt %d thumbnail(s).", galleryName, processed))
	}()
	return nil
}

func (s *Server) vacuumDBPost(w http.ResponseWriter, r *http.Request) {
	// VACUUM holds the writer for tens of seconds on a multi-GB DB. Take
	// a job slot so the status bar reflects what's running and the
	// scheduler / a concurrent user-triggered job is refused with the
	// usual "a job is already running" message instead of silently
	// queueing behind the writer.
	if err := s.jobs.Start(models.JobTypeVacuum); err != nil {
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	// Run the (long) VACUUM + checkpoint sequence in a goroutine so the
	// HTTP request returns immediately (mirrors every other long-running
	// maintenance handler); running synchronously would block the
	// request thread for tens of seconds.
	go func() {
		beforeSize := dbFileSize(s.dbPath())
		if _, err := s.db().Write.Exec(`VACUUM`); err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		// VACUUM in WAL mode writes the rebuilt pages into the -wal file,
		// so the user sees no drop in on-disk footprint until the WAL is
		// consolidated. Truncate the WAL explicitly so the reclaimed
		// space is actually released.
		if _, err := s.db().Write.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			logx.Warnf("vacuum wal_checkpoint: %v", err)
		}
		afterSize := dbFileSize(s.dbPath())
		freed := beforeSize - afterSize
		if freed < 0 {
			freed = 0
		}
		s.jobs.Complete(fmt.Sprintf("Vacuumed (reclaimed %s).", humanBytesFmt(freed)))
	}()
	w.Write([]byte(`<div class="flash flash-ok">Vacuum started. Watch the status bar for the reclaimed-space report.</div>`))
}

// freeMemoryPost runs the on-demand version of runMemoryReclaim: trims
// every gallery's SQLite page cache, returns the Go heap, and SIGTERMs
// the auto-tagger worker so its CUDA libraries (when loaded) go with
// it. Refused while a job holds the manager because ShrinkMemory and
// ReleaseAll race the inference loop otherwise.
func (s *Server) freeMemoryPost(w http.ResponseWriter, r *http.Request) {
	if s.jobs.IsRunning() {
		w.Write([]byte(`<div class="flash flash-err">A job is running; try again when it finishes.</div>`))
		return
	}
	before := readVmRSS()
	s.ctxMu.RLock()
	ctxs := make([]*galleryCtx, 0, len(s.contexts))
	for _, cx := range s.contexts {
		ctxs = append(ctxs, cx)
	}
	s.ctxMu.RUnlock()
	for _, cx := range ctxs {
		if err := cx.DB.ShrinkMemory(context.Background()); err != nil {
			logx.Warnf("free memory: shrink %q: %v", cx.Name, err)
		}
	}
	debug.FreeOSMemory()
	tagger.ReleaseAll()
	after := readVmRSS()
	if before > 0 && after > 0 && before > after {
		w.Write([]byte(fmt.Sprintf(
			`<div class="flash flash-ok">Freed %s.</div>`,
			html.EscapeString(humanBytesFmt(int64(before-after))),
		)))
		return
	}
	w.Write([]byte(`<div class="flash flash-ok">Memory caches released.</div>`))
}

// dbFileSize returns the total on-disk footprint of the SQLite database -
// the main file plus the WAL and shared-memory sidecars. A post-VACUUM
// "reclaimed N" figure that only counts the main file misleads the user
// whenever the WAL holds the bulk of the pages (common after mass deletes).
func dbFileSize(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	return total
}

// humanBytesFmt formats a byte count with binary units. The template
// function "humanBytes" exposes the same body to template authors.
func humanBytesFmt(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func (s *Server) reExtractMetadataPost(w http.ResponseWriter, r *http.Request) {
	// Stream rows into a slice of lightweight structs so the DB cursor is closed
	// before the long-running goroutine starts. This avoids holding a read
	// connection open for the entire re-extraction job while keeping memory
	// usage proportional to the number of images (IDs + short paths only).
	type imgRow struct {
		ID       int64
		Path     string
		FileType string
		// Current persisted hashes; we use them to skip the rewrite when the
		// new extraction would produce the same generation_hash - most runs
		// on an unchanged library now turn into pure reads.
		sdHash    string
		comfyHash string
		source    string
	}

	rows, err := s.db().Read.Query(`
		SELECT i.id, i.canonical_path, i.file_type, i.source_type,
		       COALESCE(sm.generation_hash, ''),
		       COALESCE(cm.generation_hash, '')
		FROM images i
		LEFT JOIN sd_metadata sm ON sm.image_id = i.id
		LEFT JOIN comfyui_metadata cm ON cm.image_id = i.id
		WHERE i.is_missing = 0
	`)
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	var imgs []imgRow
	for rows.Next() {
		var img imgRow
		if err := rows.Scan(&img.ID, &img.Path, &img.FileType, &img.source, &img.sdHash, &img.comfyHash); err != nil {
			logx.Warnf("re-extract scan: %v", err)
			continue
		}
		imgs = append(imgs, img)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}

	if err := s.jobs.Start(models.JobTypeReExtract); err != nil {
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}

	database := s.db()
	go func() {
		ctx := s.jobs.Context()
		processed := 0
		updated := 0
		total := len(imgs)
		for _, img := range imgs {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("re-extraction cancelled (%d/%d processed, %d updated)", processed, total, updated))
				return
			}
			s.jobs.Update(processed, total, fmt.Sprintf("Processing %d/%d…", processed, total))
			sdMeta, comfyMeta, _ := meta.Extract(img.Path, img.FileType)

			sourceType := models.SourceTypeNone
			if sdMeta != nil && comfyMeta != nil {
				sourceType = models.SourceTypeBoth
			} else if sdMeta != nil {
				sourceType = models.SourceTypeA1111
			} else if comfyMeta != nil {
				sourceType = models.SourceTypeComfyUI
			}

			newSDHash := ""
			if sdMeta != nil {
				newSDHash = sdMeta.GenerationHash
			}
			newComfyHash := ""
			if comfyMeta != nil {
				newComfyHash = comfyMeta.GenerationHash
			}
			// Skip the delete+insert churn when the new extraction lines up
			// with what the DB already holds. Any pipeline change that adds
			// or drops fields changes the generation hash, so this stays
			// responsive to real metadata schema updates.
			if newSDHash == img.sdHash && newComfyHash == img.comfyHash && sourceType == img.source {
				processed++
				continue
			}

			// Single transaction per image so a mid-flight failure can't leave
			// images.source_type updated against a half-deleted metadata table
			// or a deleted-but-not-reinserted row.
			if err := reExtractApply(ctx, database, img.ID, sourceType, sdMeta, comfyMeta); err != nil {
				logx.Warnf("re-extract image %d: %v", img.ID, err)
				processed++
				continue
			}
			processed++
			updated++
		}
		s.jobs.Complete(fmt.Sprintf("Re-extracted metadata for %d image(s) (%d updated).", processed, updated))
	}()

	w.Write([]byte(`<div class="flash flash-ok">Re-extraction started.</div>`))
}

// reExtractApply commits a re-extracted image's source_type, deletes the
// previous SD/ComfyUI rows, and reinserts whichever the parser produced.
// All four steps run in one transaction so a partial failure (writer
// contention, ctx cancellation mid-statement) never leaves the row with
// updated source_type but missing metadata.
func reExtractApply(ctx context.Context, database *db.DB, imageID int64, sourceType string, sdMeta *models.SDMetadata, comfyMeta *models.ComfyUIMetadata) error {
	tx, err := database.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE images SET source_type = ? WHERE id = ?`, sourceType, imageID); err != nil {
		return fmt.Errorf("update source_type: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sd_metadata WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("delete sd_metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM comfyui_metadata WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("delete comfyui_metadata: %w", err)
	}
	if sdMeta != nil {
		sdMeta.ImageID = imageID
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sd_metadata (image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sdMeta.ImageID, sdMeta.Prompt, sdMeta.NegativePrompt, sdMeta.Model,
			sdMeta.Seed, sdMeta.Sampler, sdMeta.Steps, sdMeta.CFGScale, sdMeta.RawParams, sdMeta.GenerationHash,
		); err != nil {
			return fmt.Errorf("insert sd_metadata: %w", err)
		}
	}
	if comfyMeta != nil {
		comfyMeta.ImageID = imageID
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO comfyui_metadata (image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			comfyMeta.ImageID, comfyMeta.Prompt, comfyMeta.ModelCheckpoint,
			comfyMeta.Seed, comfyMeta.Sampler, comfyMeta.Steps, comfyMeta.CFGScale, comfyMeta.RawWorkflow, comfyMeta.GenerationHash,
		); err != nil {
			return fmt.Errorf("insert comfyui_metadata: %w", err)
		}
	}
	return tx.Commit()
}
