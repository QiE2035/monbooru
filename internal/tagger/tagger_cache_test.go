//go:build tagger

package tagger

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leqwin/monbooru/internal/config"
)

func TestCacheSatisfies(t *testing.T) {
	// Profiles must match what ResolveProfile produces for these
	// (taggerName, tagsFile) pairs, otherwise the fingerprint check
	// trips before the file-name check.
	wdProfile, err := ResolveProfile("", "wd-swinv2", "tags.csv")
	if err != nil {
		t.Fatalf("resolve wd profile: %v", err)
	}
	joyProfile, err := ResolveProfile("", "joytag", "tags.txt")
	if err != nil {
		t.Fatalf("resolve joytag profile: %v", err)
	}
	c := inprocBackend{
		initialized: true,
		useCUDA:     false,
		sessions: map[string]*loadedSession{
			"wd-swinv2": {modelFile: "model.onnx", tagsFile: "tags.csv", profileFP: wdProfile.fingerprint()},
			"joytag":    {modelFile: "model.onnx", tagsFile: "tags.txt", profileFP: joyProfile.fingerprint()},
		},
	}

	mk := func(name, model, tags string) TaggerStatus {
		return TaggerStatus{TaggerInstance: config.TaggerInstance{
			Name: name, ModelFile: model, TagsFile: tags,
		}}
	}

	tests := []struct {
		name    string
		req     []TaggerStatus
		useCUDA bool
		want    bool
	}{
		{"single match", []TaggerStatus{mk("wd-swinv2", "model.onnx", "tags.csv")}, false, true},
		{"both match", []TaggerStatus{
			mk("wd-swinv2", "model.onnx", "tags.csv"),
			mk("joytag", "model.onnx", "tags.txt"),
		}, false, true},
		{"useCUDA flip invalidates", []TaggerStatus{mk("wd-swinv2", "model.onnx", "tags.csv")}, true, false},
		{"unknown tagger", []TaggerStatus{mk("ghost", "model.onnx", "tags.csv")}, false, false},
		{"model file swapped", []TaggerStatus{mk("wd-swinv2", "v3.onnx", "tags.csv")}, false, false},
		{"tags file swapped", []TaggerStatus{mk("wd-swinv2", "model.onnx", "selected_tags.csv")}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.satisfies("", tc.req, tc.useCUDA); got != tc.want {
				t.Fatalf("satisfies: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCacheSatisfies_Uninitialized(t *testing.T) {
	c := inprocBackend{}
	if c.satisfies("", nil, false) {
		t.Fatalf("uninitialized cache must not satisfy any request")
	}
}

func TestCacheSatisfies_ProfileSidecarInvalidates(t *testing.T) {
	// A sidecar tagger.json that flips one axis must be detected by the
	// profile-fingerprint half of satisfies, even when modelFile/tagsFile
	// stay identical. This is the cache-invalidation path that lets
	// operators edit a profile without restarting.
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "wd-swinv2"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cached, err := ResolveProfile(tmp, "wd-swinv2", "tags.csv")
	if err != nil {
		t.Fatalf("resolve cached: %v", err)
	}
	c := inprocBackend{
		initialized: true,
		sessions: map[string]*loadedSession{
			"wd-swinv2": {modelFile: "model.onnx", tagsFile: "tags.csv", profileFP: cached.fingerprint()},
		},
	}
	req := []TaggerStatus{{TaggerInstance: config.TaggerInstance{
		Name: "wd-swinv2", ModelFile: "model.onnx", TagsFile: "tags.csv",
	}}}
	if !c.satisfies(tmp, req, false) {
		t.Fatalf("baseline satisfies must hold before sidecar edit")
	}
	// Drop a sidecar that flips activation; cache must invalidate.
	sidecar := `{"version":1,"profile":{"activation":"logits"}}`
	if err := os.WriteFile(filepath.Join(tmp, "wd-swinv2", "tagger.json"), []byte(sidecar), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	if c.satisfies(tmp, req, false) {
		t.Errorf("sidecar edit must invalidate the cached profile fingerprint")
	}
}

func TestReleaseIdle_Cold(t *testing.T) {
	// Cold package-global cache: ReleaseIdle is a no-op.
	if ReleaseIdle(time.Hour) {
		t.Fatalf("ReleaseIdle on a cold cache must return false")
	}
}

func TestReleaseAll_Cold(t *testing.T) {
	// A second teardown on a cold cache must be safe; it's the path
	// hit when the server closes without ever running a tagger job.
	ReleaseAll()
	ReleaseAll()
}
