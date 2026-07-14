//go:build tagger

package tagger

import (
	"context"
	"image/color"
	"os"
	"path/filepath"
	"testing"

	"github.com/leqwin/monbooru/internal/config"
)

// Two galleries whose tag_categories id layouts disagree: id 9 is
// `medium` in A - where the wd-swinv2 dispatch routes monochrome, sketch
// and friends - and `person` in B. The INSERT OR IGNORE seed produces
// exactly this skew on a library that predates the medium/person/year
// categories.
var switchCatsA = map[string]int64{
	"general": 1, "character": 2, "artist": 3, "copyright": 4,
	"meta": 5, "rating": 6, "year": 7, "person": 8, "medium": 9,
}

var switchCatsB = map[string]int64{
	"general": 1, "character": 2, "artist": 3, "copyright": 4,
	"meta": 5, "rating": 6, "year": 7, "person": 9, "medium": 10,
}

// dispatchTargets maps every routed label's emitted name to the category
// id it must carry under cats.
func dispatchTargets(modelPath string, cats map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for source, rule := range LoadDispatch(modelPath, "wd-swinv2", cats).rules {
		if rule.Drop {
			continue
		}
		name := rule.Name
		if name == "" {
			name = source
		}
		out[name] = rule.CatID
	}
	return out
}

// TestRunDispatchFollowsGallerySwitch pins the invariant that makes warm
// cache reuse safe. The dispatch table is the only routing input whose
// targets are category ids rather than names, so a table compiled against
// another gallery files every routed label into whatever category happens
// to own the stale id. Runs real inference; skips without the runtime or
// the bundled model.
func TestRunDispatchFollowsGallerySwitch(t *testing.T) {
	if _, err := os.Stat(sharedLibPath()); err != nil {
		t.Skipf("onnxruntime shared library not available: %v", err)
	}
	modelPath, err := filepath.Abs("../../project/ressources/models")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(modelPath, "wd-swinv2", "model.onnx")); err != nil {
		t.Skipf("wd-swinv2 model not present: %v", err)
	}

	cfg := &config.Config{}
	cfg.Paths.ModelPath = modelPath
	cfg.Tagger.Parallel = 1
	// Run tears the cache down after every call when this is <= 0, which
	// would rebuild the dispatch from fresh ids and hide the regression.
	cfg.Tagger.IdleReleaseAfterMinutes = 15

	// Threshold 0 with every per-category cap disabled so the audit covers
	// as much of the label set as the pre-floor allows, independent of
	// what the probe frame depicts.
	uncapped := map[string]int{}
	for name := range switchCatsA {
		uncapped[name] = 0
	}
	taggers := []TaggerStatus{{TaggerInstance: config.TaggerInstance{
		Name: "wd-swinv2", ModelFile: "model.onnx", TagsFile: "tags.csv",
		ConfidenceThreshold: 0, PerCategoryTopK: uncapped,
	}}}

	frame := filepath.Join(t.TempDir(), "frame.png")
	if err := os.WriteFile(frame, solidPNG(t, 64, 64, color.RGBA{90, 120, 160, 255}), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &inprocBackend{}
	t.Cleanup(b.ReleaseAll)

	run := func(cats map[string]int64) map[TagKey]Scored {
		t.Helper()
		resp, err := b.Run(context.Background(), RunRequest{
			Cfg: cfg, Taggers: taggers, UseCUDA: false,
			CatIDs: cats, GeneralCatID: cats["general"],
			MinHitFraction: 0.05, Parallel: 1,
			Images: []BackendImageRequest{{ID: 1, FramePaths: []string{frame}}},
		})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if resp.Results[0].Err != "" {
			t.Fatalf("image error: %s", resp.Results[0].Err)
		}
		return resp.Results[0].Tags
	}

	audit := func(label string, tags map[TagKey]Scored, cats map[string]int64) {
		t.Helper()
		want := dispatchTargets(modelPath, cats)
		checked := 0
		for k := range tags {
			exp, ok := want[k.Name]
			if !ok {
				continue
			}
			checked++
			if k.CatID != exp {
				t.Errorf("%s: %q filed under %q, want %q", label, k.Name,
					catNameByID(cats, k.CatID), catNameByID(cats, exp))
			}
		}
		if checked == 0 {
			t.Fatalf("%s: no routed label cleared the pre-floor; the audit is vacuous", label)
		}
		t.Logf("%s: %d routed labels checked", label, checked)
	}

	audit("cold cache", run(switchCatsA), switchCatsA)
	audit("warm cache after switch", run(switchCatsB), switchCatsB)
}
