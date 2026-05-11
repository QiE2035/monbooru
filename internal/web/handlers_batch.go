package web

import (
	"database/sql"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/search"
)

func (s *Server) batchDelete(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	idStrs := r.Form["ids"]

	var targets []search.DeleteTarget
	for _, idStr := range idStrs {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		t := search.DeleteTarget{ID: id}
		var isMissing int
		if err := s.db().Read.QueryRow(
			`SELECT canonical_path, folder_path, is_missing FROM images WHERE id = ?`, id,
		).Scan(&t.CanonicalPath, &t.FolderPath, &isMissing); err != nil {
			continue
		}
		t.IsMissing = isMissing == 1
		targets = append(targets, t)
	}

	s.startBulkDelete(w, targets)
}

func (s *Server) deleteSearchPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	queryStr := r.FormValue("q")

	expr, parseErr := search.Parse(queryStr)
	if parseErr != nil {
		logx.Warnf("delete-search parse: %v", parseErr)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">Could not parse search: ` +
			html.EscapeString(parseErr.Error()) + `</div>`))
		return
	}

	// Stream the matching targets off the cursor so very large result sets
	// don't allocate a second intermediate copy on top of whatever the
	// bulk-delete worker holds.
	var targets []search.DeleteTarget
	err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
		targets = append(targets, t)
		return nil
	})
	if err != nil {
		logx.Errorf("delete-search: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="flash flash-err">Search error.</div>`))
		return
	}

	s.startBulkDelete(w, targets)
}

// startBulkDelete kicks off a background delete job for the given targets and
// writes the response. The job reports progress via jobs.Manager; the client
// sees the running state in the top-right status bar.
func (s *Server) startBulkDelete(w http.ResponseWriter, targets []search.DeleteTarget) {
	if len(targets) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.jobs.Start(models.JobTypeDelete); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	go s.runBulkDelete(targets)
	w.WriteHeader(http.StatusAccepted)
}

// runBulkDelete processes targets in chunks with one transaction per chunk.
// The images schema cascades image_tags / image_paths / sd_metadata /
// comfyui_metadata on image delete, so a single DELETE FROM images clears the
// dependent rows. Tag usage counts are reconciled at the end by a targeted
// recalc scoped to the tag IDs actually touched by the cascade (collected
// from image_tags before the DELETE), avoiding a full-table Recalc
// that would walk every tag in the library.
func (s *Server) runBulkDelete(targets []search.DeleteTarget) {
	ctx := s.jobs.Context()
	total := len(targets)
	folders := map[string]struct{}{}
	byID := make(map[int64]search.DeleteTarget, len(targets))
	ids := make([]int64, 0, len(targets))
	for _, t := range targets {
		ids = append(ids, t.ID)
		byID[t.ID] = t
	}

	s.jobs.Update(0, total, fmt.Sprintf("deleting 0/%d…", total))
	done := 0
	affectedTags, processed, cancelled, err := s.tagSvc().ChunkedDeleteWithTagRecalc(
		ctx, ids, "", nil,
		func(tx *sql.Tx, placeholders string, args []any) error {
			_, err := tx.Exec(`DELETE FROM images WHERE id IN (`+placeholders+`)`, args...)
			return err
		},
		func(chunk []int64) {
			for _, id := range chunk {
				t := byID[id]
				os.Remove(gallery.ThumbnailPath(s.thumbnailsPath(), id))
				os.Remove(gallery.HoverPath(s.thumbnailsPath(), id))
				gallery.RemoveMangaCache(s.thumbnailsPath(), id)
				if !t.IsMissing && t.CanonicalPath != "" {
					if err := os.Remove(t.CanonicalPath); err != nil && !os.IsNotExist(err) {
						logx.Warnf("bulk delete file %q: %v", t.CanonicalPath, err)
					}
				}
				if !t.IsMissing && t.FolderPath != "" {
					folders[t.FolderPath] = struct{}{}
				}
			}
			done += len(chunk)
			s.jobs.Update(done, total, fmt.Sprintf("deleting %d/%d…", done, total))
		},
	)
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	if len(affectedTags) > 0 {
		s.jobs.Update(processed, total, "reconciling tag counts…")
		if err := s.tagSvc().RecalcIDs(affectedTags); err != nil {
			logx.Warnf("bulk delete recalc IDs: %v", err)
		}
	}

	for fp := range folders {
		gallery.DeleteEmptyFolderIfEmpty(s.galleryPath(), fp)
	}

	if processed > 0 {
		s.Active().InvalidateCaches()
	}
	if cancelled {
		s.jobs.Complete(fmt.Sprintf("delete cancelled (%d/%d deleted)", processed, total))
		return
	}
	s.jobs.Complete(fmt.Sprintf("Deleted %d image(s).", processed))
}

// batchMove kicks off a background `move` job that relocates the selected
// image IDs into the requested folder. Collisions on filename auto-suffix via
// UniqueDestPath. The watcher suppresses its events while this job runs so
// the Rename pairs don't flap the images as missing in transit.
//
// scope=search materialises ids by streaming the search result through
// search.ExecuteForDeleteStream (same idiom as batchTag and deleteSearchPost);
// scope=selection (or empty) reads ids[] from the form.
func (s *Server) batchMove(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	targetFolder := strings.TrimSpace(r.FormValue("folder"))

	// Validate the folder once up-front so the user sees the error inline
	// rather than as a per-image log entry once the job starts.
	if _, err := gallery.ResolveSubdir(s.galleryPath(), targetFolder); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}

	scope := strings.TrimSpace(r.FormValue("scope"))
	var ids []int64
	if scope == "search" {
		expr, parseErr := search.Parse(r.FormValue("q"))
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`<div class="flash flash-err">Could not parse search: ` +
				html.EscapeString(parseErr.Error()) + `</div>`))
			return
		}
		err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
			ids = append(ids, t.ID)
			return nil
		})
		if err != nil {
			logx.Errorf("batch-move search: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`<div class="flash flash-err">Search error.</div>`))
			return
		}
	} else {
		for _, idStr := range r.Form["ids"] {
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
	}

	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if err := s.jobs.Start(models.JobTypeMove); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	go s.runBatchMove(ids, targetFolder)
	w.WriteHeader(http.StatusAccepted)
}

// runBatchMove processes move targets one image at a time. Each MoveImage has
// its own small write txn + Rename; per-image failures are logged and counted
// but don't stop the run so a single unreadable file can't strand the rest.
// Empty source folders are cleaned up at the end, matching single-image move.
func (s *Server) runBatchMove(ids []int64, targetFolder string) {
	ctx := s.jobs.Context()
	total := len(ids)
	moved, failed := 0, 0
	cancelled := false
	// Track every observed source folder, not just successful ones. A
	// failed move can still be the last image in its source folder
	// (because earlier successful moves emptied it), and the post-loop
	// cleanup must consider those too. DeleteEmptyFolderIfEmpty is a
	// no-op on non-empty folders so over-eager calls are safe.
	observedSources := map[string]struct{}{}

	s.jobs.Update(0, total, fmt.Sprintf("moving 0/%d…", total))

	for i, id := range ids {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		res, err := gallery.MoveImage(s.db(), s.galleryPath(), id, targetFolder)
		if err != nil {
			logx.Warnf("batch move %d: %v", id, err)
			failed++
			// Pull the source folder from the row directly so we can
			// still try to clean it up. MoveImage rolls back on
			// failure but the row's folder_path is still known.
			var oldFolder string
			_ = s.db().Read.QueryRow(`SELECT folder_path FROM images WHERE id = ?`, id).Scan(&oldFolder)
			if oldFolder != "" {
				observedSources[oldFolder] = struct{}{}
			}
			continue
		}
		if res.OldFolderPath != res.NewFolderPath && res.OldFolderPath != "" {
			observedSources[res.OldFolderPath] = struct{}{}
		}
		moved++
		if (i+1)%25 == 0 || i == total-1 {
			s.jobs.Update(i+1, total, fmt.Sprintf("moving %d/%d…", i+1, total))
		}
	}

	for fp := range observedSources {
		gallery.DeleteEmptyFolderIfEmpty(s.galleryPath(), fp)
	}

	if moved > 0 {
		s.Active().InvalidateCaches()
	}
	if cancelled {
		s.jobs.Complete(fmt.Sprintf("move cancelled (%d/%d moved)", moved, total))
		return
	}
	summary := fmt.Sprintf("Moved %d image(s).", moved)
	if failed > 0 {
		summary = fmt.Sprintf("Moved %d image(s), %d failed.", moved, failed)
	}
	s.jobs.Complete(summary)
}

// batchTag kicks off a background `tag` job that adds (op=add) or removes
// (op=remove) a tag set across either every image in the current search
// (scope=search) or just the checked ids (scope=selection). The dialogs in
// gallery.html post the tags string verbatim (parsed server-side so
// category:name and quoted spans behave identically to the detail-page
// tag input). The op=remove path is the "specific tags by name" branch of
// #batch-strip-dialog; the bulk user/auto/all branches go through batchStrip.
func (s *Server) batchTag(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	op := strings.TrimSpace(r.FormValue("op"))
	if op != "add" && op != "remove" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">op must be add or remove</div>`))
		return
	}
	tagInput := strings.TrimSpace(r.FormValue("tags"))
	if tagInput == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">No tags provided.</div>`))
		return
	}
	catTags, parseErrMsg := s.parseTagInput(tagInput)
	if parseErrMsg != "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(parseErrMsg) + `</div>`))
		return
	}
	if len(catTags) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">No tags to apply.</div>`))
		return
	}

	scope := strings.TrimSpace(r.FormValue("scope"))
	var ids []int64
	switch scope {
	case "selection":
		for _, idStr := range r.Form["ids"] {
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
	case "search":
		expr, parseErr := search.Parse(r.FormValue("q"))
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`<div class="flash flash-err">Could not parse search: ` +
				html.EscapeString(parseErr.Error()) + `</div>`))
			return
		}
		// ExecuteForDeleteStream is just "iterate matching image ids"; reuse
		// it so the search → ids materialisation is identical to delete-all.
		err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
			ids = append(ids, t.ID)
			return nil
		})
		if err != nil {
			logx.Errorf("batch-tag search: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`<div class="flash flash-err">Search error.</div>`))
			return
		}
	default:
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">scope must be search or selection</div>`))
		return
	}

	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.jobs.Start(models.JobTypeTag); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	go s.runBatchTag(ids, op, catTags)
	w.WriteHeader(http.StatusAccepted)
}

// runBatchTag resolves each (catID, name) token to a tag id once up front
// (creating new tags on add, looking up only existing ones on remove) and
// applies the resolved set to every image in turn. Cancellable via the
// shared job context, identical to runBulkDelete's pattern.
func (s *Server) runBatchTag(ids []int64, op string, catTags []catTag) {
	type resolvedTag struct {
		id   int64
		name string
	}
	var resolved []resolvedTag
	if op == "add" {
		for _, ct := range catTags {
			t, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
			if err != nil {
				logx.Warnf("batch-tag get-or-create %q: %v", ct.name, err)
				continue
			}
			resolved = append(resolved, resolvedTag{t.ID, t.Name})
		}
	} else {
		for _, ct := range catTags {
			var id int64
			err := s.db().Read.QueryRow(
				`SELECT id FROM tags WHERE name = ? AND category_id = ?`, ct.name, ct.catID,
			).Scan(&id)
			if err != nil {
				continue // unknown tag; nothing to remove
			}
			resolved = append(resolved, resolvedTag{id, ct.name})
		}
	}
	if len(resolved) == 0 {
		s.jobs.Complete(fmt.Sprintf("nothing to %s (no matching tags)", op))
		return
	}

	label, summary := "tagging", "Tagged"
	if op == "remove" {
		label, summary = "untagging", "Untagged"
	}

	ctx := s.jobs.Context()
	total := len(ids)
	processed, applied := 0, 0
	cancelled := false
	affectedTags := map[int64]struct{}{}
	for _, t := range resolved {
		affectedTags[t.id] = struct{}{}
	}

	tagIDs := make([]int64, 0, len(resolved))
	for _, t := range resolved {
		tagIDs = append(tagIDs, t.id)
	}

	s.jobs.Update(0, total, fmt.Sprintf("%s 0/%d…", label, total))
	const chunkSize = 500
	for start := 0; start < total; start += chunkSize {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		end := start + chunkSize
		if end > total {
			end = total
		}
		chunk := ids[start:end]
		tx, err := s.db().Write.Begin()
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		var n int
		if op == "add" {
			n, err = s.tagSvc().BatchAddTagsTx(tx, chunk, tagIDs)
		} else {
			n, err = s.tagSvc().BatchRemoveTagsTx(tx, chunk, tagIDs)
		}
		if err != nil {
			tx.Rollback()
			logx.Warnf("batch-tag %s chunk [%d, %d): %v", op, start, end, err)
			s.jobs.Fail(err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		applied += n
		processed = end
		s.jobs.Update(processed, total, fmt.Sprintf("%s %d/%d…", label, processed, total))
	}

	if len(affectedTags) > 0 {
		tagIDs := make([]int64, 0, len(affectedTags))
		for id := range affectedTags {
			tagIDs = append(tagIDs, id)
		}
		if err := s.tagSvc().RecalcIDs(tagIDs); err != nil {
			logx.Warnf("batch-tag recalc IDs: %v", err)
		}
	}
	s.Active().InvalidateCaches()

	if cancelled {
		s.jobs.Complete(fmt.Sprintf("%s cancelled (%d/%d processed)", label, processed, total))
		return
	}
	s.jobs.Complete(fmt.Sprintf("%s %d image(s) (%d row change(s)).", summary, processed, applied))
}

// batchStrip kicks off a background `tag` job that strips tags by category
// (mode=user|auto|all) across either every image in the current search
// (scope=search) or the checked ids (scope=selection). Mirrors batchTag's
// scope dispatch; the per-mode predicate decides which image_tags rows the
// chunked DELETE in runBatchStrip touches. When mode=auto and tagger_name is
// set, the strip is further scoped to that tagger's output rows.
func (s *Server) batchStrip(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	mode := strings.TrimSpace(r.FormValue("mode"))
	switch mode {
	case "user", "auto", "all":
	default:
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">mode must be user, auto, or all</div>`))
		return
	}
	taggerName := strings.TrimSpace(r.FormValue("tagger_name"))
	if taggerName != "" && mode != "auto" {
		// tagger_name only narrows mode=auto; user/all carry no tagger_name
		// concept. Reject silently to keep the predicate composition simple.
		taggerName = ""
	}

	scope := strings.TrimSpace(r.FormValue("scope"))
	var ids []int64
	switch scope {
	case "selection":
		for _, idStr := range r.Form["ids"] {
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
	case "search":
		expr, parseErr := search.Parse(r.FormValue("q"))
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`<div class="flash flash-err">Could not parse search: ` +
				html.EscapeString(parseErr.Error()) + `</div>`))
			return
		}
		err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
			ids = append(ids, t.ID)
			return nil
		})
		if err != nil {
			logx.Errorf("batch-strip search: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`<div class="flash flash-err">Search error.</div>`))
			return
		}
	default:
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">scope must be search or selection</div>`))
		return
	}

	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.jobs.Start(models.JobTypeTag); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	go s.runBatchStrip(ids, mode, taggerName)
	w.WriteHeader(http.StatusAccepted)
}

// runBatchStrip processes targets in chunks of 500 with one transaction per
// chunk. The per-chunk pattern collects the distinct touched tag_ids before
// the DELETE so the post-pass RecalcIDs is scoped to the tags that
// actually changed (mirrors runBulkDelete). modePredicate narrows the strip:
//
//	user → AND is_auto = 0
//	auto → AND is_auto = 1                  (+ AND tagger_name = ? when scoped)
//	all  → (no extra predicate)
func (s *Server) runBatchStrip(ids []int64, mode, taggerName string) {
	var modePredicate, label, summary string
	var extraArgs []any
	switch mode {
	case "user":
		modePredicate = ` AND is_auto = 0`
		label, summary = "removing user tags", "Removed user tags from"
	case "auto":
		modePredicate = ` AND is_auto = 1`
		if taggerName != "" {
			modePredicate += ` AND tagger_name = ?`
			extraArgs = append(extraArgs, taggerName)
			label = fmt.Sprintf("removing %s auto-tags", taggerName)
			summary = fmt.Sprintf("Removed %s auto-tags from", taggerName)
		} else {
			label, summary = "removing auto-tags", "Removed auto-tags from"
		}
	case "all":
		modePredicate = ``
		label, summary = "removing tags", "Removed all tags from"
	}

	ctx := s.jobs.Context()
	total := len(ids)
	s.jobs.Update(0, total, fmt.Sprintf("%s 0/%d…", label, total))
	done := 0
	affectedTags, processed, cancelled, err := s.tagSvc().ChunkedDeleteWithTagRecalc(
		ctx, ids, modePredicate, extraArgs,
		func(tx *sql.Tx, placeholders string, args []any) error {
			_, err := tx.Exec(
				`DELETE FROM image_tags WHERE image_id IN (`+placeholders+`)`+modePredicate, args...)
			return err
		},
		func(chunk []int64) {
			done += len(chunk)
			s.jobs.Update(done, total, fmt.Sprintf("%s %d/%d…", label, done, total))
		},
	)
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	if len(affectedTags) > 0 {
		s.jobs.Update(processed, total, "reconciling tag counts…")
		if err := s.tagSvc().RecalcIDs(affectedTags); err != nil {
			logx.Warnf("batch-strip recalc IDs: %v", err)
		}
	}
	s.Active().InvalidateCaches()

	if cancelled {
		s.jobs.Complete(fmt.Sprintf("%s cancelled (%d/%d processed)", label, processed, total))
		return
	}
	s.jobs.Complete(fmt.Sprintf("%s %d image(s).", summary, processed))
}

// batchInbox kicks off a background `tag` job that flips is_inbox across
// every image in the current search (scope=search) or the checked ids
// (scope=selection). The op is always a per-row toggle: inbox rows
// become archived, archived become inbox. Mirrors batchTag's scope
// dispatch and runBulkDelete's chunked-tx shape.
func (s *Server) batchInbox(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}

	scope := strings.TrimSpace(r.FormValue("scope"))
	var ids []int64
	switch scope {
	case "selection":
		for _, idStr := range r.Form["ids"] {
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
	case "search":
		expr, parseErr := search.Parse(r.FormValue("q"))
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`<div class="flash flash-err">Could not parse search: ` +
				html.EscapeString(parseErr.Error()) + `</div>`))
			return
		}
		err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
			ids = append(ids, t.ID)
			return nil
		})
		if err != nil {
			logx.Errorf("batch-inbox search: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`<div class="flash flash-err">Search error.</div>`))
			return
		}
	default:
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">scope must be search or selection</div>`))
		return
	}

	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.jobs.Start(models.JobTypeTag); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	go s.runBatchInbox(ids)
	w.WriteHeader(http.StatusAccepted)
}

// runBatchInbox processes ids in chunks of 500 with one transaction per
// chunk. Each row's is_inbox flips; SQLite's `1 - is_inbox` does the
// per-row toggle in a single UPDATE so a mixed selection (some inbox,
// some archived) ends up cleanly inverted.
func (s *Server) runBatchInbox(ids []int64) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(ids)
	processed := 0
	cancelled := false

	s.jobs.Update(0, total, fmt.Sprintf("toggling inbox state 0/%d…", total))

	for start := 0; start < total; start += chunkSize {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		end := start + chunkSize
		if end > total {
			end = total
		}
		chunk := ids[start:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}

		tx, err := s.db().Write.Begin()
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		if _, err := tx.Exec(
			`UPDATE images SET is_inbox = 1 - is_inbox WHERE id IN (`+placeholders+`)`, args...,
		); err != nil {
			tx.Rollback()
			s.jobs.Fail(err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		processed = end
		s.jobs.Update(processed, total, fmt.Sprintf("toggling inbox state %d/%d…", processed, total))
	}

	s.Active().InvalidateCaches()

	if cancelled {
		s.jobs.Complete(fmt.Sprintf("inbox toggle cancelled (%d/%d toggled)", processed, total))
		return
	}
	s.jobs.Complete(fmt.Sprintf("Toggled inbox state for %d image(s).", processed))
}

// batchCollection assigns a collection label to every image in
// `scope=search` (q + sort + order) or every checked id in
// `scope=selection`. Mirrors batchInbox's id-collection shape; the
// per-chunk UPDATE writes the same label to every row, so a 100k-row
// job is one indexed write per 500-row chunk. The underlying column
// is still named `series` for schema stability.
func (s *Server) batchCollection(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	collectionVal := strings.TrimSpace(r.FormValue("collection"))
	if len(collectionVal) > maxExternalSourceLen {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">Collection label too long.</div>`))
		return
	}

	scope := strings.TrimSpace(r.FormValue("scope"))
	var ids []int64
	switch scope {
	case "selection":
		for _, idStr := range r.Form["ids"] {
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
	case "search":
		expr, parseErr := search.Parse(r.FormValue("q"))
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`<div class="flash flash-err">Could not parse search: ` +
				html.EscapeString(parseErr.Error()) + `</div>`))
			return
		}
		err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
			ids = append(ids, t.ID)
			return nil
		})
		if err != nil {
			logx.Errorf("batch-collection search: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`<div class="flash flash-err">Search error.</div>`))
			return
		}
	default:
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">scope must be search or selection</div>`))
		return
	}

	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.jobs.Start(models.JobTypeTag); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	go s.runBatchCollection(ids, collectionVal)
	w.WriteHeader(http.StatusAccepted)
}

// runBatchCollection writes the collection label across the supplied id
// list in chunks. Every row gets the same label; series_order is left
// untouched.
func (s *Server) runBatchCollection(ids []int64, label string) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(ids)
	processed := 0
	cancelled := false

	s.jobs.Update(0, total, fmt.Sprintf("setting collection 0/%d…", total))

	for start := 0; start < total; start += chunkSize {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		end := start + chunkSize
		if end > total {
			end = total
		}
		chunk := ids[start:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]

		tx, err := s.db().Write.Begin()
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		args := make([]any, 0, 1+len(chunk))
		args = append(args, label)
		for _, id := range chunk {
			args = append(args, id)
		}
		if _, err := tx.Exec(
			`UPDATE images SET series = ? WHERE id IN (`+placeholders+`)`, args...,
		); err != nil {
			tx.Rollback()
			s.jobs.Fail(err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		processed = end
		s.jobs.Update(processed, total, fmt.Sprintf("setting collection %d/%d…", processed, total))
	}

	s.Active().InvalidateCaches()

	if cancelled {
		s.jobs.Complete(fmt.Sprintf("series cancelled (%d/%d set)", processed, total))
		return
	}
	if label == "" {
		s.jobs.Complete(fmt.Sprintf("Cleared series on %d image(s).", processed))
		return
	}
	s.jobs.Complete(fmt.Sprintf("Set collection on %d image(s).", processed))
}

func (s *Server) deleteFolderPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	folderPath := r.FormValue("folder")

	if folderPath == "" {
		http.Error(w, "invalid folder path", http.StatusBadRequest)
		return
	}

	// Reuse the gallery-root validator from the upload path: filepath.Rel
	// rejects sibling directories that share the gallery prefix (e.g.
	// `/data/gallery_backup`) without false-positiving on `foo..bar`.
	absPath, err := gallery.ResolveSubdir(s.galleryPath(), folderPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := os.Remove(absPath); err != nil {
		// Treat "already gone" as success so a stale UI can re-issue the
		// delete without an error toast. ENOTEMPTY (raised by Linux when
		// the directory still has children) maps to the same 409 the UI
		// already surfaces. Anything else is a real failure - permission
		// denied, busy, etc. - and must not silently masquerade as a
		// successful redirect.
		switch {
		case os.IsNotExist(err):
			// nothing to do - fall through to the success redirect
		case errors.Is(err, syscall.ENOTEMPTY):
			http.Error(w, "directory not empty", http.StatusConflict)
			return
		default:
			logx.Warnf("delete folder %q: %v", absPath, err)
			http.Error(w, "could not delete folder: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
