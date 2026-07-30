//go:build tagger

package tagger

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/logx"
	ort "github.com/yalue/onnxruntime_go"
)

// IsAvailable reports whether at least one enabled tagger has its files.
func IsAvailable(cfg *config.Config) bool {
	return len(EnabledTaggers(cfg)) > 0
}

// buildSupportsInference is true in the tagger build, false in the noop
// build.
func buildSupportsInference() bool { return true }

// UnavailableReason explains why auto-tagging can't run, mirroring the
// reason shown in Settings → Auto-Tagger. Returns "" when IsAvailable.
func UnavailableReason(cfg *config.Config) string {
	if IsAvailable(cfg) {
		return ""
	}
	taggers := DiscoverTaggers(cfg)
	if len(taggers) == 0 {
		return "no tagger subfolders found under paths.model_path"
	}
	for _, t := range taggers {
		if t.Enabled && !t.Available {
			return t.Reason
		}
	}
	return "no enabled tagger"
}

// CheckProviderAvailable probes whether the ONNX Runtime library can
// initialize the requested execution provider. The settings handler calls
// it before persisting a non-CPU provider so the operator sees a library
// or device issue immediately rather than at tagger-job time.
func CheckProviderAvailable(provider string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" || provider == "cpu" {
		return nil
	}
	if !config.IsValidExecutionProvider(provider) {
		return fmt.Errorf("unsupported execution provider %q", provider)
	}

	ort.SetSharedLibraryPath(sharedLibPath())
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("ort init: %w", err)
	}
	defer func() { _ = ort.DestroyEnvironment() }()

	opts, err := ort.NewSessionOptions()
	if err != nil {
		return fmt.Errorf("ort session options: %w", err)
	}
	defer func() { _ = opts.Destroy() }()

	cleanup, err := appendExecutionProvider(opts, provider, "")
	if cleanup != nil {
		cleanup()
	}
	if err != nil {
		return fmt.Errorf("libonnxruntime does not support %s: %w", provider, err)
	}

	// A CUDA-capable library says nothing about the device: in a container
	// without the GPU passed in, session creation only fails at job time.
	if provider == "cuda" && runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/nvidia0"); err != nil {
			return fmt.Errorf("no NVIDIA GPU device found (pass the GPU into the container, e.g. Podman AddDevice=nvidia.com/gpu=all)")
		}
	}
	return nil
}

// AvailableTaggers returns every known tagger with availability set.
func AvailableTaggers(cfg *config.Config) []TaggerStatus {
	return DiscoverTaggers(cfg)
}

// Status snapshots the registered backend's cache state for the
// operator UI.
func Status() CacheStatus {
	if b := activeBackend(); b != nil {
		return b.Status()
	}
	return CacheStatus{}
}

// ReleaseIdle tears down the cached session set when it has been idle
// for at least `after`.
func ReleaseIdle(after time.Duration) bool {
	if b := activeBackend(); b != nil {
		return b.ReleaseIdle(after)
	}
	return false
}

// ReleaseAll unconditionally tears down the cached session set.
func ReleaseAll() {
	if b := activeBackend(); b != nil {
		b.ReleaseAll()
	}
}

// RunWithTaggers tags ids through the supplied taggers, merging
// results so each image ends up with one row per unique tag. Callers
// must pass only enabled+available taggers. provider overrides
// cfg.Tagger.ExecutionProvider so per-request callers can keep single-image
// runs on the CPU. mangaCacheDir is the per-gallery <data_path>/
// <gallery>/manga directory used to extract and cache cbz pages on
// demand; pass "" to fall back to a per-image temp directory.
// Returns the count of submitted ids left without auto_tagged_at.
func RunWithTaggers(ctx context.Context, database *db.DB, cfg *config.Config, ids []int64, taggers []TaggerStatus, mgr *jobs.Manager, provider string, mangaCacheDir string) (int, error) {
	if len(taggers) == 0 {
		return 0, fmt.Errorf("no tagger is enabled or available")
	}
	backend := activeBackend()
	if backend == nil {
		return 0, fmt.Errorf("auto-tagger disabled (no backend registered)")
	}

	// Loaded ahead of the backend so a fresh session set picks up the
	// current tag_categories rows when LoadDispatch resolves rule
	// targets. Reused below for the rating / wd14 / inferred chains.
	catIDs := map[string]int64{}
	catRows, err := database.Read.QueryContext(ctx, `SELECT id, name FROM tag_categories`)
	if err == nil {
		for catRows.Next() {
			var id int64
			var name string
			if scanErr := catRows.Scan(&id, &name); scanErr != nil {
				logx.Warnf("tagger: scan tag_categories: %v", scanErr)
				continue
			}
			catIDs[name] = id
		}
		_ = catRows.Close()
	}
	generalCatID := catIDs["general"]

	// Inference map for taggers whose category scheme can't tell apart
	// general from categorised counterparts (joytag's single_general,
	// camie when its category is "general"). Maps tag name → catID for
	// an existing non-general non-meta categorised tag. Ambiguous names
	// (multiple categorised variants) are dropped and fall back to
	// general. Lets joytag's `hakurei_reimu` attach to a pre-existing
	// `character:hakurei_reimu` instead of going under general.
	inferredCats := map[string]int64{}
	hasSingleGeneral := false
	for _, t := range taggers {
		profile, perr := ResolveProfile(cfg.Paths.ModelPath, t.Name, t.TagsFile)
		if perr == nil && profile.CategoryScheme == "single_general" {
			hasSingleGeneral = true
			break
		}
	}
	if hasSingleGeneral && generalCatID != 0 {
		// Skip names whose general counterpart already carries a manual
		// image_tag - that's an explicit user choice.
		infRows, err := database.Read.QueryContext(ctx, `
			SELECT t.name, t.category_id
			FROM tags t
			JOIN tag_categories tc ON tc.id = t.category_id
			WHERE t.is_alias = 0
			  AND tc.name NOT IN ('general', 'meta')
			  AND NOT EXISTS (
			      SELECT 1 FROM tags g
			      JOIN image_tags it ON it.tag_id = g.id
			      WHERE g.name = t.name
			        AND g.category_id = ?
			        AND g.is_alias = 0
			        AND it.is_auto = 0
			  )`, generalCatID)
		if err == nil {
			ambiguous := map[string]bool{}
			for infRows.Next() {
				var n string
				var cid int64
				if err := infRows.Scan(&n, &cid); err != nil {
					continue
				}
				if ambiguous[n] {
					continue
				}
				if existing, ok := inferredCats[n]; ok && existing != cid {
					ambiguous[n] = true
					delete(inferredCats, n)
					continue
				}
				inferredCats[n] = cid
			}
			_ = infRows.Close()
		}
	}

	parallel := min(max(1, cfg.Tagger.Parallel), len(ids))

	// jobs.Manager carries a single status string; parallel workers
	// writing into it would each clobber the others' progress, making
	// the displayed message hop between mangas. Per-worker slots plus
	// a serialising mutex turn every emission into a single combined
	// snapshot - workers see and write the same view of all peers, so
	// the displayed message is always consistent regardless of which
	// goroutine fired the update.
	total := len(ids)
	var completed atomic.Int64
	var statusMu sync.Mutex
	workerStatus := make([]string, parallel)
	// Cap the number of per-worker entries the status bar shows; at
	// parallel=8 with every worker on a long cbz the joined string
	// otherwise overflows the flash slot.
	const maxVisibleWorkers = 3
	emitStatus := func(workerIdx int, msg string) {
		statusMu.Lock()
		defer statusMu.Unlock()
		workerStatus[workerIdx] = msg
		active := slices.DeleteFunc(slices.Clone(workerStatus), func(s string) bool { return s == "" })
		out := "tagging images"
		if len(active) > 0 {
			shown := active
			if len(shown) > maxVisibleWorkers {
				shown = shown[:maxVisibleWorkers]
			}
			out = strings.Join(shown, " · ")
			if extra := len(active) - len(shown); extra > 0 {
				out = fmt.Sprintf("%s (+%d more)", out, extra)
			}
		}
		mgr.Update(int(completed.Load()), total, out)
	}

	taggerNames := make([]string, 0, len(taggers))
	for _, t := range taggers {
		taggerNames = append(taggerNames, t.Name)
	}

	// Build the batch payload: look up each id's canonical path and
	// file type, extract frames (videos, cbz pages), and ship the
	// resolved paths to the backend. Frame cleanup runs after the
	// backend returns this image's slot in the response.
	var skipped atomic.Int64
	requests := make([]BackendImageRequest, 0, len(ids))
	cleanups := make([]func(), 0, len(ids))
	for _, imageID := range ids {
		if ctx.Err() != nil {
			break
		}
		var canonPath, fileType string
		if err := database.Read.QueryRowContext(ctx,
			`SELECT canonical_path, file_type FROM images WHERE id = ?`, imageID,
		).Scan(&canonPath, &fileType); err != nil {
			logx.Warnf("tagger: skip image %d: lookup failed: %v", imageID, err)
			skipped.Add(1)
			continue
		}
		framePaths, cleanup := framesForTagging(canonPath, fileType, mangaCacheDir, imageID)
		if len(framePaths) == 0 {
			logx.Warnf("tagger: skip image %d: no frames available (missing file, archive, or ffmpeg)", imageID)
			skipped.Add(1)
			cleanup()
			continue
		}
		requests = append(requests, BackendImageRequest{
			ID:            imageID,
			FramePaths:    framePaths,
			MangaProgress: fileType == "cbz" && len(framePaths) > 1,
		})
		cleanups = append(cleanups, cleanup)
	}
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	resp, err := backend.Run(ctx, RunRequest{
		Cfg:            cfg,
		Taggers:        taggers,
		Provider:       provider,
		CatIDs:         catIDs,
		GeneralCatID:   generalCatID,
		InferredCats:   inferredCats,
		MinHitFraction: cfg.Tagger.Aggregation.MinHitFraction,
		Parallel:       parallel,
		Images:         requests,
		OnProgress: func(workerIdx int, msg string) {
			// The backend's per-image-done convention is OnProgress
			// with an empty msg; non-empty msg is per-page cbz
			// status. Use the empty-msg event to drive the live
			// counter so the flash shows N/total during the run
			// instead of jumping from 0 to total at completion.
			if msg == "" {
				completed.Add(1)
			}
			emitStatus(workerIdx, msg)
		},
	})
	if err != nil {
		return int(skipped.Load()), err
	}

	for _, r := range resp.Results {
		if r.Err != "" {
			skipped.Add(1)
			continue
		}
		if r.Tags == nil {
			// Cancelled mid-image - skip writing partial state.
			continue
		}
		if storeErr := storeResults(ctx, database, r.ID, r.Tags, taggerNames, catIDs["rating"]); storeErr != nil {
			logx.Warnf("tagger: store results for image %d: %v", r.ID, storeErr)
			skipped.Add(1)
		}
	}

	// Final status update so the progress bar reaches total when the
	// last image is the cancelled / skipped tail.
	mgr.Update(int(completed.Load()), total, "tagging images")
	return int(skipped.Load()), ctx.Err()
}

// RunRemoteImages runs the tagger backend on in-memory image data and
// returns merged tag results without touching any database.
func RunRemoteImages(ctx context.Context, cfg *config.Config, taggers []TaggerStatus, catIDs map[string]int64, images []BackendImageRequest) (RunResponse, error) {
	backend := activeBackend()
	if backend == nil {
		return RunResponse{}, fmt.Errorf("auto-tagger disabled (no backend registered)")
	}

	generalCatID := catIDs["general"]

	parallel := min(max(1, cfg.Tagger.Parallel), len(images))

	resp, err := backend.Run(ctx, RunRequest{
		Cfg:            cfg,
		Taggers:        taggers,
		CatIDs:         catIDs,
		GeneralCatID:   generalCatID,
		MinHitFraction: cfg.Tagger.Aggregation.MinHitFraction,
		Parallel:       parallel,
		Images:         images,
	})
	if err != nil {
		return RunResponse{}, err
	}
	return resp, nil
}

// framesForTagging returns the file paths to feed the tagger plus a
// cleanup func. Branches by file type:
//   - static images: [canonPath], no-op cleanup.
//   - videos: up to five frames sampled via ffmpeg, removed by cleanup.
//   - cbz manga: every page extracted into the per-gallery manga cache
//     (or a temp directory when mangaCacheDir is empty); the cache
//     entries are deliberately left on disk so idle reclaim handles
//     eviction five minutes after the last use, mirroring the
//     reader's serve path.
//
// With ffmpeg missing or failing, videos yield no frames and the
// caller skips the asset; an unreadable archive does the same.
func framesForTagging(canonPath, fileType, mangaCacheDir string, imageID int64) ([]string, func()) {
	switch fileType {
	case "mp4", "webm":
		positions := []float64{0.10, 0.30, 0.50, 0.70, 0.90}
		frames, _ := gallery.ExtractVideoFrames(canonPath, os.TempDir(), positions)
		cleanup := func() {
			for _, p := range frames {
				_ = os.Remove(p)
			}
		}
		return frames, cleanup
	case "cbz":
		archive, err := gallery.OpenManga(canonPath)
		if err != nil {
			logx.Warnf("tagger: open manga %q: %v", canonPath, err)
			return nil, func() {}
		}
		pageCount := len(archive.Pages)
		_ = archive.Close()
		cacheRoot := mangaCacheDir
		var tempDir string
		if cacheRoot == "" {
			tempDir, err = os.MkdirTemp("", "manga-frames-*")
			if err != nil {
				logx.Warnf("tagger: temp dir for manga frames: %v", err)
				return nil, func() {}
			}
			cacheRoot = tempDir
		}
		paths := make([]string, 0, pageCount)
		for i := 1; i <= pageCount; i++ {
			path, err := gallery.EnsureMangaPageInCache(cacheRoot, canonPath, imageID, i)
			if err != nil {
				logx.Warnf("tagger: extract page %d of %q: %v", i, canonPath, err)
				continue
			}
			paths = append(paths, path)
		}
		cleanup := func() {
			if tempDir != "" {
				_ = os.RemoveAll(tempDir)
			}
		}
		return paths, cleanup
	}
	return []string{canonPath}, func() {}
}
