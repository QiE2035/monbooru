package tagger

import (
	"context"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
)

// Backend is the boundary the inference loop crosses. The default
// implementation is the subprocess client (ipc.go); the in-process
// fallback lives in inproc.go for the worker subcommand and the
// rollback env var.
type Backend interface {
	// Run executes inference for one or more pre-prepared images.
	// Per-image inference errors land on the matching Result.Err so
	// the orchestrator can skip individual images without aborting
	// the whole batch.
	Run(ctx context.Context, req RunRequest) (RunResponse, error)
	// Status snapshots the warm-cache state for the operator UI.
	Status() CacheStatus
	// ReleaseIdle tears the cache down when it has been idle for at
	// least after and no run is in flight. Returns true on teardown
	// so the caller can log it.
	ReleaseIdle(after time.Duration) bool
	// ReleaseAll unconditionally tears the cache down. Called on
	// use_cuda flips and on server shutdown.
	ReleaseAll()
}

// WorkerPID returns the live tagger-worker child's PID and true when
// the IPC backend currently has a worker running, or (0, false)
// otherwise (in-process backend, child not yet spawned, child exited).
// The stats panel uses this to read /proc/<pid>/smaps for the
// child's resident-set breakdown.
func WorkerPID() (int, bool) {
	if b := activeBackend(); b != nil {
		if w, ok := b.(workerPIDer); ok {
			return w.WorkerPID()
		}
	}
	return 0, false
}

type workerPIDer interface {
	WorkerPID() (int, bool)
}

// TagKey identifies one (name, category_id) pair so multi-tagger
// merges never insert the same tag twice on the same image. Both the
// orchestrator and the worker backend use this as the merge key.
type TagKey struct {
	Name  string
	CatID int64
}

// Scored carries the highest confidence seen across taggers for one
// TagKey plus the tagger that produced that score, so attribution
// survives multi-tagger merges.
type Scored struct {
	Score      float32
	TaggerName string
}

// CacheStatus is a snapshot of the tagger cache for surfacing in the
// operator UI. Sessions lists the loaded model names sorted so a
// refresh doesn't reorder the list. Returns Loaded=false when no
// model set is currently warm.
type CacheStatus struct {
	Loaded   bool
	UseCUDA  bool
	InUse    bool
	Sessions []string
	LastUsed time.Time
}

// RunRequest is the per-batch payload. Each BackendImageRequest carries
// the pre-extracted frame paths so the backend never reads images from
// the gallery DB or unpacks videos / cbz archives - that work stays in
// the orchestrator and ports unchanged to a future subprocess split.
type RunRequest struct {
	Cfg            *config.Config
	Taggers        []TaggerStatus
	UseCUDA        bool
	CatIDs         map[string]int64
	GeneralCatID   int64
	InferredCats   map[string]int64
	MinHitFraction float64
	Parallel       int
	Images         []BackendImageRequest
	// OnProgress fires from inside the inference loop, e.g. cbz
	// per-page status. workerIdx scopes the worker pool. Optional;
	// nil disables emission.
	OnProgress func(workerIdx int, msg string)
}

// BackendImageRequest is one image's contribution to the batch: the
// gallery id (used for log lines + as the result key) and the
// already-extracted frame paths to feed inference. MangaProgress is
// true only on cbz rows whose page count makes the wait worth
// narrating; the backend then fires OnProgress per page.
type BackendImageRequest struct {
	ID            int64
	FramePaths    []string
	MangaProgress bool
}

// RunResponse mirrors RunRequest.Images in order. Tags is the merged
// per-image score set, keyed by TagKey so the orchestrator can hand
// it straight to storeResults.
type RunResponse struct {
	Results []BackendImageResult
}

// BackendImageResult is one image's outcome. Err carries per-image
// inference failures so the orchestrator can increment skipped without
// aborting the whole batch. Err is a string (not an error interface)
// because the result travels over the IPC channel via gob, and gob
// can't encode an interface field without Register'd concrete types.
type BackendImageResult struct {
	ID   int64
	Tags map[TagKey]Scored
	Err  string
}

var (
	backendMu      sync.RWMutex
	currentBackend Backend
)

// SetBackend installs the inference implementation. inproc.go's init
// picks the IPC backend by default and falls back to the in-proc one
// when MONBOORU_TAGGER_BACKEND=inproc or the IPC constructor fails.
func SetBackend(b Backend) {
	backendMu.Lock()
	currentBackend = b
	backendMu.Unlock()
}

func activeBackend() Backend {
	backendMu.RLock()
	defer backendMu.RUnlock()
	return currentBackend
}
