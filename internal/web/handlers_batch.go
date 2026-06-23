package web

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/search"
)

// resolveBatchScope returns the image-id slice the caller's batch
// operates on, materialised from either the "selection" (checked ids)
// or "search" (everything matching the current query) form scope.
// Writes an error fragment and returns ok=false on bad input.
func (s *Server) resolveBatchScope(w http.ResponseWriter, r *http.Request, errLabel string) ([]int64, bool) {
	scope := strings.TrimSpace(r.FormValue("scope"))
	switch scope {
	case "selection":
		var ids []int64
		for _, idStr := range r.Form["ids"] {
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		return ids, true
	case "search":
		expr, parseErr := search.Parse(r.FormValue("q"))
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeInlineFlash(w, "err", "Could not parse search: "+parseErr.Error())
			return nil, false
		}
		// "act on current search" must mirror what the operator sees in
		// the gallery - including the cookie ceiling. Without this wrap
		// a SFW-ceiling-on operator clicking "delete all current search"
		// would wipe explicit rows they can't even see.
		expr = resolveCeiling(r, s.Active()).Apply(expr)
		var ids []int64
		err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
			ids = append(ids, t.ID)
			return nil
		})
		if err != nil {
			logx.Errorf("%s search: %v", errLabel, err)
			w.WriteHeader(http.StatusInternalServerError)
			writeInlineFlash(w, "err", "Search error.")
			return nil, false
		}
		return ids, true
	default:
		w.WriteHeader(http.StatusBadRequest)
		writeInlineFlash(w, "err", "scope must be search or selection")
		return nil, false
	}
}

func (s *Server) batchDelete(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	idStrs := r.Form["ids"]

	ids := make([]int64, 0, len(idStrs))
	for _, idStr := range idStrs {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		s.startBulkDelete(w, nil)
		return
	}
	// Single IN query feeds every target through one round-trip; a
	// 1000-checkbox selection used to pay 1000 reads here. The order
	// returned by the SELECT is undefined under SQLite without an
	// ORDER BY, so re-emit in the caller's input order via a map.
	placeholders, args := db.InPlaceholders(ids)
	rows, err := s.db().Read.Query(
		`SELECT id, canonical_path, folder_path, is_missing FROM images WHERE id IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		// startBulkDelete(nil) would 202 with nothing queued, which the
		// client reads as success - so surface the failure instead.
		logx.Warnf("batch delete: load targets: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		writeInlineFlash(w, "err", "Could not load the selected images.")
		return
	}
	defer func() { _ = rows.Close() }()
	byID := make(map[int64]search.DeleteTarget, len(ids))
	for rows.Next() {
		var t search.DeleteTarget
		var isMissing int
		if err := rows.Scan(&t.ID, &t.CanonicalPath, &t.FolderPath, &isMissing); err != nil {
			continue
		}
		t.IsMissing = isMissing == 1
		byID[t.ID] = t
	}
	if err := rows.Err(); err != nil {
		logx.Warnf("batch delete: scan targets: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		writeInlineFlash(w, "err", "Could not load the selected images.")
		return
	}
	targets := make([]search.DeleteTarget, 0, len(ids))
	for _, id := range ids {
		if t, ok := byID[id]; ok {
			targets = append(targets, t)
		}
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
		writeInlineFlash(w, "err", "Could not parse search: "+parseErr.Error())
		return
	}
	expr = resolveCeiling(r, s.Active()).Apply(expr)

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
		writeInlineFlash(w, "err", "Search error.")
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
	if !s.startJob(w, models.JobTypeDelete) {
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
				_ = os.Remove(gallery.ThumbnailPath(s.thumbnailsPath(), id))
				_ = os.Remove(gallery.HoverPath(s.thumbnailsPath(), id))
				gallery.RemoveMangaCache(s.thumbnailsPath(), id)
				if !t.IsMissing && t.CanonicalPath != "" {
					if err := os.Remove(t.CanonicalPath); err != nil && !os.IsNotExist(err) {
						logx.Warnf("bulk delete file %q: %v", t.CanonicalPath, err)
					}
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
		writeInlineFlash(w, "err", err.Error())
		return
	}

	scope := strings.TrimSpace(r.FormValue("scope"))
	var ids []int64
	if scope == "search" {
		expr, parseErr := search.Parse(r.FormValue("q"))
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeInlineFlash(w, "err", "Could not parse search: "+parseErr.Error())
			return
		}
		expr = resolveCeiling(r, s.Active()).Apply(expr)
		err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
			ids = append(ids, t.ID)
			return nil
		})
		if err != nil {
			logx.Errorf("batch-move search: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			writeInlineFlash(w, "err", "Search error.")
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

	if !s.startJob(w, models.JobTypeMove) {
		return
	}
	go s.runBatchMove(ids, targetFolder)
	w.WriteHeader(http.StatusAccepted)
}

// runBatchMove processes move targets one image at a time. Each MoveImage has
// its own small write txn + Rename; per-image failures are logged and counted
// but don't stop the run so a single unreadable file can't strand the rest.
func (s *Server) runBatchMove(ids []int64, targetFolder string) {
	ctx := s.jobs.Context()
	total := len(ids)
	moved, failed := 0, 0
	cancelled := false

	s.jobs.Update(0, total, fmt.Sprintf("moving 0/%d…", total))

	for i, id := range ids {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		if _, err := gallery.MoveImage(s.db(), s.galleryPath(), id, targetFolder); err != nil {
			logx.Warnf("batch move %d: %v", id, err)
			failed++
			continue
		}
		moved++
		if (i+1)%25 == 0 || i == total-1 {
			s.jobs.Update(i+1, total, fmt.Sprintf("moving %d/%d…", i+1, total))
		}
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
		writeInlineFlash(w, "err", "op must be add or remove")
		return
	}
	tagInput := strings.TrimSpace(r.FormValue("tags"))
	if tagInput == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeInlineFlash(w, "err", "No tags provided.")
		return
	}
	catTags, parseErrMsg := s.parseTagInput(tagInput)
	if parseErrMsg != "" {
		w.WriteHeader(http.StatusBadRequest)
		writeInlineFlash(w, "err", parseErrMsg)
		return
	}
	if len(catTags) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		writeInlineFlash(w, "err", "No tags to apply.")
		return
	}

	ids, ok := s.resolveBatchScope(w, r, "batch-tag")
	if !ok {
		return
	}

	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !s.startJob(w, models.JobTypeTag) {
		return
	}
	go s.runBatchTag(ids, op, catTags)
	w.WriteHeader(http.StatusAccepted)
}

// anyTagHasImplications reports whether any of the supplied tag ids
// appears as a parent in tag_implications. Used by runBatchTag to pick
// a smaller chunk size when the fan-out closure would otherwise pin
// the writer for tens of seconds per 500-row chunk.
func (s *Server) anyTagHasImplications(tagIDs []int64) bool {
	if len(tagIDs) == 0 {
		return false
	}
	placeholders, args := db.InPlaceholders(tagIDs)
	var n int
	if err := s.db().Read.QueryRow(
		`SELECT 1 FROM tag_implications WHERE parent_tag_id IN (`+placeholders+`) LIMIT 1`,
		args...,
	).Scan(&n); err != nil {
		return false
	}
	return n == 1
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
	applied := 0

	tagIDs := make([]int64, 0, len(resolved))
	for _, t := range resolved {
		tagIDs = append(tagIDs, t.id)
	}

	// Chunk size compresses to 100 when any resolved tag carries
	// implications so the per-row fan-out cost in addTagToImageTxReportingDup
	// doesn't hold the writer for tens of seconds on a 500-row chunk.
	// The 500-row default still applies to bare-add jobs where the
	// per-row work is just an INSERT OR IGNORE + usage_count bump.
	chunkSize := 500
	if op == "add" && s.anyTagHasImplications(tagIDs) {
		chunkSize = 100
	}
	processed, cancelled, err := chunkedJob(ctx, s.jobs, ids, chunkSize, label, func(chunk []int64) error {
		tx, err := s.db().Write.Begin()
		if err != nil {
			return err
		}
		var n int
		if op == "add" {
			n, err = s.tagSvc().BatchAddTagsTx(tx, chunk, tagIDs)
		} else {
			n, err = s.tagSvc().BatchRemoveTagsTx(tx, chunk, tagIDs)
		}
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		applied += n
		return nil
	})
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	if err := s.tagSvc().RecalcIDs(tagIDs); err != nil {
		logx.Warnf("batch-tag recalc IDs: %v", err)
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
		writeInlineFlash(w, "err", "mode must be user, auto, or all")
		return
	}
	taggerName := strings.TrimSpace(r.FormValue("tagger_name"))
	if taggerName != "" && mode != "auto" {
		// tagger_name only narrows mode=auto; user/all carry no tagger_name
		// concept. Reject silently to keep the predicate composition simple.
		taggerName = ""
	}

	ids, ok := s.resolveBatchScope(w, r, "batch-strip")
	if !ok {
		return
	}

	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !s.startJob(w, models.JobTypeTag) {
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
	s.startBatchToggleJob(w, r, "batch-inbox", s.runBatchInbox)
}

// runBatchInbox processes ids in chunks of 500 with one transaction per
// chunk. Each row's is_inbox flips; SQLite's `1 - is_inbox` does the
// per-row toggle in a single UPDATE so a mixed selection (some inbox,
// some archived) ends up cleanly inverted.
func (s *Server) runBatchInbox(ids []int64) {
	s.runBulkToggle(ids, "is_inbox", "inbox state", "inbox toggle", "Toggled inbox state")
}

// batchFavorite mirrors batchInbox for the is_favorited column: a
// per-row toggle that flips favorited rows to unfavorited and vice
// versa across the resolved scope.
func (s *Server) batchFavorite(w http.ResponseWriter, r *http.Request) {
	s.startBatchToggleJob(w, r, "batch-favorite", s.runBatchFavorite)
}

func (s *Server) runBatchFavorite(ids []int64) {
	s.runBulkToggle(ids, "is_favorited", "favorite state", "favorite toggle", "Toggled favorite state")
}

// startBatchToggleJob is the HTTP shell behind the per-row toggle
// handlers: parse form, resolve scope, claim the jobs lane, spawn,
// 202.
func (s *Server) startBatchToggleJob(w http.ResponseWriter, r *http.Request, scopeLabel string, run func([]int64)) {
	if !parseFormOK(w, r) {
		return
	}
	ids, ok := s.resolveBatchScope(w, r, scopeLabel)
	if !ok {
		return
	}
	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !s.startJob(w, models.JobTypeTag) {
		return
	}
	go run(ids)
	w.WriteHeader(http.StatusAccepted)
}

// runBulkToggle flips the named INTEGER column on every id via
// SQLite's `1 - col` toggle, chunked at 500 ids per write tx.
// progress/cancel/successNoun fill the per-chunk and completion
// summaries.
func (s *Server) runBulkToggle(ids []int64, column, progressNoun, cancelNoun, successNoun string) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(ids)

	processed, cancelled, err := chunkedJob(ctx, s.jobs, ids, chunkSize, "toggling "+progressNoun, func(chunk []int64) error {
		placeholders, args := db.InPlaceholders(chunk)
		tx, err := s.db().Write.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE images SET `+column+` = 1 - `+column+` WHERE id IN (`+placeholders+`)`, args...,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	s.Active().InvalidateCaches()

	if cancelled {
		s.jobs.Complete(fmt.Sprintf("%s cancelled (%d/%d toggled)", cancelNoun, processed, total))
		return
	}
	s.jobs.Complete(fmt.Sprintf("%s for %d image(s).", successNoun, processed))
}

// batchCollection adds or removes a collection label across every image
// in `scope=search` (q + sort + order) or every checked id in
// `scope=selection`. `mode=add` (default) files each image under the
// label, keeping any other memberships; `mode=remove` drops the label.
// One indexed write per 500-row chunk.
func (s *Server) batchCollection(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	collectionVal := strings.TrimSpace(r.FormValue("collection"))
	if collectionVal == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeInlineFlash(w, "err", "Collection label required.")
		return
	}
	if len(collectionVal) > maxExternalSourceLen {
		w.WriteHeader(http.StatusBadRequest)
		writeInlineFlash(w, "err", "Collection label too long.")
		return
	}
	mode := r.FormValue("mode")
	if mode != "remove" {
		mode = "add"
	}

	ids, ok := s.resolveBatchScope(w, r, "batch-collection")
	if !ok {
		return
	}

	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !s.startJob(w, models.JobTypeTag) {
		return
	}
	go s.runBatchCollection(ids, collectionVal, mode)
	w.WriteHeader(http.StatusAccepted)
}

// runBatchCollection adds or removes the label across the supplied id
// list in chunks. Add keeps existing memberships (a row with no home
// adopts the label); remove drops the membership and promotes another to
// home (or clears the mirror) for rows whose home was the removed label.
func (s *Server) runBatchCollection(ids []int64, label, mode string) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(ids)
	remove := mode == "remove"
	verb := "adding to collection"
	if remove {
		verb = "removing from collection"
	}

	processed, cancelled, err := chunkedJob(ctx, s.jobs, ids, chunkSize, verb, func(chunk []int64) error {
		placeholders, chunkArgs := db.InPlaceholders(chunk)
		labelArgs := append([]any{label}, chunkArgs...)
		tx, err := s.db().Write.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if remove {
			if _, err := tx.Exec(
				`DELETE FROM image_collections WHERE name = ? AND image_id IN (`+placeholders+`)`,
				labelArgs...,
			); err != nil {
				return err
			}
			// Rebind the home mirror for rows whose home was the removed label.
			if _, err := tx.Exec(
				`UPDATE images SET
				   series = COALESCE((SELECT name FROM image_collections c WHERE c.image_id = images.id
				                      ORDER BY c.position IS NULL, c.position, c.name LIMIT 1), ''),
				   series_order = (SELECT position FROM image_collections c WHERE c.image_id = images.id
				                   ORDER BY c.position IS NULL, c.position, c.name LIMIT 1)
				 WHERE series = ? COLLATE NOCASE AND id IN (`+placeholders+`)`,
				labelArgs...,
			); err != nil {
				return err
			}
			return tx.Commit()
		}
		if _, err := tx.Exec(
			`INSERT INTO image_collections (image_id, name, position)
			 SELECT id, ?, NULL FROM images WHERE id IN (`+placeholders+`)
			 ON CONFLICT(image_id, name) DO NOTHING`,
			labelArgs...,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE images SET series = ?, series_order = NULL
			 WHERE series = '' AND id IN (`+placeholders+`)`,
			labelArgs...,
		); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	s.Active().InvalidateCaches()

	if cancelled {
		s.jobs.Complete(fmt.Sprintf("collection cancelled (%d/%d processed)", processed, total))
		return
	}
	if remove {
		s.jobs.Complete(fmt.Sprintf("Removed %d image(s) from collection.", processed))
		return
	}
	s.jobs.Complete(fmt.Sprintf("Added %d image(s) to collection.", processed))
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
