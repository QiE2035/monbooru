package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/tags"
)

// implicationsDialogHandler renders the body of the implications dialog
// on the /tags page: one chip per direct implication with a delete
// button, plus a multi-tag input with autocomplete to declare new edges.
func (s *Server) implicationsDialogHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	// Opened as an htmx dialog from the /tags table; a non-htmx caller
	// (refresh, bookmark, shared link) gets the tag's row on the tags
	// page rather than a chrome-less fragment.
	if !isHTMXRequest(r) {
		http.Redirect(w, r, fmt.Sprintf("/tags#tag-row-%d", id), http.StatusSeeOther)
		return
	}
	parent, err := s.tagSvc().GetTag(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	imps, err := s.tagSvc().ListImplications(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Parent":       parent,
		"Implications": imps,
		"CSRFToken":    s.csrfToken(sessionFromContext(r.Context())),
	}
	s.renderTemplate(w, "partials/implications_dialog.html", data)
}

func (s *Server) addImplicationPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	parentID, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	rawInput := strings.TrimSpace(r.FormValue("implied_id"))
	if rawInput == "" {
		writeInlineFlash(w, "err", "Tag name is required.")
		return
	}

	// Parse the same way the detail-page tag input does so users get
	// space-separated multi-add and "category:name" / quoted spans.
	catTags, parseErrMsg := s.parseTagInput(rawInput)
	if parseErrMsg != "" {
		writeInlineFlash(w, "err", parseErrMsg)
		return
	}

	added := 0
	var failures []string
	for _, ct := range catTags {
		tag, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
		if err != nil {
			failures = append(failures, ct.name+": "+err.Error())
			continue
		}
		isNew, err := s.tagSvc().AddImplication(parentID, tag.ID)
		if err != nil {
			failures = append(failures, ct.name+": "+err.Error())
			continue
		}
		if isNew {
			added++
			s.startImplicationPropagation(parentID, tag.ID, "add")
		}
	}

	if added > 0 {
		// New implication targets may have been created via GetOrCreateTag,
		// so the cached tag count is stale until next render.
		s.Active().InvalidateCaches()
		// implication-added drives the dialog's after-request hook (re-fetch
		// body without closing the modal); monbooru:flash rides the shared
		// helper so the next /tags reload surfaces the green message above
		// the table.
		noun := "implication"
		if added != 1 {
			noun = "implications"
		}
		setFlashHeader(w, strconv.Itoa(added)+" "+noun+" added.", "ok",
			map[string]any{"implication-added": ""})
	}
	switch {
	case len(failures) == 0 && added > 0:
		w.WriteHeader(http.StatusNoContent)
	case len(failures) == 0 && added == 0:
		writeInlineFlash(w, "ok", "Already declared.")
	case added > 0:
		writeInlineFlash(w, "err", "Added "+strconv.Itoa(added)+". Failed: "+strings.Join(failures, "; "))
	default:
		writeInlineFlash(w, "err", strings.Join(failures, "; "))
	}
}

func (s *Server) removeImplicationDelete(w http.ResponseWriter, r *http.Request) {
	parentID, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	impliedID, ok := pathInt64(w, r, "impliedID")
	if !ok {
		return
	}
	if err := s.tagSvc().RemoveImplication(parentID, impliedID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.startImplicationPropagation(parentID, impliedID, "remove")
	// Seed the cross-navigation flash slot; the dialog stays open and the
	// /tags reload on close surfaces this above the table.
	setFlashHeader(w, "Implication removed.", "ok", nil)
	w.WriteHeader(http.StatusNoContent)
}

// startImplicationPropagation kicks off the background job that fans
// out (op="add") or sweeps (op="remove") the parent → implied edge
// across every image carrying parent. Skipped when a job is already
// running; the user can re-trigger by editing the implication again
// (the in-DB edge is independent of this propagation, so search and
// future adds still see it through the synchronous transitive walk).
func (s *Server) startImplicationPropagation(parentID, impliedID int64, op string) {
	if err := s.jobs.Start(models.JobTypeTag); err != nil {
		logx.Warnf("implication %s skipped: %v", op, err)
		return
	}
	go s.runImplicationPropagation(parentID, impliedID, op)
}

func (s *Server) runImplicationPropagation(parentID, impliedID int64, op string) {
	ctx := s.jobs.Context()
	const chunkSize = 500

	rows, err := s.db().Read.QueryContext(ctx,
		`SELECT image_id FROM image_tags WHERE tag_id = ? ORDER BY image_id`, parentID,
	)
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}
	ids, err := db.ScanIDs(rows)
	_ = rows.Close()
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	verb := "applying implication"
	if op == "remove" {
		verb = "removing implication"
	}

	// Pre-walk the implied tag's closure once for the remove path; it's
	// invariant across the propagation job. The chunked tx loop below
	// then passes the resolved slice to propagateRemoveImplication so a
	// big-fan-out parent doesn't pay an N×graph-walk inside the
	// writer-held chunk transaction.
	var removeClosure []int64
	if op == "remove" {
		closureTx, err := s.db().Read.Begin()
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		removeClosure, err = tags.TransitiveImpliedTx(closureTx, []int64{impliedID})
		_ = closureTx.Rollback()
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		removeClosure = append([]int64{impliedID}, removeClosure...)
	}

	processed, cancelled, err := chunkedJob(ctx, s.jobs, ids, chunkSize, verb, func(chunk []int64) error {
		tx, err := s.db().Write.Begin()
		if err != nil {
			return err
		}
		ratingCatID := s.tagSvc().RatingCategoryID()
		for _, imageID := range chunk {
			if op == "add" {
				if err := propagateAddImplication(tx, imageID, parentID, ratingCatID); err != nil {
					_ = tx.Rollback()
					return err
				}
			} else {
				if err := propagateRemoveImplication(tx, imageID, parentID, removeClosure); err != nil {
					_ = tx.Rollback()
					return err
				}
			}
		}
		return tx.Commit()
	})
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}
	if cancelled {
		s.jobs.Complete(fmt.Sprintf("%s cancelled (%d/%d)", verb, processed, len(ids)))
		return
	}

	if err := s.tagSvc().RecalcIDs([]int64{impliedID}); err != nil {
		logx.Warnf("implication propagation recalc: %v", err)
	}
	s.Active().InvalidateCaches()
	s.jobs.Complete(fmt.Sprintf("%s applied to %d image(s)", verb, processed))
}

// propagateAddImplication backfills implied rows for the parent on the
// given image, mirroring what addTagToImageTxReportingDup would have
// done if the implication had existed at the original add time.
// Existing rows are left alone; only fresh INSERTs get is_implied=1.
func propagateAddImplication(tx *sql.Tx, imageID, parentID, ratingCatID int64) error {
	var isAuto int
	err := tx.QueryRow(
		`SELECT is_auto FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, parentID,
	).Scan(&isAuto)
	if err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	return tags.ApplyImpliedFanoutTx(tx, imageID, parentID, ratingCatID, isAuto == 1)
}

// propagateRemoveImplication walks the supplied implied-tag closure
// on this image and drops any row whose only justification was the
// now-deleted edge. is_implied=0 rows (user-owned) and rows still
// implied by another parent on the image are preserved. The closure
// (impliedID plus its transitive children) is resolved once by the
// caller and reused across every image carrying the parent.
func propagateRemoveImplication(tx *sql.Tx, imageID, parentID int64, closure []int64) error {
	for _, id := range closure {
		var rowImplied int
		err := tx.QueryRow(
			`SELECT is_implied FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, id,
		).Scan(&rowImplied)
		if err == sql.ErrNoRows {
			continue
		} else if err != nil {
			return err
		}
		if rowImplied != 1 {
			continue
		}
		// Still implied by another parent on the image? Keep it.
		var alt int64
		err = tx.QueryRow(
			`SELECT ti.parent_tag_id
			 FROM tag_implications ti
			 JOIN image_tags it ON it.tag_id = ti.parent_tag_id
			 WHERE ti.implied_tag_id = ? AND it.image_id = ?
			 LIMIT 1`,
			id, imageID,
		).Scan(&alt)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err := tx.Exec(
			`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, id,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE tags SET usage_count = MAX(0, usage_count - 1) WHERE id = ?`, id,
		); err != nil {
			return err
		}
	}
	return nil
}
