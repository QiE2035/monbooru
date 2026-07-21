//go:build tagger

package tagger

import (
	"github.com/monbooru/monbooru/internal/logx"
)

// DispatchTable maps a model's emitted source label to a runtime decision
// (drop the label, or route it into a chosen category, optionally under
// a different name). The processOne label loop checks Lookup ahead of
// the wd14RatingTags / wd14Category / inferredCats chain; misses fall
// through to the default mapping.
type DispatchTable struct {
	rules map[string]DispatchRule
}

// DispatchRule is the resolved per-source decision. CatID and CatName
// are meaningful only when Drop is false. An empty Name means "keep the
// source label as the tag name". CatName mirrors the source rule's
// `category` string so per-category threshold lookups can route a
// dispatched label without an extra id→name reverse pass.
type DispatchRule struct {
	Drop    bool
	CatID   int64
	CatName string
	Name    string
}

// LoadDispatch returns the runtime dispatch table for one tagger. The
// embedded default (dispatch_default/<taggerName>.json) is the base; an
// optional <modelPath>/<taggerName>/dispatch.json overlay applies on top
// with same-source entries replacing the default and new sources
// appending - mirroring the models.json overlay shape used by
// LoadCatalog.
//
// Categories resolve against catIDs at load time; a rule pointing at a
// category that no longer exists is skipped with a debug log so a stale
// dispatch doesn't drop a label into the wrong slot. A skipped overlay
// rule does not blot out the embedded default for the same source -
// the embedded rule survives the failed override. Empty Category means
// "drop the source entirely". Empty Name keeps the source as-is;
// non-empty Name runs through sanitizeLabel so the resulting tag name
// stays inside the documented allowlist.
//
// Returns a non-nil empty table when no dispatch is configured so
// callers can call Lookup without a nil check.
func LoadDispatch(modelPath, taggerName string, catIDs map[string]int64) *DispatchTable {
	out := &DispatchTable{rules: map[string]DispatchRule{}}
	for _, r := range parseEmbeddedDispatch(taggerName) {
		if rule, ok := compileDispatchRule(r, catIDs); ok {
			out.rules[r.Source] = rule
		}
	}
	for _, r := range parseOverlayDispatch(modelPath, taggerName) {
		if rule, ok := compileDispatchRule(r, catIDs); ok {
			out.rules[r.Source] = rule
		}
	}
	return out
}

// compileDispatchRule validates one entry and returns the resolved
// runtime rule. The bool is false when the entry can't be honoured
// (unknown category, unsupported name); callers skip it and the source
// falls through to whatever rule was registered before, or to the
// default chain when none was.
func compileDispatchRule(r dispatchEntry, catIDs map[string]int64) (DispatchRule, bool) {
	rule := DispatchRule{}
	if r.Category == "" {
		rule.Drop = true
	} else {
		cid, ok := catIDs[r.Category]
		if !ok {
			logx.Debugf("tagger: dispatch %q drops unknown target category %q", r.Source, r.Category)
			return DispatchRule{}, false
		}
		rule.CatID = cid
		rule.CatName = r.Category
	}
	if r.Name != "" {
		name, ok := sanitizeLabel(r.Name, 0)
		if !ok {
			logx.Debugf("tagger: dispatch %q drops unsupported target name %q", r.Source, r.Name)
			return DispatchRule{}, false
		}
		rule.Name = name
	}
	return rule, true
}

// Lookup returns the rule matching the source label. The bool is false
// when no rule matches; callers fall through to the default chain.
func (d *DispatchTable) Lookup(source string) (DispatchRule, bool) {
	if d == nil {
		return DispatchRule{}, false
	}
	r, ok := d.rules[source]
	return r, ok
}
