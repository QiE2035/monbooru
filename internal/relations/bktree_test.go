package relations

import (
	"math/rand"
	"sort"
	"testing"
)

func TestBKTreeInsertAndExactSearch(t *testing.T) {
	tree := NewBKTree()
	tree.Insert(1, 0x1234567890ABCDEF)
	tree.Insert(2, 0x1234567890ABCDEF) // same phash as 1
	tree.Insert(3, 0x0)

	got := tree.SearchWithinDistance(0x1234567890ABCDEF, 0)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("exact match: got %v, want [1 2]", got)
	}
}

// Linear scan ground truth: the BK-tree must return the same set as a
// brute-force loop over every entry. Run against a random fixture of
// 200 phashes to cover the metric pruning paths.
func TestBKTreeMatchesLinearScan(t *testing.T) {
	tree := NewBKTree()
	rng := rand.New(rand.NewSource(42))
	const n = 200
	hashes := make([]int64, n)
	for i := 0; i < n; i++ {
		h := int64(rng.Uint64())
		hashes[i] = h
		tree.Insert(int64(i+1), h)
	}
	queries := []int64{0, hashes[0], hashes[42], hashes[199], int64(rng.Uint64())}
	for _, q := range queries {
		for _, d := range []int{0, 2, 4, 8, 16, 32, 64} {
			want := linearSearch(hashes, q, d)
			got := tree.SearchWithinDistance(q, d)
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			sort.Ints(want)
			if len(got) != len(want) {
				t.Fatalf("q=%016x d=%d: tree=%v linear=%v", uint64(q), d, got, want)
			}
			for i := range got {
				if int(got[i]) != want[i]+1 {
					t.Fatalf("q=%016x d=%d at %d: tree=%v linear=%v", uint64(q), d, i, got, want)
				}
			}
		}
	}
}

func TestBKTreeRemove(t *testing.T) {
	tree := NewBKTree()
	tree.Insert(1, 0xAA)
	tree.Insert(2, 0xBB)
	tree.Insert(3, 0xCC)
	tree.Remove(2)
	if got := tree.SearchWithinDistance(0xBB, 0); len(got) != 0 {
		t.Fatalf("after Remove(2), got %v, want []", got)
	}
	if tree.Size() != 2 {
		t.Fatalf("size = %d, want 2", tree.Size())
	}
	// Idempotent.
	tree.Remove(2)
	tree.Remove(999)
}

func TestBKTreeInsertReplacesPreviousPhash(t *testing.T) {
	tree := NewBKTree()
	tree.Insert(1, 0xAA)
	tree.Insert(1, 0xBB) // same id, different phash
	if got := tree.SearchWithinDistance(0xAA, 0); len(got) != 0 {
		t.Fatalf("after re-insert, old phash still matches: %v", got)
	}
	if got := tree.SearchWithinDistance(0xBB, 0); len(got) != 1 || got[0] != 1 {
		t.Fatalf("new phash search: got %v, want [1]", got)
	}
}

func TestBKTreeReset(t *testing.T) {
	tree := NewBKTree()
	tree.Insert(1, 0xAA)
	tree.Insert(2, 0xBB)
	tree.Reset()
	if tree.Size() != 0 {
		t.Fatalf("size after Reset = %d, want 0", tree.Size())
	}
	if got := tree.SearchWithinDistance(0xAA, 64); len(got) != 0 {
		t.Fatalf("Reset leaked entries: %v", got)
	}
	if tree.Built() {
		t.Fatal("Built() = true after Reset")
	}
}

func TestBKTreeBuildFromDB(t *testing.T) {
	database, svc := setupTestDB(t)
	_ = svc
	// Manually write phashes for a few rows; BuildFromDB picks them up.
	a := insertImage(t, database, "a", 100)
	b := insertImage(t, database, "b", 200)
	c := insertImage(t, database, "c", 300)
	if _, err := database.Write.Exec(`UPDATE images SET phash = ? WHERE id = ?`, int64(0xAA), a); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(`UPDATE images SET phash = ? WHERE id = ?`, int64(0xBB), b); err != nil {
		t.Fatal(err)
	}
	// c has NULL phash - should be skipped.

	tree := NewBKTree()
	if err := tree.BuildFromDB(database); err != nil {
		t.Fatalf("BuildFromDB: %v", err)
	}
	if tree.Size() != 2 {
		t.Fatalf("Size = %d, want 2 (c has NULL phash)", tree.Size())
	}
	if !tree.Built() {
		t.Fatal("Built() = false after BuildFromDB")
	}
	got := tree.SearchWithinDistance(0xAA, 0)
	if len(got) != 1 || got[0] != a {
		t.Fatalf("search for a: got %v, want [%d]", got, a)
	}
	_ = c
}

func TestRegistryRegisterLookupUnregister(t *testing.T) {
	database, _ := setupTestDB(t)
	tree := NewBKTree()
	DefaultRegistry.Register(database, tree)
	t.Cleanup(func() { DefaultRegistry.Unregister(database) })
	if DefaultRegistry.Lookup(database) != tree {
		t.Fatal("Lookup did not return registered tree")
	}
	DefaultRegistry.Unregister(database)
	if DefaultRegistry.Lookup(database) != nil {
		t.Fatal("Lookup after Unregister returned non-nil")
	}
}

// linearSearch returns the indexes (0-based into hashes) within
// Hamming distance d of query. The BK-tree must produce the same set
// after offsetting by +1 since the test inserts ids = idx+1.
func linearSearch(hashes []int64, query int64, d int) []int {
	var out []int
	for i, h := range hashes {
		if hammingDistance(h, query) <= d {
			out = append(out, i)
		}
	}
	return out
}
