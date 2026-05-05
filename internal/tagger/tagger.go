//go:build tagger

package tagger

import (
	"context"
	"database/sql"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/tags"
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

// CheckCUDAAvailable probes the ONNX Runtime for CUDA support and
// verifies an NVIDIA GPU device file exists. The settings handler calls
// it before persisting use_cuda=true so the user gets an immediate
// error rather than a surprise at tagger-job time.
func CheckCUDAAvailable() error {
	ort.SetSharedLibraryPath(sharedLibPath())
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("ort init: %w", err)
	}
	defer ort.DestroyEnvironment()

	opts, err := ort.NewCUDAProviderOptions()
	if err != nil {
		return fmt.Errorf("libonnxruntime is not CUDA-capable (use the -cuda Docker image): %w", err)
	}
	opts.Destroy()

	if _, err := os.Stat("/dev/nvidia0"); err != nil {
		return fmt.Errorf("no NVIDIA GPU device found (pass the GPU into the container, e.g. Podman AddDevice=nvidia.com/gpu=all)")
	}
	return nil
}

// AvailableTaggers returns every known tagger with availability set.
func AvailableTaggers(cfg *config.Config) []TaggerStatus {
	return DiscoverTaggers(cfg)
}

// tagKey identifies one (name, category_id) pair so multi-tagger merges
// never insert the same tag twice on the same image.
type tagKey struct {
	name  string
	catID int64
}

// scored carries the highest confidence seen across taggers for one
// tagKey plus the tagger that produced that score, so attribution
// survives multi-tagger merges.
type scored struct {
	score      float32
	taggerName string
}

// loadedTagger pairs a cached ORT session with the per-call config the
// inference loop reads, so threshold edits take effect without
// rebuilding the session.
type loadedTagger struct {
	cfg       config.TaggerInstance
	session   *ort.DynamicAdvancedSession
	labels    []tagLabel
	profile   Profile
	inputSize int
	dispatch  *DispatchTable
}

// loadedSession is the cached half of loadedTagger: ORT state keyed by
// tagger name. modelFile and tagsFile gate cache reuse - a TOML edit
// that swaps either invalidates the entry; profileFP additionally
// invalidates on a tagger.json sidecar edit.
type loadedSession struct {
	modelFile  string
	tagsFile   string
	profileFP  string
	session    *ort.DynamicAdvancedSession
	labels     []tagLabel
	profile    Profile
	inputSize  int
	dispatch   *DispatchTable
}

// taggerCache holds the warm ORT environment and per-tagger sessions
// across RunWithTaggers calls. Without it the bytes ORT frees on
// teardown stay parked in glibc's arenas; teardown calls mallocTrim to
// hand them back. inUse blocks the idle reaper from racing a run.
type taggerCache struct {
	mu          sync.Mutex
	inUse       bool
	initialized bool
	useCUDA     bool
	sessionOpts *ort.SessionOptions
	cudaOpts    *ort.CUDAProviderOptions
	sessions    map[string]*loadedSession
	lastUsed    time.Time
}

var cache taggerCache

// satisfies returns true when the cached set covers every requested
// tagger with the same execution-provider mode, the same model / tags
// filenames, and the same profile fingerprint. The profile check picks
// up tagger.json sidecar edits without a manual reload. Caller must
// hold c.mu.
func (c *taggerCache) satisfies(modelPath string, taggers []TaggerStatus, useCUDA bool) bool {
	if !c.initialized || c.useCUDA != useCUDA {
		return false
	}
	for _, t := range taggers {
		s, ok := c.sessions[t.Name]
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
// c.mu. catIDs feeds dispatch resolution at session-build time so a
// dispatch rule pointing at a renamed/deleted category surfaces as a
// debug log instead of mis-routing labels.
func (c *taggerCache) ensure(cfg *config.Config, taggers []TaggerStatus, useCUDA bool, catIDs map[string]int64) error {
	if c.satisfies(cfg.Paths.ModelPath, taggers, useCUDA) {
		return nil
	}
	if c.initialized {
		c.teardownLocked()
	}

	ort.SetSharedLibraryPath(sharedLibPath())
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("ort init: %w", err)
	}
	c.initialized = true

	if useCUDA {
		opts, err := ort.NewSessionOptions()
		if err != nil {
			c.teardownLocked()
			return fmt.Errorf("ort session options: %w", err)
		}
		c.sessionOpts = opts
		cudaOpts, err := ort.NewCUDAProviderOptions()
		if err != nil {
			c.teardownLocked()
			return fmt.Errorf("ort cuda options (ensure libonnxruntime was built with CUDA): %w", err)
		}
		c.cudaOpts = cudaOpts
		if err := opts.AppendExecutionProviderCUDA(cudaOpts); err != nil {
			c.teardownLocked()
			return fmt.Errorf("append cuda provider: %w", err)
		}
	}
	c.useCUDA = useCUDA

	c.sessions = make(map[string]*loadedSession, len(taggers))
	for _, t := range taggers {
		onnxPath := filepath.Join(cfg.Paths.ModelPath, t.Name, t.ModelFile)
		tagsPath := filepath.Join(cfg.Paths.ModelPath, t.Name, t.TagsFile)
		profile, err := ResolveProfile(cfg.Paths.ModelPath, t.Name, t.TagsFile)
		if err != nil {
			c.teardownLocked()
			return fmt.Errorf("resolve profile for %q: %w", t.Name, err)
		}
		labels, err := loadLabels(tagsPath, profile)
		if err != nil {
			c.teardownLocked()
			return fmt.Errorf("load labels for %q: %w", t.Name, err)
		}
		inputs, outputs, err := ort.GetInputOutputInfo(onnxPath)
		if err != nil {
			c.teardownLocked()
			return fmt.Errorf("inspect ort model for %q: %w", t.Name, err)
		}
		if len(inputs) == 0 || len(outputs) == 0 {
			c.teardownLocked()
			return fmt.Errorf("ort model for %q has no input/output", t.Name)
		}
		outIdx := profile.OutputIndex
		if outIdx < 0 || outIdx >= len(outputs) {
			c.teardownLocked()
			return fmt.Errorf("ort model for %q: profile output_index %d out of range (have %d)", t.Name, outIdx, len(outputs))
		}
		inputSize := profile.InputSize
		if inputSize == 0 {
			inputSize = inferInputSize(inputs[0].Dimensions, profile.Layout)
			if inputSize <= 0 {
				c.teardownLocked()
				return fmt.Errorf("ort model for %q: cannot infer input size from dimensions %v", t.Name, inputs[0].Dimensions)
			}
		}
		session, err := ort.NewDynamicAdvancedSession(onnxPath,
			[]string{inputs[0].Name}, []string{outputs[outIdx].Name}, c.sessionOpts)
		if err != nil {
			c.teardownLocked()
			return fmt.Errorf("create ort session for %q: %w", t.Name, err)
		}
		c.sessions[t.Name] = &loadedSession{
			modelFile: t.ModelFile,
			tagsFile:  t.TagsFile,
			profileFP: profile.fingerprint(),
			session:   session,
			labels:    labels,
			profile:   profile,
			inputSize: inputSize,
			dispatch:  LoadDispatch(cfg.Paths.ModelPath, t.Name, catIDs),
		}
	}
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
// return the freed bytes to the kernel. Caller must hold c.mu.
func (c *taggerCache) teardownLocked() {
	for _, s := range c.sessions {
		s.session.Destroy()
	}
	c.sessions = nil
	if c.cudaOpts != nil {
		c.cudaOpts.Destroy()
		c.cudaOpts = nil
	}
	if c.sessionOpts != nil {
		c.sessionOpts.Destroy()
		c.sessionOpts = nil
	}
	if c.initialized {
		ort.DestroyEnvironment()
		c.initialized = false
	}
	c.useCUDA = false
	mallocTrim()
}

// ReleaseIdle tears down the cached session set when it has been idle
// for at least `after` and no run is in flight. Returns true on
// teardown so the caller can log it.
func ReleaseIdle(after time.Duration) bool {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.inUse || !cache.initialized {
		return false
	}
	if time.Since(cache.lastUsed) < after {
		return false
	}
	cache.teardownLocked()
	return true
}

// ReleaseAll unconditionally tears down the cached session set, e.g.
// on shutdown or when use_cuda flips and the cache must be rebuilt.
func ReleaseAll() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.initialized {
		cache.teardownLocked()
	}
}

// RunWithTaggers tags ids through the supplied taggers, merging results
// so each image ends up with one row per unique tag. Callers must pass
// only enabled+available taggers. useCUDA overrides cfg.Tagger.UseCUDA
// so per-request callers can keep single-image runs on the CPU.
// Returns the count of submitted ids left without auto_tagged_at.
func RunWithTaggers(ctx context.Context, database *db.DB, cfg *config.Config, ids []int64, taggers []TaggerStatus, mgr *jobs.Manager, useCUDA bool) (int, error) {
	if len(taggers) == 0 {
		return 0, fmt.Errorf("no tagger is enabled or available")
	}

	// Loaded ahead of cache.ensure so a fresh session set picks up the
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
		catRows.Close()
	}
	generalCatID := catIDs["general"]

	cache.mu.Lock()
	if err := cache.ensure(cfg, taggers, useCUDA, catIDs); err != nil {
		cache.mu.Unlock()
		return 0, err
	}
	loaded := make([]loadedTagger, len(taggers))
	for i, t := range taggers {
		s := cache.sessions[t.Name]
		loaded[i] = loadedTagger{
			cfg:       t.TaggerInstance,
			session:   s.session,
			labels:    s.labels,
			profile:   s.profile,
			inputSize: s.inputSize,
			dispatch:  s.dispatch,
		}
	}
	cache.inUse = true
	cache.mu.Unlock()

	defer func() {
		cache.mu.Lock()
		cache.inUse = false
		cache.lastUsed = time.Now()
		// idle_release_after_minutes <= 0 disables caching: tear
		// down right after the run so RSS drops back to baseline.
		if cfg.Tagger.IdleReleaseAfterMinutes <= 0 {
			cache.teardownLocked()
		}
		cache.mu.Unlock()
	}()

	// Names of the taggers running this job; used so the replace step
	// only wipes rows produced by these taggers.
	taggerNames := make([]string, 0, len(loaded))
	for _, lt := range loaded {
		taggerNames = append(taggerNames, lt.cfg.Name)
	}

	// Inference map for taggers whose category scheme can't tell apart
	// general from categorised counterparts (joytag's single_general,
	// camie when its category is "general"). Maps tag name → catID for
	// an existing non-general non-meta categorised tag. Ambiguous names
	// (multiple categorised variants) are dropped and fall back to
	// general. Lets joytag's `hakurei_reimu` attach to a pre-existing
	// `character:hakurei_reimu` instead of going under general.
	inferredCats := map[string]int64{}
	hasSingleGeneral := false
	for _, lt := range loaded {
		if lt.profile.CategoryScheme == "single_general" {
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
			infRows.Close()
		}
	}

	// processOne runs the per-image tagging pipeline. Called from one
	// or more worker goroutines; ORT sessions are safe for concurrent
	// Run calls and the DB write pool serialises storeResults.
	var skipped atomic.Int64
	processOne := func(imageID int64) {
		var canonPath, fileType string
		if err := database.Read.QueryRowContext(ctx,
			`SELECT canonical_path, file_type FROM images WHERE id = ?`, imageID,
		).Scan(&canonPath, &fileType); err != nil {
			logx.Warnf("tagger: skip image %d: lookup failed: %v", imageID, err)
			skipped.Add(1)
			return
		}

		framePaths, cleanup := framesForTagging(canonPath, fileType)
		defer cleanup()
		if len(framePaths) == 0 {
			logx.Warnf("tagger: skip image %d: no frames available (missing file or ffmpeg)", imageID)
			skipped.Add(1)
			return
		}

		merged := map[tagKey]scored{}
		for _, lt := range loaded {
			// Videos keep the highest score per label across the sampled frames.
			best := map[int]float32{}
			for _, fp := range framePaths {
				scores, err := inferImage(lt, fp)
				if err != nil {
					continue
				}
				for idx, score := range scores {
					if score > best[idx] {
						best[idx] = score
					}
				}
			}
			globalThreshold := float32(lt.cfg.ConfidenceThreshold)
			for idx, score := range best {
				if idx >= len(lt.labels) {
					continue
				}
				label := lt.labels[idx]
				if label.placeholder {
					continue
				}
				// Cheap floor that drops the long tail of near-zero
				// scores before we resolve the category for the
				// per-category threshold lookup.
				if score < 0.001 {
					continue
				}
				res := resolveCategory(lt.profile, label, catIDs, lt.dispatch)
				if res.skip {
					continue
				}
				threshold := globalThreshold
				if v, ok := lt.cfg.CategoryThresholds[res.catName]; ok {
					threshold = float32(v)
				}
				if score < threshold {
					continue
				}
				catID := res.catID
				name := label.name
				if rule, ok := lt.dispatch.Lookup(label.name); ok && rule.Name != "" {
					name = rule.Name
				}
				// single_general taggers have no category info; if a
				// unique categorised tag with this name already exists,
				// attach to it instead of dropping into general. Skip the
				// lift when a dispatch rule already routed the label.
				if !res.override &&
					lt.profile.CategoryScheme == "single_general" &&
					catID == generalCatID {
					if inferred, ok := inferredCats[label.name]; ok {
						catID = inferred
					}
				}
				k := tagKey{name: name, catID: catID}
				if prev, ok := merged[k]; !ok || score > prev.score {
					merged[k] = scored{score: score, taggerName: lt.cfg.Name}
				}
			}
		}

		if err := storeResults(ctx, database, imageID, merged, taggerNames, catIDs["rating"]); err != nil {
			logx.Warnf("tagger: store results for image %d: %v", imageID, err)
			skipped.Add(1)
		}
	}

	parallel := cfg.Tagger.Parallel
	if parallel < 1 {
		parallel = 1
	}
	if parallel > len(ids) {
		parallel = len(ids)
	}

	total := len(ids)
	var completed atomic.Int64
	queue := make(chan int64, parallel)
	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for imageID := range queue {
				if ctx.Err() != nil {
					continue
				}
				processOne(imageID)
				done := int(completed.Add(1))
				mgr.Update(done, total, "tagging images")
			}
		}()
	}

	for _, imageID := range ids {
		if ctx.Err() != nil {
			break
		}
		queue <- imageID
	}
	close(queue)
	wg.Wait()

	return int(skipped.Load()), ctx.Err()
}

// framesForTagging returns the file paths to feed the tagger plus a
// cleanup func. Static images return [canonPath]; videos sample up to
// five frames via ffmpeg. With ffmpeg missing or failing, videos
// yield no frames and the caller skips the asset.
func framesForTagging(canonPath, fileType string) ([]string, func()) {
	if fileType != "mp4" && fileType != "webm" {
		return []string{canonPath}, func() {}
	}
	positions := []float64{0.10, 0.30, 0.50, 0.70, 0.90}
	frames, err := gallery.ExtractVideoFrames(canonPath, os.TempDir(), positions)
	cleanup := func() {
		for _, p := range frames {
			os.Remove(p)
		}
	}
	if err != nil {
		return frames, cleanup
	}
	return frames, cleanup
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

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}

	processed := padAndResize(img, lt.inputSize, lt.profile)
	tensor, inputShape, err := buildTensor(processed, lt.inputSize, lt.profile)
	if err != nil {
		return nil, err
	}
	inputTensor, err := ort.NewTensor(inputShape, tensor)
	if err != nil {
		return nil, err
	}
	defer inputTensor.Destroy()

	// nil output lets DynamicAdvancedSession allocate it.
	outputs := []ort.Value{nil}
	if err := lt.session.Run([]ort.Value{inputTensor}, outputs); err != nil {
		return nil, err
	}
	defer outputs[0].Destroy()

	outTensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("unexpected output type: %T", outputs[0])
	}
	data := outTensor.GetData()
	if lt.profile.Activation == "logits" {
		out := make([]float32, len(data))
		for i, v := range data {
			out[i] = float32(1 / (1 + math.Exp(-float64(v))))
		}
		return out, nil
	}
	return data, nil
}

// storeResults commits the merged auto-tag set for one image and keeps
// usage_count in sync. The replace step is scoped to taggerNames so
// other taggers' rows survive. ratingCatID gates the highest-rank-wins
// rating prune that fires when any of merged's tags is a rating-category
// row; pass 0 to skip (pre-bootstrap DB).
func storeResults(
	ctx context.Context, database *db.DB,
	imageID int64, merged map[tagKey]scored, taggerNames []string, ratingCatID int64,
) error {
	tx, err := database.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Resolve each desired tag to a tag_id, creating new rows as
	// needed. Alias rows redirect to their canonical so we never
	// attach an alias to an image (matches GetOrCreateTag). Two labels
	// that collapse onto the same canonical keep the higher score.
	type target struct {
		score      float32
		taggerName string
	}
	targets := make(map[int64]target, len(merged))
	for k, s := range merged {
		var tagID int64
		var isAlias int
		var canonicalID sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT id, is_alias, canonical_tag_id FROM tags WHERE name = ? AND category_id = ?`, k.name, k.catID,
		).Scan(&tagID, &isAlias, &canonicalID)
		if err == sql.ErrNoRows {
			res, err2 := tx.ExecContext(ctx,
				`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 0)`, k.name, k.catID)
			if err2 != nil {
				return fmt.Errorf("insert tag %q (cat=%d): %w", k.name, k.catID, err2)
			}
			tagID, _ = res.LastInsertId()
		} else if err != nil {
			return fmt.Errorf("lookup tag %q (cat=%d): %w", k.name, k.catID, err)
		} else if isAlias == 1 && canonicalID.Valid {
			tagID = canonicalID.Int64
		}
		if prev, ok := targets[tagID]; !ok || s.score > prev.score {
			targets[tagID] = target{score: s.score, taggerName: s.taggerName}
		}
	}

	type rowInfo struct {
		isAuto     bool
		taggerName string
	}
	current := map[int64]rowInfo{}
	rows, err := tx.QueryContext(ctx,
		`SELECT tag_id, is_auto, tagger_name FROM image_tags WHERE image_id = ?`, imageID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var tid int64
		var isAuto int
		var tname sql.NullString
		if err := rows.Scan(&tid, &isAuto, &tname); err != nil {
			rows.Close()
			return err
		}
		current[tid] = rowInfo{isAuto: isAuto == 1, taggerName: tname.String}
	}
	rows.Close()

	toRemove := map[int64]struct{}{}
	if len(taggerNames) > 0 {
		scope := make(map[string]struct{}, len(taggerNames))
		for _, n := range taggerNames {
			scope[n] = struct{}{}
		}
		for tid, info := range current {
			if !info.isAuto {
				continue
			}
			if _, ok := scope[info.taggerName]; !ok {
				continue
			}
			if _, keep := targets[tid]; keep {
				continue
			}
			toRemove[tid] = struct{}{}
		}
	}
	toAdd := map[int64]target{}
	for tid, t := range targets {
		if _, exists := current[tid]; !exists {
			toAdd[tid] = t
		}
	}

	for tid := range toRemove {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ? AND is_auto = 1`, imageID, tid); err != nil {
			return fmt.Errorf("remove auto tag %d: %w", tid, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tags SET usage_count = MAX(0, usage_count - 1) WHERE id = ?`, tid); err != nil {
			return fmt.Errorf("decrement usage for tag %d: %w", tid, err)
		}
	}

	for tid, t := range targets {
		info, exists := current[tid]
		if !exists || !info.isAuto {
			continue
		}
		var tname any
		if t.taggerName != "" {
			tname = t.taggerName
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE image_tags SET confidence = ?, tagger_name = ? WHERE image_id = ? AND tag_id = ? AND is_auto = 1`,
			t.score, tname, imageID, tid); err != nil {
			return fmt.Errorf("refresh attribution for tag %d: %w", tid, err)
		}
	}

	for tid, t := range toAdd {
		var tname any
		if t.taggerName != "" {
			tname = t.taggerName
		}
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name) VALUES (?, ?, 1, 0, ?, ?)`,
			imageID, tid, t.score, tname)
		if err != nil {
			return fmt.Errorf("insert auto tag %d: %w", tid, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, tid); err != nil {
			return fmt.Errorf("increment usage for tag %d: %w", tid, err)
		}
		if err := tags.ApplyImpliedFanoutTx(tx, imageID, tid, true); err != nil {
			return fmt.Errorf("fan out implications for tag %d: %w", tid, err)
		}
	}

	// WD14 emits every rating label that beats its threshold, so a
	// single image can pick up `sensitive` and `questionable` in one
	// pass. Sweep lower-rank rating rows so highest-rank wins matches
	// what search resolves to anyway.
	if ratingCatID != 0 {
		hasRating := false
		for k := range merged {
			if k.catID == ratingCatID {
				hasRating = true
				break
			}
		}
		if hasRating {
			if err := tags.PruneLowerRatingsTx(tx, ratingCatID, imageID); err != nil {
				return fmt.Errorf("prune lower ratings: %w", err)
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE images SET auto_tagged_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), imageID); err != nil {
		return fmt.Errorf("stamp auto_tagged_at on image %d: %w", imageID, err)
	}

	return tx.Commit()
}

// sanitizeLabel coerces a label-file name into the documented tag
// allowlist. Spaces collapse to underscores; out-of-set runes drop.
// The colon is preserved so labels like `:3` and `rating:general` round
// trip unchanged. A label that empties out becomes
// `_unsupported_<idx>` so the slice index keeps its 1:1 mapping with
// the model's output channels - dropping the entry would shift every
// later label and corrupt downstream attribution. The returned bool is
// false in that fallback case so callers can flag the slot as a
// placeholder and skip emission at inference time.
func sanitizeLabel(raw string, idx int) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '_' || r == '(' || r == ')' || r == '!' ||
				r == '@' || r == '#' || r == '$' || r == '.' ||
				r == '~' || r == '+' || r == '-' || r == ':' ||
				r == '?' || r == '<' || r == '>' || r == '=' ||
				r == '^':
			b.WriteRune(r)
		case r == ' ' || r == '\t':
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 200 {
		out = out[:200]
	}
	// Match ValidateTagName: emoticon-only labels like "??", ">_<", "^_^"
	// are accepted alongside alphanumeric ones; only pure separator-class
	// punctuation drops to the placeholder slot.
	hasContent := false
	for _, r := range out {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			r == '?' || r == '<' || r == '>' || r == '=' || r == '^' {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return fmt.Sprintf("_unsupported_%d", idx), false
	}
	return out, true
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
