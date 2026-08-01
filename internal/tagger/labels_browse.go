package tagger

import (
	"path/filepath"
	"sort"
)

// LabelView is one row of the settings mappings browser: a model label
// and where it effectively lands. Rule names the layer that produced
// the routing - "" (the model's own), "default" (embedded rule) or
// "custom" (overlay rule).
type LabelView struct {
	Source  string
	CatName string // effective category; "" when Muted
	TagName string // effective tag name (rename applied)
	Muted   bool
	Rule    string
}

// BrowseLabels loads the tagger's label file and resolves every
// non-placeholder label through the same dispatch-then-scheme chain
// the inference pipeline uses, returning the rows source-sorted.
// catIDs is the gallery's category name→id map; rules pointing at a
// category the gallery lacks fall through exactly like LoadDispatch.
// The single_general inferred-category lift is a per-job DB lookup and
// is not applied here - those labels read as their static routing.
func BrowseLabels(modelPath, taggerName, tagsFile string, catIDs map[string]int64) ([]LabelView, error) {
	profile, err := ResolveProfile(modelPath, taggerName, tagsFile)
	if err != nil {
		return nil, err
	}
	labels, err := loadLabels(filepath.Join(modelPath, taggerName, tagsFile), profile)
	if err != nil {
		return nil, err
	}
	embedded := compileEntries(parseEmbeddedDispatch(taggerName), catIDs)
	overlay := compileEntries(parseOverlayDispatch(modelPath, taggerName), catIDs)
	empty := &DispatchTable{}

	out := make([]LabelView, 0, len(labels))
	for _, l := range labels {
		if l.placeholder {
			continue
		}
		v := LabelView{Source: l.name, TagName: l.name}
		rule, ok := overlay[l.name]
		if ok {
			v.Rule = "custom"
		} else if rule, ok = embedded[l.name]; ok {
			v.Rule = "default"
		}
		if ok {
			if rule.Drop {
				v.Muted = true
			} else {
				v.CatName = rule.CatName
				if rule.Name != "" {
					v.TagName = rule.Name
				}
			}
		} else {
			v.CatName = resolveCategory(profile, l, catIDs, empty).catName
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out, nil
}

func compileEntries(entries []DispatchEntry, catIDs map[string]int64) map[string]DispatchRule {
	out := map[string]DispatchRule{}
	for _, e := range entries {
		if rule, ok := compileDispatchRule(e, catIDs); ok {
			out[e.Source] = rule
		}
	}
	return out
}
