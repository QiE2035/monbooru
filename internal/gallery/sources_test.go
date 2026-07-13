package gallery

import (
	"errors"
	"testing"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/models"
)

func scalarSource(t *testing.T, database *db.DB, id int64) (string, string) {
	t.Helper()
	var src, u string
	if err := database.Read.QueryRow(`SELECT source, url FROM images WHERE id = ?`, id).Scan(&src, &u); err != nil {
		t.Fatal(err)
	}
	return src, u
}

func TestSourcesForImage_OrderAndPrimaryMirror(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	if err := AddSourceMembership(database, id, "danbooru", "111", "https://danbooru/posts/111"); err != nil {
		t.Fatal(err)
	}
	if err := AddSourceMembership(database, id, "gelbooru", "222", "https://gelbooru/posts/222"); err != nil {
		t.Fatal(err)
	}
	srcs, err := SourcesForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 2 || srcs[0].Site != "danbooru" || srcs[1].Site != "gelbooru" {
		t.Fatalf("sources = %+v, want danbooru then gelbooru", srcs)
	}
	if s, u := scalarSource(t, database, id); s != "danbooru" || u != "https://danbooru/posts/111" {
		t.Errorf("mirror = (%q,%q), want the danbooru primary", s, u)
	}
}

func TestRemoveSourceMembership_RebindsAndClearsMirror(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "111", "https://d/111")
	_ = AddSourceMembership(database, id, "gelbooru", "222", "https://g/222")

	if err := RemoveSourceMembership(database, id, "danbooru", "111"); err != nil {
		t.Fatal(err)
	}
	if s, u := scalarSource(t, database, id); s != "gelbooru" || u != "https://g/222" {
		t.Errorf("after removing primary, mirror = (%q,%q), want gelbooru", s, u)
	}
	if err := RemoveSourceMembership(database, id, "gelbooru", "222"); err != nil {
		t.Fatal(err)
	}
	if s, u := scalarSource(t, database, id); s != "" || u != "" {
		t.Errorf("after removing all, mirror = (%q,%q), want empty", s, u)
	}
}

func TestAddSourceMembership_UpsertsURL(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "111", "https://old")
	if err := AddSourceMembership(database, id, "danbooru", "111", "https://new"); err != nil {
		t.Fatal(err)
	}
	srcs, _ := SourcesForImage(database, id)
	if len(srcs) != 1 || srcs[0].URL != "https://new" {
		t.Fatalf("sources = %+v, want a single row with the updated url", srcs)
	}
}

func TestSetPrimarySource_CreateEditClear(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	if err := SetPrimarySource(database, id, "pixiv", "https://pixiv/1"); err != nil {
		t.Fatal(err)
	}
	if s, u := scalarSource(t, database, id); s != "pixiv" || u != "https://pixiv/1" {
		t.Errorf("mirror = (%q,%q), want pixiv", s, u)
	}
	if err := SetPrimarySource(database, id, "twitter", "https://twitter/2"); err != nil {
		t.Fatal(err)
	}
	srcs, _ := SourcesForImage(database, id)
	if len(srcs) != 1 || srcs[0].Site != "twitter" {
		t.Fatalf("sources = %+v, want a single twitter origin", srcs)
	}
	if err := SetPrimarySource(database, id, "", ""); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := SourcesForImage(database, id); len(srcs) != 0 {
		t.Fatalf("sources = %+v, want none after clear", srcs)
	}
	if s, u := scalarSource(t, database, id); s != "" || u != "" {
		t.Errorf("mirror = (%q,%q), want empty after clear", s, u)
	}
}

func TestSetSourceCommentary_UpsertClearAndCreate(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "111", "https://d/111")

	if err := SetSourceCommentary(database, id, "danbooru", "111", "hello\nworld"); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := SourcesForImage(database, id); len(srcs) != 1 || srcs[0].Commentary != "hello\nworld" {
		t.Fatalf("sources = %+v, want commentary set", srcs)
	}

	// A re-pull overwrites the stored body.
	if err := SetSourceCommentary(database, id, "danbooru", "111", "new text"); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := SourcesForImage(database, id); srcs[0].Commentary != "new text" {
		t.Fatalf("commentary = %q, want overwritten", srcs[0].Commentary)
	}

	// Clearing leaves the origin in place.
	if err := SetSourceCommentary(database, id, "danbooru", "111", ""); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := SourcesForImage(database, id); len(srcs) != 1 || srcs[0].Commentary != "" {
		t.Fatalf("sources = %+v, want the origin kept with empty commentary", srcs)
	}

	// Clearing a label with no origin must not conjure one.
	if err := SetSourceCommentary(database, id, "pixiv", "", ""); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := SourcesForImage(database, id); len(srcs) != 1 {
		t.Fatalf("sources = %+v, want no phantom origin", srcs)
	}

	// Setting commentary on a fresh label adds an origin.
	if err := SetSourceCommentary(database, id, "pixiv", "", "art notes"); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := SourcesForImage(database, id); len(srcs) != 2 {
		t.Fatalf("sources = %+v, want a new pixiv origin", srcs)
	}
}

func TestSetSourceOriginal_UpsertClearAndCreate(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "111", "https://d/111")

	if err := SetSourceOriginal(database, id, "danbooru", "111", "https://pixiv/artworks/1"); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := SourcesForImage(database, id); len(srcs) != 1 || srcs[0].Original != "https://pixiv/artworks/1" {
		t.Fatalf("sources = %+v, want original set", srcs)
	}

	// A re-pull overwrites the stored value.
	if err := SetSourceOriginal(database, id, "danbooru", "111", "https://twitter/2"); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := SourcesForImage(database, id); srcs[0].Original != "https://twitter/2" {
		t.Fatalf("original = %q, want overwritten", srcs[0].Original)
	}

	// Clearing leaves the origin in place.
	if err := SetSourceOriginal(database, id, "danbooru", "111", ""); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := SourcesForImage(database, id); len(srcs) != 1 || srcs[0].Original != "" {
		t.Fatalf("sources = %+v, want the origin kept with empty original", srcs)
	}

	// Clearing a label with no origin must not conjure one.
	if err := SetSourceOriginal(database, id, "pixiv", "", ""); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := SourcesForImage(database, id); len(srcs) != 1 {
		t.Fatalf("sources = %+v, want no phantom origin", srcs)
	}

	// Setting an original on a fresh label adds an origin.
	if err := SetSourceOriginal(database, id, "pixiv", "", "http://tksn.web.infoseek.co.jp/"); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := SourcesForImage(database, id); len(srcs) != 2 {
		t.Fatalf("sources = %+v, want a new pixiv origin", srcs)
	}
}

func TestSeedImageSourcesFromScalar(t *testing.T) {
	database := newCollectionsTestDB(t)
	// An image that predates image_sources: only the scalar is set.
	res, err := database.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size, source, url) VALUES (?,?,?,?,?,?)`,
		randSHA(t), "/y.png", "image", 1, "danbooru", "https://d/9")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	// Re-run the one-time seed (no image_sources rows exist yet in this DB).
	if _, err := database.Write.Exec(
		`INSERT OR IGNORE INTO image_sources (image_id, site, post_id, url)
		 SELECT id, source, '', url FROM images
		 WHERE (source != '' OR url != '') AND NOT EXISTS (SELECT 1 FROM image_sources)`); err != nil {
		t.Fatal(err)
	}
	srcs, _ := SourcesForImage(database, id)
	if len(srcs) != 1 || srcs[0].Site != "danbooru" || srcs[0].URL != "https://d/9" {
		t.Fatalf("seeded sources = %+v, want one danbooru origin", srcs)
	}
}

func TestRenameSourceMembership_KeepsCommentaryAndRekeysAnnotations(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "111", "https://d/111")
	if err := SetSourceCommentary(database, id, "danbooru", "111", "artist words"); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceSourceAnnotations(database, id, "danbooru", "111",
		[]models.Annotation{{X: 1, Y: 2, W: 3, H: 4, Body: "box"}}); err != nil {
		t.Fatal(err)
	}

	if err := SetSourceOriginal(database, id, "danbooru", "111", "https://pixiv/artworks/1"); err != nil {
		t.Fatal(err)
	}

	if err := RenameSourceMembership(database, id, "danbooru", "111", "gelbooru", "111", "https://g/111"); err != nil {
		t.Fatal(err)
	}
	srcs, err := SourcesForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].Site != "gelbooru" || srcs[0].Commentary != "artist words" || srcs[0].Original != "https://pixiv/artworks/1" {
		t.Fatalf("after rename: %+v, want gelbooru row keeping its commentary and original", srcs)
	}
	anns, err := AnnotationsForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(anns) != 1 || anns[0].Site != "gelbooru" {
		t.Fatalf("annotations after rename = %+v, want re-keyed to gelbooru", anns)
	}
	if s, u := scalarSource(t, database, id); s != "gelbooru" || u != "https://g/111" {
		t.Errorf("mirror = (%q,%q), want the renamed primary", s, u)
	}
}

func TestRenameSourceMembership_MergesOntoExistingIdentity(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "", "https://d/1")
	if err := SetSourceCommentary(database, id, "danbooru", "", "kept words"); err != nil {
		t.Fatal(err)
	}
	_ = AddSourceMembership(database, id, "gelbooru", "", "https://g/1")
	if err := SetSourceOriginal(database, id, "gelbooru", "", "https://pixiv/artworks/1"); err != nil {
		t.Fatal(err)
	}

	// Renaming gelbooru onto the existing danbooru identity merges the rows.
	if err := RenameSourceMembership(database, id, "gelbooru", "", "danbooru", "", "https://g/1"); err != nil {
		t.Fatal(err)
	}
	srcs, err := SourcesForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].Site != "danbooru" || srcs[0].Commentary != "kept words" || srcs[0].URL != "https://g/1" {
		t.Fatalf("after merge: %+v, want one danbooru row keeping its commentary with the submitted url", srcs)
	}
	if srcs[0].Original != "https://pixiv/artworks/1" {
		t.Fatalf("original = %q, want the merged row to fill its empty original from the dropped one", srcs[0].Original)
	}
}

func TestRemoveSourceMembership_CascadesAnnotations(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "111", "https://d/111")
	_ = AddSourceMembership(database, id, "gelbooru", "222", "https://g/222")
	if err := ReplaceSourceAnnotations(database, id, "danbooru", "111",
		[]models.Annotation{{X: 1, Y: 2, W: 3, H: 4, Body: "goes away"}}); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceSourceAnnotations(database, id, "gelbooru", "222",
		[]models.Annotation{{X: 5, Y: 6, W: 7, H: 8, Body: "stays"}}); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSourceMembership(database, id, "danbooru", "111"); err != nil {
		t.Fatal(err)
	}
	anns, err := AnnotationsForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(anns) != 1 || anns[0].Site != "gelbooru" {
		t.Fatalf("annotations after remove = %+v, want only gelbooru's", anns)
	}
}

func TestSetPrimarySource_ClearCascadesAnnotations(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "", "https://d/1")
	if err := ReplaceSourceAnnotations(database, id, "danbooru", "",
		[]models.Annotation{{X: 1, Y: 2, W: 3, H: 4, Body: "box"}}); err != nil {
		t.Fatal(err)
	}
	if err := SetPrimarySource(database, id, "", ""); err != nil {
		t.Fatal(err)
	}
	anns, err := AnnotationsForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(anns) != 0 {
		t.Fatalf("annotations after clearing the only origin = %+v, want none", anns)
	}
}

func TestAddSourceMembership_EmptyURLKeepsStoredOne(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "", "https://d/100")

	// A url-less same-site re-push must not wipe the stored url.
	if err := AddSourceMembership(database, id, "danbooru", "", ""); err != nil {
		t.Fatal(err)
	}
	srcs, err := SourcesForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].URL != "https://d/100" {
		t.Fatalf("after url-less re-add: %+v, want the stored url kept", srcs)
	}
	if _, u := scalarSource(t, database, id); u != "https://d/100" {
		t.Errorf("mirror url = %q, want kept", u)
	}

	// A non-empty incoming url still updates.
	if err := AddSourceMembership(database, id, "danbooru", "", "https://d/200"); err != nil {
		t.Fatal(err)
	}
	if srcs, _ = SourcesForImage(database, id); srcs[0].URL != "https://d/200" {
		t.Errorf("url after non-empty re-add = %q, want https://d/200", srcs[0].URL)
	}
}

func TestSetPrimarySource_RefusesCollidingIdentity(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "", "https://d/1")
	_ = AddSourceMembership(database, id, "gelbooru", "", "https://g/1")

	err := SetPrimarySource(database, id, "gelbooru", "https://g/2")
	if !errors.Is(err, ErrSourceIdentityExists) {
		t.Fatalf("err = %v, want ErrSourceIdentityExists", err)
	}
	srcs, qerr := SourcesForImage(database, id)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(srcs) != 2 || srcs[0].Site != "danbooru" || srcs[0].URL != "https://d/1" {
		t.Fatalf("rows after refused relabel = %+v, want unchanged", srcs)
	}
}

func TestMakeSourcePrimary_ReordersAndRebindsMirror(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "ptr", "", "")
	_ = AddSourceMembership(database, id, "danbooru", "111", "https://d/111")

	if err := MakeSourcePrimary(database, id, "danbooru", "111"); err != nil {
		t.Fatal(err)
	}
	srcs, err := SourcesForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 2 || srcs[0].Site != "danbooru" || srcs[1].Site != "ptr" {
		t.Fatalf("sources = %+v, want danbooru promoted first", srcs)
	}
	if s, u := scalarSource(t, database, id); s != "danbooru" || u != "https://d/111" {
		t.Errorf("mirror = (%q,%q), want the promoted danbooru row", s, u)
	}

	// Promoting the current primary keeps it primary.
	if err := MakeSourcePrimary(database, id, "danbooru", "111"); err != nil {
		t.Fatal(err)
	}
	if s, _ := scalarSource(t, database, id); s != "danbooru" {
		t.Errorf("mirror after re-promoting = %q, want danbooru", s)
	}

	if err := MakeSourcePrimary(database, id, "nowhere", ""); err == nil {
		t.Error("promoting a missing source should error")
	}
}
