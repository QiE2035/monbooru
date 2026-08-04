package relations

import (
	"math/bits"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
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
	// lastUsed stamps the last search so the reclaim loop can drop a
	// tree nobody is browsing against.
	lastUsed atomic.Int64
}

type bkNode struct {
	phash int64
	ids   []int64
	// Edges keyed by distance, as a slice rather than a map: fanout is
	// at most 65 and in practice a handful, where a map costs an order
	// of magnitude more per node than the linear scan saves.
	children []bkEdge
}

type bkEdge struct {
	dist int
	node *bkNode
}

// child returns the edge at distance dist, or nil.
func (n *bkNode) child(dist int) *bkNode {
	for i := range n.children {
		if n.children[i].dist == dist {
			return n.children[i].node
		}
	}
	return nil
}

// NewBKTree returns an empty tree. Use BuildFromDB to populate from
// SQLite, or call Insert directly when feeding individual rows.
func NewBKTree() *BKTree {
	return &BKTree{idIndex: make(map[int64]int64)}
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
	t.resetLocked()
}

// ReleaseIdle drops a built tree nobody has searched for at least
// `after`, returning whether it did. The next query rebuilds it from
// the phash column; on a large library the index is tens of megabytes
// that only a relations or phash: browse needs.
func (t *BKTree) ReleaseIdle(after time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.built.Load() || time.Since(time.Unix(0, t.lastUsed.Load())) < after {
		return false
	}
	t.resetLocked()
	return true
}

func (t *BKTree) resetLocked() {
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
	t.lastUsed.Store(time.Now().UnixNano())
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
// are included. Result order is unspecified. The second return reports
// whether a built tree answered: a caller that gets false has to fall
// back rather than read the empty result as "no matches", since the
// tree can be dropped between its EnsureBuilt and this call.
func (t *BKTree) SearchWithinDistance(query int64, d int) ([]int64, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	t.lastUsed.Store(time.Now().UnixNano())
	var out []int64
	if t.root != nil {
		t.searchLocked(t.root, query, d, &out)
	}
	return out, t.built.Load()
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
		if child := cur.child(dist); child != nil {
			cur = child
			continue
		}
		cur.children = append(cur.children, bkEdge{dist: dist, node: &bkNode{phash: phash, ids: []int64{id}}})
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
			if i := slices.Index(cur.ids, id); i >= 0 {
				cur.ids = slices.Delete(cur.ids, i, i+1)
			}
			return
		}
		cur = cur.child(hammingDistance(cur.phash, phash))
	}
}

func (t *BKTree) searchLocked(node *bkNode, query int64, d int, out *[]int64) {
	dist := hammingDistance(node.phash, query)
	if dist <= d {
		*out = append(*out, node.ids...)
	}
	lo := max(dist-d, 0)
	hi := dist + d
	for _, edge := range node.children {
		if edge.dist < lo || edge.dist > hi {
			continue
		}
		t.searchLocked(edge.node, query, d, out)
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
// gallery.RecomputeAndStorePhash and Service.OnImageDeleteTx route
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
