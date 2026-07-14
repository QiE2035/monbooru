//go:build tagger

package tagger

import "testing"

func TestResolveCategory_WD14Numeric(t *testing.T) {
	catIDs := canonicalCatIDs
	dispatch := &DispatchTable{rules: map[string]DispatchRule{}}
	prof := wd14Profile

	res := resolveCategory(prof, tagLabel{name: "1girl", categoryID: 0}, catIDs, dispatch)
	if res.catID != catIDs["general"] || res.skip {
		t.Errorf("general label routed wrong: %+v", res)
	}

	res = resolveCategory(prof, tagLabel{name: "hatsune_miku", categoryID: 4}, catIDs, dispatch)
	if res.catID != catIDs["character"] {
		t.Errorf("character label routed wrong: %+v", res)
	}

	res = resolveCategory(prof, tagLabel{name: "touhou", categoryID: 3}, catIDs, dispatch)
	if res.catID != catIDs["copyright"] {
		t.Errorf("copyright label routed wrong: %+v", res)
	}

	// Unknown WD14 category id falls to general.
	res = resolveCategory(prof, tagLabel{name: "stray", categoryID: 99}, catIDs, dispatch)
	if res.catID != catIDs["general"] {
		t.Errorf("unknown category did not fall to general: %+v", res)
	}

	// A label declaring the rating category but not named like one of the
	// four canonical ratings cannot legally join `rating`, so it lands in
	// general instead of being forced somewhere arbitrary.
	res = resolveCategory(prof, tagLabel{name: "rating_safe", categoryID: 9}, catIDs, dispatch)
	if res.catID != catIDs["general"] {
		t.Errorf("non-canonical category 9 did not fall to general: %+v", res)
	}
}

func TestResolveCategory_RatingShortCircuits(t *testing.T) {
	// WD14 ships rating labels in category 9. The rating special case
	// must beat the wd14_numeric routing.
	catIDs := canonicalCatIDs
	dispatch := &DispatchTable{rules: map[string]DispatchRule{}}
	prof := wd14Profile

	res := resolveCategory(prof, tagLabel{name: "general", categoryID: 9}, catIDs, dispatch)
	if res.catID != catIDs["rating"] {
		t.Errorf("rating short-circuit missed: %+v", res)
	}
}

func TestResolveCategory_DispatchOverrides(t *testing.T) {
	catIDs := canonicalCatIDs
	dispatch := &DispatchTable{rules: map[string]DispatchRule{
		"monochrome": {CatID: catIDs["medium"]},
		"annoying":   {Drop: true},
	}}
	prof := wd14Profile

	res := resolveCategory(prof, tagLabel{name: "monochrome", categoryID: 0}, catIDs, dispatch)
	if res.catID != catIDs["medium"] || !res.override {
		t.Errorf("dispatch routing missed: %+v", res)
	}

	res = resolveCategory(prof, tagLabel{name: "annoying", categoryID: 0}, catIDs, dispatch)
	if !res.skip || !res.override {
		t.Errorf("dispatch drop missed: %+v", res)
	}
}

func TestResolveCategory_NameStringScheme(t *testing.T) {
	catIDs := canonicalCatIDs
	dispatch := &DispatchTable{rules: map[string]DispatchRule{}}
	prof := Profile{CategoryScheme: "name_string"}

	res := resolveCategory(prof, tagLabel{name: "neon_genesis_evangelion", categoryName: "copyright"}, catIDs, dispatch)
	if res.catID != catIDs["copyright"] {
		t.Errorf("name_string copyright routing wrong: %+v", res)
	}
	if res.catName != "copyright" {
		t.Errorf("name_string catName = %q, want copyright", res.catName)
	}

	// Unknown category name falls to general.
	res = resolveCategory(prof, tagLabel{name: "x", categoryName: "totally-not-a-cat"}, catIDs, dispatch)
	if res.catID != catIDs["general"] {
		t.Errorf("unknown name_string category did not fall to general: %+v", res)
	}
}

func TestResolveCategory_NameStringRatingShortCircuit(t *testing.T) {
	// The rating special case fires regardless of profile - a label
	// named "explicit" lands in `rating` even when name_string would
	// otherwise route it elsewhere.
	catIDs := canonicalCatIDs
	dispatch := &DispatchTable{rules: map[string]DispatchRule{}}
	prof := Profile{CategoryScheme: "name_string"}

	res := resolveCategory(prof, tagLabel{name: "explicit", categoryName: "rating"}, catIDs, dispatch)
	if res.catID != catIDs["rating"] || res.catName != "rating" {
		t.Errorf("rating short-circuit missed for name_string: %+v", res)
	}
}

func TestResolveCategory_SingleGeneral(t *testing.T) {
	catIDs := canonicalCatIDs
	dispatch := &DispatchTable{rules: map[string]DispatchRule{}}
	prof := joytagProfile

	res := resolveCategory(prof, tagLabel{name: "1girl", categoryID: 4}, catIDs, dispatch)
	if res.catID != catIDs["general"] {
		t.Errorf("single_general should route everything to general: %+v", res)
	}
}
