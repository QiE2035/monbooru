//go:build tagger

package tagger

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	ort "github.com/yalue/onnxruntime_go"
)

// loadedTagger pairs a cached ORT session with the per-call config the
// inference loop reads, so threshold edits take effect without
// rebuilding the session. Candidates carries the per-label routing
// resolved once at the top of Run so the aggregator can index in
// directly instead of recomputing per image.
type loadedTagger struct {
	cfg        config.TaggerInstance
	session    *ort.DynamicAdvancedSession
	labels     []tagLabel
	candidates []CandidateLabel
	profile    Profile
	inputSize  int
	dispatch   *DispatchTable
}

// loadedSession is the cached half of loadedTagger: ORT state keyed
// by tagger name. modelFile and tagsFile gate cache reuse - a TOML
// edit that swaps either invalidates the entry; profileFP additionally
// invalidates on a tagger.json sidecar edit.
type loadedSession struct {
	modelFile string
	tagsFile  string
	profileFP string
	session   *ort.DynamicAdvancedSession
	labels    []tagLabel
	profile   Profile
	inputSize int
}

// inprocBackend is the in-process implementation of Backend. It owns
// the long-lived ORT environment plus the per-tagger session cache;
// idle release and warm-cache reuse mirror the previous package-level
// cache exactly.
type inprocBackend struct {
	mu          sync.Mutex
	inUse       bool
	initialized bool
	useCUDA     bool
	sessionOpts *ort.SessionOptions
	cudaOpts    *ort.CUDAProviderOptions
	sessions    map[string]*loadedSession
	lastUsed    time.Time
}

var defaultBackend = &inprocBackend{}

// UseInprocBackend forces the in-process backend, replacing any
// previously-registered Backend. The tagger-worker subcommand calls
// this on entry so the child runs inference itself instead of trying
// to spawn a grandchild.
func UseInprocBackend() {
	SetBackend(defaultBackend)
}

func init() {
	// Default backend is the subprocess client: the parent never
	// loads CUDA libraries or holds an ORT environment, so its RSS
	// stays at the no-tagger baseline even after autotag jobs have
	// run. MONBOORU_TAGGER_BACKEND=inproc is an undocumented
	// rollback for operators who hit a subprocess regression.
	if os.Getenv("MONBOORU_TAGGER_BACKEND") == "inproc" {
		SetBackend(defaultBackend)
		return
	}
	if b, err := newIPCBackend(); err == nil {
		SetBackend(b)
		return
	}
	SetBackend(defaultBackend)
}

// satisfies returns true when the cached set covers every requested
// tagger with the same execution-provider mode, the same model / tags
// filenames, and the same profile fingerprint. The profile check picks
// up tagger.json sidecar edits without a manual reload. Caller must
// hold b.mu.
func (b *inprocBackend) satisfies(modelPath string, taggers []TaggerStatus, useCUDA bool) bool {
	if !b.initialized || b.useCUDA != useCUDA {
		return false
	}
	for _, t := range taggers {
		s, ok := b.sessions[t.Name]
		if !ok {
			return false
		}
		if s.modelFile != t.ModelFile || s.tagsFile != t.TagsFile {
			return false
		}
		profile, err := ResolveProfile(modelPath, t.Name, t.TagsFile)
		if err != nil {
			return false
		}
		if profile.fingerprint() != s.profileFP {
			return false
		}
	}
	return true
}

// ensure populates the cache for (taggers, useCUDA). On signature
// mismatch the existing cache is torn down first. Caller must hold
// b.mu.
func (b *inprocBackend) ensure(cfg *config.Config, taggers []TaggerStatus, useCUDA bool) error {
	if b.satisfies(cfg.Paths.ModelPath, taggers, useCUDA) {
		logx.Infof("tagger: reusing warm cache (%d session(s))", len(b.sessions))
		return nil
	}
	if b.initialized {
		b.teardownLocked()
	}

	mode := "CPU"
	if useCUDA {
		mode = "CUDA"
	}
	logx.Infof("tagger: loading %d session(s) on %s", len(taggers), mode)
	loadStart := time.Now()

	// First-ever CUDA inference on a host with a recent GPU pays a
	// JIT-compilation cost (cuDNN compiles PTX kernels for the live
	// compute capability, tens of seconds on Blackwell). CUDA caches
	// the JIT'd kernels under $HOME/.nv/ComputeCache by default;
	// inside a container that path is in the writable overlay and
	// disappears on restart. Point CUDA_CACHE_PATH at <data_path>/
	// .nv-cache so the cache survives container recycles. Honour an
	// operator-set CUDA_CACHE_PATH if one is already in the
	// environment.
	if useCUDA && os.Getenv("CUDA_CACHE_PATH") == "" {
		cacheDir := filepath.Join(cfg.Paths.DataPath, ".nv-cache")
		if err := os.MkdirAll(cacheDir, 0o755); err == nil {
			os.Setenv("CUDA_CACHE_PATH", cacheDir)
		}
	}

	ort.SetSharedLibraryPath(sharedLibPath())
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("ort init: %w", err)
	}
	b.initialized = true

	opts, err := ort.NewSessionOptions()
	if err != nil {
		b.teardownLocked()
		return fmt.Errorf("ort session options: %w", err)
	}
	b.sessionOpts = opts

	// ORT defaults intra_op_num_threads to the host's physical core
	// count. With cfg.Tagger.Parallel goroutines each calling Run, that
	// oversubscribes the CPU; split the cores evenly across workers and
	// pin inter-op to 1 so the per-Run scheduler doesn't fan out again.
	// CUDA ignores these for kernel work but cuDNN still honours them
	// for host-side helpers.
	parallel := max(1, cfg.Tagger.Parallel)
	intra := max(1, runtime.NumCPU()/parallel)
	if err := opts.SetIntraOpNumThreads(intra); err != nil {
		b.teardownLocked()
		return fmt.Errorf("set intra-op threads: %w", err)
	}
	if err := opts.SetInterOpNumThreads(1); err != nil {
		b.teardownLocked()
		return fmt.Errorf("set inter-op threads: %w", err)
	}

	if useCUDA {
		// Drop the CPU-side initializer copies once weights upload to
		// the device; ORT otherwise keeps a duplicate in host memory
		// for the lifetime of the session.
		if err := opts.AddSessionConfigEntry("session.use_device_allocator_for_initializers", "1"); err != nil {
			b.teardownLocked()
			return fmt.Errorf("ort session config: %w", err)
		}
		cudaOpts, err := ort.NewCUDAProviderOptions()
		if err != nil {
			b.teardownLocked()
			return fmt.Errorf("ort cuda options (ensure libonnxruntime was built with CUDA): %w", err)
		}
		b.cudaOpts = cudaOpts
		// HEURISTIC trades the multi-second first-Run cuDNN search
		// (EXHAUSTIVE default) for a fast algorithm pick that ORT's
		// own docs put within a few percent of optimal on most CNNs.
		// kSameAsRequested grows the GPU arena by the requested size
		// instead of doubling; default is fine for training but
		// inflates the arena on small-batch inference. Keeping copies
		// on the default stream avoids the cross-stream sync that
		// only pays off when the secondary stream is fully utilised.
		if err := cudaOpts.Update(map[string]string{
			"cudnn_conv_algo_search":    "HEURISTIC",
			"arena_extend_strategy":     "kSameAsRequested",
			"do_copy_in_default_stream": "1",
		}); err != nil {
			b.teardownLocked()
			return fmt.Errorf("update cuda options: %w", err)
		}
		if err := opts.AppendExecutionProviderCUDA(cudaOpts); err != nil {
			b.teardownLocked()
			return fmt.Errorf("append cuda provider: %w", err)
		}
	}
	b.useCUDA = useCUDA

	b.sessions = make(map[string]*loadedSession, len(taggers))
	for _, t := range taggers {
		onnxPath := filepath.Join(cfg.Paths.ModelPath, t.Name, t.ModelFile)
		tagsPath := filepath.Join(cfg.Paths.ModelPath, t.Name, t.TagsFile)
		profile, err := ResolveProfile(cfg.Paths.ModelPath, t.Name, t.TagsFile)
		if err != nil {
			b.teardownLocked()
			return fmt.Errorf("resolve profile for %q: %w", t.Name, err)
		}
		labels, err := loadLabels(tagsPath, profile)
		if err != nil {
			b.teardownLocked()
			return fmt.Errorf("load labels for %q: %w", t.Name, err)
		}
		inputs, outputs, err := ort.GetInputOutputInfo(onnxPath)
		if err != nil {
			b.teardownLocked()
			return fmt.Errorf("inspect ort model for %q: %w", t.Name, err)
		}
		if len(inputs) == 0 || len(outputs) == 0 {
			b.teardownLocked()
			return fmt.Errorf("ort model for %q has no input/output", t.Name)
		}
		outIdx := profile.OutputIndex
		if outIdx < 0 || outIdx >= len(outputs) {
			b.teardownLocked()
			return fmt.Errorf("ort model for %q: profile output_index %d out of range (have %d)", t.Name, outIdx, len(outputs))
		}
		inputSize := profile.InputSize
		if inputSize == 0 {
			inputSize = inferInputSize(inputs[0].Dimensions, profile.Layout)
			if inputSize <= 0 {
				b.teardownLocked()
				return fmt.Errorf("ort model for %q: cannot infer input size from dimensions %v", t.Name, inputs[0].Dimensions)
			}
		}
		session, err := ort.NewDynamicAdvancedSession(onnxPath,
			[]string{inputs[0].Name}, []string{outputs[outIdx].Name}, b.sessionOpts)
		if err != nil {
			b.teardownLocked()
			return fmt.Errorf("create ort session for %q: %w", t.Name, err)
		}
		b.sessions[t.Name] = &loadedSession{
			modelFile: t.ModelFile,
			tagsFile:  t.TagsFile,
			profileFP: profile.fingerprint(),
			session:   session,
			labels:    labels,
			profile:   profile,
			inputSize: inputSize,
		}
	}
	logx.Infof("tagger: cache ready in %s", time.Since(loadStart).Round(10*time.Millisecond))
	return nil
}

// inferInputSize picks the spatial axis from an ONNX input's Dimensions
// shape based on the profile's layout. Returns 0 when the shape is
// degenerate (dynamic axis as -1, or unexpected rank). NHWC: shape is
// [N,H,W,C]; NCHW: shape is [N,C,H,W]. We prefer H over W; the two are
// equal for every square-input tagger we ship.
func inferInputSize(dims ort.Shape, layout string) int {
	if len(dims) != 4 {
		return 0
	}
	var d int64
	switch layout {
	case "nhwc":
		d = dims[1]
	case "nchw":
		d = dims[2]
	default:
		return 0
	}
	if d <= 0 {
		return 0
	}
	return int(d)
}

// teardownLocked destroys every cached ORT object and asks glibc to
// return the freed bytes to the kernel. Caller must hold b.mu.
func (b *inprocBackend) teardownLocked() {
	for _, s := range b.sessions {
		s.session.Destroy()
	}
	b.sessions = nil
	if b.cudaOpts != nil {
		b.cudaOpts.Destroy()
		b.cudaOpts = nil
	}
	if b.sessionOpts != nil {
		b.sessionOpts.Destroy()
		b.sessionOpts = nil
	}
	if b.initialized {
		ort.DestroyEnvironment()
		b.initialized = false
	}
	b.useCUDA = false
	mallocTrim()
}

// ReleaseIdle tears down the cached session set when it has been idle
// for at least `after` and no run is in flight.
func (b *inprocBackend) ReleaseIdle(after time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inUse || !b.initialized {
		return false
	}
	if time.Since(b.lastUsed) < after {
		return false
	}
	b.teardownLocked()
	return true
}

// ReleaseAll unconditionally tears down the cached session set.
func (b *inprocBackend) ReleaseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.initialized {
		b.teardownLocked()
	}
}

// Status copies the cache state under the lock so the caller never
// touches backend internals. Returns Loaded=false when no model set
// is currently warm.
func (b *inprocBackend) Status() CacheStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.initialized {
		return CacheStatus{}
	}
	out := CacheStatus{
		Loaded:   true,
		UseCUDA:  b.useCUDA,
		InUse:    b.inUse,
		LastUsed: b.lastUsed,
		Sessions: make([]string, 0, len(b.sessions)),
	}
	for name := range b.sessions {
		out.Sessions = append(out.Sessions, name)
	}
	sort.Strings(out.Sessions)
	return out
}

// Run executes inference for a batch of pre-prepared images and
// returns one result per image in submission order. Per-image errors
// land on Result.Err; whole-batch failures (no taggers, ORT init
// failure) come back as the error return.
func (b *inprocBackend) Run(ctx context.Context, req RunRequest) (RunResponse, error) {
	if len(req.Taggers) == 0 {
		return RunResponse{}, fmt.Errorf("no tagger is enabled or available")
	}

	b.mu.Lock()
	if err := b.ensure(req.Cfg, req.Taggers, req.UseCUDA); err != nil {
		b.mu.Unlock()
		return RunResponse{}, err
	}
	loaded := make([]loadedTagger, len(req.Taggers))
	for i, t := range req.Taggers {
		s := b.sessions[t.Name]
		loaded[i] = loadedTagger{
			cfg:       t.TaggerInstance,
			session:   s.session,
			labels:    s.labels,
			profile:   s.profile,
			inputSize: s.inputSize,
			dispatch:  LoadDispatch(req.Cfg.Paths.ModelPath, t.Name, req.CatIDs),
		}
	}
	b.inUse = true
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		b.inUse = false
		b.lastUsed = time.Now()
		// idle_release_after_minutes <= 0 disables caching: tear
		// down right after the run so RSS drops back to baseline.
		if req.Cfg.Tagger.IdleReleaseAfterMinutes <= 0 {
			b.teardownLocked()
		}
		b.mu.Unlock()
	}()

	// Resolve every label's routing once per loaded tagger. Inputs
	// (profile, catIDs, dispatch, inferredCats) are invariant across
	// images; doing this per-image burns 10k+ map lookups and
	// function calls on each pass.
	for i := range loaded {
		lt := &loaded[i]
		cands := make([]CandidateLabel, len(lt.labels))
		for idx, label := range lt.labels {
			if label.placeholder {
				cands[idx].Placeholder = true
				continue
			}
			res := resolveCategory(lt.profile, label, req.CatIDs, lt.dispatch)
			if res.skip {
				cands[idx].Placeholder = true
				continue
			}
			catID := res.catID
			name := label.name
			if rule, ok := lt.dispatch.Lookup(label.name); ok && rule.Name != "" {
				name = rule.Name
			}
			if !res.override &&
				lt.profile.CategoryScheme == "single_general" &&
				catID == req.GeneralCatID {
				if inferred, ok := req.InferredCats[label.name]; ok {
					catID = inferred
				}
			}
			cands[idx] = CandidateLabel{
				Name:    name,
				CatID:   catID,
				CatName: catNameByID(req.CatIDs, catID),
			}
		}
		lt.candidates = cands
	}

	parallel := min(max(1, req.Parallel), len(req.Images))

	results := make([]BackendImageResult, len(req.Images))
	for i, im := range req.Images {
		results[i].ID = im.ID
	}

	// Periodic mallocTrim during long runs: every trimInterval images
	// the worker that crosses the threshold hands glibc's freed pages
	// back to the kernel. Without it a many-thousand-image run inflates
	// the arena monotonically; the existing teardown-time trim only
	// fires at idle release.
	const trimInterval = 256
	var sinceTrim atomic.Int64

	processOne := func(idx int, workerIdx int) {
		if ctx.Err() != nil {
			return
		}
		im := req.Images[idx]
		if len(im.FramePaths) == 0 {
			results[idx].Err = "no frames"
			return
		}
		merged := map[TagKey]Scored{}
		anyInferred := false
		minHits := ResolveMinHits(req.MinHitFraction, len(im.FramePaths))
		for tIdx, lt := range loaded {
			if ctx.Err() != nil {
				return
			}
			perFrame := make([][]float32, 0, len(im.FramePaths))
			for fIdx, fp := range im.FramePaths {
				if ctx.Err() != nil {
					return
				}
				if im.MangaProgress && req.OnProgress != nil {
					msg := fmt.Sprintf("image %d: page %d/%d", im.ID, fIdx+1, len(im.FramePaths))
					if len(loaded) > 1 {
						msg = fmt.Sprintf("%s (tagger %d/%d)", msg, tIdx+1, len(loaded))
					}
					req.OnProgress(workerIdx, msg)
				}
				scores, err := inferImage(lt, fp)
				if err != nil {
					logx.Warnf("tagger: inference failed: image %d via %q frame %d/%d (%s): %v",
						im.ID, lt.cfg.Name, fIdx+1, len(im.FramePaths), fp, err)
					continue
				}
				anyInferred = true
				perFrame = append(perFrame, scores)
			}

			cands := AggregateInferenceScores(perFrame, lt.candidates, AggregateOpts{
				MinHits:            minHits,
				GlobalThreshold:    float32(lt.cfg.ConfidenceThreshold),
				CategoryThresholds: lt.cfg.CategoryThresholds,
				PerCategoryTopK:    lt.cfg.PerCategoryTopK,
				DisabledCategories: lt.cfg.DisabledCategories,
			})
			for _, c := range cands {
				mk := TagKey{Name: c.Name, CatID: c.CatID}
				if prev, ok := merged[mk]; !ok || c.Score > prev.Score {
					merged[mk] = Scored{Score: c.Score, TaggerName: lt.cfg.Name}
				}
			}
		}
		// Every frame failed to decode or infer. Report an error rather
		// than an empty map: the store loop reconciles an empty result by
		// deleting the image's existing auto-tags.
		if !anyInferred {
			results[idx].Err = "all frames failed"
			return
		}
		results[idx].Tags = merged
	}

	queue := make(chan int, parallel)
	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(workerIdx int) {
			defer wg.Done()
			for idx := range queue {
				if ctx.Err() != nil {
					continue
				}
				processOne(idx, workerIdx)
				if sinceTrim.Add(1) >= trimInterval {
					sinceTrim.Store(0)
					mallocTrim()
				}
				if req.OnProgress != nil {
					req.OnProgress(workerIdx, "")
				}
			}
		}(i)
	}

	for idx := range req.Images {
		if ctx.Err() != nil {
			break
		}
		queue <- idx
	}
	close(queue)
	wg.Wait()

	return RunResponse{Results: results}, ctx.Err()
}

// inferImage loads, preprocesses, and runs inference on a single image
// against the tagger's resolved profile. The profile drives every
// preprocessing axis (pad, layout, channel order, normalisation) and
// the output transform (raw logits get sigmoid'd; sigmoid_in_model
// outputs pass through).
func inferImage(lt loadedTagger, path string) ([]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	img, err := gallery.DecodeImageWithCap(f)
	if err != nil {
		return nil, err
	}

	processed := padAndResize(img, lt.inputSize, lt.profile)
	tensor := acquireTensor(lt.inputSize)
	inputShape, err := buildTensor(processed, tensor, lt.inputSize, lt.profile)
	if err != nil {
		releaseTensor(lt.inputSize, tensor)
		return nil, err
	}
	inputTensor, err := ort.NewTensor(inputShape, tensor)
	if err != nil {
		releaseTensor(lt.inputSize, tensor)
		return nil, err
	}

	// ort.NewTensor aliases the float32 slice, so the buffer must
	// outlive the Run call. Destroy releases ORT's reference; only
	// then can we hand the slice back to the pool. Explicit order
	// instead of defer so the Put happens after Destroy on success
	// and after Destroy on error too.
	outputs := []ort.Value{nil}
	runErr := lt.session.Run([]ort.Value{inputTensor}, outputs)
	inputTensor.Destroy()
	releaseTensor(lt.inputSize, tensor)
	if runErr != nil {
		return nil, runErr
	}
	defer outputs[0].Destroy()

	outTensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("unexpected output type: %T", outputs[0])
	}
	// GetData returns ORT-owned memory backing the output ortValue;
	// the deferred Destroy releases that memory. Always copy into a
	// Go-owned slice before returning so the caller never holds a
	// dangling pointer.
	data := outTensor.GetData()
	out := make([]float32, len(data))
	if lt.profile.Activation == "logits" {
		for i, v := range data {
			out[i] = float32(1 / (1 + math.Exp(-float64(v))))
		}
		return out, nil
	}
	copy(out, data)
	return out, nil
}

// sharedLibPath finds the ONNX Runtime shared library. ORT_LIB_PATH
// overrides; otherwise we try the usual install locations.
func sharedLibPath() string {
	if p := os.Getenv("ORT_LIB_PATH"); p != "" {
		return p
	}
	candidates := []string{
		"/usr/lib/libonnxruntime.so",
		"/usr/local/lib/libonnxruntime.so",
		"libonnxruntime.so",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "libonnxruntime.so"
}

// catNameByID reverses a name→id map for the top-K lookup. The map
// is small (tag_categories rarely exceeds a couple dozen rows) so a
// linear scan beats maintaining a parallel inverse map. Returns ""
// when the id is unknown; the caller then falls through to the
// fallback cap.
func catNameByID(catIDs map[string]int64, id int64) string {
	for name, cid := range catIDs {
		if cid == id {
			return name
		}
	}
	return ""
}
