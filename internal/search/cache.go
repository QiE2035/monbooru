package search

import (
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Adjacency cache: keyed snapshots of the gallery's sorted match-id
// list, populated by Execute when the page-1 result holds the full
// match set and consumed by ExecuteAdjacent so prev/next is an O(log n)
// slice scan instead of a fresh cursor query.
//
// Sized for the home-LAN deployment: a small handful of concurrent
// queries, each capped at adjacencyCacheMaxIDs so the cache memory
// budget stays bounded even on popular-tag queries. Stale entries fall
// off via TTL; no invalidation hook on writes - inserts/deletes that
// race a browse session may surface a missing prev/next, which the
// detail handler already tolerates.
//
// Cap math: maxEntries * maxIDs * 8 bytes / entry = worst-case bytes.
// 4 * 1 000 000 * 8 = ~32 MB. Average case is far lower because the
// cache only seeds entries whose total fits the cap; sparse queries
// land in single-digit KB. The home-box deployment is single-user
// with a small handful of active tabs, so 4 hot entries cover the
// realistic working set, and the 1 M cap is wide enough to seat
// popular single-tag random-sort queries that would otherwise run
// five parallel temp-sorts under c=5 contention.
const (
	adjacencyCacheTTL        = 5 * time.Minute
	adjacencyCacheMaxEntries = 4
	adjacencyCacheMaxIDs     = 1000000
)

type adjacencyCacheEntry struct {
	ids       []int64
	expiresAt time.Time
}

var (
	adjCacheMu      sync.Mutex
	adjCacheEntries = make(map[string]adjacencyCacheEntry)
	adjCacheOrder   []string

	// fanInFlight dedupes background match-id fans launched by Execute
	// when the cache misses: a burst of concurrent cache-miss requests
	// for the same key would otherwise spawn a fan goroutine each, all
	// running the same SELECT and contending for the read pool. With
	// the gate, only the first goroutine fans; the rest see cache miss
	// and skip the populate path, falling back to the regular Execute
	// shape that's already cheap on a single page.
	fanInFlightMu sync.Mutex
	fanInFlight   = map[string]bool{}
)

// AdjacencyCacheTryAcquireFan returns true when the caller wins the
// race to fan the match-ids for key. The winner must call
// AdjacencyCacheReleaseFan on completion regardless of outcome.
// Losers must not fan; the winning goroutine will populate the cache.
func AdjacencyCacheTryAcquireFan(key string) bool {
	if key == "" {
		return false
	}
	fanInFlightMu.Lock()
	defer fanInFlightMu.Unlock()
	if fanInFlight[key] {
		return false
	}
	fanInFlight[key] = true
	return true
}

// AdjacencyCacheReleaseFan releases the in-flight gate so the next
// cache miss after TTL expiry can fan again.
func AdjacencyCacheReleaseFan(key string) {
	fanInFlightMu.Lock()
	delete(fanInFlight, key)
	fanInFlightMu.Unlock()
}

// AdjacencyCacheGet returns the cached sorted match-id list for key,
// or ok=false on miss / expiry. The returned slice aliases the cached
// backing array (copying it on every hit would blow the adjacency-cache
// latency budget on large result sets), so callers must treat it as
// read-only - never sort, append into, or mutate it.
func AdjacencyCacheGet(key string) ([]int64, bool) {
	if key == "" {
		return nil, false
	}
	adjCacheMu.Lock()
	defer adjCacheMu.Unlock()
	entry, ok := adjCacheEntries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(adjCacheEntries, key)
		removeFromOrder(key)
		return nil, false
	}
	return entry.ids, true
}

// AdjacencyCacheSet stores ids for key under the configured TTL. Empty
// keys, empty lists, and lists above adjacencyCacheMaxIDs are skipped
// so a popular query can't push the cache over its memory budget.
//
// Re-setting an existing key (typical after the entry's TTL expired
// without an intervening Get to remove it) refreshes its slot in the
// LRU order so a freshly written entry isn't immediately evicted by
// the next unrelated Set.
func AdjacencyCacheSet(key string, ids []int64) {
	if key == "" || len(ids) == 0 || len(ids) > adjacencyCacheMaxIDs {
		return
	}
	adjCacheMu.Lock()
	defer adjCacheMu.Unlock()
	if _, exists := adjCacheEntries[key]; exists {
		removeFromOrder(key)
	}
	adjCacheOrder = append(adjCacheOrder, key)
	snapshot := make([]int64, len(ids))
	copy(snapshot, ids)
	adjCacheEntries[key] = adjacencyCacheEntry{
		ids:       snapshot,
		expiresAt: time.Now().Add(adjacencyCacheTTL),
	}
	for len(adjCacheOrder) > adjacencyCacheMaxEntries {
		oldest := adjCacheOrder[0]
		adjCacheOrder = adjCacheOrder[1:]
		delete(adjCacheEntries, oldest)
	}
}

// AdjacencyCacheDropForGallery drops every entry whose key starts with
// the given gallery name. Called from a gallery's InvalidateCaches so a
// cached match-id list can't survive a write that changed result-set
// membership (delete, move, inbox/favourite toggle, batch tag, ...). The
// per-gallery cap is small enough that walking the map on every write
// is cheap; a global Clear would also drop other galleries' entries
// unnecessarily.
func AdjacencyCacheDropForGallery(gallery string) {
	if gallery == "" {
		return
	}
	prefix := gallery + "\x00"
	adjCacheMu.Lock()
	defer adjCacheMu.Unlock()
	for k := range adjCacheEntries {
		if strings.HasPrefix(k, prefix) {
			delete(adjCacheEntries, k)
		}
	}
	// Rebuild the LRU order without the dropped keys; len(adjCacheOrder)
	// stays bounded by adjacencyCacheMaxEntries (4) so the rebuild is
	// constant time.
	newOrder := adjCacheOrder[:0]
	for _, k := range adjCacheOrder {
		if _, exists := adjCacheEntries[k]; exists {
			newOrder = append(newOrder, k)
		}
	}
	adjCacheOrder = newOrder
}

// AdjacencyCacheSweep drops every entry past its TTL. Get evicts one
// on the way past and Set evicts by LRU, so without this an idle
// process keeps expired lists - up to the cache's whole budget - until
// something touches the cache again.
func AdjacencyCacheSweep() {
	adjCacheMu.Lock()
	defer adjCacheMu.Unlock()
	now := time.Now()
	for k, entry := range adjCacheEntries {
		if now.After(entry.expiresAt) {
			delete(adjCacheEntries, k)
			removeFromOrder(k)
		}
	}
}

func removeFromOrder(key string) {
	if i := slices.Index(adjCacheOrder, key); i >= 0 {
		adjCacheOrder = slices.Delete(adjCacheOrder, i, i+1)
	}
}

// BuildAdjacencyCacheKey returns the stable key the gallery's Execute
// and the detail's ExecuteAdjacent use for the same browsing session.
// The components are joined NUL-separated so substrings can't collide
// across boundaries (a query "foo|bar" is still distinct from a
// gallery "foo" + query "bar"). A zero seed under a non-random sort
// is normalised to the empty seed so newest/filesize sorts hit the
// cache regardless of any leftover seed param on the URL.
func BuildAdjacencyCacheKey(gallery, query, sort, order string, seed int64, ceiling string) string {
	seedStr := ""
	if sort == "random" && seed != 0 {
		seedStr = strconv.FormatInt(seed, 10)
	}
	var b strings.Builder
	b.Grow(len(gallery) + len(query) + len(sort) + len(order) + len(seedStr) + len(ceiling) + 5)
	b.WriteString(gallery)
	b.WriteByte(0)
	b.WriteString(query)
	b.WriteByte(0)
	b.WriteString(sort)
	b.WriteByte(0)
	b.WriteString(order)
	b.WriteByte(0)
	b.WriteString(seedStr)
	b.WriteByte(0)
	b.WriteString(ceiling)
	return b.String()
}

// findInAdjacencyList returns the prev/next image ids around currentID
// in a sorted match-id list. Returns nil pointers for out-of-bounds
// neighbours; (nil, nil) when currentID isn't in the list (typically
// because it was deleted or the list belongs to a different query).
func findInAdjacencyList(ids []int64, currentID int64) (*int64, *int64) {
	for i, id := range ids {
		if id != currentID {
			continue
		}
		var prev, next *int64
		if i > 0 {
			p := ids[i-1]
			prev = &p
		}
		if i < len(ids)-1 {
			n := ids[i+1]
			next = &n
		}
		return prev, next
	}
	return nil, nil
}
