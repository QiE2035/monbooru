package relations

import (
	"context"
	"database/sql"

	"github.com/monbooru/monbooru/internal/db"
)

// PruneQueue drops the queue rows the current detector settings would
// no longer nominate: phash rows past the distance cap, tag rows under
// the score threshold, every tag row when the pass is off. A pair both
// detectors filed keeps its place under whichever one still backs it,
// demoted to that detector alone. Requeued pairs stay: the operator
// asked to see those, not a detector.
//
// Only tightening a setting strands rows; loosening one leaves pairs
// missing instead, which the next find-pairs run fills.
func PruneQueue(ctx context.Context, database *db.DB, opts FindPairsOptions) (int, error) {
	threshold := opts.TagPairThreshold
	if !opts.TagPairs {
		// Above every reachable score, so the tag side fails for every row.
		threshold = 2
	}
	removed := 0
	err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE potential_relation_pairs SET source = ?, score = NULL
			  WHERE source = ? AND COALESCE(score, 0) < ?`,
			SourcePhash, SourceBoth, threshold,
		); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM potential_relation_pairs WHERE source = ? AND COALESCE(score, 0) < ?`,
			SourceTags, threshold)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		removed += int(n)
		if err := demoteOverDistanceTx(ctx, tx, opts.Distance); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx,
			`DELETE FROM potential_relation_pairs WHERE source = ? AND distance > ?`,
			SourcePhash, opts.Distance)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		removed += int(n)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// demoteOverDistanceTx turns every both-detector row whose pixel
// distance now exceeds the cap into a tag-only row, re-keying it into
// the tag band so the queue order stays consistent. Runs per row
// because the band mapping lives in TagPairDistance.
func demoteOverDistanceTx(ctx context.Context, tx *sql.Tx, distance int) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT a_image_id, b_image_id, COALESCE(score, 0)
		   FROM potential_relation_pairs WHERE source = ? AND distance > ?`,
		SourceBoth, distance)
	if err != nil {
		return err
	}
	type demotion struct {
		a, b  int64
		score float64
	}
	var pending []demotion
	for rows.Next() {
		var d demotion
		if err := rows.Scan(&d.a, &d.b, &d.score); err != nil {
			_ = rows.Close()
			return err
		}
		pending = append(pending, d)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range pending {
		if _, err := tx.ExecContext(ctx,
			`UPDATE potential_relation_pairs SET source = ?, distance = ?
			  WHERE a_image_id = ? AND b_image_id = ?`,
			SourceTags, TagPairDistance(d.score), d.a, d.b,
		); err != nil {
			return err
		}
	}
	return nil
}
