package relations

import (
	"database/sql"
	"errors"
	"fmt"
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
	t.Cleanup(func() { _ = database.Close() })
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

// Chains are valid as long as each image has at most one parent and one
// child; the schema permits X->Y->Z so the service must too.
func TestAddVersionEdgeAllowsSuccessiveChain(t *testing.T) {
	database, svc := setupTestDB(t)
	x := insertImage(t, database, "x", 100)
	y := insertImage(t, database, "y", 100)
	z := insertImage(t, database, "z", 100)
	if err := svc.AddVersionEdge(x, y); err != nil {
		t.Fatalf("x->y: %v", err)
	}
	if err := svc.AddVersionEdge(y, z); err != nil {
		t.Fatalf("y->z (chain extension): %v", err)
	}
}

// ClearVersionEdgeConflictsFor must wipe only the edges that block the
// new (parent, child) insert: a row where the new child already has a
// parent, or a row where the new parent already has a child. Edges
// that name one endpoint but don't violate the per-row uniqueness for
// the new (parent, child) survive.
func TestClearVersionEdgeConflictsForKeepsThirdImageEdges(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	c := insertImage(t, database, "c", 100)
	e := insertImage(t, database, "e", 100)
	// Two unrelated edges seed the table: b is newer than a (a -> b)
	// and e is newer than c (c -> e). Adding (parent=a, child=c) would
	// conflict only on the first row (a already parents b); the
	// second row touches c on its parent side but doesn't violate any
	// uniqueness for (a, c).
	if err := svc.AddVersionEdge(a, b); err != nil {
		t.Fatalf("seed a->b: %v", err)
	}
	if err := svc.AddVersionEdge(c, e); err != nil {
		t.Fatalf("seed c->e: %v", err)
	}
	if err := svc.ClearVersionEdgeConflictsFor(a, c); err != nil {
		t.Fatalf("ClearVersionEdgeConflictsFor: %v", err)
	}
	var count int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM version_edges WHERE child_image_id = ? AND parent_image_id = ?`, b, a,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("a->b row count = %d, want 0 (was the conflict)", count)
	}
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM version_edges WHERE child_image_id = ? AND parent_image_id = ?`, e, c,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("c->e row count = %d, want 1 (unrelated third-image edge)", count)
	}
}

// A back-edge that would close a loop with the existing chain must be
// rejected even though neither endpoint violates the per-row schema
// constraint directly.
func TestAddVersionEdgeRejectsCycle(t *testing.T) {
	database, svc := setupTestDB(t)
	x := insertImage(t, database, "x", 100)
	y := insertImage(t, database, "y", 100)
	z := insertImage(t, database, "z", 100)
	if err := svc.AddVersionEdge(x, y); err != nil {
		t.Fatalf("x->y: %v", err)
	}
	if err := svc.AddVersionEdge(y, z); err != nil {
		t.Fatalf("y->z: %v", err)
	}
	if err := svc.AddVersionEdge(z, x); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("z->x (cycle): got %v, want ErrVersionExists", err)
	}
}

// Identical re-add of the same edge is a silent success; only a
// different edge that would conflict with the per-row schema still
// raises ErrVersionExists. This lets REST callers retry idempotently
// after a network blip.
func TestAddVersionEdgeIdempotentSameEdge(t *testing.T) {
	database, svc := setupTestDB(t)
	parent := insertImage(t, database, "p", 100)
	child := insertImage(t, database, "c", 100)
	if err := svc.AddVersionEdge(parent, child); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := svc.AddVersionEdge(parent, child); err != nil {
		t.Errorf("idempotent re-add: got %v, want nil", err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM version_edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("version_edges = %d, want 1", n)
	}
}

func TestAddDerivativeEdgeIdempotentSameEdge(t *testing.T) {
	database, svc := setupTestDB(t)
	src := insertImage(t, database, "src", 100)
	d := insertImage(t, database, "d", 100)
	if err := svc.AddDerivativeEdge(src, d); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := svc.AddDerivativeEdge(src, d); err != nil {
		t.Errorf("idempotent re-add: got %v, want nil", err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM derivative_edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("derivative_edges = %d, want 1", n)
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

// Adding an edge whose derivative is already an ancestor of source on
// the existing source chain closes a loop; the cycle walk must catch
// it.
func TestAddDerivativeEdgeRejectsCycle(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	c := insertImage(t, database, "c", 100)
	if err := svc.AddDerivativeEdge(a, b); err != nil {
		t.Fatalf("a->b: %v", err)
	}
	if err := svc.AddDerivativeEdge(b, c); err != nil {
		t.Fatalf("b->c: %v", err)
	}
	if err := svc.AddDerivativeEdge(c, a); !errors.Is(err, ErrDerivativeExists) {
		t.Fatalf("c->a (cycle): got %v, want ErrDerivativeExists", err)
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

// TestDuplicateDoesNotMergeAlternates pins §9.2: a pair carries at most
// one relation type. Declaring a and b duplicates must not fold their
// separate alternate groups together, which would also make a and b
// alternates of each other on top of being duplicates.
func TestDuplicateDoesNotMergeAlternates(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	c := insertImage(t, database, "c", 100)
	d := insertImage(t, database, "d", 100)
	if err := svc.AddAlternate(a, c); err != nil {
		t.Fatalf("a-c alt: %v", err)
	}
	if err := svc.AddAlternate(b, d); err != nil {
		t.Fatalf("b-d alt: %v", err)
	}
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("AddDuplicate: %v", err)
	}
	dA, _ := dupGroupOf(t, database, a)
	dB, _ := dupGroupOf(t, database, b)
	if !dA.Valid || dA.Int64 != dB.Int64 {
		t.Fatalf("a and b should share a dup group: a=%v b=%v", dA, dB)
	}
	gA, gB := altGroupOf(t, database, a), altGroupOf(t, database, b)
	gC, gD := altGroupOf(t, database, c), altGroupOf(t, database, d)
	if gA.Int64 != gC.Int64 || gB.Int64 != gD.Int64 {
		t.Fatalf("alt memberships should be untouched: a=%d c=%d b=%d d=%d", gA.Int64, gC.Int64, gB.Int64, gD.Int64)
	}
	if gA.Int64 == gB.Int64 {
		t.Fatal("a and b must not share an alt group (they are duplicates, not alternates)")
	}
}

// TestOverwriteAlternateWithDuplicate mirrors the detail-page "Overwrite
// existing relation" path (ClearBetween then AddDuplicate) when the
// existing relation is a multi-member alternate group. The overwritten
// pair must end up duplicates only, never duplicates and alternates at
// once.
func TestOverwriteAlternateWithDuplicate(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	c := insertImage(t, database, "c", 100)
	if err := svc.AddAlternate(a, b); err != nil {
		t.Fatalf("a-b alt: %v", err)
	}
	if err := svc.AddAlternate(a, c); err != nil { // alt group {a, b, c}
		t.Fatalf("a-c alt: %v", err)
	}
	if err := svc.ClearBetween(a, b); err != nil {
		t.Fatalf("ClearBetween: %v", err)
	}
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("AddDuplicate: %v", err)
	}
	dA, _ := dupGroupOf(t, database, a)
	dB, _ := dupGroupOf(t, database, b)
	if !dA.Valid || dA.Int64 != dB.Int64 {
		t.Fatal("a and b should share a dup group")
	}
	if altGroupOf(t, database, b).Valid {
		t.Fatal("b must not be in an alt group after being overwritten to a duplicate of a")
	}
	if altGroupOf(t, database, a).Int64 != altGroupOf(t, database, c).Int64 {
		t.Fatal("c should remain an alternate of a")
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

func TestRemoveVersionEdgeSwappedSides(t *testing.T) {
	database, svc := setupTestDB(t)
	p := insertImage(t, database, "p", 100)
	c := insertImage(t, database, "c", 100)
	if err := svc.AddVersionEdge(p, c); err != nil {
		t.Fatal(err)
	}
	// Operator-typed form posts the sides reversed; the DELETE must
	// still drop the edge so the click isn't a silent no-op.
	if err := svc.RemoveVersionEdge(c, p); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM version_edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("version_edges remaining after swapped-side remove = %d, want 0", n)
	}
}

func TestRemoveDerivativeEdgeSwappedSides(t *testing.T) {
	database, svc := setupTestDB(t)
	s := insertImage(t, database, "s", 100)
	d := insertImage(t, database, "d", 100)
	if err := svc.AddDerivativeEdge(s, d); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveDerivativeEdge(d, s); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM derivative_edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("derivative_edges remaining after swapped-side remove = %d, want 0", n)
	}
}

// TestClearDerivativeSourceOf pins the detail-page "Replace existing
// source" path: declaring a source -> derivative edge then calling
// ClearDerivativeSourceOf(derivative) drops exactly that edge, leaving
// the derivative free to be re-sourced.
func TestClearDerivativeSourceOf(t *testing.T) {
	database, svc := setupTestDB(t)
	source := insertImage(t, database, "source", 100)
	derivative := insertImage(t, database, "derivative", 200)
	if err := svc.AddDerivativeEdge(source, derivative); err != nil {
		t.Fatal(err)
	}
	if err := svc.ClearDerivativeSourceOf(derivative); err != nil {
		t.Fatalf("ClearDerivativeSourceOf: %v", err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM derivative_edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("derivative_edges remaining = %d, want 0", n)
	}
	// The whole point of clearing the source is that a fresh source can be
	// declared; the per-derivative uniqueness no longer blocks it.
	newSource := insertImage(t, database, "newsource", 300)
	if err := svc.AddDerivativeEdge(newSource, derivative); err != nil {
		t.Fatalf("re-source after clear: %v", err)
	}
	var srcID int64
	if err := database.Read.QueryRow(
		`SELECT source_image_id FROM derivative_edges WHERE derivative_image_id = ?`, derivative,
	).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if srcID != newSource {
		t.Fatalf("source after re-source = %d, want %d", srcID, newSource)
	}
}

// TestClearDerivativeSourceOfKeepsSiblingsAndSubtree confirms the clear
// is scoped to the named derivative's incoming edge only: a sibling
// sharing the same source keeps its edge, and edges below the cleared
// node (where it was itself a source) are untouched - so the tree stays
// consistent rather than collapsing.
func TestClearDerivativeSourceOfKeepsSiblingsAndSubtree(t *testing.T) {
	database, svc := setupTestDB(t)
	source := insertImage(t, database, "source", 100)
	target := insertImage(t, database, "target", 200)   // its incoming edge gets cleared
	sibling := insertImage(t, database, "sibling", 300) // shares source with target
	child := insertImage(t, database, "child", 400)     // derivative of target
	for _, e := range []struct{ src, der int64 }{
		{source, target},
		{source, sibling},
		{target, child},
	} {
		if err := svc.AddDerivativeEdge(e.src, e.der); err != nil {
			t.Fatalf("seed edge (%d -> %d): %v", e.src, e.der, err)
		}
	}

	if err := svc.ClearDerivativeSourceOf(target); err != nil {
		t.Fatalf("ClearDerivativeSourceOf: %v", err)
	}

	// target's incoming edge is gone.
	var hasIncoming int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM derivative_edges WHERE derivative_image_id = ?`, target,
	).Scan(&hasIncoming); err != nil {
		t.Fatal(err)
	}
	if hasIncoming != 0 {
		t.Fatalf("target still has %d incoming edge(s), want 0", hasIncoming)
	}
	// The sibling's edge from the same source survives.
	var siblingSrc int64
	if err := database.Read.QueryRow(
		`SELECT source_image_id FROM derivative_edges WHERE derivative_image_id = ?`, sibling,
	).Scan(&siblingSrc); err != nil {
		t.Fatalf("sibling edge dropped, want intact: %v", err)
	}
	if siblingSrc != source {
		t.Fatalf("sibling source = %d, want %d", siblingSrc, source)
	}
	// The subtree below target (target -> child) is untouched: clearing an
	// incoming edge must not orphan or delete outgoing edges.
	var childSrc int64
	if err := database.Read.QueryRow(
		`SELECT source_image_id FROM derivative_edges WHERE derivative_image_id = ?`, child,
	).Scan(&childSrc); err != nil {
		t.Fatalf("child edge dropped, want intact: %v", err)
	}
	if childSrc != target {
		t.Fatalf("child source = %d, want %d (target)", childSrc, target)
	}
	// Two edges remain in total (source->sibling, target->child).
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM derivative_edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("derivative_edges = %d, want 2 (sibling + subtree intact)", n)
	}
}

// TestClearDerivativeSourceOfNoEdge is the idempotent / no-op case: the
// detail affordance may fire on a derivative that has no source, and a
// bare DELETE must not error.
func TestClearDerivativeSourceOfNoEdge(t *testing.T) {
	_, svc := setupTestDB(t)
	if err := svc.ClearDerivativeSourceOf(42); err != nil {
		t.Fatalf("ClearDerivativeSourceOf on sourceless derivative: %v", err)
	}
}

func TestReverseVersionEdgeMidChainRaisesTypedError(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	c := insertImage(t, database, "c", 100)
	if err := svc.AddVersionEdge(a, b); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddVersionEdge(b, c); err != nil {
		t.Fatal(err)
	}
	// Reversing the (a, b) edge in a > b > c chain would land (b, a)
	// where b already has c as its child, violating the per-parent
	// uniqueness. The function returns ErrVersionExists rather than the
	// raw SQLite constraint error.
	err := svc.ReverseVersionEdge(a, b)
	if !errors.Is(err, ErrVersionExists) {
		t.Fatalf("ReverseVersionEdge mid-chain expected ErrVersionExists, got %v", err)
	}
	// The original chain is intact (transaction rolled back).
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM version_edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("version_edges = %d after failed reverse, want 2", n)
	}
}

func TestReverseDerivativeEdgeMidTreeRaisesTypedError(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 100)
	c := insertImage(t, database, "c", 100)
	if err := svc.AddDerivativeEdge(a, b); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddDerivativeEdge(c, a); err != nil {
		t.Fatal(err)
	}
	// Reversing (a, b) would land (b, a) where a is already a derivative
	// of c (PK on derivative_image_id).
	err := svc.ReverseDerivativeEdge(a, b)
	if !errors.Is(err, ErrDerivativeExists) {
		t.Fatalf("ReverseDerivativeEdge mid-tree expected ErrDerivativeExists, got %v", err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM derivative_edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("derivative_edges = %d after failed reverse, want 2", n)
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

// TestAddDuplicateSweepsCoGroupedQueueRows: after a merge lands two
// endpoints in the same dup group, any queue row whose endpoints are
// now both in that group is dropped so the session doesn't re-ask
// the operator about a pair that already shares the group.
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

// AddDuplicate(A, B) when A and B already sit in distinct dup groups
// must merge those groups instead of erroring. The survivor (lowest
// group id) keeps its original_image_id; every member of the higher-id
// group migrates over.
func TestAddDuplicateAutoMergesDistinctGroups(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 500)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 600)
	d := insertImage(t, database, "d", 250)
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("a-b dup: %v", err)
	}
	if err := svc.AddDuplicate(c, d); err != nil {
		t.Fatalf("c-d dup: %v", err)
	}
	gAB, origAB := dupGroupOf(t, database, a)
	gCD, _ := dupGroupOf(t, database, c)
	if !gAB.Valid || !gCD.Valid || gAB.Int64 == gCD.Int64 {
		t.Fatalf("setup: two distinct dup groups expected")
	}
	// Session presses Duplicate on (b, d); both already grouped.
	if err := svc.AddDuplicate(b, d); err != nil {
		t.Fatalf("AddDuplicate(b, d) on cross-group pair: %v", err)
	}
	gPost, origPost := dupGroupOf(t, database, a)
	gPostD, _ := dupGroupOf(t, database, d)
	if !gPost.Valid || !gPostD.Valid || gPost.Int64 != gPostD.Int64 {
		t.Fatalf("after auto-merge: a in %v, d in %v; expected same group", gPost, gPostD)
	}
	if dupGroupSize(t, database, gPost.Int64) != 4 {
		t.Errorf("merged dup group size = %d, want 4", dupGroupSize(t, database, gPost.Int64))
	}
	// Survivor is the lower id; its original_image_id is preserved.
	if origPost.Int64 != origAB.Int64 {
		t.Errorf("original_image_id after merge = %d, want %d (survivor kept)", origPost.Int64, origAB.Int64)
	}
	var groups int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM dup_groups`).Scan(&groups); err != nil {
		t.Fatal(err)
	}
	if groups != 1 {
		t.Errorf("dup_groups rows after merge = %d, want 1", groups)
	}
}

// AddAlternate(A, B) when both sides already sit in distinct alt
// groups must fold those groups together rather than error.
func TestAddAlternateAutoMergesDistinctGroups(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 150)
	d := insertImage(t, database, "d", 250)
	if err := svc.AddAlternate(a, b); err != nil {
		t.Fatalf("a-b alt: %v", err)
	}
	if err := svc.AddAlternate(c, d); err != nil {
		t.Fatalf("c-d alt: %v", err)
	}
	gAB := altGroupOf(t, database, a)
	gCD := altGroupOf(t, database, c)
	if !gAB.Valid || !gCD.Valid || gAB.Int64 == gCD.Int64 {
		t.Fatalf("setup: two distinct alt groups expected")
	}
	if err := svc.AddAlternate(b, d); err != nil {
		t.Fatalf("AddAlternate(b, d) cross-group: %v", err)
	}
	gA := altGroupOf(t, database, a)
	gD := altGroupOf(t, database, d)
	if !gA.Valid || !gD.Valid || gA.Int64 != gD.Int64 {
		t.Fatalf("after auto-merge: a alt = %v, d alt = %v; expected same group", gA, gD)
	}
	var rowCount, memberCount int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM alt_groups`).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 1 {
		t.Errorf("alt_groups rows after merge = %d, want 1", rowCount)
	}
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM alt_group_members WHERE group_id = ?`, gA.Int64).Scan(&memberCount); err != nil {
		t.Fatal(err)
	}
	if memberCount != 4 {
		t.Errorf("merged alt group size = %d, want 4", memberCount)
	}
}

// MergeAltGroups consolidates two distinct alt groups into one: every
// member of the higher-id group ends up under the lower-id group, and
// the higher-id alt_groups row goes away.
func TestMergeAltGroupsCombinesTwoGroups(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 150)
	d := insertImage(t, database, "d", 250)
	if err := svc.AddAlternate(a, b); err != nil {
		t.Fatalf("a-b alt: %v", err)
	}
	if err := svc.AddAlternate(c, d); err != nil {
		t.Fatalf("c-d alt: %v", err)
	}
	gAB := altGroupOf(t, database, a)
	gCD := altGroupOf(t, database, c)
	if !gAB.Valid || !gCD.Valid || gAB.Int64 == gCD.Int64 {
		t.Fatalf("setup: expected two distinct alt groups, got %v / %v", gAB, gCD)
	}
	if err := svc.MergeAltGroups([]int64{gAB.Int64, gCD.Int64}); err != nil {
		t.Fatalf("MergeAltGroups: %v", err)
	}
	gA := altGroupOf(t, database, a)
	gD := altGroupOf(t, database, d)
	if !gA.Valid || !gD.Valid || gA.Int64 != gD.Int64 {
		t.Fatalf("after merge: a alt = %v, d alt = %v; expected the same group", gA, gD)
	}
	var rowCount int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM alt_groups`).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 1 {
		t.Errorf("alt_groups rows = %d, want 1", rowCount)
	}
	var memberCount int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM alt_group_members WHERE group_id = ?`, gA.Int64).Scan(&memberCount); err != nil {
		t.Fatal(err)
	}
	if memberCount != 4 {
		t.Errorf("merged alt group size = %d, want 4", memberCount)
	}
}

// MergeAltGroups on a single-group input must be a no-op (the
// idempotent contract the dup-group merge UI relies on).
func TestMergeAltGroupsSingleGroupNoOp(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	if err := svc.AddAlternate(a, b); err != nil {
		t.Fatalf("AddAlternate: %v", err)
	}
	gid := altGroupOf(t, database, a)
	if !gid.Valid {
		t.Fatal("alt group missing")
	}
	if err := svc.MergeAltGroups([]int64{gid.Int64}); err != nil {
		t.Fatalf("MergeAltGroups single: %v", err)
	}
	post := altGroupOf(t, database, a)
	if !post.Valid || post.Int64 != gid.Int64 {
		t.Errorf("single-group merge changed membership: pre=%v post=%v", gid, post)
	}
}

// MergeDupGroups picks the lowest id as the survivor and keeps the
// survivor's original_image_id when keepOriginalFrom is 0.
func TestMergeDupGroupsKeepsSurvivorOriginal(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 500)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 600)
	d := insertImage(t, database, "d", 250)
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("a-b dup: %v", err)
	}
	if err := svc.AddDuplicate(c, d); err != nil {
		t.Fatalf("c-d dup: %v", err)
	}
	gAB, origAB := dupGroupOf(t, database, a)
	gCD, origCD := dupGroupOf(t, database, c)
	if !gAB.Valid || !gCD.Valid || gAB.Int64 == gCD.Int64 {
		t.Fatalf("setup: two distinct dup groups expected")
	}
	// Sanity: AddDuplicate(a, b) makes a the original; same for c-d.
	if origAB.Int64 != a {
		t.Fatalf("origAB = %d, want %d", origAB.Int64, a)
	}
	if origCD.Int64 != c {
		t.Fatalf("origCD = %d, want %d", origCD.Int64, c)
	}
	if err := svc.MergeDupGroups([]int64{gAB.Int64, gCD.Int64}, 0); err != nil {
		t.Fatalf("MergeDupGroups: %v", err)
	}
	gPost, origPost := dupGroupOf(t, database, a)
	gPostD, _ := dupGroupOf(t, database, d)
	if !gPost.Valid || !gPostD.Valid || gPost.Int64 != gPostD.Int64 {
		t.Fatalf("post-merge: a in %v, d in %v; expected the same group", gPost, gPostD)
	}
	if dupGroupSize(t, database, gPost.Int64) != 4 {
		t.Errorf("merged dup group size = %d, want 4", dupGroupSize(t, database, gPost.Int64))
	}
	// Survivor is the lower id. keepOriginalFrom=0 preserves its original.
	if origPost.Int64 != a {
		t.Errorf("post-merge original = %d, want %d (survivor kept)", origPost.Int64, a)
	}
}

// MergeDupGroups with keepOriginalFrom set to the non-survivor group
// copies that group's original_image_id onto the survivor row.
func TestMergeDupGroupsKeepOriginalFromNonSurvivor(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 500)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 600)
	d := insertImage(t, database, "d", 250)
	if err := svc.AddDuplicate(a, b); err != nil {
		t.Fatalf("a-b dup: %v", err)
	}
	if err := svc.AddDuplicate(c, d); err != nil {
		t.Fatalf("c-d dup: %v", err)
	}
	gAB, _ := dupGroupOf(t, database, a)
	gCD, _ := dupGroupOf(t, database, c)
	if err := svc.MergeDupGroups([]int64{gAB.Int64, gCD.Int64}, gCD.Int64); err != nil {
		t.Fatalf("MergeDupGroups: %v", err)
	}
	_, origPost := dupGroupOf(t, database, a)
	if origPost.Int64 != c {
		t.Errorf("post-merge original = %d, want %d (inherited from %d)", origPost.Int64, c, gCD.Int64)
	}
}

// DissolveVersionChain drops every edge in the chain, regardless of
// which member id the operator clicked from - the walker locates the
// root from either side.
func TestDissolveVersionChain(t *testing.T) {
	database, svc := setupTestDB(t)
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)
	if err := svc.AddVersionEdge(a, b); err != nil {
		t.Fatalf("a->b: %v", err)
	}
	if err := svc.AddVersionEdge(b, c); err != nil {
		t.Fatalf("b->c: %v", err)
	}
	// Dissolve from a middle node to prove the walker reaches the root.
	if err := svc.DissolveVersionChain(b); err != nil {
		t.Fatalf("DissolveVersionChain: %v", err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM version_edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("version_edges remaining = %d, want 0", n)
	}
}

// DissolveVersionChain on an image with no version edges must be a
// silent no-op (matching the other Dissolve* idempotence contract).
func TestDissolveVersionChainNoEdges(t *testing.T) {
	_, svc := setupTestDB(t)
	if err := svc.DissolveVersionChain(42); err != nil {
		t.Fatalf("DissolveVersionChain on missing chain: %v", err)
	}
}

// DissolveDerivativeTree drops every edge in the tree, including the
// branches that fan out from non-root nodes. The walker reaches the
// root from any member; the DFS-down catches every descendant.
func TestDissolveDerivativeTree(t *testing.T) {
	database, svc := setupTestDB(t)
	src := insertImage(t, database, "src", 100)
	d1 := insertImage(t, database, "d1", 110)
	d2 := insertImage(t, database, "d2", 120)
	d1a := insertImage(t, database, "d1a", 130)
	if err := svc.AddDerivativeEdge(src, d1); err != nil {
		t.Fatalf("src->d1: %v", err)
	}
	if err := svc.AddDerivativeEdge(src, d2); err != nil {
		t.Fatalf("src->d2: %v", err)
	}
	if err := svc.AddDerivativeEdge(d1, d1a); err != nil {
		t.Fatalf("d1->d1a: %v", err)
	}
	// Dissolve from a deep leaf to prove the up-walk finds the root.
	if err := svc.DissolveDerivativeTree(d1a); err != nil {
		t.Fatalf("DissolveDerivativeTree: %v", err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM derivative_edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("derivative_edges remaining = %d, want 0", n)
	}
}

// DissolveDerivativeTree on an image with no derivative edges must be
// a silent no-op.
func TestDissolveDerivativeTreeNoEdges(t *testing.T) {
	_, svc := setupTestDB(t)
	if err := svc.DissolveDerivativeTree(42); err != nil {
		t.Fatalf("DissolveDerivativeTree on missing tree: %v", err)
	}
}

// A wide derivative tree (single source with many siblings) must not
// silently truncate when the walker's depth budget is mis-applied to
// fan-out instead of vertical reach. Pins the BFS-by-level cap so a
// future regression that re-counts depth-per-node would fail here.
func TestDissolveDerivativeTreeWideFanout(t *testing.T) {
	database, svc := setupTestDB(t)
	src := insertImage(t, database, "src", 100)
	const fanout = 300
	for i := 0; i < fanout; i++ {
		d := insertImage(t, database, fmt.Sprintf("d%03d", i), int64(200+i))
		if err := svc.AddDerivativeEdge(src, d); err != nil {
			t.Fatalf("src->d%d: %v", i, err)
		}
	}
	if err := svc.DissolveDerivativeTree(src); err != nil {
		t.Fatalf("DissolveDerivativeTree: %v", err)
	}
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM derivative_edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("derivative_edges remaining = %d, want 0", n)
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
