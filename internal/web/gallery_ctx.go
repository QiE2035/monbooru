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
	"github.com/leqwin/monbooru/internal/relations"
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
	RelationsSvc   *relations.Service
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
	folderTree        atomic.Pointer[[]gallery.FolderNode]
	sourceCounts      atomic.Pointer[gallery.SourceCounts]
	seriesCounts      atomic.Pointer[[]gallery.SeriesCount]
	sourceLabelCounts atomic.Pointer[[]gallery.SourceLabelCount]
	visibleCount      atomic.Pointer[int]
	inboxCount        atomic.Pointer[int]
	favoritedCount    atomic.Pointer[int]
	tagCount          atomic.Pointer[int]
	collectionsCount  atomic.Pointer[int]

	// Parallel caches keyed by ceiling level, populated lazily on first
	// access from a sidebar / relations-hub render under that ceiling
	// and dropped together with the blind caches on InvalidateCaches.
	// The maps are stored by atomic pointer so reads are lock-free; the
	// helper below performs copy-on-write to add a level. Three active
	// ceilings × N galleries at steady state is trivial storage.
	visibleCountUnder      atomic.Pointer[map[string]int]
	inboxCountUnder        atomic.Pointer[map[string]int]
	favoritedCountUnder    atomic.Pointer[map[string]int]
	phashMissingUnder      atomic.Pointer[map[string]int]
	folderTreeUnder        atomic.Pointer[map[string][]gallery.FolderNode]
	sourceCountsUnder      atomic.Pointer[map[string]gallery.SourceCounts]
	seriesCountsUnder      atomic.Pointer[map[string][]gallery.SeriesCount]
	sourceLabelCountsUnder atomic.Pointer[map[string][]gallery.SourceLabelCount]

	// bkTree is the in-memory phash index used by the find-pairs job
	// and the phash: search keyword. Built lazily on first relations
	// query; the ingest/delete hooks in internal/relations keep it
	// consistent with subsequent writes once it is built.
	bkTree *relations.BKTree

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
// mark-missing, watcher remove). Tag and collection counts are dropped too:
// the same image-mutation paths typically change those, and the Settings
// page's per-gallery cells need them fresh.
func (cx *galleryCtx) InvalidateCaches() {
	if cx == nil {
		return
	}
	cx.folderTree.Store(nil)
	cx.sourceCounts.Store(nil)
	cx.seriesCounts.Store(nil)
	cx.sourceLabelCounts.Store(nil)
	cx.visibleCount.Store(nil)
	cx.inboxCount.Store(nil)
	cx.favoritedCount.Store(nil)
	cx.tagCount.Store(nil)
	cx.collectionsCount.Store(nil)
	cx.visibleCountUnder.Store(nil)
	cx.inboxCountUnder.Store(nil)
	cx.favoritedCountUnder.Store(nil)
	cx.phashMissingUnder.Store(nil)
	cx.folderTreeUnder.Store(nil)
	cx.sourceCountsUnder.Store(nil)
	cx.seriesCountsUnder.Store(nil)
	cx.sourceLabelCountsUnder.Store(nil)
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

// SourceLabelCounts returns the cached top-25 source labels for the
// gallery's non-missing image rows. Empty when no image carries a
// source label; the sidebar partial gates rendering on the slice
// being non-empty.
func (cx *galleryCtx) SourceLabelCounts() ([]gallery.SourceLabelCount, error) {
	if p := cx.sourceLabelCounts.Load(); p != nil {
		return *p, nil
	}
	sc, err := gallery.SourceLabelCountsQuery(cx.DB, 25)
	if err != nil {
		return nil, err
	}
	cx.sourceLabelCounts.Store(&sc)
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

// FavoritedCount returns the cached count of visible favourited images
// (is_favorited = 1, is_missing = 0). Used by the sidebar's Favorites
// panel to surface "<N> favourites / <M> not favourites" without
// running a search per render.
func (cx *galleryCtx) FavoritedCount() (int, error) {
	return cx.cachedCount(&cx.favoritedCount, `SELECT COUNT(*) FROM images WHERE is_missing = 0 AND is_favorited = 1`)
}

// TagCount returns the cached count of non-alias tags or queries it on demand.
// Surfaced in the Settings galleries table and the layout footer; uncached the
// query runs once per render per gallery, which adds up on multi-gallery boxes.
func (cx *galleryCtx) TagCount() (int, error) {
	return cx.cachedCount(&cx.tagCount, `SELECT COUNT(*) FROM tags WHERE is_alias = 0`)
}

// CollectionsCount returns the cached count of distinct non-empty
// collection labels (the `series` column) across non-missing images.
// Surfaced in the layout footer. Reads off idx_images_series (partial
// on `series != ''`) so the GROUP BY scans only labelled rows.
func (cx *galleryCtx) CollectionsCount() (int, error) {
	return cx.cachedCount(&cx.collectionsCount,
		`SELECT COUNT(*) FROM (SELECT 1 FROM images WHERE is_missing = 0 AND series != '' GROUP BY series)`)
}

// lookupByCeiling reads a per-ceiling cache slot; returns (zero, false)
// when the level isn't yet cached. Copy-on-write semantics: the caller
// stages a new map and Stores it on a miss so concurrent readers see a
// fully-formed snapshot.
func lookupByCeiling[V any](slot *atomic.Pointer[map[string]V], level string) (V, bool) {
	if m := slot.Load(); m != nil {
		if v, ok := (*m)[level]; ok {
			return v, true
		}
	}
	var zero V
	return zero, false
}

func storeByCeiling[V any](slot *atomic.Pointer[map[string]V], level string, value V) {
	for {
		current := slot.Load()
		newMap := make(map[string]V, 4)
		if current != nil {
			for k, v := range *current {
				newMap[k] = v
			}
		}
		newMap[level] = value
		if slot.CompareAndSwap(current, &newMap) {
			return
		}
	}
}

// VisibleCountUnder returns the count of non-missing images excluding
// any whose tag list intersects the ceiling's excluded rating ids. An
// inactive ceiling delegates to the blind VisibleCount.
func (cx *galleryCtx) VisibleCountUnder(c *Ceiling) (int, error) {
	if c == nil || !c.IsActive() {
		return cx.VisibleCount()
	}
	if v, ok := lookupByCeiling(&cx.visibleCountUnder, c.level); ok {
		return v, nil
	}
	n, err := gallery.VisibleCountUnder(cx.DB, c.ExcludedTagIDs())
	if err != nil {
		return 0, err
	}
	storeByCeiling(&cx.visibleCountUnder, c.level, n)
	return n, nil
}

// InboxCountUnder is the inbox analogue of VisibleCountUnder.
func (cx *galleryCtx) InboxCountUnder(c *Ceiling) (int, error) {
	if c == nil || !c.IsActive() {
		return cx.InboxCount()
	}
	if v, ok := lookupByCeiling(&cx.inboxCountUnder, c.level); ok {
		return v, nil
	}
	n, err := gallery.InboxCountUnder(cx.DB, c.ExcludedTagIDs())
	if err != nil {
		return 0, err
	}
	storeByCeiling(&cx.inboxCountUnder, c.level, n)
	return n, nil
}

// FavoritedCountUnder is the favourited analogue.
func (cx *galleryCtx) FavoritedCountUnder(c *Ceiling) (int, error) {
	if c == nil || !c.IsActive() {
		return cx.FavoritedCount()
	}
	if v, ok := lookupByCeiling(&cx.favoritedCountUnder, c.level); ok {
		return v, nil
	}
	n, err := gallery.FavoritedCountUnder(cx.DB, c.ExcludedTagIDs())
	if err != nil {
		return 0, err
	}
	storeByCeiling(&cx.favoritedCountUnder, c.level, n)
	return n, nil
}

// PhashMissingUnder returns the relations-hub "PhashMissing" count
// excluding rows above the ceiling. An inactive ceiling reads the
// uncached blind query (same shape loadRelationsCounts uses today) so
// the cold path stays equivalent to the existing behaviour.
func (cx *galleryCtx) PhashMissingUnder(c *Ceiling) (int, error) {
	if c == nil || !c.IsActive() {
		var n int
		err := cx.DB.Read.QueryRow(
			`SELECT COUNT(*) FROM images WHERE phash IS NULL AND is_missing = 0`,
		).Scan(&n)
		return n, err
	}
	if v, ok := lookupByCeiling(&cx.phashMissingUnder, c.level); ok {
		return v, nil
	}
	n, err := gallery.PhashMissingUnder(cx.DB, c.ExcludedTagIDs())
	if err != nil {
		return 0, err
	}
	storeByCeiling(&cx.phashMissingUnder, c.level, n)
	return n, nil
}

// FolderTreeUnder returns the ceiling-aware folder tree. An inactive
// ceiling delegates to the blind FolderTree cache.
func (cx *galleryCtx) FolderTreeUnder(c *Ceiling) ([]gallery.FolderNode, error) {
	if c == nil || !c.IsActive() {
		return cx.FolderTree()
	}
	if v, ok := lookupByCeiling(&cx.folderTreeUnder, c.level); ok {
		return v, nil
	}
	tree, err := gallery.FolderTreeUnder(cx.DB, c.ExcludedTagIDs())
	if err != nil {
		return nil, err
	}
	storeByCeiling(&cx.folderTreeUnder, c.level, tree)
	return tree, nil
}

// SourceCountsUnder returns the ceiling-aware AI source breakdown.
func (cx *galleryCtx) SourceCountsUnder(c *Ceiling) (gallery.SourceCounts, error) {
	if c == nil || !c.IsActive() {
		return cx.SourceCounts()
	}
	if v, ok := lookupByCeiling(&cx.sourceCountsUnder, c.level); ok {
		return v, nil
	}
	sc, err := gallery.SourceCountsUnderQuery(cx.DB, c.ExcludedTagIDs())
	if err != nil {
		return gallery.SourceCounts{}, err
	}
	storeByCeiling(&cx.sourceCountsUnder, c.level, sc)
	return sc, nil
}

// SeriesCountsUnder returns the ceiling-aware top-25 collection labels.
func (cx *galleryCtx) SeriesCountsUnder(c *Ceiling) ([]gallery.SeriesCount, error) {
	if c == nil || !c.IsActive() {
		return cx.SeriesCounts()
	}
	if v, ok := lookupByCeiling(&cx.seriesCountsUnder, c.level); ok {
		return v, nil
	}
	sc, err := gallery.SeriesCountsUnderQuery(cx.DB, 25, c.ExcludedTagIDs())
	if err != nil {
		return nil, err
	}
	storeByCeiling(&cx.seriesCountsUnder, c.level, sc)
	return sc, nil
}

// SourceLabelCountsUnder returns the ceiling-aware top-25 source labels.
func (cx *galleryCtx) SourceLabelCountsUnder(c *Ceiling) ([]gallery.SourceLabelCount, error) {
	if c == nil || !c.IsActive() {
		return cx.SourceLabelCounts()
	}
	if v, ok := lookupByCeiling(&cx.sourceLabelCountsUnder, c.level); ok {
		return v, nil
	}
	sc, err := gallery.SourceLabelCountsUnderQuery(cx.DB, 25, c.ExcludedTagIDs())
	if err != nil {
		return nil, err
	}
	storeByCeiling(&cx.sourceLabelCountsUnder, c.level, sc)
	return sc, nil
}

// warmCaches primes the per-gallery aggregations so the first user-facing
// sidebar/gallery/settings request doesn't pay the cold scan. Errors are
// ignored: the lazy path in each accessor still recomputes on demand if the
// warm failed.
func (cx *galleryCtx) warmCaches() {
	if cx == nil || cx.DB == nil {
		return
	}
	cx.FolderTree()        //nolint:errcheck
	cx.SourceCounts()      //nolint:errcheck
	cx.SeriesCounts()      //nolint:errcheck
	cx.SourceLabelCounts() //nolint:errcheck
	cx.VisibleCount()      //nolint:errcheck
	cx.InboxCount()        //nolint:errcheck
	cx.FavoritedCount()    //nolint:errcheck
	cx.TagCount()          //nolint:errcheck
	cx.CollectionsCount()  //nolint:errcheck
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
	tree := relations.NewBKTree()
	relations.DefaultRegistry.Register(database, tree)
	return &galleryCtx{
		Name:              g.Name,
		GalleryPath:       g.GalleryPath,
		DBPath:            g.DBPath,
		ThumbnailsPath:    g.ThumbnailsPath,
		DB:                database,
		TagSvc:            tags.New(database),
		RelationsSvc:      relations.New(database),
		Degraded:          degraded,
		GeneralCategoryID: generalID,
		bkTree:            tree,
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
		relations.DefaultRegistry.Unregister(cx.DB)
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

func (s *Server) relationsSvc() *relations.Service {
	if cx := s.Active(); cx != nil {
		return cx.RelationsSvc
	}
	return nil
}

// BKTree returns this gallery's lazily-built phash BK-tree. Subsequent
// callers see the ready tree without re-paying the build cost; the
// build runs serialised under the tree's own write lock so concurrent
// first-time callers don't race-rebuild.
func (cx *galleryCtx) BKTree() (*relations.BKTree, error) {
	if cx == nil || cx.bkTree == nil {
		return nil, nil
	}
	if err := cx.bkTree.EnsureBuilt(cx.DB); err != nil {
		return nil, err
	}
	return cx.bkTree, nil
}

// onImageDeleteCallback wires the active gallery's relations service
// into the gallery.DeleteImage signature. Returns nil when the
// service isn't available (e.g. mid-switch), so DeleteImage skips the
// relations cleanup step rather than crashing.
func (s *Server) onImageDeleteCallback() func(int64) error {
	svc := s.relationsSvc()
	if svc == nil {
		return nil
	}
	return svc.OnImageDelete
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
