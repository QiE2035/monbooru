package relations

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/leqwin/monbooru/internal/db"
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
	t.Cleanup(func() { database.Close() })
	return database, New(database)
}

// insertImage inserts a minimal images row with the given file_size so
// the dup-group original-by-filesize logic can pick deterministically.
func insertImage(t *testing.T, database *db.DB, sha string, fileSize int64) int64 {
	t.Helper()
	var id int64
	err := database.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES (?, ?, 'png', ?) RETURNING id`,
		sha, "/gallery/"+sha+".png", fileSize,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insertImage: %v", err)
	}
	return id
}

func dupGroupOf(t *testing.T, database *db.DB, imageID int64) (groupID sql.NullInt64, original sql.NullInt64) {
	t.Helper()
	err := database.Read.QueryRow(
		`SELECT m.group_id, g.original_image_id FROM dup_group_members m JOIN dup_groups g ON g.id = m.group_id WHERE m.image_id = ?`,
		imageID,
	).Scan(&groupID, &original)
	if err == sql.ErrNoRows {
		return sql.NullInt64{}, sql.NullInt64{}
	}
	if err != nil {
		t.Fatalf("dupGroupOf: %v", err)
	}
	return
}

func altGroupOf(t *testing.T, database *db.DB, imageID int64) sql.NullInt64 {
	t.Helper()
	var gid sql.NullInt64
	err := database.Read.QueryRow(`SELECT group_id FROM alt_group_members WHERE image_id = ?`, imageID).Scan(&gid)
	if err == sql.ErrNoRows {
		return sql.NullInt64{}
	}
	if err != nil {
		t.Fatalf("altGroupOf: %v", err)
	}
	return gid
}

func dupGroupSize(t *testing.T, database *db.DB, groupID int64) int {
	t.Helper()
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM dup_group_members WHERE group_id = ?`, groupID).Scan(&n); err != nil {
		t.Fatalf("dupGroupSize: %v", err)
	}
	return n
}

func TestAddDuplicateSelf(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	if err := svc.AddDuplicate(a, a); !errors.Is(err, ErrSelfRelation) {
		t.Fatalf("AddDuplicate(self) = %v, want ErrSelfRelation", err)
	}
}

func TestAddDuplicateBothSingleton(t *testing.T) {
	database, svc := setupTestDB(t)
	small := insertImage(t, database, "small", 100)
	big := insertImage(t, database, "big", 5000)
	// Case 1: caller's first argument becomes the original. The session
	// handler puts the bigger-filesize side first by default; passing
	// the smaller first here proves the choice is caller-driven, not
	// implicit on filesize.
	if err := svc.AddDuplicate(small, big); err != nil {
		t.Fatalf("AddDuplicate: %v", err)
	}
	gid, orig := dupGroupOf(t, database, small)
	if !gid.Valid {
		t.Fatal("small not in a dup group")
	}
	if !orig.Valid || orig.Int64 != small {
		t.Fatalf("original = %v, want %d (first arg)", orig.Int64, small)
	}
	if dupGroupSize(t, database, gid.Int64) != 2 {
		t.Fatalf("dup group size = %d, want 2", dupGroupSize(t, database, gid.Int64))
	}
}

func TestAddDuplicateIdempotent(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("first AddDuplicate: %v", err)
	}
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("idempotent AddDuplicate: %v", err)
	}
	gid, _ := dupGroupOf(t, database, a)
	if dupGroupSize(t, database, gid.Int64) != 2 {
		t.Fatalf("group size after second add = %d, want 2", dupGroupSize(t, database, gid.Int64))
	}
}

func TestAddDuplicateJoinExisting(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)
	// Pass b first so b becomes the new group's original; the second
	// merge then proves c joins an existing group without disturbing it.
	if err := svc.AddDuplicate(b, a); err != nil {
		t.Fatalf("b-a: %v", err)
	}
	if err := svc.AddDuplicate(c, b); err != nil {
		t.Fatalf("c-b: %v", err)
	}
	gA, _ := dupGroupOf(t, database, a)
	gC, origC := dupGroupOf(t, database, c)
	if gA.Int64 != gC.Int64 {
		t.Fatalf("a and c not in same group: %d vs %d", gA.Int64, gC.Int64)
	}
	if origC.Int64 != b {
		t.Fatalf("original = %d, want %d (existing)", origC.Int64, b)
	}
	if dupGroupSize(t, database, gA.Int64) != 3 {
		t.Fatalf("group size = %d, want 3", dupGroupSize(t, database, gA.Int64))
	}
}

// TestAddDuplicateRightInGroupKeepsOriginal pins §6.4 case 3: when
// the right side is already in a group as original, the merge inserts
// the left as a new member and the existing original survives.
func TestAddDuplicateRightInGroupKeepsOriginal(t *testing.T) {
	database, svc := setupTestDB(t)
	original := insertImage(t, database, "orig", 5000)
	dup := insertImage(t, database, "dup", 100)
	newcomer := insertImage(t, database, "new", 200)
	if err := svc.AddDuplicate(original, dup); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// (newcomer, original) - newcomer is left. Original of the existing
	// group must not flip to newcomer.
	if err := svc.AddDuplicate(newcomer, original); err != nil {
		t.Fatalf("AddDuplicate: %v", err)
	}
	gid, orig := dupGroupOf(t, database, newcomer)
	if !gid.Valid {
		t.Fatal("newcomer not in a group")
	}
	if orig.Int64 != original {
		t.Fatalf("original = %d, want %d (existing untouched)", orig.Int64, original)
	}
	if dupGroupSize(t, database, gid.Int64) != 3 {
		t.Fatalf("group size = %d, want 3", dupGroupSize(t, database, gid.Int64))
	}
}

// TestAddDuplicateSameGroupNoOp pins §6.4 case 5: both already in the
// same group is a no-op; the existing original survives.
func TestAddDuplicateSameGroupNoOp(t *testing.T) {
	database, svc := setupTestDB(t)
	original := insertImage(t, database, "orig", 5000)
	dup := insertImage(t, database, "dup", 100)
	if err := svc.AddDuplicate(original, dup); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Re-add with dup as the left side; original must stay as original.
	if err := svc.AddDuplicate(dup, original); err != nil {
		t.Fatalf("AddDuplicate same group: %v", err)
	}
	gid, orig := dupGroupOf(t, database, dup)
	if orig.Int64 != original {
		t.Fatalf("original flipped to %d, want %d", orig.Int64, original)
	}
	if dupGroupSize(t, database, gid.Int64) != 2 {
		t.Fatalf("group size = %d, want 2 (no duplicate insert)", dupGroupSize(t, database, gid.Int64))
	}
}

func TestAddDuplicateMergesGroups(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 150)
	d := insertImage(t, database, "d", 250)
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("a-b: %v", err)
	}
	if err := svc.AddDuplicate(c, d); err != nil {
		t.Fatalf("c-d: %v", err)
	}
	if err := svc.AddDuplicate(b, c); err != nil {
		t.Fatalf("b-c (merge): %v", err)
	}
	gA, _ := dupGroupOf(t, database, a)
	gD, _ := dupGroupOf(t, database, d)
	if gA.Int64 != gD.Int64 {
		t.Fatalf("merge incomplete: a in %d, d in %d", gA.Int64, gD.Int64)
	}
	if dupGroupSize(t, database, gA.Int64) != 4 {
		t.Fatalf("merged group size = %d, want 4", dupGroupSize(t, database, gA.Int64))
	}
	var groups int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM dup_groups`).Scan(&groups); err != nil {
		t.Fatal(err)
	}
	if groups != 1 {
		t.Fatalf("dup_groups rows = %d, want 1", groups)
	}
}

func TestAddDuplicateConflictsWithVersion(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	if err := svc.AddVersionEdge(a, b); err != nil {
		t.Fatalf("AddVersionEdge: %v", err)
	}
	if err := svc.AddDuplicate(a, b); !errors.Is(err, ErrRelationConflict) {
		t.Fatalf("AddDuplicate after version: got %v, want ErrRelationConflict", err)
	}
}

func TestAddAlternateConflictsWithDuplicate(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("AddDuplicate: %v", err)
	}
	if err := svc.AddAlternate(a, b); !errors.Is(err, ErrRelationConflict) {
		t.Fatalf("AddAlternate after duplicate: got %v, want ErrRelationConflict", err)
	}
}

func TestAddVersionEdgeRefusesSecondParent(t *testing.T) {
	database, svc := setupTestDB(t)
	parent1 := insertImage(t, database, "p1", 100)
	parent2 := insertImage(t, database, "p2", 100)
	child := insertImage(t, database, "c", 100)
	if err := svc.AddVersionEdge(parent1, child); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := svc.AddVersionEdge(parent2, child); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("second parent: got %v, want ErrVersionExists", err)
	}
}

func TestAddVersionEdgeRefusesSecondChild(t *testing.T) {
	database, svc := setupTestDB(t)
	parent := insertImage(t, database, "p", 100)
	c1 := insertImage(t, database, "c1", 100)
	c2 := insertImage(t, database, "c2", 100)
	if err := svc.AddVersionEdge(parent, c1); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := svc.AddVersionEdge(parent, c2); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("second child: got %v, want ErrVersionExists", err)
	}
}

func TestAddDerivativeEdgeAllowsMultipleDerivatives(t *testing.T) {
	database, svc := setupTestDB(t)
	source := insertImage(t, database, "src", 100)
	d1 := insertImage(t, database, "d1", 100)
	d2 := insertImage(t, database, "d2", 100)
	if err := svc.AddDerivativeEdge(source, d1); err != nil {
		t.Fatalf("d1: %v", err)
	}
	if err := svc.AddDerivativeEdge(source, d2); err != nil {
		t.Fatalf("d2: %v", err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM derivative_edges WHERE source_image_id = ?`, source).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("derivative count = %d, want 2", n)
	}
}

func TestAddDerivativeEdgeRefusesSecondSource(t *testing.T) {
	database, svc := setupTestDB(t)
	s1 := insertImage(t, database, "s1", 100)
	s2 := insertImage(t, database, "s2", 100)
	d := insertImage(t, database, "d", 100)
	if err := svc.AddDerivativeEdge(s1, d); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := svc.AddDerivativeEdge(s2, d); !errors.Is(err, ErrDerivativeExists) {
		t.Fatalf("second source: got %v, want ErrDerivativeExists", err)
	}
}

func TestAddNotRelated(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	if err := svc.AddNotRelated(b, a); err != nil { // canonicalises to (a, b)
		t.Fatalf("AddNotRelated: %v", err)
	}
	var lo, hi int64
	if err := database.Read.QueryRow(`SELECT a_image_id, b_image_id FROM not_related_pairs`).Scan(&lo, &hi); err != nil {
		t.Fatal(err)
	}
	if lo != a || hi != b {
		t.Fatalf("not_related_pairs canonical = (%d, %d), want (%d, %d)", lo, hi, a, b)
	}
	// Subsequent AddDuplicate should conflict.
	if err := svc.AddDuplicate(a, b); !errors.Is(err, ErrRelationConflict) {
		t.Fatalf("AddDuplicate after not_related: got %v, want ErrRelationConflict", err)
	}
}

func TestAltPropagationOnDuplicate(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	c := insertImage(t, database, "c", 100)
	d := insertImage(t, database, "d", 100)
	// a is alternate to c
	if err := svc.AddAlternate(a, c); err != nil {
		t.Fatalf("a-c alt: %v", err)
	}
	// b is alternate to d (different alt group)
	if err := svc.AddAlternate(b, d); err != nil {
		t.Fatalf("b-d alt: %v", err)
	}
	// a and b become duplicates -> their alt groups must merge so c
	// becomes alternate to d transitively.
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("AddDuplicate: %v", err)
	}
	gA := altGroupOf(t, database, a)
	gC := altGroupOf(t, database, c)
	gD := altGroupOf(t, database, d)
	if !gA.Valid || gA.Int64 != gC.Int64 || gA.Int64 != gD.Int64 {
		t.Fatalf("alt-group merge failed: a=%d c=%d d=%d", gA.Int64, gC.Int64, gD.Int64)
	}
}

func TestOnImageDeletePromotesOriginal(t *testing.T) {
	database, svc := setupTestDB(t)
	big := insertImage(t, database, "big", 5000)
	mid := insertImage(t, database, "mid", 3000)
	small := insertImage(t, database, "small", 1000)
	if err := svc.AddDuplicate(big, mid); err != nil {
		t.Fatalf("big-mid: %v", err)
	}
	if err := svc.AddDuplicate(mid, small); err != nil {
		t.Fatalf("mid-small: %v", err)
	}
	// big is currently the original (largest). Delete big - the
	// largest remaining should be promoted.
	if err := svc.OnImageDelete(big); err != nil {
		t.Fatalf("OnImageDelete: %v", err)
	}
	if _, err := database.Write.Exec(`DELETE FROM images WHERE id = ?`, big); err != nil {
		t.Fatalf("delete image: %v", err)
	}
	gid, orig := dupGroupOf(t, database, mid)
	if !gid.Valid {
		t.Fatal("mid lost its group")
	}
	if orig.Int64 != mid {
		t.Fatalf("promoted original = %d, want %d (largest remaining)", orig.Int64, mid)
	}
}

func TestOnImageDeleteDissolvesSingletonGroup(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("AddDuplicate: %v", err)
	}
	if err := svc.OnImageDelete(a); err != nil {
		t.Fatalf("OnImageDelete: %v", err)
	}
	if _, err := database.Write.Exec(`DELETE FROM images WHERE id = ?`, a); err != nil {
		t.Fatalf("delete image a: %v", err)
	}
	// Group should be dissolved; b is now a singleton again.
	gid, _ := dupGroupOf(t, database, b)
	if gid.Valid {
		t.Fatalf("b should not be in any group, got %d", gid.Int64)
	}
	var groups int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM dup_groups`).Scan(&groups); err != nil {
		t.Fatal(err)
	}
	if groups != 0 {
		t.Fatalf("dup_groups remaining = %d, want 0", groups)
	}
}

func TestImageDeleteCascadesEdges(t *testing.T) {
	database, svc := setupTestDB(t)
	parent := insertImage(t, database, "p", 100)
	child := insertImage(t, database, "c", 100)
	if err := svc.AddVersionEdge(parent, child); err != nil {
		t.Fatalf("AddVersionEdge: %v", err)
	}
	// CASCADE handles version_edges directly - no OnImageDelete needed.
	if _, err := database.Write.Exec(`DELETE FROM images WHERE id = ?`, parent); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM version_edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("version edges remaining = %d, want 0", n)
	}
}

func TestImageDeleteCascadesNotRelated(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	if err := svc.AddNotRelated(a, b); err != nil {
		t.Fatalf("AddNotRelated: %v", err)
	}
	if _, err := database.Write.Exec(`DELETE FROM images WHERE id = ?`, a); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM not_related_pairs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("not_related_pairs remaining = %d, want 0", n)
	}
}

// Sanity: the foreign-keys pragma is on (Open sets it) and the
// schema's NO-CASCADE dup_groups.original_image_id constraint actually
// rejects an unhooked image-delete. This is the contract OnImageDelete
// is paying for.
func TestImageDeleteBlockedWithoutHook(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("AddDuplicate: %v", err)
	}
	// Try the raw delete without calling OnImageDelete first - the
	// dup_groups.original_image_id FK must refuse it. `a` is the
	// caller-chosen original (case 1 uses the first argument).
	_, err := database.Write.Exec(`DELETE FROM images WHERE id = ?`, a)
	if err == nil {
		t.Fatal("expected FK error deleting an original without OnImageDelete first")
	}
	if !contains(err.Error(), "FOREIGN KEY") && !contains(err.Error(), "constraint failed") {
		t.Fatalf("unexpected error shape: %v", err)
	}
}

func TestRemoveDupMemberIdempotent(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	if err := svc.RemoveDupMember(a); err != nil {
		t.Fatalf("RemoveDupMember(non-member): %v", err)
	}
}

func TestRemoveDupMemberDissolvesPair(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("AddDuplicate: %v", err)
	}
	if err := svc.RemoveDupMember(a); err != nil {
		t.Fatalf("RemoveDupMember: %v", err)
	}
	if gid, _ := dupGroupOf(t, database, b); gid.Valid {
		t.Fatalf("group survived; b still in group %d", gid.Int64)
	}
	var groups int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM dup_groups`).Scan(&groups); err != nil {
		t.Fatal(err)
	}
	if groups != 0 {
		t.Fatalf("dup_groups remaining = %d, want 0", groups)
	}
}

func TestRemoveDupMemberPromotesOriginal(t *testing.T) {
	database, svc := setupTestDB(t)
	big := insertImage(t, database, "big", 5000)
	mid := insertImage(t, database, "mid", 3000)
	small := insertImage(t, database, "small", 1000)
	if err := svc.AddDuplicate(big, mid); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddDuplicate(big, small); err != nil {
		t.Fatal(err)
	}
	// big is original. Remove big - mid should be promoted.
	if err := svc.RemoveDupMember(big); err != nil {
		t.Fatalf("RemoveDupMember: %v", err)
	}
	_, orig := dupGroupOf(t, database, mid)
	if orig.Int64 != mid {
		t.Fatalf("new original = %d, want %d", orig.Int64, mid)
	}
}

func TestDissolveDupGroup(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 150)
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddDuplicate(b, c); err != nil {
		t.Fatal(err)
	}
	gid, _ := dupGroupOf(t, database, a)
	if err := svc.DissolveDupGroup(gid.Int64); err != nil {
		t.Fatalf("Dissolve: %v", err)
	}
	for _, id := range []int64{a, b, c} {
		if g, _ := dupGroupOf(t, database, id); g.Valid {
			t.Fatalf("image %d still in group %d", id, g.Int64)
		}
	}
}

func TestPromoteToOriginal(t *testing.T) {
	database, svc := setupTestDB(t)
	big := insertImage(t, database, "big", 5000)
	small := insertImage(t, database, "small", 100)
	if err := svc.AddDuplicate(big, small); err != nil {
		t.Fatal(err)
	}
	gid, orig := dupGroupOf(t, database, big)
	if orig.Int64 != big {
		t.Fatalf("starting original = %d, want %d", orig.Int64, big)
	}
	if err := svc.PromoteToOriginal(gid.Int64, small); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	_, orig2 := dupGroupOf(t, database, small)
	if orig2.Int64 != small {
		t.Fatalf("promoted original = %d, want %d", orig2.Int64, small)
	}
}

func TestPromoteToOriginalNonMember(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatal(err)
	}
	gid, _ := dupGroupOf(t, database, a)
	if err := svc.PromoteToOriginal(gid.Int64, c); !errors.Is(err, ErrNotInGroup) {
		t.Fatalf("Promote non-member: got %v, want ErrNotInGroup", err)
	}
}

func TestRemoveAltMember(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 150)
	if err := svc.AddAlternate(a, b); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddAlternate(b, c); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveAltMember(b); err != nil {
		t.Fatal(err)
	}
	if altGroupOf(t, database, b).Valid {
		t.Fatal("b still in alt group")
	}
	// a and c are still in the original 3-member group, now reduced to 2 members.
	if !altGroupOf(t, database, a).Valid || !altGroupOf(t, database, c).Valid {
		t.Fatal("a or c lost alt membership unexpectedly")
	}
}

func TestRemoveVersionEdge(t *testing.T) {
	database, svc := setupTestDB(t)
	p := insertImage(t, database, "p", 100)
	c := insertImage(t, database, "c", 100)
	if err := svc.AddVersionEdge(p, c); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveVersionEdge(p, c); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM version_edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("version_edges remaining = %d, want 0", n)
	}
	// Removing an absent edge is idempotent.
	if err := svc.RemoveVersionEdge(p, c); err != nil {
		t.Fatalf("idempotent: %v", err)
	}
}

func TestRemoveDerivativeEdge(t *testing.T) {
	database, svc := setupTestDB(t)
	s := insertImage(t, database, "s", 100)
	d := insertImage(t, database, "d", 100)
	if err := svc.AddDerivativeEdge(s, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveDerivativeEdge(s, d); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM derivative_edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("derivative_edges remaining = %d, want 0", n)
	}
}

func TestRemoveNotRelated(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	if err := svc.AddNotRelated(a, b); err != nil {
		t.Fatal(err)
	}
	// Remove using the reversed order; canonicalisation handles it.
	if err := svc.RemoveNotRelated(b, a); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM not_related_pairs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("not_related_pairs remaining = %d, want 0", n)
	}
	// Pair becomes addable again.
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("re-add after forget: %v", err)
	}
}

func TestCopyTagsFromDuplicatesToOriginal(t *testing.T) {
	database, svc := setupTestDB(t)
	original := insertImage(t, database, "orig", 10000)
	dup := insertImage(t, database, "dup", 100)
	// Tag the duplicate with a non-rating tag. The original has no tags.
	var tagID int64
	if err := database.Write.QueryRow(
		`INSERT INTO tags (name, category_id) VALUES ('cat', (SELECT id FROM tag_categories WHERE name = 'general')) RETURNING id`,
	).Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id) VALUES (?, ?)`, dup, tagID,
	); err != nil {
		t.Fatal(err)
	}
	// Tag the duplicate with a rating tag - must NOT be copied.
	var explicitID int64
	if err := database.Read.QueryRow(
		`SELECT id FROM tags WHERE name = 'explicit' AND category_id = (SELECT id FROM tag_categories WHERE name = 'rating')`,
	).Scan(&explicitID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id) VALUES (?, ?)`, dup, explicitID,
	); err != nil {
		t.Fatal(err)
	}

	if err := svc.AddDuplicate(original, dup); err != nil {
		t.Fatal(err)
	}
	gid, _ := dupGroupOf(t, database, original)
	added, err := svc.CopyTagsFromDuplicatesToOriginal(gid.Int64)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1 (rating excluded)", added)
	}
	var hasGeneral, hasRating int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ? AND tag_id = ?`, original, tagID).Scan(&hasGeneral); err != nil {
		t.Fatal(err)
	}
	if hasGeneral != 1 {
		t.Fatal("original did not pick up the general tag")
	}
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ? AND tag_id = ?`, original, explicitID).Scan(&hasRating); err != nil {
		t.Fatal(err)
	}
	if hasRating != 0 {
		t.Fatal("original picked up rating tag; rating exclusion broken")
	}
	// Idempotent re-run.
	added2, err := svc.CopyTagsFromDuplicatesToOriginal(gid.Int64)
	if err != nil {
		t.Fatal(err)
	}
	if added2 != 0 {
		t.Fatalf("second run added = %d, want 0", added2)
	}
}

// TestAddDuplicateSweepsCoGroupedQueueRows pins F007: after a merge
// lands two endpoints in the same dup group, any queue row whose
// endpoints are now both in that group is dropped so the session
// doesn't re-ask the operator about a pair that already shares the
// group.
func TestAddDuplicateSweepsCoGroupedQueueRows(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)
	// Seed three queue rows that the find-pairs job would have written.
	for _, pair := range [][2]int64{{a, b}, {a, c}, {b, c}} {
		if _, err := database.Write.Exec(
			`INSERT INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?, ?, 0, ?)`,
			pair[0], pair[1], "now",
		); err != nil {
			t.Fatalf("seed queue: %v", err)
		}
	}
	// Mark a-b as duplicates: queue row (a, b) is consumed by the
	// session handler, but the new sweep should also drop (a, c) and
	// (b, c) once c joins the group.
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("a-b: %v", err)
	}
	if err := svc.AddDuplicate(b, c); err != nil {
		t.Fatalf("b-c: %v", err)
	}
	var rest int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM potential_relation_pairs`).Scan(&rest); err != nil {
		t.Fatal(err)
	}
	if rest != 0 {
		t.Fatalf("queue rows remaining = %d, want 0 (all three pairs co-grouped)", rest)
	}
}

// TestNextOriginalIfRemovedPicksBiggest pins the preview helper that
// the detail-page unlink confirm reads: same ORDER BY as the actual
// promotion, returns 0 when the group is small enough to dissolve.
func TestNextOriginalIfRemovedPicksBiggest(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 5000)
	c := insertImage(t, database, "c", 3000)
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("a-b: %v", err)
	}
	if err := svc.AddDuplicate(b, c); err != nil {
		t.Fatalf("b-c: %v", err)
	}
	gid, _ := dupGroupOf(t, database, a)
	// Removing a (the current original) from a 3-member group should
	// promote b (largest remaining).
	next, err := svc.NextOriginalIfRemoved(gid.Int64, a)
	if err != nil {
		t.Fatal(err)
	}
	if next != b {
		t.Fatalf("next original = %d, want %d (largest remaining)", next, b)
	}
	// Two-member group: the helper signals "no promotion to preview" with
	// (0, nil) because removeDupMember dissolves the group instead.
	if err := svc.RemoveDupMember(c); err != nil {
		t.Fatalf("RemoveDupMember c: %v", err)
	}
	next, err = svc.NextOriginalIfRemoved(gid.Int64, a)
	if err != nil {
		t.Fatal(err)
	}
	if next != 0 {
		t.Fatalf("next original on 2-member group = %d, want 0", next)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && find(s, sub))
}

func find(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

