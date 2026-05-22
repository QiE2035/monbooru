package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strconv"

	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/relations"
)

// findRelationPairsPost queues the find-pairs background job against
// the active gallery's BK-tree. Form-encoded knobs:
//   - distance (int, 0..12, default 4) - Hamming distance cap.
//   - replace ("true" / unset) - wipe potential_relation_pairs before
//     re-scanning.
func (s *Server) findRelationPairsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.Active()
	if cx == nil || cx.DB == nil || cx.bkTree == nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="flash flash-err">No active gallery.</div>`))
		return
	}
	s.cfgMu.Lock()
	distance := s.cfg.Relations.DefaultDistance
	s.cfgMu.Unlock()
	if distance < 0 || distance > 12 {
		distance = 4
	}
	if v := r.FormValue("distance"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 12 {
			distance = n
		}
	}
	replace := r.FormValue("replace") == "true"
	// replace=true wipes potential_relation_pairs; gate it behind an
	// explicit confirm so a non-HTMX caller (curl, bookmarked URL, broken
	// script) can't drop the queue with a single missing flag.
	if replace && r.FormValue("confirm") != "REBUILD" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">replace=true requires confirm=REBUILD.</div>`))
		return
	}

	if err := s.jobs.Start(models.JobTypeRelations); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	database := cx.DB
	thumbnailsPath := cx.ThumbnailsPath
	tree := cx.bkTree
	opts := relations.FindPairsOptions{
		Distance:       distance,
		Replace:        replace,
		ThumbnailsPath: thumbnailsPath,
	}
	go func() {
		ctx := s.jobs.Context()
		added, err := relations.FindPairs(ctx, database, tree, opts, func(processed, total int, phase string) {
			s.jobs.Update(processed, total, fmt.Sprintf("find-pairs: %s", phase))
		})
		if err == context.Canceled || ctx.Err() != nil {
			s.jobs.Complete(fmt.Sprintf("find-pairs cancelled (%d added)", added))
			return
		}
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		s.jobs.Complete(fmt.Sprintf("find-pairs added %d candidate(s).", added))
	}()
	w.Write([]byte(`<div class="flash flash-ok">Find-pairs started.</div>`))
}

// resetSkippedPost clears skipped_at on every potential_relation_pairs
// row so previously-skipped pairs surface again at the front of the
// queue on the next session render.
func (s *Server) resetSkippedPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="flash flash-err">No active gallery.</div>`))
		return
	}
	res, err := cx.DB.Write.ExecContext(r.Context(),
		`UPDATE potential_relation_pairs SET skipped_at = NULL WHERE skipped_at IS NOT NULL`)
	if err != nil {
		logx.Warnf("reset skipped: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	n, _ := res.RowsAffected()
	fmt.Fprintf(w, `<div class="flash flash-ok">Reset %d skipped pair(s).</div>`, n)
}
