package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/search"
	"github.com/leqwin/monbooru/internal/tags"
)

// galleryCtx holds everything per-gallery: paths, DB, tag service, degraded
// flag, and watcher bookkeeping.
type galleryCtx struct {
	Name           string
	GalleryPath    string
	DBPath         string
	ThumbnailsPath string
	DB             *db.DB
	TagSvc         *tags.Service
	Degraded       bool

	// GeneralCategoryID is the resolved id of the built-in `general`
	// tag_categories row. parseTagInput hits this every detail-page tag
	// add, every upload, every batch-tag job; resolving once at open
	// time saves a SELECT per call. Built-in rows are immutable so no
	// invalidation is needed.
	GeneralCategoryID int64

	// Per-gallery caches of queries that scan every visible image. All are
	// nilled by InvalidateCaches after any ingest/delete/missing-toggle so
	// the next reader re-populates from SQLite. The int counts are pointers
	// so "not cached" is distinguishable from "cached zero".
	folderTree   atomic.Pointer[[]gallery.FolderNode]
	sourceCounts atomic.Pointer[gallery.SourceCounts]
	seriesCounts atomic.Pointer[[]gallery.SeriesCount]
	visibleCount atomic.Pointer[int]
	inboxCount   atomic.Pointer[int]
	tagCount     atomic.Pointer[int]
	savedCount   atomic.Pointer[int]

	watcherCancel context.CancelFunc
	watcherDone   chan struct{}

	mangaReclaim *gallery.MangaCacheReclaimer
}

// Sync runs gallery.Sync against this context and drops the per-cx
// caches that the sync's mark-missing / move / ingest steps touch.
// Centralising the invalidation here keeps the contract local: every
// caller (manual sync handler, scheduler, future scheduled phase) gets
// the cache hygiene by construction instead of relying on the caller's
// goroutine to remember the InvalidateCaches at the right point.
func (cx *galleryCtx) Sync(ctx context.Context, maxFileSizeMB int, progress func(processed, total int, message string)) (gallery.SyncResult, error) {
	result, err := gallery.Sync(ctx, cx.DB, cx.GalleryPath, cx.ThumbnailsPath, maxFileSizeMB, progress)
	cx.InvalidateCaches()
	return result, err
}

// InvalidateCaches drops the folder-tree and visible-count caches. Call after
// any mutation that changes which images are visible (ingest, delete, sync
// mark-missing, watcher remove). Tag and saved-search counts are dropped too:
// the same image-mutation paths typically change those, and the Settings
// page's per-gallery cells need them fresh.
func (cx *galleryCtx) InvalidateCaches() {
	if cx == nil {
		return
	}
	cx.folderTree.Store(nil)
	cx.sourceCounts.Store(nil)
	cx.seriesCounts.Store(nil)
	cx.visibleCount.Store(nil)
	cx.inboxCount.Store(nil)
	cx.tagCount.Store(nil)
	cx.savedCount.Store(nil)
	if cx.DB != nil {
		cx.DB.InvalidateCachedCounts()
	}
	// The adjacency cache holds sorted match-id snapshots that pre-date
	// any membership-changing write (delete, move, inbox/favourite
	// toggle, batch tag). Without dropping them here, a re-render of
	// the same query within the 5-min TTL serves the stale list and
	// the gallery shows rows that no longer match.
	search.AdjacencyCacheDropForGallery(cx.Name)
}

// FolderTree returns the cached tree or builds one on demand. The cache is
// invalidated by InvalidateCaches.
func (cx *galleryCtx) FolderTree() ([]gallery.FolderNode, error) {
	if p := cx.folderTree.Load(); p != nil {
		return *p, nil
	}
	tree, err := gallery.FolderTree(cx.DB)
	if err != nil {
		return nil, err
	}
	cx.folderTree.Store(&tree)
	return tree, nil
}

// SourceCounts returns the cached source-tree counts or queries them on
// demand. The cache is invalidated by InvalidateCaches.
func (cx *galleryCtx) SourceCounts() (gallery.SourceCounts, error) {
	if p := cx.sourceCounts.Load(); p != nil {
		return *p, nil
	}
	sc, err := gallery.SourceCountsQuery(cx.DB)
	if err != nil {
		return gallery.SourceCounts{}, err
	}
	cx.sourceCounts.Store(&sc)
	return sc, nil
}

// SeriesCounts returns the cached top-25 series labels for the
// gallery's non-missing manga rows. Empty when no manga carries a
// series label - the sidebar partial gates rendering on the slice
// being non-empty.
func (cx *galleryCtx) SeriesCounts() ([]gallery.SeriesCount, error) {
	if p := cx.seriesCounts.Load(); p != nil {
		return *p, nil
	}
	sc, err := gallery.SeriesCountsQuery(cx.DB, 25)
	if err != nil {
		return nil, err
	}
	cx.seriesCounts.Store(&sc)
	return sc, nil
}

// cachedCount lazy-loads and caches a scalar COUNT query. The atomic
// pointer doubles as the cache slot and the "loaded?" flag; nil means
// re-query.
func (cx *galleryCtx) cachedCount(slot *atomic.Pointer[int], query string) (int, error) {
	if p := slot.Load(); p != nil {
		return *p, nil
	}
	var n int
	if err := cx.DB.Read.QueryRow(query).Scan(&n); err != nil {
		return 0, err
	}
	slot.Store(&n)
	return n, nil
}

// VisibleCount returns the cached count of non-missing images or queries it
// on demand. Only used for the unfiltered gallery page - filtered searches
// bypass the cache.
func (cx *galleryCtx) VisibleCount() (int, error) {
	return cx.cachedCount(&cx.visibleCount, `SELECT COUNT(*) FROM images WHERE is_missing = 0`)
}

// InboxCount returns the cached count of visible images sitting in the
// inbox (is_inbox = 1, is_missing = 0). Surfaced in the gallery toolbar's
// inbox toggle so the user sees the triage backlog at a glance. Reads
// off idx_images_inbox_visible.
func (cx *galleryCtx) InboxCount() (int, error) {
	return cx.cachedCount(&cx.inboxCount, `SELECT COUNT(*) FROM images WHERE is_missing = 0 AND is_inbox = 1`)
}

// TagCount returns the cached count of non-alias tags or queries it on demand.
// Surfaced in the Settings galleries table and the layout footer; uncached the
// query runs once per render per gallery, which adds up on multi-gallery boxes.
func (cx *galleryCtx) TagCount() (int, error) {
	return cx.cachedCount(&cx.tagCount, `SELECT COUNT(*) FROM tags WHERE is_alias = 0`)
}

// SavedCount returns the cached count of saved searches or queries it on
// demand. Same role as TagCount on the Settings page and footer.
func (cx *galleryCtx) SavedCount() (int, error) {
	return cx.cachedCount(&cx.savedCount, `SELECT COUNT(*) FROM saved_searches`)
}

// warmCaches primes the per-gallery aggregations so the first user-facing
// sidebar/gallery/settings request doesn't pay the cold scan. Errors are
// ignored: the lazy path in each accessor still recomputes on demand if the
// warm failed.
func (cx *galleryCtx) warmCaches() {
	if cx == nil || cx.DB == nil {
		return
	}
	cx.FolderTree()   //nolint:errcheck
	cx.SourceCounts() //nolint:errcheck
	cx.SeriesCounts() //nolint:errcheck
	cx.VisibleCount() //nolint:errcheck
	cx.InboxCount()   //nolint:errcheck
	cx.TagCount()     //nolint:errcheck
	cx.SavedCount()   //nolint:errcheck
}

// openGalleryCtx opens the DB and creates the thumbnails directory. The
// watcher is started separately so only the active gallery runs one.
func openGalleryCtx(g config.Gallery) (*galleryCtx, error) {
	if dbDir := filepath.Dir(g.DBPath); dbDir != "" && dbDir != "." {
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			return nil, fmt.Errorf("gallery %q: create db dir: %w", g.Name, err)
		}
	}
	database, err := db.Open(g.DBPath)
	if err != nil {
		return nil, fmt.Errorf("gallery %q: open db: %w", g.Name, err)
	}
	if err := db.Bootstrap(database); err != nil {
		database.Close()
		return nil, fmt.Errorf("gallery %q: bootstrap db: %w", g.Name, err)
	}
	if err := os.MkdirAll(g.ThumbnailsPath, 0o755); err != nil {
		database.Close()
		return nil, fmt.Errorf("gallery %q: create thumbnails dir: %w", g.Name, err)
	}
	degraded := false
	if _, err := os.ReadDir(g.GalleryPath); err != nil {
		logx.Warnf("gallery %q: path %q unreadable: %v - degraded mode", g.Name, g.GalleryPath, err)
		degraded = true
	}
	var generalID int64
	if err := database.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = 'general'`,
	).Scan(&generalID); err != nil {
		database.Close()
		return nil, fmt.Errorf("gallery %q: resolve general category: %w", g.Name, err)
	}
	return &galleryCtx{
		Name:              g.Name,
		GalleryPath:       g.GalleryPath,
		DBPath:            g.DBPath,
		ThumbnailsPath:    g.ThumbnailsPath,
		DB:                database,
		TagSvc:            tags.New(database),
		Degraded:          degraded,
		GeneralCategoryID: generalID,
	}, nil
}

// MangaCacheDir returns the per-gallery manga page cache directory.
// Sibling to ThumbnailsPath under the gallery's data root.
func (cx *galleryCtx) MangaCacheDir() string {
	if cx == nil {
		return ""
	}
	return gallery.MangaCacheDir(cx.ThumbnailsPath)
}

// close stops the watcher and closes the DB. Keeps cx.DB non-nil afterwards:
// a concurrent warmCaches goroutine (spawned by StartWatchers, not joined)
// can still race against close at shutdown or gallery removal. A closed pool
// returns "database is closed" on subsequent calls, which the accessors
// discard; a nil pool would panic on the field deref. sql.DB.Close is
// idempotent so a later close still behaves.
func (cx *galleryCtx) close() {
	cx.stopWatcher()
	cx.stopMangaReclaim()
	if cx.DB != nil {
		cx.DB.Close()
	}
}

// startWatcher no-ops when watching is disabled, the gallery is degraded,
// or a watcher is already running.
func (cx *galleryCtx) startWatcher(watchEnabled bool, maxFileSizeMB int, jm *jobs.Manager) {
	if !watchEnabled || cx.Degraded || cx.watcherCancel != nil {
		return
	}
	w, err := gallery.NewWatcher(cx.Name, cx.GalleryPath, cx.ThumbnailsPath, maxFileSizeMB, cx.DB, jm)
	if err != nil {
		logx.Warnf("gallery %q: watcher start: %v", cx.Name, err)
		return
	}
	w.OnEvent = jm.SetWatcherMessage
	w.OnChange = cx.InvalidateCaches
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	cx.watcherCancel = cancel
	cx.watcherDone = done
	go func() {
		defer close(done)
		if err := w.Run(ctx); err != nil {
			logx.Warnf("gallery %q: watcher stopped: %v", cx.Name, err)
		}
	}()
	logx.Infof("gallery %q: watcher started", cx.Name)
}

func (cx *galleryCtx) stopWatcher() {
	if cx.watcherCancel == nil {
		return
	}
	cx.watcherCancel()
	<-cx.watcherDone
	cx.watcherCancel = nil
	cx.watcherDone = nil
}

// startMangaReclaim spawns the per-gallery idle-page evictor. Idempotent
// against repeated calls. The reclaimer is harmless on galleries that
// have never ingested a manga - sweepOnce no-ops when the cache dir
// doesn't exist.
func (cx *galleryCtx) startMangaReclaim() {
	if cx.mangaReclaim != nil {
		return
	}
	r := gallery.NewMangaCacheReclaimer(cx.MangaCacheDir())
	r.Start(context.Background())
	cx.mangaReclaim = r
}

func (cx *galleryCtx) stopMangaReclaim() {
	if cx.mangaReclaim == nil {
		return
	}
	cx.mangaReclaim.Stop()
	cx.mangaReclaim = nil
}

// Accessors below resolve to the active gallery's fields. The
// ContextMiddleware RLock keeps the returned pointers stable per request.

func (s *Server) db() *db.DB {
	if cx := s.Active(); cx != nil {
		return cx.DB
	}
	return nil
}

func (s *Server) tagSvc() *tags.Service {
	if cx := s.Active(); cx != nil {
		return cx.TagSvc
	}
	return nil
}

// categoryExists reports whether name matches a row in tag_categories on
// the active gallery. Callers use it to disambiguate a `prefix:value`
// token that might be category-qualified or a literal tag containing a
// colon. Database errors (including nil gallery) count as "no match" so
// an ambiguous input degrades to literal.
func (s *Server) categoryExists(name string) bool {
	d := s.db()
	if d == nil {
		return false
	}
	var n int
	return d.Read.QueryRow(
		`SELECT 1 FROM tag_categories WHERE name = ? LIMIT 1`, name,
	).Scan(&n) == nil
}

func (s *Server) galleryPath() string {
	if cx := s.Active(); cx != nil {
		return cx.GalleryPath
	}
	return ""
}

func (s *Server) thumbnailsPath() string {
	if cx := s.Active(); cx != nil {
		return cx.ThumbnailsPath
	}
	return ""
}

func (s *Server) dbPath() string {
	if cx := s.Active(); cx != nil {
		return cx.DBPath
	}
	return ""
}
