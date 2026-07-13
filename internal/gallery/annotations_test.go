package gallery

import (
	"testing"

	"github.com/leqwin/monbooru/internal/models"
)

func TestReplaceSourceAnnotations_CloneAndScope(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)

	boxes := func(site string, bodies ...string) []models.Annotation {
		var a []models.Annotation
		for i, b := range bodies {
			a = append(a, models.Annotation{Site: site, X: i, Y: i, W: 10, H: 10, Body: b})
		}
		return a
	}
	if err := ReplaceSourceAnnotations(database, id, "danbooru", "", boxes("danbooru", "a", "b")); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceSourceAnnotations(database, id, "gelbooru", "", boxes("gelbooru", "c")); err != nil {
		t.Fatal(err)
	}
	if anns, _ := AnnotationsForImage(database, id); len(anns) != 3 {
		t.Fatalf("got %d annotations, want 3", len(anns))
	}

	// A re-pull replaces only that source's boxes.
	if err := ReplaceSourceAnnotations(database, id, "danbooru", "", boxes("danbooru", "a2")); err != nil {
		t.Fatal(err)
	}
	if anns, _ := AnnotationsForImage(database, id); len(anns) != 2 {
		t.Fatalf("got %d after re-pull, want 2", len(anns))
	}

	// Clearing danbooru leaves gelbooru untouched.
	if err := ReplaceSourceAnnotations(database, id, "danbooru", "", nil); err != nil {
		t.Fatal(err)
	}
	anns, _ := AnnotationsForImage(database, id)
	if len(anns) != 1 || anns[0].Site != "gelbooru" {
		t.Fatalf("got %+v after clear, want only gelbooru", anns)
	}
}

func TestAnnotations_CRUDAndManualSurvivesSourceOps(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)

	if err := ReplaceSourceAnnotations(database, id, "danbooru", "",
		[]models.Annotation{{Site: "danbooru", X: 1, Y: 1, W: 5, H: 5, Body: "src"}}); err != nil {
		t.Fatal(err)
	}
	if err := AddManualAnnotation(database, id, 2, 3, 4, 5, "mine"); err != nil {
		t.Fatal(err)
	}

	anns, _ := AnnotationsForImage(database, id)
	var manual, source models.Annotation
	for _, a := range anns {
		if a.Manual {
			manual = a
		} else {
			source = a
		}
	}
	if manual.ID == 0 || manual.Body != "mine" {
		t.Fatalf("expected one manual box with an id, got %+v", anns)
	}

	// Both operator-drawn and source-pulled boxes edit by id.
	if err := UpdateAnnotation(database, manual.ID, 10, 11, 12, 13, "edited"); err != nil {
		t.Fatal(err)
	}
	if err := UpdateAnnotation(database, source.ID, 6, 7, 8, 9, "src edited"); err != nil {
		t.Fatalf("source box should be editable: %v", err)
	}
	// The source box also deletes by id.
	if err := DeleteAnnotation(database, source.ID); err != nil {
		t.Fatalf("source box should be removable: %v", err)
	}

	// The bulk source-keyed removal still spares the operator box.
	if err := ReplaceSourceAnnotations(database, id, "danbooru", "",
		[]models.Annotation{{Site: "danbooru", X: 1, Y: 1, W: 5, H: 5, Body: "src2"}}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSourceMembership(database, id, "danbooru", ""); err != nil {
		t.Fatal(err)
	}
	after, _ := AnnotationsForImage(database, id)
	if len(after) != 1 || !after[0].Manual || after[0].Body != "edited" {
		t.Fatalf("manual box must survive source removal, got %+v", after)
	}

	if err := DeleteAnnotation(database, after[0].ID); err != nil {
		t.Fatal(err)
	}
	if final, _ := AnnotationsForImage(database, id); len(final) != 0 {
		t.Fatalf("want 0 boxes after delete, got %d", len(final))
	}
}
