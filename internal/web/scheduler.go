package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/tagger"
)

// runScheduler is a background goroutine that fires once per day at
// cfg.Schedule.Time and runs the enabled actions sequentially on every
// configured gallery. Started from NewServer; exits when s.done is closed.
func (s *Server) runScheduler() {
	for {
		next, ok := s.nextScheduledFire(time.Now())
		if !ok {
			// No action enabled (or invalid time). Sleep an hour then re-check
			// so Settings edits pick up without a server restart - and wake
			// early when a save signals via schedReload.
			select {
			case <-s.done:
				return
			case <-s.schedReload:
				continue
			case <-time.After(time.Hour):
				continue
			}
		}
		d := time.Until(next)
		if d < 0 {
			d = 0
		}
		logx.Infof("scheduler: next run at %s (in %s)", next.Format(time.RFC3339), d.Round(time.Second))
		select {
		case <-s.done:
			return
		case <-s.schedReload:
			continue
		case <-time.After(d):
			s.runScheduledActions()
		}
	}
}

// nextScheduledFire returns the next local time cfg.Schedule.Time will hit.
// Returns ok=false when no schedule flag is enabled or the time is unparseable.
func (s *Server) nextScheduledFire(now time.Time) (time.Time, bool) {
	s.cfgMu.Lock()
	sched := s.cfg.Schedule
	s.cfgMu.Unlock()
	if !schedHasAnyEnabled(sched) {
		return time.Time{}, false
	}
	t, err := parseScheduleTime(sched.Time)
	if err != nil {
		return time.Time{}, false
	}
	year, month, day := now.Date()
	fire := time.Date(year, month, day, t.hour, t.minute, 0, 0, now.Location())
	if !fire.After(now) {
		// time.Date normalises components, so passing day+1 walks the
		// calendar through DST transitions correctly. Add(24h) would
		// slip the local fire time by an hour twice a year.
		fire = time.Date(year, month, day+1, t.hour, t.minute, 0, 0, now.Location())
	}
	return fire, true
}

type schedTime struct{ hour, minute int }

func parseScheduleTime(v string) (schedTime, error) {
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 {
		return schedTime{}, fmt.Errorf("bad time %q", v)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return schedTime{}, fmt.Errorf("bad hour in %q", v)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return schedTime{}, fmt.Errorf("bad minute in %q", v)
	}
	return schedTime{hour: h, minute: m}, nil
}

func schedHasAnyEnabled(sc config.ScheduleConfig) bool {
	return sc.SyncGallery || sc.RemoveOrphans || sc.RunAutoTaggers ||
		sc.MergeGeneralTags
}

// runScheduledActions iterates every configured gallery and runs the enabled
// maintenance actions in a fixed order: sync → remove orphans → autotag →
// merge general tags. Skips the whole run when a user-triggered job is
// already holding the job manager. The reservation blocks user-triggered
// Start() calls for the duration so the lock-less phases below
// (RemoveOrphans, MergeGeneral) can't be raced by external handlers.
func (s *Server) runScheduledActions() {
	if err := s.jobs.BeginSchedule(); err != nil {
		logx.Warnf("scheduler: skipping run (a job is already running)")
		return
	}
	defer s.jobs.EndSchedule()

	started := time.Now()
	var failures []string
	defer func() {
		info := "OK"
		if len(failures) > 0 {
			info = strings.Join(failures, "; ")
		}
		s.recordScheduleRun(started, time.Since(started), info)
	}()

	s.cfgMu.Lock()
	sched := s.cfg.Schedule
	s.cfgMu.Unlock()

	s.ctxMu.RLock()
	names := make([]string, 0, len(s.contexts))
	for name := range s.contexts {
		names = append(names, name)
	}
	s.ctxMu.RUnlock()

	// User cancel mid-run clears scheduleHeld (Manager.Cancel does this)
	// so the outer loop can bail at the next phase boundary. Without
	// the gate, the cancelled phase would observe ctx.Err and complete,
	// then the next phase's StartScheduled would fire and run normally,
	// so one user click would cancel exactly one phase rather than the
	// remaining run.
	abort := func() bool {
		if !s.jobs.IsScheduleHeld() {
			logx.Infof("scheduler: run cancelled mid-flight; remaining phases skipped")
			return true
		}
		return false
	}

	for _, name := range names {
		if abort() {
			return
		}
		cx := s.Get(name)
		if cx == nil {
			continue
		}
		logx.Infof("scheduler: running actions on gallery %q", name)

		if sched.SyncGallery && !cx.Degraded {
			if err := s.scheduledSync(cx); err != nil {
				failures = append(failures, "sync "+name+": "+err.Error())
			}
			if abort() {
				return
			}
		}
		if sched.RemoveOrphans {
			if err := s.scheduledRemoveOrphans(cx); err != nil {
				failures = append(failures, "remove-orphans "+name+": "+err.Error())
			}
			if abort() {
				return
			}
		}
		if sched.RunAutoTaggers && tagger.IsAvailable(s.cfg) {
			if err := s.scheduledAutotag(cx); err != nil {
				failures = append(failures, "autotag "+name+": "+err.Error())
			}
			if abort() {
				return
			}
		}
		if sched.MergeGeneralTags {
			if err := s.scheduledMergeGeneral(cx); err != nil {
				failures = append(failures, "merge-general "+name+": "+err.Error())
			}
		}
	}
}

func (s *Server) scheduledSync(cx *galleryCtx) error {
	if err := s.jobs.StartScheduled(models.JobTypeSync); err != nil {
		logx.Warnf("scheduler sync %q: %v", cx.Name, err)
		return err
	}
	ctx := s.jobs.Context()
	result, err := cx.Sync(ctx, s.cfg.Gallery.MaxFileSizeMB, s.jobs.Update)
	// Match the user-trigger handlers' shape: ctx cancellation produces
	// a clean Complete summary, only real failures fall to Fail().
	if ctx.Err() != nil {
		s.jobs.Complete(fmt.Sprintf("[%s] sync cancelled (%d added, %d missing, %d moved)",
			cx.Name, result.Added, result.Removed, result.Moved))
		return nil
	}
	if err != nil {
		s.jobs.Fail(err.Error())
		logx.Warnf("scheduler sync %q: %v", cx.Name, err)
		return err
	}
	s.jobs.Complete(fmt.Sprintf("[%s] %d added, %d missing, %d moved",
		cx.Name, result.Added, result.Removed, result.Moved))
	return nil
}

func (s *Server) scheduledRemoveOrphans(cx *galleryCtx) error {
	if err := s.jobs.StartScheduled(models.JobTypePruneThumbs); err != nil {
		logx.Warnf("scheduler orphans %q: %v", cx.Name, err)
		return err
	}
	ctx := s.jobs.Context()
	removed, processed, total, err := s.runOrphanSweep(ctx, cx)
	if err != nil {
		s.jobs.Fail(err.Error())
		logx.Warnf("scheduler orphans %q: %v", cx.Name, err)
		return err
	}
	if ctx.Err() != nil {
		s.jobs.Complete(fmt.Sprintf("[%s] orphan sweep cancelled (%d/%d scanned, %d removed)", cx.Name, processed, total, removed))
		return nil
	}
	s.jobs.Complete(fmt.Sprintf("[%s] removed %d orphaned thumbnail(s)", cx.Name, removed))
	logx.Infof("scheduler: [%s] removed %d orphaned thumbnail(s)", cx.Name, removed)
	return nil
}

// runOrphanSweep walks the thumbnails directory and unlinks files
// whose id no longer matches a row in images. ctx aborts the sweep at
// the next entry; the returned counts reflect partial progress so the
// caller's cancelled summary stays accurate. Shared by the scheduler
// (StartScheduled wrapper) and the user-triggered prune handler
// (Start + goroutine wrapper) so the actual sweep lives in one place.
//
// Returns (removed, processed, total, err): removed is the number of
// orphan files unlinked, processed is the number of directory entries
// inspected (including non-thumbnail bystanders that are kept), total
// is the entry count from the initial ReadDir, err is set only when
// the prerequisite reads (ReadDir, the SELECT id FROM images cursor)
// fail. A truncated cursor returns err so the sweep doesn't delete
// legit thumbnails as orphans.
func (s *Server) runOrphanSweep(ctx context.Context, cx *galleryCtx) (removed, processed, total int, err error) {
	entries, err := os.ReadDir(cx.ThumbnailsPath)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read thumbnails dir: %w", err)
	}
	total = len(entries)
	known := map[int64]struct{}{}
	rows, err := cx.DB.Read.QueryContext(ctx, `SELECT id FROM images`)
	if err != nil {
		return 0, 0, total, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			known[id] = struct{}{}
		}
	}
	if iterErr := rows.Err(); iterErr != nil {
		rows.Close()
		return 0, 0, total, fmt.Errorf("cursor: %w", iterErr)
	}
	rows.Close()

	s.jobs.Update(0, total, fmt.Sprintf("[%s] pruning 0/%d…", cx.Name, total))
	for i, e := range entries {
		if ctx.Err() != nil {
			return removed, i, total, nil
		}
		if e.IsDir() {
			continue
		}
		name := e.Name()
		var idStr string
		switch {
		case strings.HasSuffix(name, "_hover.webp"):
			idStr = strings.TrimSuffix(name, "_hover.webp")
		case strings.HasSuffix(name, ".jpg"):
			idStr = strings.TrimSuffix(name, ".jpg")
		default:
			continue
		}
		id, parseErr := strconv.ParseInt(idStr, 10, 64)
		if parseErr != nil {
			continue
		}
		if _, ok := known[id]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(cx.ThumbnailsPath, name)); err == nil {
			removed++
		}
		if (i+1)%50 == 0 || i == total-1 {
			s.jobs.Update(i+1, total, fmt.Sprintf("[%s] pruning %d/%d…", cx.Name, i+1, total))
		}
	}
	return removed, total, total, nil
}

func (s *Server) scheduledAutotag(cx *galleryCtx) error {
	var ids []int64
	rows, err := cx.DB.Read.Query(
		`SELECT i.id FROM images i WHERE i.is_missing = 0
		 AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id AND it.is_auto = 1)`,
	)
	if err != nil {
		logx.Warnf("scheduler autotag %q: %v", cx.Name, err)
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return nil
	}
	enabled := tagger.EnabledTaggersForGallery(s.cfg, cx.Name)
	if len(enabled) == 0 {
		return nil
	}
	if err := s.jobs.StartScheduled(models.JobTypeAutotag); err != nil {
		logx.Warnf("scheduler autotag %q: %v", cx.Name, err)
		return err
	}
	ctx := s.jobs.Context()
	skipped, err := tagger.RunWithTaggers(ctx, cx.DB, s.cfg, ids, enabled, s.jobs, s.cfg.Tagger.UseCUDA, cx.MangaCacheDir())
	cx.InvalidateCaches()
	if ctx.Err() != nil {
		s.jobs.Complete(fmt.Sprintf("[%s] auto-tagging cancelled (%d image(s) queued)", cx.Name, len(ids)))
		return nil
	}
	if err != nil {
		s.jobs.Fail(err.Error())
		logx.Warnf("scheduler autotag %q: %v", cx.Name, err)
		return err
	}
	if skipped > 0 {
		s.jobs.Complete(fmt.Sprintf("[%s] auto-tagged %d of %d image(s), %d skipped", cx.Name, len(ids)-skipped, len(ids), skipped))
		return nil
	}
	s.jobs.Complete(fmt.Sprintf("[%s] auto-tagged %d image(s)", cx.Name, len(ids)))
	return nil
}

func (s *Server) scheduledMergeGeneral(cx *galleryCtx) error {
	merged, err := cx.TagSvc.MergeGeneralIntoCategorized()
	if err != nil {
		logx.Warnf("scheduler merge-general %q: %v", cx.Name, err)
		return err
	}
	if merged > 0 {
		// MergeGeneralIntoCategorized rewires image_tags rows and the
		// tag catalog itself, so the per-cx tag/folder/source caches
		// observe the wrong totals until the next mutation through
		// this same context. Drop them here so a Settings or sidebar
		// hit in the same gallery sees the merged state.
		cx.InvalidateCaches()
	}
	logx.Infof("scheduler: [%s] merged %d general tag(s)", cx.Name, merged)
	return nil
}

// recordScheduleRun stores the completion of a scheduler run so the Schedule
// settings section can show "Last run: ... (OK, 3m12s)". info is a short
// status string ("OK" or a failure summary).
func (s *Server) recordScheduleRun(started time.Time, dur time.Duration, info string) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	s.schedLastRun = started
	s.schedLastDur = dur
	s.schedLastInfo = info
}

// ScheduleStatus reports the last recorded scheduler run plus the next fire
// time. Used by the Schedule settings section.
type ScheduleStatus struct {
	LastRun  time.Time     // zero value when no run has happened yet
	LastDur  time.Duration // zero when LastRun is zero
	LastInfo string        // "OK" or a short failure summary; empty when never run
	NextRun  time.Time     // zero when no schedule action is enabled
}

// ScheduleStatus returns the current scheduler status for the settings page.
func (s *Server) ScheduleStatus() ScheduleStatus {
	s.schedMu.Lock()
	st := ScheduleStatus{LastRun: s.schedLastRun, LastDur: s.schedLastDur, LastInfo: s.schedLastInfo}
	s.schedMu.Unlock()
	if next, ok := s.nextScheduledFire(time.Now()); ok {
		st.NextRun = next
	}
	return st
}
