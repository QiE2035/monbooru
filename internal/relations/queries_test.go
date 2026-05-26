package relations

import "testing"

// TestLoadImageRelationsIncludesSelf: dup and alt groups both
// surface member_ids that include the queried image, so the API
// shape is consistent. Templates filter self when they want to render
// peers; consumers that want the membership get it all.
func TestLoadImageRelationsIncludesSelf(t *testing.T) {
	database, svc := setupTestDB(t)
	dupA := insertImage(t, database, "dupA", 100)
	dupB := insertImage(t, database, "dupB", 200)
	if err := svc.AddDuplicate(dupA, dupB); err != nil {
		t.Fatalf("AddDuplicate: %v", err)
	}
	altA := insertImage(t, database, "altA", 100)
	altB := insertImage(t, database, "altB", 100)
	if err := svc.AddAlternate(altA, altB); err != nil {
		t.Fatalf("AddAlternate: %v", err)
	}

	rels, err := LoadImageRelations(database, altA)
	if err != nil {
		t.Fatalf("LoadImageRelations(altA): %v", err)
	}
	if !containsID(rels.AltGroupMembers, altA) || !containsID(rels.AltGroupMembers, altB) {
		t.Fatalf("AltGroupMembers = %v, want self %d and peer %d", rels.AltGroupMembers, altA, altB)
	}

	relsDup, err := LoadImageRelations(database, dupA)
	if err != nil {
		t.Fatalf("LoadImageRelations(dupA): %v", err)
	}
	if relsDup.DupGroup == nil || !containsID(relsDup.DupGroup.Members, dupA) {
		t.Fatalf("DupGroup.Members = %v, want self %d", relsDup.DupGroup.Members, dupA)
	}
}

func containsID(ids []int64, target int64) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}
