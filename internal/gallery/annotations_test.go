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
