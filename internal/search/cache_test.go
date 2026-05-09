package search

import (
	"testing"
	"time"
)

func TestAdjacencyCache_GetMissEmptyKey(t *testing.T) {
	AdjacencyCacheClear()
	if _, ok := AdjacencyCacheGet(""); ok {
		t.Errorf("empty key should miss")
	}
}

func TestAdjacencyCache_SetGetRoundTrip(t *testing.T) {
	AdjacencyCacheClear()
	key := "g\x00q\x00sort"
	want := []int64{10, 20, 30}
	AdjacencyCacheSet(key, want)
	got, ok := AdjacencyCacheGet(key)
	if !ok {
		t.Fatalf("set+get: miss")
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ids[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestAdjacencyCache_SetIsolatesCallerSlice(t *testing.T) {
	AdjacencyCacheClear()
	key := "snapshot"
	src := []int64{1, 2, 3}
	AdjacencyCacheSet(key, src)
	src[0] = 99
	got, _ := AdjacencyCacheGet(key)
	if got[0] != 1 {
		t.Errorf("cache aliased caller's slice: got[0] = %d, want 1", got[0])
	}
}

func TestAdjacencyCache_SetSkipsOversized(t *testing.T) {
	AdjacencyCacheClear()
	key := "oversize"
	huge := make([]int64, adjacencyCacheMaxIDs+1)
	AdjacencyCacheSet(key, huge)
	if _, ok := AdjacencyCacheGet(key); ok {
		t.Errorf("oversized list should not have been cached")
	}
}

func TestAdjacencyCache_LRUEviction(t *testing.T) {
	AdjacencyCacheClear()
	// Fill exactly to the cap, then add one more and confirm the oldest
	// entry was evicted.
	for i := 0; i < adjacencyCacheMaxEntries; i++ {
		AdjacencyCacheSet(keyN(i), []int64{int64(i)})
	}
	AdjacencyCacheSet(keyN(adjacencyCacheMaxEntries), []int64{42})
	if _, ok := AdjacencyCacheGet(keyN(0)); ok {
		t.Errorf("oldest entry should have been evicted")
	}
	if _, ok := AdjacencyCacheGet(keyN(adjacencyCacheMaxEntries)); !ok {
		t.Errorf("newest entry should be present")
	}
}

// TestAdjacencyCache_ResetMovesToMostRecentSlot pins the LRU-order
// refresh: a re-Set of an existing key (typical after its TTL has
// lapsed without a Get to remove the entry) must move the entry to
// the most-recent slot, not leave it in its original (oldest) one
// where the next unrelated Set would evict it.
func TestAdjacencyCache_ResetMovesToMostRecentSlot(t *testing.T) {
	AdjacencyCacheClear()
	target := "refreshed"
	// Plant target first so it's at the oldest slot, then fill the rest
	// of the cap with unrelated keys.
	AdjacencyCacheSet(target, []int64{1})
	for i := 0; i < adjacencyCacheMaxEntries-1; i++ {
		AdjacencyCacheSet(keyN(i), []int64{int64(i + 100)})
	}
	// Backdate so a re-Set takes the existing-key branch as it would
	// after a real TTL expiry without an intervening Get.
	adjCacheMu.Lock()
	entry := adjCacheEntries[target]
	entry.expiresAt = time.Now().Add(-time.Second)
	adjCacheEntries[target] = entry
	adjCacheMu.Unlock()
	AdjacencyCacheSet(target, []int64{2})

	// One more unrelated Set tips the cap. With the order refresh the
	// oldest unrelated key is evicted; without it, target sits at the
	// head of the order list and is the one dropped.
	AdjacencyCacheSet("trigger-eviction", []int64{999})
	if _, ok := AdjacencyCacheGet(target); !ok {
		t.Errorf("re-Set entry should have moved to the most-recent slot and survived the eviction")
	}
}

func TestAdjacencyCache_Expiry(t *testing.T) {
	AdjacencyCacheClear()
	key := "expire"
	AdjacencyCacheSet(key, []int64{1, 2})
	// Backdate the entry past the TTL boundary by mutating internal
	// state directly - the runtime never wakes the past-due entry up.
	adjCacheMu.Lock()
	entry := adjCacheEntries[key]
	entry.expiresAt = time.Now().Add(-time.Second)
	adjCacheEntries[key] = entry
	adjCacheMu.Unlock()
	if _, ok := AdjacencyCacheGet(key); ok {
		t.Errorf("expired entry should have missed")
	}
	// Get on miss removes the entry from the order list too.
	adjCacheMu.Lock()
	for _, k := range adjCacheOrder {
		if k == key {
			t.Errorf("expired key still in order list after miss")
		}
	}
	adjCacheMu.Unlock()
}

func TestBuildAdjacencyCacheKey_DeterministicAndDistinct(t *testing.T) {
	a := BuildAdjacencyCacheKey("g1", "q", "newest", "desc", 0, "")
	b := BuildAdjacencyCacheKey("g1", "q", "newest", "desc", 0, "")
	if a != b {
		t.Errorf("same inputs produced different keys")
	}
	c := BuildAdjacencyCacheKey("g2", "q", "newest", "desc", 0, "")
	if a == c {
		t.Errorf("different gallery should produce different key")
	}
	d := BuildAdjacencyCacheKey("g1", "q", "random", "desc", 1234, "")
	e := BuildAdjacencyCacheKey("g1", "q", "random", "desc", 5678, "")
	if d == e {
		t.Errorf("different random seeds should produce different keys")
	}
}

func TestBuildAdjacencyCacheKey_NormalisesNonRandomSeed(t *testing.T) {
	// A leftover seed= URL param under a non-random sort must not
	// segregate the cache from the gallery's seedless render.
	a := BuildAdjacencyCacheKey("g", "q", "newest", "desc", 0, "")
	b := BuildAdjacencyCacheKey("g", "q", "newest", "desc", 1234, "")
	if a != b {
		t.Errorf("seed should be ignored for non-random sort")
	}
}

func TestFindInAdjacencyList(t *testing.T) {
	ids := []int64{10, 20, 30, 40, 50}
	prev, next := findInAdjacencyList(ids, 30)
	if prev == nil || *prev != 20 {
		t.Errorf("prev = %v, want 20", prev)
	}
	if next == nil || *next != 40 {
		t.Errorf("next = %v, want 40", next)
	}
	prev, next = findInAdjacencyList(ids, 10)
	if prev != nil {
		t.Errorf("first prev = %v, want nil", prev)
	}
	if next == nil || *next != 20 {
		t.Errorf("first next = %v, want 20", next)
	}
	prev, next = findInAdjacencyList(ids, 50)
	if prev == nil || *prev != 40 {
		t.Errorf("last prev = %v, want 40", prev)
	}
	if next != nil {
		t.Errorf("last next = %v, want nil", next)
	}
	prev, next = findInAdjacencyList(ids, 99)
	if prev != nil || next != nil {
		t.Errorf("missing id: got (%v, %v), want (nil, nil)", prev, next)
	}
}

func keyN(n int) string {
	return "k" + string(rune('0'+n%10)) + string(rune('a'+n/10))
}
