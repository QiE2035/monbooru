package relations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
)

// IncrementalProbeDistance is the Hamming distance the on-ingest
// probe (fired from gallery.PhashHooks.OnStored) walks the BK-tree
// for. Atomic so the config-edit path can flip it without a restart.
var IncrementalProbeDistance atomic.Int32

// IncrementalProbeEnabled toggles the on-ingest probe. Default true;
// operators who'd rather batch can disable it through the same
// config block.
var IncrementalProbeEnabled atomic.Bool

func init() {
	IncrementalProbeDistance.Store(4)
	IncrementalProbeEnabled.Store(true)
}

// FindPairsOptions controls one find-pairs invocation. Distance clamps
// to 0..12 by the caller per RELATIONS.md §5.1; Replace=true wipes the
// queue before re-scanning, otherwise existing rows survive and only
// new candidates are added.
type FindPairsOptions struct {
	Distance       int
	Replace        bool
	ThumbnailsPath string
}

// FindPairsProgress is the per-row callback the caller's job manager
// uses to drive the status-bar progress percentage. Phase strings:
// "phashing" while the lazy phash backfill runs, "probing" while the
// BK-tree scan runs.
type FindPairsProgress func(processed, total int, phase string)

// FindPairs walks every visible image, computes any missing phash
// inline (the lazy compute documented in §5.2), probes the per-gallery
// BK-tree for candidates within opts.Distance, and inserts canonicalised
// (a, b, distance) rows into potential_relation_pairs.
//
// Skips already-related pairs and pairs in not_related_pairs / the
// existing queue (unless opts.Replace wipes the queue first). 500-row
// transactions on insert match the implication-fanout cadence.
func FindPairs(ctx context.Context, database *db.DB, tree *BKTree, opts FindPairsOptions, progress FindPairsProgress) (added int, err error) {
	if tree == nil {
		return 0, errors.New("relations: nil bk-tree")
	}
	if opts.Replace {
		if _, err := database.Write.ExecContext(ctx, `DELETE FROM potential_relation_pairs`); err != nil {
			return 0, fmt.Errorf("wipe queue: %w", err)
		}
		tree.Reset()
	}

	// Pull the full id + phash list. NULL phashes get computed inline
	// during the walk; the tree picks them up via the OnStored hook.
	type row struct {
		id    int64
		phash sql.NullInt64
	}
	rows, err := database.Read.Query(`SELECT id, phash FROM images WHERE is_missing = 0 ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("load image ids: %w", err)
	}
	var entries []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.phash); err != nil {
			rows.Close()
			return 0, err
		}
		entries = append(entries, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if err := tree.EnsureBuilt(database); err != nil {
		return 0, fmt.Errorf("bk-tree build: %w", err)
	}

	total := len(entries)
	now := time.Now().UTC().Format(time.RFC3339)
	const txChunk = 500
	var pending []pairToInsert

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		tx, err := database.Write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		stmt, err := tx.Prepare(`INSERT OR IGNORE INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?, ?, ?, ?)`)
		if err != nil {
			tx.Rollback()
			return err
		}
		for _, p := range pending {
			if _, err := stmt.Exec(p.a, p.b, p.distance, now); err != nil {
				stmt.Close()
				tx.Rollback()
				return err
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
		pending = pending[:0]
		return nil
	}

	for idx, e := range entries {
		if ctx.Err() != nil {
			if flushErr := flush(); flushErr != nil {
				logx.Debugf("find-pairs flush during cancel: %v", flushErr)
			}
			return added, ctx.Err()
		}
		// Lazy phash compute when the row's still NULL.
		phash := e.phash
		if !phash.Valid {
			if progress != nil {
				progress(idx, total, "phashing")
			}
			if err := gallery.RecomputeAndStorePhash(ctx, database, e.id, opts.ThumbnailsPath); err != nil {
				logx.Debugf("find-pairs phash %d: %v", e.id, err)
				continue
			}
			// Re-read so the BK-tree probe below sees what was just
			// stored. The OnStored hook has already added the entry to
			// the tree if it was built before the row ran through.
			if err := database.Read.QueryRow(`SELECT phash FROM images WHERE id = ?`, e.id).Scan(&phash); err != nil {
				logx.Debugf("find-pairs reread %d: %v", e.id, err)
				continue
			}
			if !phash.Valid {
				continue
			}
			// EnsureBuilt was called above before the OnStored hook
			// could have fired, but a row whose phash was NULL at the
			// scan time wouldn't have been in the just-built tree.
			// Insert now so it participates in subsequent probes inside
			// this same job.
			tree.Insert(e.id, phash.Int64)
		}
		if progress != nil && idx%64 == 0 {
			progress(idx, total, "probing")
		}
		candidates := tree.SearchWithinDistance(phash.Int64, opts.Distance)
		for _, cid := range candidates {
			if cid <= e.id {
				continue // canonicalise a < b; the symmetric pair will surface when we hit b
			}
			// Skip if pair already carries a real relation or is on
			// the not-related list. Cheap correlated COUNTs that ride
			// covering indexes; well under a millisecond apiece.
			already, err := pairAlreadyKnown(ctx, database, e.id, cid)
			if err != nil {
				return added, err
			}
			if already {
				continue
			}
			pending = append(pending, pairToInsert{
				a: e.id, b: cid, distance: hammingDistance(phash.Int64, lookupPhashFromTree(tree, cid)),
			})
			added++
			if len(pending) >= txChunk {
				if err := flush(); err != nil {
					return added, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return added, err
	}
	if progress != nil {
		progress(total, total, "probing")
	}
	return added, nil
}

type pairToInsert struct {
	a, b     int64
	distance int
}

// pairAlreadyKnown reports whether (a, b) is already represented by any
// relation table or by not_related_pairs / the existing queue. The
// queue check is intentional: re-running find-pairs without replace=true
// shouldn't insert duplicate canonical rows.
func pairAlreadyKnown(ctx context.Context, database *db.DB, a, b int64) (bool, error) {
	tx, err := database.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Use the shared service-layer helper for the relation tables;
	// `not_related` blocks find-pairs from resurfacing rejections.
	if got, err := pairHasOtherRelationTx(txAsSqlTx(tx), a, b, ""); err != nil {
		return false, err
	} else if got {
		return true, nil
	}
	lo, hi := canonicalPair(a, b)
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// txAsSqlTx adapts a read-only sql.Tx interface so the helper that
// usually takes a write-side *sql.Tx works against the read pool too.
// pairHasOtherRelationTx only does SELECTs, so the read pool is
// sufficient.
func txAsSqlTx(tx *sql.Tx) *sql.Tx { return tx }

// lookupPhashFromTree retrieves an image's phash through the tree's
// id->phash index. Used to compute the stored Hamming distance for
// queue rows without re-querying SQLite per candidate.
func lookupPhashFromTree(tree *BKTree, id int64) int64 {
	tree.mu.RLock()
	defer tree.mu.RUnlock()
	return tree.idIndex[id]
}

// incrementalProbe runs one BK-tree probe for the just-stored (id,
// phash) row and inserts canonicalised pairs into the queue. Skips
// already-related / not-related / queued pairs. Best-effort: an
// error short-circuits the rest of the candidates rather than
// failing the surrounding ingest.
func incrementalProbe(database *db.DB, tree *BKTree, id, phash int64, distance int) error {
	candidates := tree.SearchWithinDistance(phash, distance)
	if len(candidates) == 0 {
		return nil
	}
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, cid := range candidates {
		if cid == id {
			continue
		}
		lo, hi := id, cid
		if hi < lo {
			lo, hi = hi, lo
		}
		known, err := pairAlreadyKnown(ctx, database, lo, hi)
		if err != nil {
			return err
		}
		if known {
			continue
		}
		other := lookupPhashFromTree(tree, cid)
		if _, err := database.Write.Exec(
			`INSERT OR IGNORE INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at) VALUES (?, ?, ?, ?)`,
			lo, hi, hammingDistance(phash, other), now,
		); err != nil {
			return err
		}
	}
	return nil
}
