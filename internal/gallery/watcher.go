package gallery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/logx"
)

// Watcher watches the gallery directory for new files and ingests them.
type Watcher struct {
	fsw            *fsnotify.Watcher
	galleryName    string // for prefixing status messages when multiple galleries are watched
	galleryPath    string
	thumbnailsPath string
	maxFileSizeMB  int
	db             *db.DB
	jobs           *jobs.Manager
	OnEvent        func(msg string) // callback for status notifications (may be nil)
	OnChange       func()           // callback fired after any image add/remove (may be nil)

	mu     sync.Mutex
	timers map[string]*time.Timer
}

// NewWatcher creates and initializes a filesystem watcher for one gallery.
// galleryName prefixes status messages so multi-gallery setups can tell
// which gallery an event came from.
func NewWatcher(galleryName, galleryPath, thumbnailsPath string, maxFileSizeMB int, database *db.DB, jobManager *jobs.Manager) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		fsw:            fsw,
		galleryName:    galleryName,
		galleryPath:    galleryPath,
		thumbnailsPath: thumbnailsPath,
		maxFileSizeMB:  maxFileSizeMB,
		db:             database,
		jobs:           jobManager,
		timers:         map[string]*time.Timer{},
	}

	if addErr := fsw.Add(galleryPath); addErr != nil {
		fsw.Close()
		return nil, fmt.Errorf("fsnotify watch gallery root: %w", addErr)
	}
	logx.Infof("watcher: watching %s", galleryPath)

	// Walk and watch every subdirectory, stopping gracefully on inotify limits.
	watchCount := 1
	limitHit := false
	if walkErr := filepath.WalkDir(galleryPath, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == galleryPath {
			return nil
		}
		if limitHit {
			return filepath.SkipAll
		}
		if addErr := fsw.Add(path); addErr != nil {
			// Prefer typed errno detection so localised glibc and a
			// future fsnotify version that wraps the syscall error
			// differently don't silently break inotify-limit handling.
			// Fall back to the substring match because some wrappers
			// stringify and don't unwrap to syscall.Errno.
			if errors.Is(addErr, syscall.ENOSPC) ||
				errors.Is(addErr, syscall.EMFILE) ||
				strings.Contains(addErr.Error(), "no space left") ||
				strings.Contains(addErr.Error(), "too many open files") {
				logx.Warnf("fsnotify: inotify limit hit at %d dirs. "+
					"Increase: echo fs.inotify.max_user_watches=524288 | sudo tee -a /etc/sysctl.conf && sudo sysctl -p", watchCount)
				limitHit = true
				return filepath.SkipAll
			}
			logx.Warnf("fsnotify add %q: %v", path, addErr)
		} else {
			watchCount++
		}
		return nil
	}); walkErr != nil {
		// A WalkDir error here means the outer traversal could not
		// finish - typically a permission denied at the gallery root or
		// a vanished symlink. Surface it at warn so the operator can
		// fix the access rights; the partially-built watcher still
		// works for the dirs it did register.
		logx.Warnf("watcher: walk %q: %v", galleryPath, walkErr)
	}

	return w, nil
}

// Run starts the event loop. Returns when ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			w.cancelPendingTimers()
			w.fsw.Close()
			return nil

		case event, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}

			// Drop events while a manual sync, move, delete, or tag job
			// is running; each one already touches image_paths /
			// image_tags under its own transaction, so a concurrent
			// watcher ingest would race on the UNIQUE constraint or
			// trip markFileMissing on the source.
			if w.jobs != nil {
				if st := w.jobs.Get(); st != nil && st.Running {
					switch st.JobType {
					case "sync", "move", "delete", "tag":
						continue
					}
				}
			}

			if event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				info, err := os.Stat(event.Name)
				if err != nil {
					continue
				}

				if info.IsDir() {
					if addErr := w.fsw.Add(event.Name); addErr != nil {
						logx.Warnf("fsnotify add new dir %q: %v", event.Name, addErr)
					}
					continue
				}

				w.debounce(event.Name)
			}

			// Slow writers (network copies, large archives) keep firing
			// IN_MODIFY long after IN_CREATE; without extending the
			// pending CREATE-timer, ingestFile reads a partial file and
			// the archive parser bails on the missing central directory.
			// Only extend - never start a fresh timer - so saves to
			// already-ingested files don't re-trigger ingestion.
			if event.Has(fsnotify.Write) {
				w.extendDebounce(event.Name)
			}

			if event.Has(fsnotify.Remove) {
				w.fsw.Remove(event.Name)
				w.markFileMissing(event.Name)
			}

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			logx.Warnf("fsnotify error: %v", err)
		}
	}
}

// eventPrefix returns `watcher: ` by default, or `watcher [name]: ` when
// the watcher has a non-empty gallery name. The bracketed form lets users
// tell multi-gallery events apart in the status bar.
func (w *Watcher) eventPrefix() string {
	if w.galleryName == "" {
		return "watcher: "
	}
	return "watcher [" + w.galleryName + "]: "
}

func (w *Watcher) cancelPendingTimers() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for path, t := range w.timers {
		t.Stop()
		delete(w.timers, path)
	}
}

func (w *Watcher) debounce(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if t, ok := w.timers[path]; ok {
		t.Reset(500 * time.Millisecond)
		return
	}

	w.timers[path] = time.AfterFunc(500*time.Millisecond, func() {
		w.mu.Lock()
		delete(w.timers, path)
		w.mu.Unlock()

		w.ingestFile(path)
	})
}

// extendDebounce resets the create-side debounce timer when one is
// already pending, or schedules a fresh ingest 500 ms out when one
// isn't. Used by Write events: while a slow writer streams the file,
// IN_MODIFY events extend the wait so ingestFile runs once on the
// settled bytes. If the create-debounce already fired on a partial
// file (large cbz where central-directory bytes arrive after the
// 500 ms create window), the late writes schedule a follow-up
// ingest that finds the now-complete archive. Saves to long-existing
// files also schedule a re-ingest, which dedups on SHA-256 and is
// a no-op past the hash cost.
func (w *Watcher) extendDebounce(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.timers[path]; ok {
		t.Reset(500 * time.Millisecond)
		return
	}
	w.timers[path] = time.AfterFunc(500*time.Millisecond, func() {
		w.mu.Lock()
		delete(w.timers, path)
		w.mu.Unlock()
		w.ingestFile(path)
	})
}

func (w *Watcher) ingestFile(path string) {
	ft, err := DetectFileType(path)
	if err != nil {
		return
	}

	// Mirror Sync's per-file size cap. Without it, dropping a multi-GB
	// video into the gallery would hang thumbnail generation and hold a
	// write transaction for minutes.
	if maxMB := w.maxFileSizeMB; maxMB > 0 {
		if info, statErr := os.Stat(path); statErr == nil {
			if info.Size() > int64(maxMB)*1024*1024 {
				logx.Warnf("watcher: skipping %q (size %d exceeds %d MB)",
					path, info.Size(), maxMB)
				return
			}
		}
	}

	_, isDup, err := Ingest(w.db, w.galleryPath, w.thumbnailsPath, path, ft, "")
	if err != nil {
		logx.Warnf("watcher ingest %q: %v", path, err)
	} else if isDup {
		logx.Infof("watcher: duplicate %q", path)
	} else {
		logx.Infof("watcher: ingested %q", path)
		if w.OnEvent != nil {
			w.OnEvent(w.eventPrefix() + "added " + filepath.Base(path))
		}
		if w.OnChange != nil {
			w.OnChange()
		}
	}
}

// markFileMissing flips is_missing=1 and rebalances the usage_count of
// every tag the image was carrying, in one write transaction. usage_count
// is the visible-image count for a tag, so removing an image from the
// visible set has to decrement (and prune zero-usage tags) the same way
// the manual recalc does.
func (w *Watcher) markFileMissing(path string) {
	// filepath.Rel containment so a sibling directory sharing a prefix
	// (/data/gallery vs /data/gallery_backup) is correctly rejected.
	rootAbs, err := filepath.Abs(w.galleryPath)
	if err != nil {
		return
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	if !PathInside(rootAbs, pathAbs) {
		return
	}

	var imgID int64
	err = w.db.Read.QueryRow(
		`SELECT id FROM images WHERE canonical_path = ? AND is_missing = 0`, path,
	).Scan(&imgID)
	if err != nil {
		err2 := w.db.Read.QueryRow(
			`SELECT ip.image_id FROM image_paths ip
			 JOIN images i ON i.id = ip.image_id
			 WHERE ip.path = ? AND ip.is_canonical = 1 AND i.is_missing = 0`, path,
		).Scan(&imgID)
		if err2 != nil {
			return
		}
	}

	tx, err := w.db.Write.Begin()
	if err != nil {
		logx.Warnf("watcher mark missing %q: begin tx: %v", path, err)
		return
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE images SET is_missing = 1 WHERE id = ?`, imgID); err != nil {
		logx.Warnf("watcher mark missing %q: %v", path, err)
		return
	}

	rows, err := tx.Query(`SELECT tag_id FROM image_tags WHERE image_id = ?`, imgID)
	if err != nil {
		logx.Warnf("watcher mark missing %q: list tags: %v", path, err)
		return
	}
	var tagIDs []int64
	for rows.Next() {
		var tid int64
		if scanErr := rows.Scan(&tid); scanErr != nil {
			rows.Close()
			logx.Warnf("watcher mark missing %q: scan tag: %v", path, scanErr)
			return
		}
		tagIDs = append(tagIDs, tid)
	}
	if iterErr := rows.Err(); iterErr != nil {
		rows.Close()
		logx.Warnf("watcher mark missing %q: iterate tags: %v", path, iterErr)
		return
	}
	rows.Close()

	for _, tid := range tagIDs {
		if _, err := tx.Exec(
			`UPDATE tags SET usage_count = MAX(0, usage_count - 1) WHERE id = ?`, tid,
		); err != nil {
			logx.Warnf("watcher mark missing %q: decrement tag %d: %v", path, tid, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		logx.Warnf("watcher mark missing %q: commit: %v", path, err)
		return
	}

	logx.Infof("watcher: marked missing %q (id=%d)", path, imgID)
	if w.OnEvent != nil {
		w.OnEvent(w.eventPrefix() + "removed " + filepath.Base(path))
	}
	if w.OnChange != nil {
		w.OnChange()
	}
}
