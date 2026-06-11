package relations

import (
	"math/bits"
	"sync"
	"sync/atomic"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
)

// init wires gallery.PhashHooks.OnStored to the registry's per-DB
// Insert path so every successful phash store keeps the in-memory
// tree (when one is built) in lockstep with the row that just
// changed. When IncrementalProbeEnabled is set, the same hook also
// probes the BK-tree for near-duplicates of the newly-stored row
// and inserts them into potential_relation_pairs - the §5.3
// "incremental on ingest" path.
//
// Tests that don't register a tree see both halves short-circuit
// on Lookup nil.
func init() {
	gallery.PhashHooks.OnStored = func(database *db.DB, id, phash int64) {
		tree := DefaultRegistry.Lookup(database)
		if tree == nil || !tree.Built() {
			return
		}
		tree.Insert(id, phash)
		if !IncrementalProbeEnabled.Load() {
			return
		}
		distance := int(IncrementalProbeDistance.Load())
		if err := incrementalProbe(database, tree, id, phash, distance); err != nil {
			logx.Debugf("incremental probe %d: %v", id, err)
		}
	}
}

// EnsureBuilt builds the tree against database when it isn't already
// populated. Idempotent; safe to call from multiple goroutines (one
// loses the race and rebuilds redundantly, but the final state is the
// same).
func (t *BKTree) EnsureBuilt(database *db.DB) error {
	if t.Built() {
		return nil
	}
	return t.BuildFromDB(database)
}

// BKTree is a 64-bit Hamming-distance metric tree over image phashes.
// Used by find-pairs and the `phash:<hex>~d` search keyword to answer
// "every image within distance d of this phash" without scanning every
// row. Concurrent-safe: Insert / Remove serialise, Search runs under a
// read lock so per-request queries don't contend with the watcher's
// incremental Inserts.
type BKTree struct {
	mu      sync.RWMutex
	root    *bkNode
	idIndex map[int64]int64 // id -> phash, drives Remove
	built   atomic.Bool
}

type bkNode struct {
	phash    int64
	ids      []int64
	children map[int]*bkNode
}

// NewBKTree returns an empty tree. Use BuildFromDB to populate from
// SQLite, or call Insert directly when feeding individual rows.
func NewBKTree() *BKTree {
	return &BKTree{idIndex: make(map[int64]int64)}
}

// Size returns the number of (image_id, phash) entries currently in
// the tree.
func (t *BKTree) Size() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.idIndex)
}

// Built reports whether BuildFromDB has been called at least once on
// this tree. Useful for "is this gallery ready for relations queries"
// gating in handlers that want to avoid showing a partial result while
// the lazy build is in flight.
func (t *BKTree) Built() bool {
	return t.built.Load()
}

// Reset clears every entry. Used after a full backfill so the next
// query rebuilds against the new DB contents instead of paying the
// per-row incremental insert.
func (t *BKTree) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.root = nil
	t.idIndex = make(map[int64]int64)
	t.built.Store(false)
}

// BuildFromDB rebuilds the tree from every (id, phash) row in images
// where phash IS NOT NULL. Idempotent: calling twice is safe. Clears
// the tree first so a partial earlier build doesn't leave stale
// entries.
func (t *BKTree) BuildFromDB(database *db.DB) error {
	rows, err := database.Read.Query(`SELECT id, phash FROM images WHERE phash IS NOT NULL`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.root = nil
	t.idIndex = make(map[int64]int64)
	for rows.Next() {
		var id, phash int64
		if err := rows.Scan(&id, &phash); err != nil {
			return err
		}
		t.insertLocked(id, phash)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	t.built.Store(true)
	return nil
}

// Insert adds (id, phash) to the tree. If id already exists, its
// previous entry is removed first so the index stays single-valued.
func (t *BKTree) Insert(id, phash int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if prev, ok := t.idIndex[id]; ok {
		t.removeIDLocked(id, prev)
	}
	t.insertLocked(id, phash)
}

// Remove deletes id from the tree. Idempotent.
func (t *BKTree) Remove(id int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	phash, ok := t.idIndex[id]
	if !ok {
		return
	}
	t.removeIDLocked(id, phash)
}

// SearchWithinDistance returns every image id whose stored phash is
// within Hamming distance `d` of `query`. Self-matches at distance 0
// are included. Result order is unspecified.
func (t *BKTree) SearchWithinDistance(query int64, d int) []int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []int64
	if t.root == nil {
		return out
	}
	t.searchLocked(t.root, query, d, &out)
	return out
}

func (t *BKTree) insertLocked(id, phash int64) {
	t.idIndex[id] = phash
	if t.root == nil {
		t.root = &bkNode{phash: phash, ids: []int64{id}}
		return
	}
	cur := t.root
	for {
		if cur.phash == phash {
			cur.ids = append(cur.ids, id)
			return
		}
		dist := hammingDistance(cur.phash, phash)
		if cur.children == nil {
			cur.children = map[int]*bkNode{}
		}
		if child, ok := cur.children[dist]; ok {
			cur = child
			continue
		}
		cur.children[dist] = &bkNode{phash: phash, ids: []int64{id}}
		return
	}
}

func (t *BKTree) removeIDLocked(id, phash int64) {
	delete(t.idIndex, id)
	// Walk to the node holding this phash and drop the id from its ids
	// list. Empty leaves are left in place rather than rewriting the
	// tree on every delete; Reset is the cheap path when fragmentation
	// matters (after a bulk backfill).
	cur := t.root
	for cur != nil {
		if cur.phash == phash {
			for i, x := range cur.ids {
				if x == id {
					cur.ids = append(cur.ids[:i], cur.ids[i+1:]...)
					break
				}
			}
			return
		}
		dist := hammingDistance(cur.phash, phash)
		cur = cur.children[dist]
	}
}

func (t *BKTree) searchLocked(node *bkNode, query int64, d int, out *[]int64) {
	dist := hammingDistance(node.phash, query)
	if dist <= d {
		*out = append(*out, node.ids...)
	}
	lo := dist - d
	if lo < 0 {
		lo = 0
	}
	hi := dist + d
	for edge, child := range node.children {
		if edge < lo || edge > hi {
			continue
		}
		t.searchLocked(child, query, d, out)
	}
}

// hammingDistance returns the number of differing bits between the
// unsigned interpretations of a and b.
func hammingDistance(a, b int64) int {
	return bits.OnesCount64(uint64(a) ^ uint64(b))
}

// Registry maps a SQLite handle to its in-memory BKTree. Per-gallery
// galleryCtx registers its handle at startup and deregisters when the
// gallery is closed, so the tree's lifetime tracks the DB. Lookup
// returns nil when the gallery doesn't have a tree wired - hooks that
// fire from a test harness or a half-constructed gallery state then
// no-op cleanly.
type Registry struct {
	mu    sync.RWMutex
	trees map[*db.DB]*BKTree
}

// DefaultRegistry is the process-wide registry the hooks in
// gallery.RecomputeAndStorePhash and Service.OnImageDelete route
// through.
var DefaultRegistry = &Registry{trees: map[*db.DB]*BKTree{}}

// Register attaches tree to database. A subsequent ingest's
// post-store hook then knows where to deposit the new (id, phash).
// Replaces any prior registration for the same database.
func (r *Registry) Register(database *db.DB, tree *BKTree) {
	r.mu.Lock()
	r.trees[database] = tree
	r.mu.Unlock()
}

// Unregister drops the database's registration. Called when the
// gallery context is destroyed (gallery removal, server shutdown).
func (r *Registry) Unregister(database *db.DB) {
	r.mu.Lock()
	delete(r.trees, database)
	r.mu.Unlock()
}

// Lookup returns the registered tree for database, or nil when no
// tree is wired.
func (r *Registry) Lookup(database *db.DB) *BKTree {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.trees[database]
}
