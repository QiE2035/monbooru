package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/tags"
)

// ptrLookupBatch caps names per monloader graph query (its request limit).
const ptrLookupBatch = 500

// ptrLookupSearchPost sweeps every tag matching the current /tags filter
// through monloader's PTR graph and adds the aliases / implications it
// knows. Mirrors deleteTagsSearchPost: resolve the id set up front, run a
// background "tag" job, return 202 so the client watches the job status bar.
func (s *Server) ptrLookupSearchPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	ids, ok := s.startTagScopeJob(w, r)
	if !ok {
		return
	}
	go s.runPTRTagLookup(ids)
	w.WriteHeader(http.StatusAccepted)
}

// ptrLookupTagPost runs the same sweep for one tag row.
func (s *Server) ptrLookupTagPost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !s.startJob(w, models.JobTypeTag) {
		return
	}
	go s.runPTRTagLookup([]int64{id})
	w.WriteHeader(http.StatusAccepted)
}

// ptrLookupCand is one tag headed for monloader's PTR graph, with its name
// in monbooru form: bare for general, category:name otherwise.
type ptrLookupCand struct {
	id   int64
	name string
}

// ptrLookupCands resolves tag ids into sweep candidates, dropping alias rows
// and rating tags (aliases have a canonical that is itself swept; rating rows
// are immutable).
func (s *Server) ptrLookupCands(ids []int64) ([]ptrLookupCand, error) {
	var cands []ptrLookupCand
	for start := 0; start < len(ids); start += ptrLookupBatch {
		placeholders, args := db.InPlaceholders(ids[start:min(start+ptrLookupBatch, len(ids))])
		rows, err := s.db().Read.Query(
			`SELECT t.id, t.name, c.name FROM tags t JOIN tag_categories c ON c.id = t.category_id
			 WHERE t.id IN (`+placeholders+`) AND t.is_alias = 0 AND c.name != 'rating'`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var name, cat string
			if err := rows.Scan(&id, &name, &cat); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if cat != "general" {
				name = cat + ":" + name
			}
			cands = append(cands, ptrLookupCand{id: id, name: name})
		}
		_ = rows.Close()
	}
	return cands, nil
}

// runPTRTagLookup queries monloader's PTR graph for each candidate tag in
// ptrLookupBatch-sized calls. Tags created by the implication fan-in are
// swept in follow-up rounds so their aliases and implications land in the
// same run; each id is swept at most once, so the rounds terminate.
func (s *Server) runPTRTagLookup(ids []int64) {
	ctx := s.jobs.Context()

	cands, err := s.ptrLookupCands(ids)
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}

	total := len(cands)
	aliases, implications, unknown, processed := 0, 0, 0, 0
	cancelled := false
	unavailable := false
	impliedTouched := map[int64]struct{}{}
	fanOutParents := map[int64]struct{}{}
	swept := make(map[int64]struct{}, total)
	s.jobs.Update(0, total, "PTR lookup…")
	for len(cands) > 0 && !cancelled && !unavailable {
		createdTags := map[int64]struct{}{}
		for start := 0; start < len(cands) && !cancelled; start += ptrLookupBatch {
			if ctx.Err() != nil {
				cancelled = true
				break
			}
			chunk := cands[start:min(start+ptrLookupBatch, len(cands))]
			names := make([]string, len(chunk))
			for i, c := range chunk {
				names[i] = c.name
			}
			results, err := s.ptrTagLookup(ctx, names)
			if errors.Is(err, errPTRUnavailable) {
				// The PTR going away mid-sweep is a degraded stop, not a
				// failure: keep what already applied and report how far it got,
				// like the batch lookup's per-image skip.
				unavailable = true
				break
			}
			if err != nil {
				s.jobs.Fail("PTR lookup failed: " + err.Error())
				return
			}
			for _, c := range chunk {
				swept[c.id] = struct{}{}
				info := results[c.name]
				if !info.Known {
					unknown++
				} else {
					a, i := s.applyPTRTagInfo(c.id, info, impliedTouched, fanOutParents, createdTags)
					aliases += a
					implications += i
				}
				processed++
			}
			s.jobs.Update(processed, total, "PTR lookup…")
		}
		var next []int64
		for id := range createdTags {
			if _, ok := swept[id]; !ok {
				next = append(next, id)
			}
		}
		if cands, err = s.ptrLookupCands(next); err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		total += len(cands)
	}

	// Backfill the new edges onto images already carrying the parents. This
	// runs inside the sweep's own job because startImplicationPropagation
	// can't start one while the runner is held; one pass per parent covers
	// all its new edges (the fan-out applies the full implied closure).
	for parentID := range fanOutParents {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		if err := s.fanOutImplicationsInline(ctx, parentID); err != nil {
			logx.Warnf("ptr lookup fan-out for tag %d: %v", parentID, err)
		}
	}
	if len(impliedTouched) > 0 {
		touched := make([]int64, 0, len(impliedTouched))
		for id := range impliedTouched {
			touched = append(touched, id)
		}
		if err := s.tagSvc().RecalcIDs(touched); err != nil {
			logx.Warnf("ptr lookup recalc: %v", err)
		}
	}
	s.Active().InvalidateCaches()

	msg := fmt.Sprintf("PTR: added %d alias(es) and %d implication(s) across %d tag(s)", aliases, implications, processed)
	if unknown > 0 {
		msg += fmt.Sprintf("; %d unknown to the PTR", unknown)
	}
	msg += "."
	if unavailable && !cancelled {
		s.jobs.Complete(fmt.Sprintf("PTR lookup stopped at %d/%d: the PTR became unavailable on monloader. %s", processed, total, msg))
		return
	}
	s.finishJob(nil, cancelled, fmt.Sprintf("PTR lookup cancelled (%d/%d processed)", processed, total), msg)
}

// applyPTRTagInfo adds the PTR's aliases and implications for one canonical
// tag. An alias is created only when its name is unused in its category, so
// the operator's catalog always wins over the PTR's preferred spelling.
// Implication edges rely on AddImplication's cycle and alias guards and are
// logged and skipped on failure so one bad edge can't abort the sweep; any
// tag GetOrCreateTag had to create lands in createdTags for the caller to
// sweep too.
func (s *Server) applyPTRTagInfo(tagID int64, info ptrTagInfo, impliedTouched, fanOutParents, createdTags map[int64]struct{}) (aliases, implications int) {
	for _, name := range info.Aliases {
		catID, bare, ok := s.splitCategoryTag(name)
		if !ok {
			continue
		}
		normalized, err := tags.ValidateTagName(bare)
		if err != nil {
			continue
		}
		var exists int
		if err := s.db().Read.QueryRow(
			`SELECT COUNT(*) FROM tags WHERE name = ? AND category_id = ?`, normalized, catID,
		).Scan(&exists); err != nil || exists > 0 {
			continue
		}
		if _, err := s.tagSvc().CreateAliasFrom(normalized, catID, tagID, "ptr"); err != nil {
			logx.Warnf("ptr alias %q: %v", name, err)
			continue
		}
		aliases++
	}
	for _, name := range info.Implications {
		catID, bare, ok := s.splitCategoryTag(name)
		if !ok {
			continue
		}
		normalized, err := tags.ValidateTagName(bare)
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		var exists int
		if err := s.db().Read.QueryRow(
			`SELECT COUNT(*) FROM tags WHERE name = ? AND category_id = ?`, normalized, catID,
		).Scan(&exists); err != nil {
			continue
		}
		implied, err := s.tagSvc().GetOrCreateTagFrom(normalized, catID, "ptr")
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		if exists == 0 {
			createdTags[implied.ID] = struct{}{}
		}
		isNew, err := s.tagSvc().AddImplicationFrom(tagID, implied.ID, "ptr")
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		if isNew {
			implications++
			impliedTouched[implied.ID] = struct{}{}
			fanOutParents[tagID] = struct{}{}
		}
	}
	return aliases, implications
}

// splitCategoryTag resolves a monbooru-form name ("bare" or "category:name")
// to its category id, treating an unknown prefix as part of a general-
// category name, matching the tag-input parser.
func (s *Server) splitCategoryTag(input string) (catID int64, bare string, ok bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, "", false
	}
	if idx := strings.Index(input, ":"); idx > 0 {
		if name := input[idx+1:]; name != "" {
			var id int64
			if err := s.db().Read.QueryRow(
				`SELECT id FROM tag_categories WHERE name = ?`, input[:idx],
			).Scan(&id); err == nil {
				return id, name, true
			}
		}
	}
	cx := s.Active()
	if cx == nil || cx.GeneralCategoryID == 0 {
		return 0, "", false
	}
	return cx.GeneralCategoryID, input, true
}

// fanOutImplicationsInline backfills a parent's implied closure onto every
// image already carrying it, chunked like the propagation job.
func (s *Server) fanOutImplicationsInline(ctx context.Context, parentID int64) error {
	ratingCatID := s.tagSvc().RatingCategoryID()
	return s.chunkImageTagsByParent(ctx, parentID, func(tx *sql.Tx, imageID int64) error {
		return propagateAddImplication(tx, imageID, parentID, ratingCatID)
	})
}
