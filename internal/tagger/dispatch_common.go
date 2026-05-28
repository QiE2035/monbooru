package tagger

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/leqwin/monbooru/internal/logx"
)

// dispatchSchemaVersion is the only schema version the dispatch loader
// accepts. Documents on a different version are skipped so a future
// schema change can roll out without breaking older binaries that still
// ship the old embedded defaults.
const dispatchSchemaVersion = 1

type dispatchDoc struct {
	Version int             `json:"version"`
	Rules   []dispatchEntry `json:"rules"`
}

type dispatchEntry struct {
	Source   string `json:"source"`
	Category string `json:"category"`
	Name     string `json:"name"`
}

// DispatchTargetCategories returns the distinct destination category
// names the tagger's dispatch table (embedded default + on-disk
// overlay) routes labels into. The Configure dialog unions these with
// the profile's natively emitted categories so an operator can tune or
// disable a category the model only reaches through dispatch - e.g.
// wd-swinv2 routes a slice of its general labels into medium / meta /
// year. Empty-category entries (drops) are skipped; order is
// deterministic (embedded rules first, then overlay additions).
func DispatchTargetCategories(modelPath, taggerName string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(entries []dispatchEntry) {
		for _, e := range entries {
			if e.Category == "" || seen[e.Category] {
				continue
			}
			seen[e.Category] = true
			out = append(out, e.Category)
		}
	}
	add(parseEmbeddedDispatch(taggerName))
	add(parseOverlayDispatch(modelPath, taggerName))
	return out
}

func parseEmbeddedDispatch(taggerName string) []dispatchEntry {
	data, err := defaultDispatchFS.ReadFile("dispatch_default/" + taggerName + ".json")
	if err != nil {
		return nil
	}
	return parseDispatchDoc(data, "embedded "+taggerName)
}

func parseOverlayDispatch(modelPath, taggerName string) []dispatchEntry {
	p := filepath.Join(modelPath, taggerName, "dispatch.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	return parseDispatchDoc(data, p)
}

func parseDispatchDoc(data []byte, source string) []dispatchEntry {
	var doc dispatchDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		logx.Warnf("tagger: %s dispatch parse failed: %v", source, err)
		return nil
	}
	if doc.Version != dispatchSchemaVersion {
		logx.Warnf("tagger: %s dispatch schema version %d unsupported (want %d)", source, doc.Version, dispatchSchemaVersion)
		return nil
	}
	return doc.Rules
}
