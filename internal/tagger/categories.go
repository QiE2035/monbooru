//go:build tagger

package tagger

// wd14Category maps WD14 numeric category IDs to Monbooru built-in
// category names, following the danbooru numbering the label sets are
// derived from. Other category schemes (single_general, name_string)
// don't go through this table.
//
// Category 9 - where WD14 files the rating family - is absent on
// purpose: the four canonical rating labels are intercepted by name
// before this lookup, and the rating category accepts only those, so
// anything else declaring category 9 falls through to general rather
// than being forced into a category it cannot legally join.
var wd14Category = map[int]string{
	0: "general",
	1: "artist",
	3: "copyright",
	4: "character",
	5: "meta",
}

// wd14RatingTags are WD14 rating labels routed to the rating category.
// Only the four canonical names are listed - the rating category accepts
// only those, and storeResults inserts directly without going through
// GetOrCreateTag's guard, so any non-canonical entry here would land a
// stray row in the rating category.
var wd14RatingTags = map[string]bool{
	"general": true, "sensitive": true, "questionable": true, "explicit": true,
}

// categoryResolution names the resolved category for one label plus the
// optional rename hint a dispatch rule contributed. Returned by
// resolveCategory so processOne can apply both in one place.
type categoryResolution struct {
	catID    int64
	catName  string
	skip     bool
	override bool // true when a dispatch rule produced this result
}

// resolveCategory turns a (profile, label) pair into a destination
// category id plus the canonical name. Precedence, in order:
//
//  1. dispatch hit - operator overrides the source label entirely.
//  2. WD14 rating tag - the four canonical rating labels go to `rating`
//     regardless of what the model declared (WD14 prints them in
//     category 9, which wd14Category deliberately leaves unmapped).
//  3. profile.CategoryScheme:
//     - wd14_numeric  : look label.categoryID up in wd14Category;
//     unknown ids fall to general.
//     - single_general: everything lands in general; the .txt-tagger
//     inferred-category fallback (callers know about
//     it) gets a chance to lift it elsewhere.
//     - name_string   : look label.categoryName up in catIDs.
//
// The returned categoryResolution.skip is true only when a dispatch rule
// dropped the source. catIDs is the tag_categories name→id map the
// caller already loaded for the run.
func resolveCategory(profile Profile, label tagLabel, catIDs map[string]int64, dispatch *DispatchTable) categoryResolution {
	if rule, ok := dispatch.Lookup(label.name); ok {
		if rule.Drop {
			return categoryResolution{skip: true, override: true}
		}
		return categoryResolution{
			catID:    rule.CatID,
			catName:  rule.CatName,
			override: true,
		}
	}
	if wd14RatingTags[label.name] {
		return categoryResolution{
			catID:   catIDs["rating"],
			catName: "rating",
		}
	}
	switch profile.CategoryScheme {
	case "wd14_numeric":
		name := wd14Category[label.categoryID]
		if name == "" {
			name = "general"
		}
		return categoryResolution{catID: catIDs[name], catName: name}
	case "single_general":
		return categoryResolution{catID: catIDs["general"], catName: "general"}
	case "name_string":
		name := label.categoryName
		if _, ok := catIDs[name]; !ok {
			name = "general"
		}
		return categoryResolution{catID: catIDs[name], catName: name}
	}
	return categoryResolution{catID: catIDs["general"], catName: "general"}
}
