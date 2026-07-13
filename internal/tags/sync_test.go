package tags

import (
	"testing"

	"github.com/leqwin/monbooru/internal/db"
)

func imageHasTag(t *testing.T, database *db.DB, imgID, tagID int64) bool {
	t.Helper()
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ? AND tag_id = ?`, imgID, tagID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func imageTagTagger(t *testing.T, database *db.DB, imgID, tagID int64) string {
	t.Helper()
	var tn *string
	if err := database.Read.QueryRow(`SELECT tagger_name FROM image_tags WHERE image_id = ? AND tag_id = ?`, imgID, tagID).Scan(&tn); err != nil {
		t.Fatal(err)
	}
	if tn == nil {
		return ""
	}
	return *tn
}

func TestSyncSourceTags_ClonesSliceKeepingOtherTags(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "sync01")
	mk := func(name string) int64 { tg, _ := svc.GetOrCreateTag(name, catID); return tg.ID }
	alpha, beta, gamma, manual := mk("alpha"), mk("beta"), mk("gamma"), mk("manual_keep")

	if err := svc.AddTagToImage(imgID, manual, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagsToImageFromTagger(imgID, []int64{gamma}, false, "gelbooru"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncSourceTags(imgID, []int64{alpha, beta}, "danbooru", true); err != nil {
		t.Fatal(err)
	}
	if got := imageTagTagger(t, database, imgID, alpha); got != "danbooru" {
		t.Errorf("alpha tagger = %q, want danbooru", got)
	}

	// A second sync drops beta and re-lists gamma: gamma keeps gelbooru's
	// attribution (OR IGNORE), and danbooru's own beta is pruned.
	if _, err := svc.SyncSourceTags(imgID, []int64{alpha, gamma}, "danbooru", true); err != nil {
		t.Fatal(err)
	}
	if imageHasTag(t, database, imgID, beta) {
		t.Error("beta should be pruned from danbooru's slice")
	}
	if !imageHasTag(t, database, imgID, alpha) {
		t.Error("alpha should remain in danbooru's set")
	}
	if !imageHasTag(t, database, imgID, manual) {
		t.Error("manual tag must survive a source sync")
	}
	if got := imageTagTagger(t, database, imgID, gamma); got != "gelbooru" {
		t.Errorf("gamma tagger = %q, want gelbooru (not re-owned by danbooru)", got)
	}
}

func TestSyncSourceTags_ProtectsExistingRating(t *testing.T) {
	database, svc := setupTestDB(t)
	genID := generalCategoryID(t, svc)
	ratingCat := svc.RatingCategoryID()
	imgID := insertTestImage(t, database, "sync02")
	plain, _ := svc.GetOrCreateTag("scenery", genID)
	general, _ := svc.GetOrCreateTag("general", ratingCat)
	explicit, _ := svc.GetOrCreateTag("explicit", ratingCat)

	// Unrated: the sync fills the rating.
	r, err := svc.SyncSourceTags(imgID, []int64{plain.ID, general.ID}, "danbooru", true)
	if err != nil {
		t.Fatal(err)
	}
	if !r.RatingFilled {
		t.Error("expected RatingFilled on an unrated image")
	}
	if !imageHasTag(t, database, imgID, general.ID) {
		t.Error("general rating should be filled")
	}

	// Already rated: a stronger incoming rating must neither replace the
	// existing one nor be pruned when the source stops listing it.
	if _, err := svc.SyncSourceTags(imgID, []int64{plain.ID, explicit.ID}, "danbooru", true); err != nil {
		t.Fatal(err)
	}
	if imageHasTag(t, database, imgID, explicit.ID) {
		t.Error("explicit must not overwrite the existing rating")
	}
	if !imageHasTag(t, database, imgID, general.ID) {
		t.Error("existing general rating must be preserved")
	}
}

func TestSyncSourceTags_NeverReownsAutoOrImpliedRows(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "sync03")
	mk := func(name string) int64 { tg, _ := svc.GetOrCreateTag(name, catID); return tg.ID }
	autoTag, parent, implied := mk("long_hair"), mk("twin_braids"), mk("braid")

	conf := 0.93
	if err := svc.AddTagToImage(imgID, autoTag, true, &conf); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddImplication(parent, implied); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTagToImage(imgID, parent, false, nil); err != nil {
		t.Fatal(err)
	}

	rowState := func(tagID int64) (isAuto, isImplied int, confidence *float64) {
		t.Helper()
		if err := database.Read.QueryRow(
			`SELECT is_auto, is_implied, confidence FROM image_tags WHERE image_id = ? AND tag_id = ?`,
			imgID, tagID).Scan(&isAuto, &isImplied, &confidence); err != nil {
			t.Fatal(err)
		}
		return
	}

	// The source lists tags that overlap the auto row and the implied row.
	if _, err := svc.SyncSourceTags(imgID, []int64{autoTag, implied}, "danbooru", true); err != nil {
		t.Fatal(err)
	}
	if isAuto, _, confidence := rowState(autoTag); isAuto != 1 || confidence == nil {
		t.Errorf("auto row after sync: is_auto=%d confidence=%v, want the tagger's attribution kept", isAuto, confidence)
	}
	if _, isImplied, _ := rowState(implied); isImplied != 1 {
		t.Errorf("implied row after sync: is_implied=%d, want 1", isImplied)
	}

	// A refetch that no longer lists them must not prune rows the source
	// never owned.
	if _, err := svc.SyncSourceTags(imgID, nil, "danbooru", true); err != nil {
		t.Fatal(err)
	}
	if !imageHasTag(t, database, imgID, autoTag) {
		t.Error("auto-tagger row pruned by a source refetch")
	}
	if !imageHasTag(t, database, imgID, implied) {
		t.Error("implied row pruned by a source refetch")
	}
}
