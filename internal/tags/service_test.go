package tags

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/models"
)

func setupTestDB(t *testing.T) (*db.DB, *Service) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	svc := New(database)
	return database, svc
}

func generalCategoryID(t *testing.T, svc *Service) int64 {
	t.Helper()
	cats, err := svc.ListCategories()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cats {
		if c.Name == "general" {
			return c.ID
		}
	}
	t.Fatal("general category not found")
	return 0
}

// insertTestImage inserts a minimal image record for testing.
func insertTestImage(t *testing.T, database *db.DB, sha string) int64 {
	t.Helper()
	var id int64
	err := database.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES (?, ?, 'png', 100) RETURNING id`,
		sha, "/gallery/"+sha+".png",
	).Scan(&id)
	if err != nil {
		t.Fatalf("insertTestImage: %v", err)
	}
	return id
}

func TestAddTagToImage_UsageCount(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	imgID := insertTestImage(t, database, "abc123")

	tag, err := svc.GetOrCreateTag("cute", catID)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.AddTagToImage(imgID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetTag(tag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsageCount != 1 {
		t.Errorf("UsageCount = %d, want 1", got.UsageCount)
	}
}

// usage_count tracks the visible-image count for the tag (RecalcDB
// rebuilds it that way). Adds/removes against missing images must not
// change it, otherwise the count silently drifts the next time an
// unrelated mutation triggers RecalcIDs.
func TestAddTagToMissingImage_DoesNotIncrementUsage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	imgID := insertTestImage(t, database, "missing-add")
	if _, err := database.Write.Exec(`UPDATE images SET is_missing = 1 WHERE id = ?`, imgID); err != nil {
		t.Fatal(err)
	}

	tag, err := svc.GetOrCreateTag("phantom", catID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(imgID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetTag(tag.ID)
	if got.UsageCount != 0 {
		t.Errorf("UsageCount = %d, want 0 after add to missing image", got.UsageCount)
	}
}

func TestRemoveTagFromMissingImage_DoesNotDecrementUsage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	visible := insertTestImage(t, database, "remove-visible")
	missing := insertTestImage(t, database, "remove-missing")
	// Mark the second image missing before the add so the visible-only
	// invariant holds at add time too. Mirrors the watcher's mark-missing
	// → user-initiated remove sequence.
	if _, err := database.Write.Exec(`UPDATE images SET is_missing = 1 WHERE id = ?`, missing); err != nil {
		t.Fatal(err)
	}

	tag, err := svc.GetOrCreateTag("dual", catID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(visible, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(missing, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetTag(tag.ID)
	if got.UsageCount != 1 {
		t.Fatalf("preflight UsageCount = %d, want 1 (visible-only)", got.UsageCount)
	}
	if err := svc.RemoveTagFromImage(missing, tag.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.GetTag(tag.ID)
	if got.UsageCount != 1 {
		t.Errorf("UsageCount = %d after remove from missing, want 1", got.UsageCount)
	}
}

func TestAddTagTwice_NoDouble(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "abc124")

	tag, _ := svc.GetOrCreateTag("cute", catID)
	if err := svc.AddTagToImage(imgID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(imgID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	got, _ := svc.GetTag(tag.ID)
	if got.UsageCount != 1 {
		t.Errorf("UsageCount = %d, want 1 after duplicate add", got.UsageCount)
	}
}

func TestAddTagsToImageFromTagger_ManualSource(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "abc130")

	tag, _ := svc.GetOrCreateTag("sourced", catID)
	if err := svc.AddTagsToImageFromTagger(imgID, []int64{tag.ID}, false, "my_app"); err != nil {
		t.Fatalf("AddTagsToImageFromTagger: %v", err)
	}

	var isAuto int
	var taggerName *string
	err := database.Read.QueryRow(
		`SELECT is_auto, tagger_name FROM image_tags WHERE image_id = ? AND tag_id = ?`,
		imgID, tag.ID,
	).Scan(&isAuto, &taggerName)
	if err != nil {
		t.Fatalf("scan image_tags: %v", err)
	}
	if isAuto != 0 {
		t.Errorf("is_auto = %d, want 0 for manual source-tagged add", isAuto)
	}
	if taggerName == nil || *taggerName != "my_app" {
		t.Errorf("tagger_name = %v, want %q", taggerName, "my_app")
	}
}

// TestAddTagToImage_PromotesAutoToUser pins the manual-re-add path: an
// existing auto-tagger row should flip to user-owned (is_auto=0,
// confidence=NULL, tagger_name=NULL) when the operator types the same
// tag into the detail page. The AddResult signals the promotion so
// the inline flash can say "promoted to user tag" instead of falling
// through to the "already on image" branch.
func TestAddTagToImage_PromotesAutoToUser(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "auto_to_user")

	tag, _ := svc.GetOrCreateTag("auto_to_user_tag", catID)
	conf := 0.87
	if _, err := svc.AddTagToImageReportingDup(imgID, tag.ID, true, &conf, "wd-swin"); err != nil {
		t.Fatalf("seed auto row: %v", err)
	}

	res, err := svc.AddTagToImageReportingDup(imgID, tag.ID, false, nil, "")
	if err != nil {
		t.Fatalf("manual re-add: %v", err)
	}
	if res.Added {
		t.Error("Added should be false; the row already existed as auto")
	}
	if !res.Promoted {
		t.Error("Promoted should be true; auto row should flip to user")
	}

	var isAuto int
	var confidence sql.NullFloat64
	var taggerName sql.NullString
	if err := database.Read.QueryRow(
		`SELECT is_auto, confidence, tagger_name FROM image_tags
		 WHERE image_id = ? AND tag_id = ?`, imgID, tag.ID,
	).Scan(&isAuto, &confidence, &taggerName); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if isAuto != 0 {
		t.Errorf("is_auto = %d, want 0", isAuto)
	}
	if confidence.Valid {
		t.Errorf("confidence = %v, want NULL after promotion", confidence.Float64)
	}
	if taggerName.Valid {
		t.Errorf("tagger_name = %q, want NULL after promotion", taggerName.String)
	}
}

// TestAddTagToImage_AutoReAddDoesNotDemoteUser pins the inverse: an
// auto-tagger pass over an image already carrying a user-owned tag
// must leave the row untouched. Otherwise a routine re-tag would
// silently strip the user's explicit choice.
func TestAddTagToImage_AutoReAddDoesNotDemoteUser(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "user_keep")

	tag, _ := svc.GetOrCreateTag("user_keep_tag", catID)
	if err := svc.AddTagToImage(imgID, tag.ID, false, nil); err != nil {
		t.Fatalf("seed user row: %v", err)
	}

	conf := 0.92
	res, err := svc.AddTagToImageReportingDup(imgID, tag.ID, true, &conf, "wd-swin")
	if err != nil {
		t.Fatalf("auto re-add: %v", err)
	}
	if res.Added || res.Promoted {
		t.Errorf("auto re-add of a user row should be a no-op, got %+v", res)
	}

	var isAuto int
	var confidence sql.NullFloat64
	var taggerName sql.NullString
	if err := database.Read.QueryRow(
		`SELECT is_auto, confidence, tagger_name FROM image_tags
		 WHERE image_id = ? AND tag_id = ?`, imgID, tag.ID,
	).Scan(&isAuto, &confidence, &taggerName); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if isAuto != 0 {
		t.Errorf("is_auto = %d, want 0 (user-owned row must stick)", isAuto)
	}
	if confidence.Valid || taggerName.Valid {
		t.Errorf("user row should not gain confidence/tagger_name; got conf=%v tagger=%v", confidence, taggerName)
	}
}

func TestRemoveTag_DecrementUsageCount(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "abc125")

	tag, _ := svc.GetOrCreateTag("cute", catID)
	if err := svc.AddTagToImage(imgID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveTagFromImage(imgID, tag.ID); err != nil {
		t.Fatal(err)
	}

	// Removing the last image leaves the tag at usage_count=0; the row
	// itself sticks around so user-declared aliases and implications keep
	// resolving against an empty library.
	got, err := svc.GetTag(tag.ID)
	if err != nil {
		t.Fatalf("tag should persist at zero usage, got err=%v", err)
	}
	if got.UsageCount != 0 {
		t.Errorf("UsageCount = %d, want 0", got.UsageCount)
	}
}

func TestDeleteCategory_ReassignsToGeneral(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	// Create custom category
	custom, err := svc.CreateCategory("custom_cat", "#aabbcc")
	if err != nil {
		t.Fatal(err)
	}

	// Create tag in custom category
	tag, err := svc.GetOrCreateTag("mytag", custom.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Delete custom category
	if err := svc.DeleteCategoryMoveOrDelete(custom.ID, "move", 0); err != nil {
		t.Fatal(err)
	}

	// Tag should now be in general
	got, err := svc.GetTag(tag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CategoryID != catID {
		t.Errorf("tag category = %d, want general (%d)", got.CategoryID, catID)
	}
}

func TestDeleteBuiltinCategory_Rejected(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	err := svc.DeleteCategoryMoveOrDelete(catID, "move", 0)
	if err != ErrBuiltinCategory {
		t.Errorf("expected ErrBuiltinCategory, got %v", err)
	}
}

// "system" is reserved because the search-bar autocomplete uses it as a
// virtual cheat-sheet namespace; a real category by that name would
// hijack `system:foo` into a category-qualified search.
func TestCreateCategory_RejectsSystemReservedName(t *testing.T) {
	_, svc := setupTestDB(t)

	if _, err := svc.CreateCategory("system", "#aabbcc"); err != ErrReservedCategoryName {
		t.Errorf("CreateCategory(system) err = %v, want ErrReservedCategoryName", err)
	}
}

// TestCreateCategory_RejectsInvalidNames: the allowlist
// stops payloads that would round-trip badly through the tagger
// threshold form-field name attribute, the cat: search syntax, and
// the rendered template context. Numbers/dashes/underscores are fine;
// quotes, slashes, dates, whitespace, and shell control characters
// must be refused.
func TestCreateCategory_RejectsInvalidNames(t *testing.T) {
	_, svc := setupTestDB(t)

	bad := []string{
		"' OR 1=1",
		"<script>",
		"2024/01/02",
		"a b",
		"foo bar",
		"foo:bar",
		"foo bar/baz",
		"foo$bar",
	}
	for _, name := range bad {
		if _, err := svc.CreateCategory(name, "#aabbcc"); err != ErrInvalidCategoryName {
			t.Errorf("CreateCategory(%q) err = %v, want ErrInvalidCategoryName", name, err)
		}
	}

	// Sanity: the legitimate shape still passes.
	if _, err := svc.CreateCategory("mood-2", "#aabbcc"); err != nil {
		t.Errorf("CreateCategory(mood-2) err = %v, want nil", err)
	}
}

func TestCreateCategory_DuplicateNameFriendlyError(t *testing.T) {
	_, svc := setupTestDB(t)

	if _, err := svc.CreateCategory("mood", "#aabbcc"); err != nil {
		t.Fatalf("first CreateCategory(mood): %v", err)
	}
	if _, err := svc.CreateCategory("mood", "#aabbcc"); err != ErrCategoryExists {
		t.Errorf("duplicate CreateCategory(mood) err = %v, want ErrCategoryExists", err)
	}
	// Built-in "general" must also surface the friendly error.
	if _, err := svc.CreateCategory("general", "#aabbcc"); err != ErrCategoryExists {
		t.Errorf("CreateCategory(general) err = %v, want ErrCategoryExists", err)
	}
}

func TestRenameCategory_DuplicateNameFriendlyError(t *testing.T) {
	_, svc := setupTestDB(t)

	a, err := svc.CreateCategory("alpha", "#aabbcc")
	if err != nil {
		t.Fatalf("CreateCategory(alpha): %v", err)
	}
	if _, err := svc.CreateCategory("bravo", "#aabbcc"); err != nil {
		t.Fatalf("CreateCategory(bravo): %v", err)
	}
	if err := svc.RenameCategory(a.ID, "bravo"); err != ErrCategoryExists {
		t.Errorf("RenameCategory(alpha→bravo) err = %v, want ErrCategoryExists", err)
	}
}

func TestRenameCategory_BuiltinUsesRenameMessage(t *testing.T) {
	_, svc := setupTestDB(t)
	if err := svc.RenameCategory(generalCategoryID(t, svc), "renamed_general"); err != ErrBuiltinCategoryName {
		t.Fatalf("RenameCategory(builtin) err = %v, want ErrBuiltinCategoryName", err)
	}
}

func TestMergeTags(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "abc127")

	tagAlias, _ := svc.GetOrCreateTag("old_tag", catID)
	tagCanon, _ := svc.GetOrCreateTag("new_tag", catID)

	if err := svc.AddTagToImage(imgID, tagAlias.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	if err := svc.MergeTags(tagAlias.ID, tagCanon.ID); err != nil {
		t.Fatal(err)
	}

	// Image should now have canonical tag
	_, imgTags, err := svc.GetImageTags(imgID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range imgTags {
		if it.TagID == tagCanon.ID {
			found = true
		}
		if it.TagID == tagAlias.ID {
			t.Error("alias tag still on image after merge")
		}
	}
	if !found {
		t.Error("canonical tag not on image after merge")
	}

	// Alias tag should be marked
	got, _ := svc.GetTag(tagAlias.ID)
	if !got.IsAlias {
		t.Error("aliasID not marked as alias")
	}
	if got.CanonicalTagID == nil || *got.CanonicalTagID != tagCanon.ID {
		t.Error("canonical_tag_id not set correctly")
	}
}

func TestSuggestTags_PrefixFirst(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	// Two prefix matches (abc_123, abc_456) + one substring match (xyz_abc).
	// Make xyz_abc the most-used tag so any plain "order by usage" would
	// float it to the top. The spec promises prefix matches win regardless.
	if _, err := svc.GetOrCreateTag("abc_123", catID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetOrCreateTag("xyz_abc", catID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetOrCreateTag("abc_456", catID); err != nil {
		t.Fatal(err)
	}
	imgA := insertTestImage(t, database, "abc_img_a")
	imgB := insertTestImage(t, database, "abc_img_b")
	xyzTag, _ := svc.GetOrCreateTag("xyz_abc", catID)
	for _, img := range []int64{imgA, imgB} {
		if err := svc.AddTagToImage(img, xyzTag.ID, false, nil); err != nil {
			t.Fatal(err)
		}
	}

	results, err := svc.SuggestTags("abc", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 3 {
		t.Fatalf("expected 3 suggestions, got %d: %+v", len(results), results)
	}
	// The first two results must both start with "abc" even though xyz_abc
	// has higher usage - prefix matches win.
	for i := 0; i < 2; i++ {
		if !strings.HasPrefix(results[i].Name, "abc") {
			t.Errorf("position %d: got %q, want a name starting with 'abc' (full order: %+v)", i, results[i].Name, results)
		}
	}
	if results[2].Name != "xyz_abc" {
		t.Errorf("position 2: got %q, want xyz_abc (substring match last)", results[2].Name)
	}
}

// TestSuggestTagsInCategory pins the category-scoped tag-input
// autocomplete (the `category:prefix` shape): it must return only
// prefix matches that live in the named category, sorted by
// usage_count DESC, and must exclude alias rows and substring-only
// matches.
func TestSuggestTagsInCategory(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	character, err := svc.CreateCategory("character", "#112233")
	if err != nil {
		// "character" is a built-in; reuse it rather than re-creating.
		if err != ErrCategoryExists {
			t.Fatalf("CreateCategory(character): %v", err)
		}
		cats, lerr := svc.ListCategories()
		if lerr != nil {
			t.Fatal(lerr)
		}
		for i := range cats {
			if cats[i].Name == "character" {
				character = &cats[i]
				break
			}
		}
	}
	if character == nil {
		t.Fatal("character category not found")
	}

	// Two character tags matching the "par" prefix; one matching tag in a
	// different category (must be excluded); one character tag that only
	// matches as a substring (must be excluded since this is prefix-only);
	// and one character alias whose name also matches the prefix (must be
	// excluded by the is_alias = 0 filter). The alias's canonical target
	// has a non-matching name so only the alias would surface if the
	// filter were wrong.
	parade, _ := svc.GetOrCreateTag("parade", character.ID)
	parasol, _ := svc.GetOrCreateTag("parasol", character.ID)
	_, _ = svc.GetOrCreateTag("parade_general", catID)      // wrong category
	_, _ = svc.GetOrCreateTag("comparable", character.ID)   // substring, not prefix
	canon, _ := svc.GetOrCreateTag("honored", character.ID) // alias target, no "par" prefix
	if _, err := svc.CreateAlias("parka_alias", character.ID, canon.ID); err != nil {
		t.Fatalf("CreateAlias: %v", err)
	}

	// Make parasol the more-used of the two prefix matches so usage_count
	// DESC ordering is observable: parasol used by two images, parade by
	// one.
	imgA := insertTestImage(t, database, "cat_suggest_a")
	imgB := insertTestImage(t, database, "cat_suggest_b")
	imgC := insertTestImage(t, database, "cat_suggest_c")
	for _, img := range []int64{imgA, imgB} {
		if err := svc.AddTagToImage(img, parasol.ID, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.AddTagToImage(imgC, parade.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	results, err := svc.SuggestTagsInCategory("par", "character", 10)
	if err != nil {
		t.Fatal(err)
	}

	got := tagNames(results)
	want := []string{"parasol", "parade"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SuggestTagsInCategory = %v, want %v (only prefix matches in the named category, usage DESC, no alias/substring)", got, want)
	}
	for _, r := range results {
		if r.CategoryName != "character" {
			t.Errorf("tag %q has category %q, want character", r.Name, r.CategoryName)
		}
	}

	// Limit is honoured.
	one, err := svc.SuggestTagsInCategory("par", "character", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Name != "parasol" {
		t.Errorf("limit=1 returned %v, want [parasol] (highest usage)", tagNames(one))
	}

	// A prefix with no matches in the category yields an empty result.
	none, err := svc.SuggestTagsInCategory("zzz", "character", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("no-match prefix returned %v, want empty", tagNames(none))
	}
}

func TestRelatedImages(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	img1 := insertTestImage(t, database, "rel1")
	img2 := insertTestImage(t, database, "rel2")
	img3 := insertTestImage(t, database, "rel3")

	tagA, _ := svc.GetOrCreateTag("rel_a", catID)
	tagB, _ := svc.GetOrCreateTag("rel_b", catID)
	tagC, _ := svc.GetOrCreateTag("rel_c", catID)

	// img1: A, B   img2: A, B   img3: C
	if err := svc.AddTagToImage(img1, tagA.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(img1, tagB.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(img2, tagA.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(img2, tagB.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(img3, tagC.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	related, err := svc.RelatedImages(img1, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(related) != 1 || related[0].ID != img2 {
		t.Fatalf("related = %+v, want only img2", related)
	}
}

// RelatedImages must surface enough info on each candidate that the
// related-images partial can render the manga-pill. Specifically the
// FileType and PageCount fields need to carry through; without them
// the template can't tell a cbz candidate from an image candidate.
// Type partition: a manga source surfaces other manga, a non-manga
// source surfaces other non-manga rows.
func TestRelatedImages_CarriesFileTypeAndPageCount(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	var src int64
	if err := database.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size, page_count) VALUES (?, ?, 'cbz', 800, 5) RETURNING id`,
		"src-cbz", "/gallery/src.cbz",
	).Scan(&src); err != nil {
		t.Fatalf("insert src manga: %v", err)
	}
	var manga int64
	if err := database.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size, page_count) VALUES (?, ?, 'cbz', 1000, 12) RETURNING id`,
		"manga-sha", "/gallery/m.cbz",
	).Scan(&manga); err != nil {
		t.Fatalf("insert manga: %v", err)
	}

	tagA, _ := svc.GetOrCreateTag("shared", catID)
	if err := svc.AddTagToImage(src, tagA.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(manga, tagA.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	related, err := svc.RelatedImages(src, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(related) != 1 || related[0].ID != manga {
		t.Fatalf("related = %+v, want one manga row", related)
	}
	if related[0].FileType != "cbz" {
		t.Errorf("FileType = %q, want cbz", related[0].FileType)
	}
	if related[0].PageCount == nil || *related[0].PageCount != 12 {
		t.Errorf("PageCount = %v, want 12", related[0].PageCount)
	}
}

// Type partition: a non-manga source must not surface manga
// candidates and vice versa, so "Similar entries" in the reader
// doesn't bounce the user to a regular-image grid (or the inverse).
func TestRelatedImages_TypePartition(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	imgSrc := insertTestImage(t, database, "img-src")
	imgPeer := insertTestImage(t, database, "img-peer")
	var manga int64
	if err := database.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size, page_count) VALUES (?, ?, 'cbz', 100, 3) RETURNING id`,
		"manga-x", "/gallery/x.cbz",
	).Scan(&manga); err != nil {
		t.Fatal(err)
	}
	tag, _ := svc.GetOrCreateTag("link", catID)
	if err := svc.AddTagToImage(imgSrc, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(imgPeer, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(manga, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	rel, err := svc.RelatedImages(imgSrc, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rel) != 1 || rel[0].ID != imgPeer {
		t.Fatalf("img-source related = %+v, want only the peer image (manga must be filtered out)", rel)
	}

	relManga, err := svc.RelatedImages(manga, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(relManga) != 0 {
		t.Fatalf("manga-source related = %+v, want empty (no other manga)", relManga)
	}
}

func TestRelatedImages_DropsPopularTags(t *testing.T) {
	// A tag whose global usage_count is above relatedMaxTagUsage carries
	// no discriminative signal, so it must not contribute to the seed
	// set. With it dropped, an image whose only shared tag is the
	// popular one yields an empty related panel.
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	img1 := insertTestImage(t, database, "rel_pop1")
	img2 := insertTestImage(t, database, "rel_pop2")

	tag, _ := svc.GetOrCreateTag("very_popular", catID)
	if err := svc.AddTagToImage(img1, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(img2, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(
		`UPDATE tags SET usage_count = ? WHERE id = ?`, relatedMaxTagUsage+1, tag.ID,
	); err != nil {
		t.Fatal(err)
	}

	related, err := svc.RelatedImages(img1, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(related) != 0 {
		t.Fatalf("related = %+v, want empty (shared tag is over the popularity cap)", related)
	}
}

func TestListTags_All(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	if _, err := svc.GetOrCreateTag("list_a", catID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetOrCreateTag("list_b", catID); err != nil {
		t.Fatal(err)
	}

	// GetOrCreateTag without an image_tags row leaves usage_count=0; the
	// listing now hides those by default, so opt in with ShowZero.
	tags, total, err := svc.ListTags(TagFilter{Limit: 100, ShowZero: true})
	if err != nil {
		t.Fatal(err)
	}
	if total < 2 {
		t.Errorf("total = %d, want >= 2", total)
	}
	_ = tags
}

func TestListTags_WithPrefix(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	if _, err := svc.GetOrCreateTag("prefix_abc", catID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetOrCreateTag("prefix_xyz", catID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetOrCreateTag("other_tag", catID); err != nil {
		t.Fatal(err)
	}

	tags, total, err := svc.ListTags(TagFilter{Prefix: "prefix", Limit: 100, ShowZero: true})
	if err != nil {
		t.Fatal(err)
	}
	if total < 2 {
		t.Errorf("prefix total = %d, want >= 2", total)
	}
	for _, tg := range tags {
		if len(tg.Name) < 6 || tg.Name[:6] != "prefix" {
			t.Errorf("unexpected tag in prefix filter: %q", tg.Name)
		}
	}
}

func TestListTags_WithCategoryFilter(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	custom, _ := svc.CreateCategory("custom_filter", "#000000")
	if _, err := svc.GetOrCreateTag("cat_tag", custom.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetOrCreateTag("gen_tag", catID); err != nil {
		t.Fatal(err)
	}

	tags, total, err := svc.ListTags(TagFilter{CategoryID: &custom.ID, Limit: 100, ShowZero: true})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("category filter total = %d, want 1", total)
	}
	_ = tags
}

func TestListTags_SortByUsage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	// Three images. sort_c used by 3, sort_a by 2, sort_b by 1. Expected
	// descending order: sort_c, sort_a, sort_b.
	img1 := insertTestImage(t, database, "usage_sort_1")
	img2 := insertTestImage(t, database, "usage_sort_2")
	img3 := insertTestImage(t, database, "usage_sort_3")
	tagA, _ := svc.GetOrCreateTag("sort_a", catID)
	tagB, _ := svc.GetOrCreateTag("sort_b", catID)
	tagC, _ := svc.GetOrCreateTag("sort_c", catID)
	for _, img := range []int64{img1, img2} {
		if err := svc.AddTagToImage(img, tagA.ID, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.AddTagToImage(img3, tagB.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	for _, img := range []int64{img1, img2, img3} {
		if err := svc.AddTagToImage(img, tagC.ID, false, nil); err != nil {
			t.Fatal(err)
		}
	}

	tags, _, err := svc.ListTags(TagFilter{Sort: "usage", Prefix: "sort_", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) < 3 {
		t.Fatalf("expected at least 3 tags with prefix sort_, got %d", len(tags))
	}
	wantOrder := []string{"sort_c", "sort_a", "sort_b"}
	for i, want := range wantOrder {
		if tags[i].Name != want {
			t.Errorf("position %d: got %q, want %q (full order: %+v)", i, tags[i].Name, want, tagNames(tags))
		}
	}
}

func tagNames(ts []models.Tag) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

func TestRecalc_CountsOnlyNonMissing(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	liveImg := insertTestImage(t, database, "recalc_live")
	goneImg := insertTestImage(t, database, "recalc_gone")
	if _, err := database.Write.Exec(`UPDATE images SET is_missing = 1 WHERE id = ?`, goneImg); err != nil {
		t.Fatal(err)
	}

	shared, _ := svc.GetOrCreateTag("recalc_shared", catID)
	onlyGone, _ := svc.GetOrCreateTag("recalc_only_gone", catID)
	if err := svc.AddTagToImage(liveImg, shared.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(goneImg, shared.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(goneImg, onlyGone.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	// Poison the counts so the recalc has work to do.
	if _, err := database.Write.Exec(`UPDATE tags SET usage_count = 99 WHERE id IN (?, ?)`, shared.ID, onlyGone.ID); err != nil {
		t.Fatal(err)
	}

	updated, err := svc.RecalcCount()
	if err != nil {
		t.Fatalf("RecalcCount: %v", err)
	}
	if updated < 2 {
		t.Errorf("updated = %d, want >= 2", updated)
	}

	got, err := svc.GetTag(shared.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsageCount != 1 {
		t.Errorf("shared UsageCount = %d, want 1 (only live image counts)", got.UsageCount)
	}
	gone, err := svc.GetTag(onlyGone.ID)
	if err != nil {
		t.Fatalf("only_gone should persist at zero usage, got err=%v", err)
	}
	if gone.UsageCount != 0 {
		t.Errorf("only_gone UsageCount = %d, want 0", gone.UsageCount)
	}
}

func TestListTags_OriginFilterMatchesStoredLabel(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	if _, err := svc.GetOrCreateTag("from_ui", catID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetOrCreateTagFrom("from_danbooru", catID, "danbooru"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetOrCreateTagFrom("from_ptr", catID, "ptr"); err != nil {
		t.Fatal(err)
	}

	names := func(f TagFilter) []string {
		f.Limit = 100
		f.ShowZero = true
		f.Sort = "name"
		list, _, err := svc.ListTags(f)
		if err != nil {
			t.Fatalf("ListTags %+v: %v", f, err)
		}
		out := []string{}
		for _, tg := range list {
			out = append(out, tg.Name)
		}
		return out
	}

	if got := names(TagFilter{Origin: "danbooru"}); !reflect.DeepEqual(got, []string{"from_danbooru"}) {
		t.Errorf("origin=danbooru => %v, want [from_danbooru]", got)
	}
	if got := names(TagFilter{Origin: "user"}); !reflect.DeepEqual(got, []string{"from_ui"}) {
		t.Errorf("origin=user => %v, want [from_ui]", got)
	}
	if got := names(TagFilter{Origin: "ptr"}); !reflect.DeepEqual(got, []string{"from_ptr"}) {
		t.Errorf("origin=ptr => %v, want [from_ptr]", got)
	}

	counts, err := svc.OriginCounts()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, oc := range counts {
		got[oc.Label] = oc.Count
	}
	// The rating category seeds four canonical rows with no origin, so
	// only the three stamped labels surface.
	if got["danbooru"] != 1 || got["ptr"] != 1 || got["user"] != 1 {
		t.Errorf("OriginCounts = %v, want danbooru/ptr/user at 1 each", got)
	}
	if _, ok := got[""]; ok {
		t.Errorf("OriginCounts surfaced the empty label: %v", got)
	}
}

func TestListTags_TypeFilterAndLegacyAliasOrigin(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	canon, err := svc.GetOrCreateTag("type_canon", catID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateAliasFrom("type_alias", catID, canon.ID, "ptr"); err != nil {
		t.Fatal(err)
	}

	names := func(f TagFilter) []string {
		f.Limit = 100
		f.ShowZero = true
		f.Sort = "name"
		f.Prefix = "type_"
		list, _, err := svc.ListTags(f)
		if err != nil {
			t.Fatalf("ListTags %+v: %v", f, err)
		}
		out := []string{}
		for _, tg := range list {
			out = append(out, tg.Name)
		}
		return out
	}

	if got := names(TagFilter{Type: "alias"}); !reflect.DeepEqual(got, []string{"type_alias"}) {
		t.Errorf("type=alias => %v, want [type_alias]", got)
	}
	if got := names(TagFilter{Type: "tag"}); !reflect.DeepEqual(got, []string{"type_canon"}) {
		t.Errorf("type=tag => %v, want [type_canon]", got)
	}
	// The legacy origin=alias spelling keeps resolving structurally.
	if got := names(TagFilter{Origin: "alias"}); !reflect.DeepEqual(got, []string{"type_alias"}) {
		t.Errorf("origin=alias => %v, want [type_alias]", got)
	}
	// Type composes with origin: a ptr-created alias is both.
	if got := names(TagFilter{Type: "alias", Origin: "ptr"}); !reflect.DeepEqual(got, []string{"type_alias"}) {
		t.Errorf("type=alias origin=ptr => %v, want [type_alias]", got)
	}
}

func TestListTags_CreatedAfterAndDateSorts(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	older, err := svc.GetOrCreateTag("dated_old", catID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(
		`UPDATE tags SET created_at = '2020-01-01T00:00:00Z' WHERE id = ?`, older.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetOrCreateTag("dated_new", catID); err != nil {
		t.Fatal(err)
	}

	list, _, err := svc.ListTags(TagFilter{Prefix: "dated_", CreatedAfter: "2025-01-01T00:00:00Z", ShowZero: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "dated_new" {
		t.Errorf("CreatedAfter => %+v, want only dated_new", list)
	}

	list, _, err = svc.ListTags(TagFilter{Prefix: "dated_", Sort: "created", ShowZero: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "dated_new" {
		t.Errorf("sort=created => %+v, want dated_new first", list)
	}

	// last_used sorts applied tags first (DESC default); never-applied
	// rows carry NULL, which SQLite sorts last on DESC.
	imgID := insertTestImage(t, database, "dated_img")
	if err := svc.AddTagToImage(imgID, older.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	list, _, err = svc.ListTags(TagFilter{Prefix: "dated_", Sort: "last_used", ShowZero: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "dated_old" {
		t.Errorf("sort=last_used => %+v, want dated_old (applied) first", list)
	}
}

func TestUpdateCategoryColor(t *testing.T) {
	_, svc := setupTestDB(t)

	cat, err := svc.CreateCategory("color_test", "#aabbcc")
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.UpdateCategoryColor(cat.ID, "#112233"); err != nil {
		t.Fatal(err)
	}

	cats, err := svc.ListCategories()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cats {
		if c.ID == cat.ID && c.Color != "#112233" {
			t.Errorf("color = %q, want #112233", c.Color)
		}
	}
}

func TestRemoveAllTagsFromImage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "remove_all")

	tagA, _ := svc.GetOrCreateTag("rem_all_a", catID)
	tagB, _ := svc.GetOrCreateTag("rem_all_b", catID)
	if err := svc.AddTagToImage(imgID, tagA.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(imgID, tagB.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	if err := svc.RemoveAllTagsFromImage(imgID); err != nil {
		t.Fatal(err)
	}

	_, imgTags, _ := svc.GetImageTags(imgID)
	if len(imgTags) != 0 {
		t.Errorf("expected 0 tags after RemoveAllTagsFromImage, got %d", len(imgTags))
	}

	// Both tag rows persist at usage_count=0 so user-declared aliases and
	// implications keep resolving against the empty image set.
	for _, id := range []int64{tagA.ID, tagB.ID} {
		got, err := svc.GetTag(id)
		if err != nil {
			t.Fatalf("tag %d should persist at zero usage, got err=%v", id, err)
		}
		if got.UsageCount != 0 {
			t.Errorf("tag %d UsageCount = %d, want 0", id, got.UsageCount)
		}
	}
}

func TestRemoveSourceTagsFromImage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "rem_source")

	manual, _ := svc.GetOrCreateTag("man", catID)
	danb, _ := svc.GetOrCreateTag("dan", catID)
	gelb, _ := svc.GetOrCreateTag("gel", catID)
	if err := svc.AddTagToImage(imgID, manual.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagsToImageFromTagger(imgID, []int64{danb.ID}, false, "danbooru"); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagsToImageFromTagger(imgID, []int64{gelb.ID}, false, "gelbooru"); err != nil {
		t.Fatal(err)
	}

	if err := svc.RemoveSourceTagsFromImage(imgID, []string{"danbooru"}); err != nil {
		t.Fatal(err)
	}

	_, imgTags, _ := svc.GetImageTags(imgID)
	got := map[string]bool{}
	for _, it := range imgTags {
		got[it.TagName] = true
	}
	if got["dan"] {
		t.Error("danbooru's tag should be removed")
	}
	if !got["man"] || !got["gel"] {
		t.Errorf("manual and gelbooru tags must survive, got %+v", imgTags)
	}

	// An empty source list is a no-op.
	if err := svc.RemoveSourceTagsFromImage(imgID, nil); err != nil {
		t.Fatal(err)
	}
	if _, after, _ := svc.GetImageTags(imgID); len(after) != 2 {
		t.Errorf("empty sources should be a no-op, got %d tags", len(after))
	}
}

func TestGetOrCreateTag_ValidatesName(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	_, err := svc.GetOrCreateTag("", catID)
	if err == nil {
		t.Error("expected error for empty tag name")
	}
}

func TestMergeThenAddAliasName_LandsOnCanonical(t *testing.T) {
	// After A is merged into B, typing A on a new image should create an
	// image_tag pointing at B - i.e. the alias is a live redirect, not a
	// one-shot move that later adds resurrect.
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	alias, _ := svc.GetOrCreateTag("cat", catID)
	canon, _ := svc.GetOrCreateTag("feline", catID)
	if err := svc.MergeTags(alias.ID, canon.ID); err != nil {
		t.Fatal(err)
	}

	imgID := insertTestImage(t, database, "alias_redirect")
	tag, err := svc.GetOrCreateTag("cat", catID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.ID != canon.ID {
		t.Fatalf("GetOrCreateTag(alias) = %d, want canonical %d", tag.ID, canon.ID)
	}
	if err := svc.AddTagToImage(imgID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	_, imgTags, _ := svc.GetImageTags(imgID)
	if len(imgTags) != 1 || imgTags[0].TagID != canon.ID {
		t.Errorf("image tags = %+v, want single canonical tag %d", imgTags, canon.ID)
	}
}

func TestMergeIntoAlias_Rejected(t *testing.T) {
	// Merging B→A where A is already an alias would install a two-hop
	// chain the resolver does not follow. Reject up front.
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	a, _ := svc.GetOrCreateTag("aaa", catID)
	b, _ := svc.GetOrCreateTag("bbb", catID)
	c, _ := svc.GetOrCreateTag("ccc", catID)
	if err := svc.MergeTags(a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.MergeTags(c.ID, a.ID); err == nil {
		t.Fatal("expected error when merging into an alias, got nil")
	}
}

func TestListTags_AliasFilterReturnsAliasesWithCanonicalJoin(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	alias, _ := svc.GetOrCreateTag("cat", catID)
	canon, _ := svc.GetOrCreateTag("feline", catID)
	if err := svc.MergeTags(alias.ID, canon.ID); err != nil {
		t.Fatal(err)
	}
	list, total, err := svc.ListTags(TagFilter{Origin: "alias", Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(list) != 1 {
		t.Fatalf("alias filter: total=%d len=%d, want 1/1", total, len(list))
	}
	got := list[0]
	if got.Name != "cat" || !got.IsAlias || got.CanonicalName != "feline" {
		t.Errorf("alias row = %+v, want cat → feline", got)
	}
}

func TestListTags_AllIncludesAliasesAndCanonicals(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	alias, _ := svc.GetOrCreateTag("cat", catID)
	canon, _ := svc.GetOrCreateTag("feline", catID)
	if _, err := svc.GetOrCreateTag("dog", catID); err != nil {
		t.Fatal(err)
	}
	if err := svc.MergeTags(alias.ID, canon.ID); err != nil {
		t.Fatal(err)
	}
	// MergeTags leaves both rows at usage_count=0 here (no image carried
	// the alias) so opt into ShowZero to surface the canonical alongside
	// the alias in this scenario.
	list, _, err := svc.ListTags(TagFilter{Limit: 40, ShowZero: true})
	if err != nil {
		t.Fatal(err)
	}
	var aliasSeen, canonSeen bool
	for _, t := range list {
		if t.Name == "cat" && t.IsAlias {
			aliasSeen = true
		}
		if t.Name == "feline" && !t.IsAlias {
			canonSeen = true
		}
	}
	if !aliasSeen || !canonSeen {
		t.Errorf("expected both the alias (cat) and canonical (feline) in default listing; got %+v", list)
	}
}

func TestMergeTags_CanonicalAlreadyOnImage(t *testing.T) {
	// Tests branch where canonical tag is already on the same image as alias
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "merge_both")

	tagAlias, _ := svc.GetOrCreateTag("alias_tag_overlap", catID)
	tagCanon, _ := svc.GetOrCreateTag("canon_tag_overlap", catID)

	// Add both alias and canonical to the same image
	if err := svc.AddTagToImage(imgID, tagAlias.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(imgID, tagCanon.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	if err := svc.MergeTags(tagAlias.ID, tagCanon.ID); err != nil {
		t.Fatal(err)
	}

	// Image should have canonical, not alias
	_, imgTags, _ := svc.GetImageTags(imgID)
	for _, it := range imgTags {
		if it.TagID == tagAlias.ID {
			t.Error("alias tag still on image")
		}
	}
}

func TestGetOrCreateTag_CaseNormalized(t *testing.T) {
	// Tag names should be lowercase
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	// Tag name must match regex - lowercase only
	tag, err := svc.GetOrCreateTag("valid_name", catID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.Name != "valid_name" {
		t.Errorf("Name = %q", tag.Name)
	}
}

func TestValidateTagName_InvalidChars(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	// Space in name should fail
	_, err := svc.GetOrCreateTag("has space", catID)
	if err == nil {
		t.Error("expected error for tag name with space")
	}
}

func TestValidateTagName_ValidSpecialChars(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	// Valid special chars: ()!@#$.~+-
	_, err := svc.GetOrCreateTag("tag(with)special", catID)
	if err != nil {
		t.Errorf("special chars should be valid: %v", err)
	}
}

// Emoticon-class characters round-trip so booru-style tags like ">_<", "<3",
// "=3", "^_^", and "nani?" are usable end-to-end.
func TestValidateTagName_AllowsEmoticonChars(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	for _, name := range []string{">_<", "<3", "=3", "=w=", "^_^", "^o^", "nani?", ":<", ":>"} {
		if _, err := svc.GetOrCreateTag(name, catID); err != nil {
			t.Errorf("GetOrCreateTag(%q) error: %v", name, err)
		}
	}
}

func TestDeleteCategory_DeletesEmpty(t *testing.T) {
	_, svc := setupTestDB(t)

	// Create a category with no tags
	cat, err := svc.CreateCategory("empty_cat", "#123456")
	if err != nil {
		t.Fatal(err)
	}

	// Delete the empty category
	if err := svc.DeleteCategoryMoveOrDelete(cat.ID, "move", 0); err != nil {
		t.Fatalf("expected no error deleting empty category, got: %v", err)
	}

	// Verify it's gone
	cats, _ := svc.ListCategories()
	for _, c := range cats {
		if c.ID == cat.ID {
			t.Error("category still present after delete")
		}
	}
}

func TestRemoveTagFromImage_NotOnImage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "rem_not_on")

	tag, err := svc.GetOrCreateTag("not_on_image", catID)
	if err != nil {
		t.Fatal(err)
	}
	// Removing a tag that isn't on the image is a no-op that must NOT error.
	// This lets callers rebuild an image's tag set idempotently.
	if err := svc.RemoveTagFromImage(imgID, tag.ID); err != nil {
		t.Errorf("removing absent tag must be a no-op, got err %v", err)
	}
	// Tags persist at usage_count=0; the row only goes away on an explicit
	// DeleteTag call. This lets users pre-declare tags and aliases against
	// images that don't yet exist.
	got, err := svc.GetTag(tag.ID)
	if err != nil {
		t.Fatalf("tag lookup: %v", err)
	}
	if got == nil {
		t.Fatal("tag should still exist (auto-prune only fires when a row is deleted)")
	}
	if got.UsageCount != 0 {
		t.Errorf("UsageCount = %d, want 0 after no-op remove", got.UsageCount)
	}
}

func TestListCategories(t *testing.T) {
	_, svc := setupTestDB(t)
	cats, err := svc.ListCategories()
	if err != nil {
		t.Fatal(err)
	}
	// Built-in categories should be seeded
	if len(cats) == 0 {
		t.Error("expected built-in categories to be seeded")
	}
	hasGeneral := false
	for _, c := range cats {
		if c.Name == "general" {
			hasGeneral = true
		}
	}
	if !hasGeneral {
		t.Error("general category not found in ListCategories")
	}
}

func TestListTags_DefaultLimit(t *testing.T) {
	// Limit <= 0 should use default limit of 40
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	if _, err := svc.GetOrCreateTag("default_lim_test", catID); err != nil {
		t.Fatal(err)
	}

	tags, total, err := svc.ListTags(TagFilter{Limit: 0, ShowZero: true}) // Limit=0 triggers default
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Error("expected at least 1 tag")
	}
	_ = tags
}

func TestListTags_WithPage(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	// Three tags so page 1 with limit=1 returns a different single tag than
	// page 0 - proves the OFFSET math.
	if _, err := svc.GetOrCreateTag("page_tag_a", catID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetOrCreateTag("page_tag_b", catID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetOrCreateTag("page_tag_c", catID); err != nil {
		t.Fatal(err)
	}

	p0, total, err := svc.ListTags(TagFilter{Prefix: "page_tag_", Sort: "name", Limit: 1, PageIndex: 0, ShowZero: true})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(p0) != 1 {
		t.Fatalf("page 0 len = %d, want 1", len(p0))
	}
	p1, _, err := svc.ListTags(TagFilter{Prefix: "page_tag_", Sort: "name", Limit: 1, PageIndex: 1, ShowZero: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 1 {
		t.Fatalf("page 1 len = %d, want 1", len(p1))
	}
	if p0[0].Name == p1[0].Name {
		t.Errorf("page 0 and page 1 returned the same tag %q; pagination is broken", p0[0].Name)
	}
}

func TestGetTag_WithCanonical(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "canon_branch")

	tagAlias, _ := svc.GetOrCreateTag("alias_for_get", catID)
	tagCanon, _ := svc.GetOrCreateTag("canon_for_get", catID)
	if err := svc.AddTagToImage(imgID, tagAlias.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.MergeTags(tagAlias.ID, tagCanon.ID); err != nil {
		t.Fatal(err)
	}

	// GetTag on alias should have CanonicalTagID set
	got, err := svc.GetTag(tagAlias.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CanonicalTagID == nil {
		t.Error("expected CanonicalTagID to be set after merge")
	}
}

func TestValidateTagName_TooLong(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	long := strings.Repeat("a", 201)
	if _, err := svc.GetOrCreateTag(long, catID); err == nil {
		t.Error("expected error for tag name > 200 chars")
	}
	// A 200-char name is on the boundary and must still be accepted.
	if _, err := svc.GetOrCreateTag(strings.Repeat("b", 200), catID); err != nil {
		t.Errorf("200-char name should be accepted, got %v", err)
	}
}

func TestValidateTagName_PunctuationOnly(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	_, err := svc.GetOrCreateTag("---", catID)
	if err == nil {
		t.Error("expected error for punctuation-only tag name")
	}
}

func TestValidateTagName_AllowsColon(t *testing.T) {
	// Names like `:3` and `nier:automata` must round-trip through the
	// validator. The colon doubles as the category:tag separator at
	// input-parse time, but that's resolved before names reach here.
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	for _, name := range []string{":3", "nier:automata"} {
		if _, err := svc.GetOrCreateTag(name, catID); err != nil {
			t.Errorf("GetOrCreateTag(%q) error: %v", name, err)
		}
	}

	// All-punctuation (colons and hyphens only) is still rejected by the
	// "must contain a letter or digit" rule.
	if _, err := svc.GetOrCreateTag("::-:", catID); err == nil {
		t.Error("expected all-punctuation name to be rejected even with colon allowed")
	}
}

func TestListTags_SortByName(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	if _, err := svc.GetOrCreateTag("name_zzz", catID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetOrCreateTag("name_aaa", catID); err != nil {
		t.Fatal(err)
	}

	tags, _, err := svc.ListTags(TagFilter{Sort: "name", Prefix: "name_", Limit: 100, ShowZero: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) < 2 {
		t.Fatalf("expected >= 2 tags, got %d", len(tags))
	}
	if tags[0].Name > tags[1].Name {
		t.Errorf("tags not sorted by name: %s > %s", tags[0].Name, tags[1].Name)
	}
}

func TestGetTag_NotFound(t *testing.T) {
	_, svc := setupTestDB(t)
	_, err := svc.GetTag(999999)
	if err == nil {
		t.Error("expected error for non-existent tag ID")
	}
}

func TestSuggestTags_Empty(t *testing.T) {
	_, svc := setupTestDB(t)
	results, err := svc.SuggestTags("nonexistent_prefix_xyz", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 suggestions, got %d", len(results))
	}
}

func TestAddTagToImage_IsAuto(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "auto_conf")

	tag, _ := svc.GetOrCreateTag("auto_with_conf", catID)
	conf := 0.95
	if err := svc.AddTagToImage(imgID, tag.ID, true, &conf); err != nil {
		t.Fatal(err)
	}

	_, imgTags, err := svc.GetImageTags(imgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgTags) != 1 {
		t.Fatalf("expected 1 tag, got %d", len(imgTags))
	}
	if !imgTags[0].IsAuto {
		t.Error("expected IsAuto=true")
	}
	if imgTags[0].Confidence == nil || *imgTags[0].Confidence != 0.95 {
		t.Errorf("Confidence = %v, want 0.95", imgTags[0].Confidence)
	}
}

func TestDeleteCategory_NotFound(t *testing.T) {
	_, svc := setupTestDB(t)
	err := svc.DeleteCategoryMoveOrDelete(999999, "move", 0)
	if err != ErrCategoryNotFound {
		t.Errorf("expected ErrCategoryNotFound, got %v", err)
	}
}

func TestCreateCategory_Duplicate(t *testing.T) {
	_, svc := setupTestDB(t)
	_, err := svc.CreateCategory("dup_cat", "#000000")
	if err != nil {
		t.Fatal(err)
	}
	// Second create with same name should error (UNIQUE constraint)
	_, err = svc.CreateCategory("dup_cat", "#111111")
	if err == nil {
		t.Error("expected error for duplicate category name")
	}
}

func TestChangeTagCategory_RejectsDuplicateInTarget(t *testing.T) {
	_, svc := setupTestDB(t)
	cats, _ := svc.ListCategories()
	var generalID, characterID int64
	for _, c := range cats {
		switch c.Name {
		case "general":
			generalID = c.ID
		case "character":
			characterID = c.ID
		}
	}

	a, _ := svc.GetOrCreateTag("cat", generalID)
	if _, err := svc.GetOrCreateTag("cat", characterID); err != nil {
		t.Fatalf("seed character:cat: %v", err)
	}

	err := svc.ChangeTagCategory(a.ID, characterID)
	if err == nil {
		t.Fatal("expected error when moving into a category that already has the same name")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error message = %q, want 'already exists'", err.Error())
	}

	// Tag must not have moved.
	got, _ := svc.GetTag(a.ID)
	if got.CategoryID != generalID {
		t.Errorf("CategoryID = %d, want %d (tag should be unchanged on rejection)", got.CategoryID, generalID)
	}
}

func TestChangeTagCategory_SameCategoryNoop(t *testing.T) {
	_, svc := setupTestDB(t)
	generalID := generalCategoryID(t, svc)
	a, _ := svc.GetOrCreateTag("cute", generalID)
	if err := svc.ChangeTagCategory(a.ID, generalID); err != nil {
		t.Errorf("expected no error moving to same category, got %v", err)
	}
}

// ratingTagID returns the seeded rating tag id for one of the four
// canonical names.
func ratingTagIDByName(t *testing.T, database *db.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := database.Read.QueryRow(
		`SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.name = ?`, name,
	).Scan(&id); err != nil {
		t.Fatalf("rating tag %q not seeded: %v", name, err)
	}
	return id
}

func TestGetOrCreateTag_RejectsNonCanonicalRating(t *testing.T) {
	_, svc := setupTestDB(t)
	if svc.RatingCategoryID() == 0 {
		t.Fatal("rating category not seeded")
	}
	if _, err := svc.GetOrCreateTag("ambiguous", svc.RatingCategoryID()); err != ErrNonCanonicalRating {
		t.Errorf("err = %v, want ErrNonCanonicalRating", err)
	}
	// Canonical name is allowed and resolves to the seeded row.
	tag, err := svc.GetOrCreateTag("explicit", svc.RatingCategoryID())
	if err != nil {
		t.Fatalf("canonical name should be allowed: %v", err)
	}
	if tag.CategoryID != svc.RatingCategoryID() {
		t.Errorf("tag.CategoryID = %d, want %d", tag.CategoryID, svc.RatingCategoryID())
	}
}

func TestRenameTag_RejectedOnRating(t *testing.T) {
	database, svc := setupTestDB(t)
	id := ratingTagIDByName(t, database, "explicit")
	if err := svc.RenameTag(id, "very_explicit"); err != ErrRatingTagImmutable {
		t.Errorf("err = %v, want ErrRatingTagImmutable", err)
	}
}

func TestDeleteTag_RatingStripsUsageButKeepsRow(t *testing.T) {
	database, svc := setupTestDB(t)
	ratingID := ratingTagIDByName(t, database, "general")
	// Seed an image that carries the rating tag so DeleteTag has rows to strip.
	imageID := insertTestImage(t, database, "rated.png")
	if err := svc.AddTagToImage(imageID, ratingID, false, nil); err != nil {
		t.Fatalf("seed AddTagToImage: %v", err)
	}
	if err := svc.DeleteTag(ratingID); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}
	// The catalog row stays (immutable rating vocabulary).
	if _, err := svc.GetTag(ratingID); err != nil {
		t.Errorf("rating tag row should still exist, got err: %v", err)
	}
	// Every image_tags row for it is gone and usage_count is zeroed.
	_, tags, err := svc.GetImageTags(imageID)
	if err != nil {
		t.Fatalf("GetImageTags: %v", err)
	}
	for _, tg := range tags {
		if tg.TagID == ratingID {
			t.Errorf("rating row still on image %d", imageID)
		}
	}
	tag, _ := svc.GetTag(ratingID)
	if tag.UsageCount != 0 {
		t.Errorf("usage_count = %d, want 0", tag.UsageCount)
	}
}

func TestMergeTags_RejectedOnRating(t *testing.T) {
	database, svc := setupTestDB(t)
	gen := ratingTagIDByName(t, database, "general")
	exp := ratingTagIDByName(t, database, "explicit")
	if err := svc.MergeTags(gen, exp); err != ErrRatingTagImmutable {
		t.Errorf("err = %v, want ErrRatingTagImmutable", err)
	}
	// Merging a non-rating tag into a rating tag is also refused.
	other, _ := svc.GetOrCreateTag("ordinary", generalCategoryID(t, svc))
	if err := svc.MergeTags(other.ID, exp); err != ErrRatingTagImmutable {
		t.Errorf("merge into rating canonical: err = %v, want ErrRatingTagImmutable", err)
	}
}

func TestChangeTagCategory_RejectedOnRating(t *testing.T) {
	database, svc := setupTestDB(t)
	expID := ratingTagIDByName(t, database, "explicit")
	if err := svc.ChangeTagCategory(expID, generalCategoryID(t, svc)); err != ErrRatingTagImmutable {
		t.Errorf("moving rating tag out: err = %v, want ErrRatingTagImmutable", err)
	}
	other, _ := svc.GetOrCreateTag("not_a_rating", generalCategoryID(t, svc))
	if err := svc.ChangeTagCategory(other.ID, svc.RatingCategoryID()); err != ErrRatingTagImmutable {
		t.Errorf("moving in to rating: err = %v, want ErrRatingTagImmutable", err)
	}
}

// Merging a parent tag must move its tag_implications onto the canonical
// so a later removal of the canonical from an image doesn't leave the
// formerly-implied row orphaned with no parent justifying it.
func TestMergeTags_RepointsImplicationsAndKeepsImpliedRowsCleanable(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "merge_implication_repoint")

	a, _ := svc.GetOrCreateTag("merge_a", catID)
	b, _ := svc.GetOrCreateTag("merge_b", catID)
	c, _ := svc.GetOrCreateTag("merge_c", catID)

	if _, err := svc.AddImplication(a.ID, c.ID); err != nil {
		t.Fatalf("AddImplication: %v", err)
	}
	if err := svc.AddTagToImage(imgID, a.ID, false, nil); err != nil {
		t.Fatalf("AddTagToImage: %v", err)
	}

	if err := svc.MergeTags(a.ID, b.ID); err != nil {
		t.Fatalf("MergeTags: %v", err)
	}

	var parent, implied int64
	if err := database.Read.QueryRow(
		`SELECT parent_tag_id, implied_tag_id FROM tag_implications WHERE implied_tag_id = ?`, c.ID,
	).Scan(&parent, &implied); err != nil {
		t.Fatalf("expected one implication after merge: %v", err)
	}
	if parent != b.ID {
		t.Errorf("implication parent = %d, want %d (canonical)", parent, b.ID)
	}

	var aliasEdges int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM tag_implications WHERE parent_tag_id = ? OR implied_tag_id = ?`, a.ID, a.ID,
	).Scan(&aliasEdges); err != nil {
		t.Fatal(err)
	}
	if aliasEdges != 0 {
		t.Errorf("alias still referenced by %d tag_implications row(s)", aliasEdges)
	}

	if err := svc.RemoveTagFromImage(imgID, b.ID); err != nil {
		t.Fatalf("RemoveTagFromImage: %v", err)
	}

	_, imgTags, err := svc.GetImageTags(imgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgTags) != 0 {
		t.Errorf("image still carries %d tag(s) after removing the only user tag: %+v", len(imgTags), imgTags)
	}
}

// A `_` in a /tags or API query prefix must match literally, not as a LIKE
// wildcard. Before escaping, prefix "a_b" also matched "axb".
func TestListTags_PrefixEscapesLikeWildcards(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	if _, err := svc.GetOrCreateTag("a_b", catID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetOrCreateTag("axb", catID); err != nil {
		t.Fatal(err)
	}
	tags, _, err := svc.ListTags(TagFilter{Prefix: "a_b", Limit: 50, ShowZero: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Name != "a_b" {
		names := make([]string, len(tags))
		for i, tg := range tags {
			names[i] = tg.Name
		}
		t.Errorf("prefix a_b matched %v, want only [a_b]", names)
	}
}

// delete_all on a category must sweep the implied closure like DeleteTag,
// so an implied child in a surviving category isn't orphaned when its only
// parent (in the deleted category) goes away.
func TestDeleteCategoryDeleteAll_SweepsImpliedClosure(t *testing.T) {
	database, svc := setupTestDB(t)
	genID := generalCategoryID(t, svc)
	cat, err := svc.CreateCategory("doomed", "#aabbcc")
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	imgID := insertTestImage(t, database, "delall_closure")

	parent, _ := svc.GetOrCreateTag("doomed_parent", cat.ID)
	child, _ := svc.GetOrCreateTag("surviving_child", genID)
	if _, err := svc.AddImplication(parent.ID, child.ID); err != nil {
		t.Fatalf("AddImplication: %v", err)
	}
	if err := svc.AddTagToImage(imgID, parent.ID, false, nil); err != nil {
		t.Fatalf("AddTagToImage: %v", err)
	}
	var implied int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM image_tags WHERE image_id = ? AND tag_id = ? AND is_implied = 1`, imgID, child.ID,
	).Scan(&implied); err != nil {
		t.Fatal(err)
	}
	if implied != 1 {
		t.Fatalf("setup: implied child row = %d, want 1", implied)
	}

	if err := svc.DeleteCategoryMoveOrDelete(cat.ID, "delete_all", 0); err != nil {
		t.Fatalf("DeleteCategoryMoveOrDelete: %v", err)
	}

	var rows int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM image_tags WHERE image_id = ? AND tag_id = ?`, imgID, child.ID,
	).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("orphaned implied child row remains: %d", rows)
	}
	var usage int
	if err := database.Read.QueryRow(`SELECT usage_count FROM tags WHERE id = ?`, child.ID).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if usage != 0 {
		t.Errorf("child usage_count = %d, want 0", usage)
	}
}

// TestMergeTags_FansCanonicalImplicationsOntoMigratedRows: an image
// previously tagged only with the alias must, after the merge, carry
// both the canonical AND the canonical's declared implied children.
// Without the fan-out the canonical lands but its implied closure is
// silently absent.
func TestMergeTags_FansCanonicalImplicationsOntoMigratedRows(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "merge_implication_fanout")

	canonical, _ := svc.GetOrCreateTag("canon", catID)
	implied, _ := svc.GetOrCreateTag("imp_child", catID)
	alias, _ := svc.GetOrCreateTag("alias_src", catID)

	if _, err := svc.AddImplication(canonical.ID, implied.ID); err != nil {
		t.Fatalf("AddImplication: %v", err)
	}
	if err := svc.AddTagToImage(imgID, alias.ID, false, nil); err != nil {
		t.Fatalf("AddTagToImage(alias): %v", err)
	}

	if err := svc.MergeTags(alias.ID, canonical.ID); err != nil {
		t.Fatalf("MergeTags: %v", err)
	}

	_, imgTags, err := svc.GetImageTags(imgID)
	if err != nil {
		t.Fatal(err)
	}
	carried := map[int64]bool{}
	for _, t := range imgTags {
		carried[t.TagID] = true
	}
	if !carried[canonical.ID] {
		t.Errorf("canonical tag missing from image after merge: %+v", imgTags)
	}
	if !carried[implied.ID] {
		t.Errorf("canonical's implied child missing from image after merge: %+v", imgTags)
	}
}

// Merging a tag whose name is also the implied side of an edge must move
// the inbound edge onto the canonical so future fan-outs don't insert
// an alias as is_implied=1.
func TestMergeTags_RepointsInboundImplications(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	parent, _ := svc.GetOrCreateTag("merge_in_parent", catID)
	a, _ := svc.GetOrCreateTag("merge_in_a", catID)
	b, _ := svc.GetOrCreateTag("merge_in_b", catID)

	if _, err := svc.AddImplication(parent.ID, a.ID); err != nil {
		t.Fatalf("AddImplication: %v", err)
	}
	if err := svc.MergeTags(a.ID, b.ID); err != nil {
		t.Fatalf("MergeTags: %v", err)
	}

	var implied int64
	if err := database.Read.QueryRow(
		`SELECT implied_tag_id FROM tag_implications WHERE parent_tag_id = ?`, parent.ID,
	).Scan(&implied); err != nil {
		t.Fatalf("expected one implication after merge: %v", err)
	}
	if implied != b.ID {
		t.Errorf("implied = %d, want %d (canonical)", implied, b.ID)
	}
	var aliasEdges int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM tag_implications WHERE parent_tag_id = ? OR implied_tag_id = ?`, a.ID, a.ID,
	).Scan(&aliasEdges); err != nil {
		t.Fatal(err)
	}
	if aliasEdges != 0 {
		t.Errorf("alias still referenced by %d tag_implications row(s)", aliasEdges)
	}
}

// Merging an alias whose canonical is already on the image as is_implied=1
// must promote the canonical row to user-owned. The common trigger is
// "user adds A which implies B, then merges A into B": without the
// promotion the row-move loop just deletes the alias side and leaves
// the image carrying only an implied B with no user tag.
func TestMergeTags_PromotesCanonicalImpliedRow(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "merge_promote_implied")

	a, _ := svc.GetOrCreateTag("merge_promote_a", catID)
	c, _ := svc.GetOrCreateTag("merge_promote_c", catID)

	if _, err := svc.AddImplication(a.ID, c.ID); err != nil {
		t.Fatalf("AddImplication: %v", err)
	}
	if err := svc.AddTagToImage(imgID, a.ID, false, nil); err != nil {
		t.Fatalf("AddTagToImage: %v", err)
	}
	if err := svc.MergeTags(a.ID, c.ID); err != nil {
		t.Fatalf("MergeTags: %v", err)
	}

	var isImplied, isAuto int
	if err := database.Read.QueryRow(
		`SELECT is_implied, is_auto FROM image_tags WHERE image_id = ? AND tag_id = ?`,
		imgID, c.ID,
	).Scan(&isImplied, &isAuto); err != nil {
		t.Fatalf("expected canonical on image: %v", err)
	}
	if isImplied != 0 {
		t.Errorf("canonical row is_implied = %d, want 0 (user-owned)", isImplied)
	}
	if isAuto != 0 {
		t.Errorf("canonical row is_auto = %d, want 0 (matches alias side)", isAuto)
	}
}

// Deleting a parent tag must sweep its implied closure on every carrier
// image. The image_tags FK cascade alone drops the parent row but
// leaves the rows it implied with is_implied=1 and no on-image parent
// justifying them.
func TestDeleteTag_SweepsImpliedClosureOnCarrierImages(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "delete_implied_sweep")

	parent, _ := svc.GetOrCreateTag("delete_parent", catID)
	implied, _ := svc.GetOrCreateTag("delete_implied", catID)

	if _, err := svc.AddImplication(parent.ID, implied.ID); err != nil {
		t.Fatalf("AddImplication: %v", err)
	}
	if err := svc.AddTagToImage(imgID, parent.ID, false, nil); err != nil {
		t.Fatalf("AddTagToImage: %v", err)
	}

	if err := svc.DeleteTag(parent.ID); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}

	_, imgTags, err := svc.GetImageTags(imgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgTags) != 0 {
		t.Errorf("image still carries %d tag(s) after deleting the only parent: %+v", len(imgTags), imgTags)
	}

	got, err := svc.GetTag(implied.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsageCount != 0 {
		t.Errorf("implied tag usage_count = %d, want 0", got.UsageCount)
	}
}

// The alias sweep in deleteTagsTx must survive alias -> alias references.
// Current write paths refuse to create them, but raw DB imports and older
// versions can carry them, and a one-level sweep then trips the
// canonical_tag_id FK: a chain leaves a grandchild dangling, a cycle
// leaves the deleted tag's own pointer dangling.
func TestDeleteTag_AliasChainAndCycle(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	insertAlias := func(name string, canonical any) int64 {
		t.Helper()
		var id int64
		err := database.Write.QueryRow(
			`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id, usage_count, origin)
			 VALUES (?, ?, 1, ?, 0, 'user') RETURNING id`,
			name, catID, canonical,
		).Scan(&id)
		if err != nil {
			t.Fatalf("insert alias %s: %v", name, err)
		}
		return id
	}
	tagCount := func(name string) int {
		t.Helper()
		var n int
		if err := database.Read.QueryRow(`SELECT COUNT(*) FROM tags WHERE name = ?`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Chain: chain_d -> chain_b -> chain_a -> chain_canon.
	canon, _ := svc.GetOrCreateTag("chain_canon", catID)
	aliasA, err := svc.CreateAlias("chain_a", catID, canon.ID)
	if err != nil {
		t.Fatalf("CreateAlias: %v", err)
	}
	bID := insertAlias("chain_b", aliasA.ID)
	insertAlias("chain_d", bID)

	if err := svc.DeleteTag(aliasA.ID); err != nil {
		t.Fatalf("DeleteTag on the chained alias: %v", err)
	}
	for _, name := range []string{"chain_a", "chain_b", "chain_d"} {
		if n := tagCount(name); n != 0 {
			t.Errorf("%s still present after delete", name)
		}
	}
	if n := tagCount("chain_canon"); n != 1 {
		t.Errorf("chain_canon count = %d, want 1", n)
	}

	// Cycle: cycle_a <-> cycle_b.
	cycleA := insertAlias("cycle_a", nil)
	cycleB := insertAlias("cycle_b", cycleA)
	if _, err := database.Write.Exec(`UPDATE tags SET canonical_tag_id = ? WHERE id = ?`, cycleB, cycleA); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteTag(cycleA); err != nil {
		t.Fatalf("DeleteTag on the alias cycle: %v", err)
	}
	for _, name := range []string{"cycle_a", "cycle_b"} {
		if n := tagCount(name); n != 0 {
			t.Errorf("%s still present after delete", name)
		}
	}
}

// CreateAlias's upgrade-in-place branch (zero-usage tag becomes an alias)
// must move tag_implications off the existing row for the same reason
// MergeTags does: AddImplication refuses aliases, so any dangling
// alias-keyed edge would only ever fire from a tag the resolver no
// longer exposes.
func TestCreateAlias_UpgradeInPlace_RepointsImplications(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	a, _ := svc.GetOrCreateTag("alias_up_a", catID)
	b, _ := svc.GetOrCreateTag("alias_up_b", catID)
	c, _ := svc.GetOrCreateTag("alias_up_c", catID)

	if _, err := svc.AddImplication(a.ID, c.ID); err != nil {
		t.Fatalf("AddImplication: %v", err)
	}

	if _, err := svc.CreateAlias("alias_up_a", catID, b.ID); err != nil {
		t.Fatalf("CreateAlias: %v", err)
	}

	var parent, implied int64
	if err := database.Read.QueryRow(
		`SELECT parent_tag_id, implied_tag_id FROM tag_implications WHERE implied_tag_id = ?`, c.ID,
	).Scan(&parent, &implied); err != nil {
		t.Fatalf("expected one implication after alias: %v", err)
	}
	if parent != b.ID {
		t.Errorf("implication parent = %d, want %d (canonical)", parent, b.ID)
	}
	var aliasEdges int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM tag_implications WHERE parent_tag_id = ? OR implied_tag_id = ?`, a.ID, a.ID,
	).Scan(&aliasEdges); err != nil {
		t.Fatal(err)
	}
	if aliasEdges != 0 {
		t.Errorf("alias still referenced by %d tag_implications row(s)", aliasEdges)
	}
}

func TestRatingTagIDsAbove(t *testing.T) {
	_, svc := setupTestDB(t)
	if got := svc.RatingTagIDsAbove("explicit"); len(got) != 0 {
		t.Errorf("explicit ceiling: got %d ids, want 0", len(got))
	}
	if got := svc.RatingTagIDsAbove("general"); len(got) != 3 {
		t.Errorf("general ceiling: got %d ids, want 3 (sensitive/questionable/explicit)", len(got))
	}
	if got := svc.RatingTagIDsAbove("sensitive"); len(got) != 2 {
		t.Errorf("sensitive ceiling: got %d ids, want 2 (questionable/explicit)", len(got))
	}
	if got := svc.RatingTagIDsAbove(""); len(got) != 0 {
		t.Errorf("empty ceiling: got %d ids, want 0", len(got))
	}
}

// TestAddRating_PrunesLowerRanksOnAdd pins highest-rank-wins on the
// manual add path: the four canonical levels are added in ascending
// order, and after each step only the highest one survives on the image.
func TestAddRating_PrunesLowerRanksOnAdd(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID := insertTestImage(t, database, "ratings.png")

	imageRatingNames := func() []string {
		t.Helper()
		rows, err := database.Read.Query(
			`SELECT t.name FROM image_tags it
			 JOIN tags t ON t.id = it.tag_id
			 WHERE it.image_id = ? AND t.category_id = ?
			 ORDER BY t.name`,
			imageID, svc.RatingCategoryID(),
		)
		if err != nil {
			t.Fatalf("query rating names: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return out
	}

	for _, level := range []string{"general", "sensitive", "questionable", "explicit"} {
		id := ratingTagIDByName(t, database, level)
		if err := svc.AddTagToImage(imageID, id, true, nil); err != nil {
			t.Fatalf("AddTagToImage(%s): %v", level, err)
		}
		got := imageRatingNames()
		if len(got) != 1 || got[0] != level {
			t.Fatalf("after adding %s: image carries %v, want only [%s]", level, got, level)
		}
	}

	// Adding a lower-rank rating to an image that already carries a
	// higher one is a no-op: the prune drops the freshly-inserted lower
	// row before commit so the higher rank wins.
	genID := ratingTagIDByName(t, database, "general")
	if err := svc.AddTagToImage(imageID, genID, true, nil); err != nil {
		t.Fatalf("AddTagToImage(general after explicit): %v", err)
	}
	got := imageRatingNames()
	if len(got) != 1 || got[0] != "explicit" {
		t.Fatalf("lower-rank add should be pruned; image carries %v, want only [explicit]", got)
	}
}

// TestAddRating_UsageCountsTrackPrune covers the usage_count side-effect
// of the prune: when a higher rating displaces a lower one, the lower
// tag's usage_count decrements (mirroring the remove path) so RecalcDB
// stays a no-op.
func TestAddRating_UsageCountsTrackPrune(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID := insertTestImage(t, database, "ratings_usage.png")

	genID := ratingTagIDByName(t, database, "general")
	expID := ratingTagIDByName(t, database, "explicit")

	if err := svc.AddTagToImage(imageID, genID, true, nil); err != nil {
		t.Fatal(err)
	}
	gen, _ := svc.GetTag(genID)
	if gen.UsageCount != 1 {
		t.Fatalf("general usage_count = %d after seed, want 1", gen.UsageCount)
	}

	if err := svc.AddTagToImage(imageID, expID, true, nil); err != nil {
		t.Fatal(err)
	}
	gen, _ = svc.GetTag(genID)
	if gen.UsageCount != 0 {
		t.Errorf("general usage_count = %d after prune, want 0", gen.UsageCount)
	}
	exp, _ := svc.GetTag(expID)
	if exp.UsageCount != 1 {
		t.Errorf("explicit usage_count = %d, want 1", exp.UsageCount)
	}
}

// TestAddRating_ManualOverwritesPriorLevel pins the manual-add rule:
// the user's typed rating wins over whatever is already attached, even
// when its rank is below an existing auto-tagger value. The auto-tagger
// path (TestAddRating_PrunesLowerRanksOnAdd) keeps highest-wins.
func TestAddRating_ManualOverwritesPriorLevel(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID := insertTestImage(t, database, "ratings_manual.png")

	imageRatingNames := func() []string {
		t.Helper()
		rows, err := database.Read.Query(
			`SELECT t.name FROM image_tags it
			 JOIN tags t ON t.id = it.tag_id
			 WHERE it.image_id = ? AND t.category_id = ?
			 ORDER BY t.name`,
			imageID, svc.RatingCategoryID(),
		)
		if err != nil {
			t.Fatalf("query rating names: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return out
	}

	// Seed via the auto-tagger entry so the image starts with `explicit`.
	expID := ratingTagIDByName(t, database, "explicit")
	conf := 0.9
	if err := svc.AddTagToImage(imageID, expID, true, &conf); err != nil {
		t.Fatalf("AddTagToImage(explicit, isAuto=true): %v", err)
	}

	// Manual add of `general` (lower rank) must overwrite, not be swept.
	genID := ratingTagIDByName(t, database, "general")
	if err := svc.AddTagToImage(imageID, genID, false, nil); err != nil {
		t.Fatalf("AddTagToImage(general, isAuto=false): %v", err)
	}
	got := imageRatingNames()
	if len(got) != 1 || got[0] != "general" {
		t.Fatalf("manual general after auto explicit: image carries %v, want only [general]", got)
	}
	if exp, _ := svc.GetTag(expID); exp.UsageCount != 0 {
		t.Errorf("explicit usage_count = %d after manual overwrite, want 0", exp.UsageCount)
	}
	if gen, _ := svc.GetTag(genID); gen.UsageCount != 1 {
		t.Errorf("general usage_count = %d after manual overwrite, want 1", gen.UsageCount)
	}

	// Manual add of `sensitive` (higher than general, lower than the
	// pruned explicit) must also overwrite cleanly.
	senID := ratingTagIDByName(t, database, "sensitive")
	if err := svc.AddTagToImage(imageID, senID, false, nil); err != nil {
		t.Fatalf("AddTagToImage(sensitive, isAuto=false): %v", err)
	}
	got = imageRatingNames()
	if len(got) != 1 || got[0] != "sensitive" {
		t.Fatalf("manual sensitive after manual general: image carries %v, want only [sensitive]", got)
	}
}

// TestAddImplication_RejectsCycle: ErrImplicationCycle fires when a
// new edge would close a loop through the existing graph. Without
// this guard the depth-bound walk runs indefinitely against a cyclic
// graph and the implied closure becomes ambiguous.
func TestAddImplication_RejectsCycle(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	a, _ := svc.GetOrCreateTag("cyc_a", catID)
	b, _ := svc.GetOrCreateTag("cyc_b", catID)
	c, _ := svc.GetOrCreateTag("cyc_c", catID)

	if _, err := svc.AddImplication(a.ID, b.ID); err != nil {
		t.Fatalf("a→b: %v", err)
	}
	if _, err := svc.AddImplication(b.ID, c.ID); err != nil {
		t.Fatalf("b→c: %v", err)
	}
	// c→a closes the loop.
	if _, err := svc.AddImplication(c.ID, a.ID); err != ErrImplicationCycle {
		t.Errorf("c→a after a→b→c expected ErrImplicationCycle, got %v", err)
	}
}

// TestAddImplication_RejectsSelf pins the self-edge guard (a different
// branch from cycle detection: parent == implied returns earlier).
func TestAddImplication_RejectsSelf(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	a, _ := svc.GetOrCreateTag("self_loop", catID)

	if _, err := svc.AddImplication(a.ID, a.ID); err == nil {
		t.Errorf("AddImplication(a, a) should fail; got nil")
	}
}

// TestAddImplication_HonoursDepthBound pins MaxImplicationDepth: a
// chain of edges right at the bound succeeds; an edge that would
// close a cycle inside the bound is refused.
func TestAddImplication_HonoursDepthBound(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	const chainLen = 6
	tags := make([]int64, chainLen)
	for i := 0; i < chainLen; i++ {
		tag, err := svc.GetOrCreateTag(fmt.Sprintf("depth_%d", i), catID)
		if err != nil {
			t.Fatalf("create tag %d: %v", i, err)
		}
		tags[i] = tag.ID
	}
	for i := 0; i < chainLen-1; i++ {
		if _, err := svc.AddImplication(tags[i], tags[i+1]); err != nil {
			t.Fatalf("chain edge %d→%d: %v", i, i+1, err)
		}
	}
	// Closing the chain should be refused even at small chain length
	// because the cycle walk traces through the whole chain back to
	// the starting node within the depth bound.
	if _, err := svc.AddImplication(tags[chainLen-1], tags[0]); err != ErrImplicationCycle {
		t.Errorf("close chain expected ErrImplicationCycle, got %v", err)
	}
}

// TestAddImplication_RatingImpliedPrunesLower: an implication whose
// implied side is a rating tag must trigger the rating-prune sweep
// when fan-out lands the implied row, otherwise the image carries
// multiple rating rows and breaks the highest-wins invariant the
// executor's fast counts rely on.
func TestAddImplication_RatingImpliedPrunesLower(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imageID := insertTestImage(t, database, "rating_imply.png")

	parent, _ := svc.GetOrCreateTag("hardcore_parent", catID)
	expRatingID := ratingTagIDByName(t, database, "explicit")

	// Pre-existing lower rating on the image.
	if err := svc.AddTagToImage(imageID, ratingTagIDByName(t, database, "general"), false, nil); err != nil {
		t.Fatalf("seed general rating: %v", err)
	}

	// Declare parent → explicit. The fan-out should land explicit and
	// the prune should sweep general.
	if _, err := svc.AddImplication(parent.ID, expRatingID); err != nil {
		t.Fatalf("AddImplication: %v", err)
	}
	if err := svc.AddTagToImage(imageID, parent.ID, false, nil); err != nil {
		t.Fatalf("AddTagToImage(parent): %v", err)
	}

	rows, err := database.Read.Query(
		`SELECT t.name FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.category_id = ?`,
		imageID, svc.RatingCategoryID(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ratings []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		ratings = append(ratings, n)
	}
	if len(ratings) != 1 || ratings[0] != "explicit" {
		t.Errorf("post-fanout image carries ratings %v, want only [explicit]", ratings)
	}
}

// TestAddRating_ManualReportsDisplacedNames: callers (the
// detail-page tag input) need to surface "replaced rating:X" inline
// when a manual rating add sweeps a prior row.
func TestAddRating_ManualReportsDisplacedNames(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID := insertTestImage(t, database, "displaced.png")

	expID := ratingTagIDByName(t, database, "explicit")
	conf := 0.9
	if err := svc.AddTagToImage(imageID, expID, true, &conf); err != nil {
		t.Fatalf("seed explicit: %v", err)
	}

	genID := ratingTagIDByName(t, database, "general")
	res, err := svc.AddTagToImageReportingDup(imageID, genID, false, nil, "")
	if err != nil {
		t.Fatalf("AddTagToImageReportingDup: %v", err)
	}
	if !res.Added {
		t.Errorf("expected Added=true on a fresh manual rating, got %+v", res)
	}
	if got, want := res.DisplacedRatings, []string{"explicit"}; !reflect.DeepEqual(got, want) {
		t.Errorf("DisplacedRatings = %v, want %v", got, want)
	}

	// Re-adding the same rating shouldn't claim it displaced anything.
	res2, err := svc.AddTagToImageReportingDup(imageID, genID, false, nil, "")
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if len(res2.DisplacedRatings) != 0 {
		t.Errorf("DisplacedRatings on no-op re-add = %v, want empty", res2.DisplacedRatings)
	}
}

// TestAddNonRating_DoesNotTriggerPrune verifies the cheap fast path:
// a non-rating add does not run the prune query, even on an image that
// already carries multiple rating tags from prior (legacy) state.
func TestAddNonRating_DoesNotTriggerPrune(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID := insertTestImage(t, database, "non_rating.png")

	// Seed two rating rows directly so we can verify the prune is NOT
	// fired by an unrelated add. Bypassing AddTagToImage skips the rule.
	tx, err := database.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sensitive", "questionable"} {
		id := ratingTagIDByName(t, database, name)
		if _, err := tx.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto, is_implied) VALUES (?, ?, 0, 0)`,
			imageID, id,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// A non-rating add must not opportunistically clean up.
	catID := generalCategoryID(t, svc)
	tag, err := svc.GetOrCreateTag("scenery", catID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(imageID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	var ratingCount int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM image_tags it JOIN tags t ON t.id = it.tag_id WHERE it.image_id = ? AND t.category_id = ?`,
		imageID, svc.RatingCategoryID(),
	).Scan(&ratingCount); err != nil {
		t.Fatal(err)
	}
	if ratingCount != 2 {
		t.Errorf("rating rows after non-rating add = %d, want 2 (legacy multi-rating left untouched)", ratingCount)
	}
}

func TestGetOrCreateTagFrom_StampsOriginOnInsertOnly(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	created, err := svc.GetOrCreateTagFrom("provenance_tag", catID, "danbooru")
	if err != nil {
		t.Fatal(err)
	}
	if created.Origin != "danbooru" {
		t.Errorf("fresh tag origin = %q, want danbooru", created.Origin)
	}

	// A later get through another creator must not relabel the row.
	again, err := svc.GetOrCreateTagFrom("provenance_tag", catID, "ptr")
	if err != nil {
		t.Fatal(err)
	}
	full, err := svc.GetTag(again.ID)
	if err != nil {
		t.Fatal(err)
	}
	if full.Origin != "danbooru" {
		t.Errorf("origin after second get = %q, want danbooru", full.Origin)
	}

	plain, err := svc.GetOrCreateTag("ui_tag", catID)
	if err != nil {
		t.Fatal(err)
	}
	full, err = svc.GetTag(plain.ID)
	if err != nil {
		t.Fatal(err)
	}
	if full.Origin != "user" {
		t.Errorf("GetOrCreateTag origin = %q, want user", full.Origin)
	}
}

func TestCreateAliasFrom_StampsFreshInsertKeepsUpgrade(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	canon, err := svc.GetOrCreateTag("alias_canon", catID)
	if err != nil {
		t.Fatal(err)
	}

	fresh, err := svc.CreateAliasFrom("swept_alias", catID, canon.ID, "ptr")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Origin != "ptr" {
		t.Errorf("fresh alias origin = %q, want ptr", fresh.Origin)
	}

	// Upgrading an existing zero-usage tag in place keeps its creator.
	if _, err := svc.GetOrCreateTag("upgraded_alias", catID); err != nil {
		t.Fatal(err)
	}
	upgraded, err := svc.CreateAliasFrom("upgraded_alias", catID, canon.ID, "ptr")
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Origin != "user" {
		t.Errorf("upgraded alias origin = %q, want user (creator preserved)", upgraded.Origin)
	}
}

func TestAddImplicationFrom_StampsEdgeOrigin(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	parent, err := svc.GetOrCreateTag("imp_parent", catID)
	if err != nil {
		t.Fatal(err)
	}
	implied, err := svc.GetOrCreateTag("imp_child", catID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddImplicationFrom(parent.ID, implied.ID, "ptr"); err != nil {
		t.Fatal(err)
	}
	imps, err := svc.ListImplications(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(imps) != 1 || imps[0].Origin != "ptr" {
		t.Errorf("implication origin = %+v, want one edge with origin ptr", imps)
	}
}

func TestRenameTagKeepAlias(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "renamekeep1")
	tag, err := svc.GetOrCreateTagFrom("old_spelling", catID, "danbooru")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(imgID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	if err := svc.RenameTagKeepAlias(tag.ID, "new_spelling"); err != nil {
		t.Fatal(err)
	}
	renamed, err := svc.GetTag(tag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "new_spelling" || renamed.Origin != "danbooru" {
		t.Errorf("renamed tag = %+v, want new_spelling with its origin kept", renamed)
	}
	// The old spelling resolves to the renamed row on the next add.
	resolved, err := svc.GetOrCreateTag("old_spelling", catID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != tag.ID {
		t.Errorf("old spelling resolves to %d, want the renamed row %d", resolved.ID, tag.ID)
	}
	var aliasOrigin string
	var isAlias int
	if err := database.Read.QueryRow(
		`SELECT is_alias, origin FROM tags WHERE name = 'old_spelling'`,
	).Scan(&isAlias, &aliasOrigin); err != nil {
		t.Fatal(err)
	}
	if isAlias != 1 || aliasOrigin != "user" {
		t.Errorf("leftover row is_alias=%d origin=%q, want a user-origin alias", isAlias, aliasOrigin)
	}

	// An alias row refuses the keep (its leftover would chain aliases).
	var aliasID int64
	_ = database.Read.QueryRow(`SELECT id FROM tags WHERE name = 'old_spelling'`).Scan(&aliasID)
	if err := svc.RenameTagKeepAlias(aliasID, "third_spelling"); err == nil {
		t.Error("RenameTagKeepAlias on an alias row should refuse")
	}
}

func TestAddTagToImage_SetsLastUsedAt(t *testing.T) {
	database, svc := setupTestDB(t)
	imageID := insertTestImage(t, database, "lastused1")
	catID := generalCategoryID(t, svc)
	tag, err := svc.GetOrCreateTag("recently_used", catID)
	if err != nil {
		t.Fatal(err)
	}
	full, err := svc.GetTag(tag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !full.LastUsedAt.IsZero() {
		t.Errorf("never-applied tag LastUsedAt = %v, want zero", full.LastUsedAt)
	}
	if err := svc.AddTagToImage(imageID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	full, err = svc.GetTag(tag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if full.LastUsedAt.IsZero() {
		t.Error("LastUsedAt still zero after applying the tag to an image")
	}
}
