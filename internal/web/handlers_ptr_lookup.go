package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
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
	s.startTagScopeRun(w, r, s.runPTRTagLookup)
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
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
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

	// Implied-by parents are pulled only for the requested tags, never for tags
	// minted mid-sweep: a series' implied-by is every character that carries it,
	// so recursing would cascade the whole graph in.
	seed := make(map[int64]bool, len(cands))
	for _, c := range cands {
		seed[c.id] = true
	}

	total := len(cands)
	aliases, implications, unknown, processed, dropped, aliased, retired := 0, 0, 0, 0, 0, 0, 0
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
					// A tag the PTR no longer knows has no fresh state left, so
					// every relation it pulled earlier is flagged.
					unknown++
					retired += s.syncPTRStaleness(c.id, nil, nil)
				} else {
					a, i, d, al, r := s.applyPTRTagInfo(c.id, info, impliedTouched, fanOutParents, createdTags, seed[c.id])
					aliases += a
					implications += i
					dropped += d
					aliased += al
					retired += r
				}
				processed++
			}
			s.jobs.Update(processed, total, "PTR lookup…")
		}
		if cancelled || unavailable {
			break
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
	if dropped > 0 {
		msg += fmt.Sprintf("; %d spelling(s) not representable", dropped)
	}
	if aliased > 0 {
		msg += fmt.Sprintf("; %d relation(s) skipped, an alias here points elsewhere", aliased)
	}
	if retired > 0 {
		msg += fmt.Sprintf("; %d relation(s) no longer on the PTR", retired)
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
// sweep too. The names the answer carried form the fresh state the tag's
// earlier PTR pulls are reconciled against: relations no longer listed are
// flagged stale, not removed. When pullImpliedBy is set the reverse edges
// (parents that imply this tag) are declared too, but those parents are only
// created and linked, never swept - their own relations stay for a pull from
// their page, so one tag's pull cannot cascade the whole cluster in.
func (s *Server) applyPTRTagInfo(tagID int64, info ptrTagInfo, impliedTouched, fanOutParents, createdTags map[int64]struct{}, pullImpliedBy bool) (aliases, implications, dropped, aliased, retired int) {
	freshAliases := map[tags.AliasKey]bool{}
	freshImplied := map[int64]bool{}
	for _, name := range info.Aliases {
		catID, bare, ok := s.splitCategoryTag(name)
		if !ok {
			dropped++
			continue
		}
		normalized, err := tags.ValidateTagName(bare)
		if err != nil {
			dropped++
			continue
		}
		freshAliases[tags.AliasKey{CategoryID: catID, Name: normalized}] = true
		exists, err := s.tagNameExists(catID, normalized)
		if err != nil {
			logx.Warnf("ptr alias %q: %v", name, err)
			continue
		}
		if exists {
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
		exists, err := s.tagNameExists(catID, normalized)
		if err != nil {
			continue
		}
		implied, err := s.tagSvc().GetOrCreateTagFrom(normalized, catID, "ptr")
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		if redirected(implied, normalized, catID) {
			aliased++
			continue
		}
		freshImplied[implied.ID] = true
		if !exists {
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
	if pullImpliedBy {
		for _, name := range info.ImpliedBy {
			catID, bare, ok := s.splitCategoryTag(name)
			if !ok {
				continue
			}
			normalized, err := tags.ValidateTagName(bare)
			if err != nil {
				logx.Warnf("ptr implied-by %q: %v", name, err)
				continue
			}
			// A reverse-edge parent is linked but never swept: its own
			// implications point at siblings of this tag (solo_futanari implies
			// futanari), and following them would drag the whole surrounding
			// cluster into the catalog. Pull the parent's graph from its page.
			parent, err := s.tagSvc().GetOrCreateTagFrom(normalized, catID, "ptr")
			if err != nil {
				logx.Warnf("ptr implied-by %q: %v", name, err)
				continue
			}
			if redirected(parent, normalized, catID) {
				aliased++
				continue
			}
			isNew, err := s.tagSvc().AddImplicationFrom(parent.ID, tagID, "ptr")
			if err != nil {
				logx.Warnf("ptr implied-by %q: %v", name, err)
				continue
			}
			if isNew {
				implications++
				impliedTouched[tagID] = struct{}{}
				fanOutParents[parent.ID] = struct{}{}
			}
		}
	}
	retired = s.syncPTRStaleness(tagID, freshAliases, freshImplied)
	return aliases, implications, dropped, aliased, retired
}

// redirected reports that GetOrCreateTag handed back a different tag than the
// name asked for, because that name is an alias here. The catalog says the two
// are the same tag; the PTR only ever spoke about the aliased spelling. Storing
// the relation against the canonical would assert an edge neither source
// declares - and the contribution dialog would then keep offering that edge
// back to the PTR as new.
func redirected(got *models.Tag, name string, categoryID int64) bool {
	return got.Name != name || got.CategoryID != categoryID
}

// syncPTRStaleness reconciles a tag's pulled relations against the fresh
// answer (nil maps mean the PTR listed nothing), returning how many were
// newly flagged. Errors are logged and skipped like the apply loops - a
// failed flag pass must not abort the sweep.
func (s *Server) syncPTRStaleness(tagID int64, freshAliases map[tags.AliasKey]bool, freshImplied map[int64]bool) int {
	retired := 0
	n, err := s.tagSvc().SyncAliasStaleness(tagID, "ptr", freshAliases)
	if err != nil {
		logx.Warnf("ptr alias staleness for tag %d: %v", tagID, err)
	}
	retired += n
	n, err = s.tagSvc().SyncImplicationStaleness(tagID, "ptr", freshImplied)
	if err != nil {
		logx.Warnf("ptr implication staleness for tag %d: %v", tagID, err)
	}
	return retired + n
}

// tagNameExists reports whether a (category, name) tag row exists,
// alias rows included.
func (s *Server) tagNameExists(catID int64, name string) (bool, error) {
	var n int
	if err := s.db().Read.QueryRow(
		`SELECT COUNT(*) FROM tags WHERE name = ? AND category_id = ?`, name, catID,
	).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
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
