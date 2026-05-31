package gallery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/leqwin/monbooru/internal/logx"
)

// MangaPageCacheTTL is the per-page idle reclaim window. Pages whose
// mtime is older than this are unlinked by the per-gallery reclaim
// goroutine; the cover thumbnail is exempt.
const MangaPageCacheTTL = 5 * time.Minute

// mangaReclaimInterval is the reclaim ticker's period. Constant rather
// than configurable per spec §14: shorter than the TTL so a freshly
// idle page evicts within a window's slack of the deadline.
const mangaReclaimInterval = 60 * time.Second

// MangaCacheDir derives the per-gallery manga cache directory from the
// gallery's thumbnails directory. Manga cache lives at
// `<data_path>/<gallery>/manga/`, sibling to thumbnails.
func MangaCacheDir(thumbnailsPath string) string {
	if thumbnailsPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(thumbnailsPath), "manga")
}

// MangaImageDir returns the per-image cache subdirectory under the
// gallery's manga cache. Created on demand by the page-extract path.
func MangaImageDir(thumbnailsPath string, imageID int64) string {
	return filepath.Join(MangaCacheDir(thumbnailsPath), fmt.Sprintf("%d", imageID))
}

// MangaPagePath returns the on-disk path for the n-th cached page (1-
// based) with the supplied original-extension tail. Zero-padded to
// four digits so a directory listing sorts in display order.
func MangaPagePath(imageDir string, n int, ext string) string {
	if ext == "" {
		ext = ".bin"
	}
	return filepath.Join(imageDir, fmt.Sprintf("page_%04d%s", n, ext))
}

// MangaPageThumbPath is the per-page thumbnail companion to
// MangaPagePath. JPEG by construction.
func MangaPageThumbPath(imageDir string, n int) string {
	return filepath.Join(imageDir, fmt.Sprintf("page_%04d_thumb.jpg", n))
}

// extractedPageInDir returns the existing on-disk file for the n-th
// page, regardless of which extension it was extracted as. Empty when
// no file matches. The cache stores at most one extension per (id, n)
// because ExtractPage uses the archive entry's extension and a comic
// can't carry the same page twice under different names.
func extractedPageInDir(imageDir string, n int) string {
	prefix := fmt.Sprintf("page_%04d", n)
	entries, err := os.ReadDir(imageDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// Skip the thumbnail companion when looking for raw bytes.
		if strings.HasSuffix(name, "_thumb.jpg") {
			continue
		}
		// Reject names with extra digits after the prefix
		// (page_00010 vs page_0001).
		rest := name[len(prefix):]
		if rest == "" || rest[0] != '.' {
			continue
		}
		return filepath.Join(imageDir, name)
	}
	return ""
}

// TouchCacheFile bumps the mtime/atime of path to now. Used on every
// cache hit so the per-gallery reclaim goroutine sees recently-served
// pages as live. Best-effort: a chtimes failure logs at debug and the
// cache hit still proceeds.
func TouchCacheFile(path string) {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		logx.Debugf("manga: chtimes %q: %v", path, err)
	}
}

// EnsureMangaPage returns the on-disk path for the n-th page of the
// archive at canonPath, extracting it into the per-image cache on
// miss. n is 1-based. The returned path is suitable for
// http.ServeFile; mtime is bumped on hit so the reclaim goroutine
// counts the access.
func EnsureMangaPage(thumbnailsPath, canonPath string, imageID int64, n int) (string, error) {
	return ensureMangaPageInDir(MangaImageDir(thumbnailsPath, imageID), canonPath, n)
}

// EnsureMangaPageInCache extracts to <cacheRoot>/<imageID>/page_NNNN
// directly. Used by the auto-tagger which threads the per-gallery
// manga cache directory through RunWithTaggers without owning the
// full thumbnails-path derivation.
func EnsureMangaPageInCache(cacheRoot, canonPath string, imageID int64, n int) (string, error) {
	return ensureMangaPageInDir(filepath.Join(cacheRoot, fmt.Sprintf("%d", imageID)), canonPath, n)
}

func ensureMangaPageInDir(imageDir, canonPath string, n int) (string, error) {
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return "", fmt.Errorf("create manga cache dir: %w", err)
	}
	if existing := extractedPageInDir(imageDir, n); existing != "" {
		TouchCacheFile(existing)
		return existing, nil
	}
	archive, err := OpenManga(canonPath)
	if err != nil {
		return "", err
	}
	defer archive.Close()
	if n < 1 || n > len(archive.Pages) {
		return "", fmt.Errorf("page %d out of range [1,%d]", n, len(archive.Pages))
	}
	ext := archive.PageCacheExt(n - 1)
	dst := MangaPagePath(imageDir, n, ext)
	if err := archive.ExtractPage(n-1, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// EnsureMangaPageThumb returns the on-disk path of the n-th page's
// thumbnail (300px-longest-side JPEG Q85). Generated on miss from the
// raw page bytes (which may themselves be extracted on demand).
func EnsureMangaPageThumb(thumbnailsPath, canonPath string, imageID int64, n int) (string, error) {
	imageDir := MangaImageDir(thumbnailsPath, imageID)
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return "", fmt.Errorf("create manga cache dir: %w", err)
	}
	thumb := MangaPageThumbPath(imageDir, n)
	if _, err := os.Stat(thumb); err == nil {
		TouchCacheFile(thumb)
		return thumb, nil
	}
	pagePath, err := EnsureMangaPage(thumbnailsPath, canonPath, imageID, n)
	if err != nil {
		return "", err
	}
	if err := generateImageThumbFromAny(pagePath, thumb); err != nil {
		return "", err
	}
	return thumb, nil
}

// generateImageThumbFromAny decodes any of the supported page formats
// and writes a 300-px-longest-side JPEG thumbnail. Mirrors
// generateImageThumb but uses image.Decode (not the file-type-keyed
// branch) so we don't need to round-trip the extension.
func generateImageThumbFromAny(srcPath, dstPath string) error {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return fmt.Errorf("create thumb dir: %w", err)
	}
	if err := generateImageThumb(srcPath, dstPath, ""); err != nil {
		return err
	}
	return nil
}

// RemoveMangaCache removes the per-image cache directory. Called from
// the per-image delete path so a deleted manga's pages and cover
// disappear with the row, and from sync's in-place-edit branch so a cbz
// whose bytes changed drops its stale page cache before the thumbnails
// are regenerated.
func RemoveMangaCache(thumbnailsPath string, imageID int64) {
	dir := MangaImageDir(thumbnailsPath, imageID)
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		logx.Warnf("manga cache: remove %q: %v", dir, err)
	}
}

// MangaCacheReclaimer ticks every minute and unlinks `page_NNNN.<ext>`
// raw-bytes files older than the TTL. cover.jpg and `page_NNNN_thumb.jpg`
// are exempt: the cover backs the gallery thumbnail and the page thumbs
// are pre-generated at ingest, so the reclaim is scoped to the lazy
// reader-bytes cache that grows during reading sessions. Run alongside
// the gallery's watcher; stops on ctx cancel.
type MangaCacheReclaimer struct {
	dir string
	ttl time.Duration

	mu       sync.Mutex
	cancel   context.CancelFunc
	doneOnce sync.Once
	done     chan struct{}
}

// NewMangaCacheReclaimer constructs a reclaimer for the given gallery
// manga cache directory. dir must be the per-gallery `<data_path>/
// <gallery>/manga` path.
func NewMangaCacheReclaimer(dir string) *MangaCacheReclaimer {
	return &MangaCacheReclaimer{dir: dir, ttl: MangaPageCacheTTL, done: make(chan struct{})}
}

// Start spawns the reclaim goroutine. Idempotent against repeated
// calls on the same instance; subsequent Start calls are no-ops.
func (r *MangaCacheReclaimer) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	go r.run(cctx)
}

// Stop cancels the goroutine and waits for it to exit. Idempotent.
func (r *MangaCacheReclaimer) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-r.done
}

func (r *MangaCacheReclaimer) run(ctx context.Context) {
	defer r.doneOnce.Do(func() { close(r.done) })
	t := time.NewTicker(mangaReclaimInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweepOnce()
		}
	}
}

// sweepOnce walks every <id>/ subdirectory and unlinks page_* files
// past the TTL. Cover.jpg files are exempt because they back the
// gallery thumbnail surface for the comic's lifetime.
func (r *MangaCacheReclaimer) sweepOnce() {
	if r.dir == "" {
		return
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-r.ttl)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		idDir := filepath.Join(r.dir, e.Name())
		files, err := os.ReadDir(idDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if !strings.HasPrefix(name, "page_") {
				continue
			}
			// Page thumbnails are pre-generated at ingest and never
			// regenerated during steady-state reading; reclaiming them
			// would force the next /pages render back onto the slow
			// lazy-extract path, defeating the precompute.
			if strings.HasSuffix(name, "_thumb.jpg") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				path := filepath.Join(idDir, name)
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					logx.Debugf("manga reclaim: remove %q: %v", path, err)
				}
			}
		}
	}
}
