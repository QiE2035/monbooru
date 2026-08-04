package tags

import (
	"cmp"
	"database/sql"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
)

// image_tag_sources is the per-tag provenance ledger: one row per
// (image, tag, source) recording every source that applied or
// re-confirmed the tag, where image_tags.tagger_name only keeps the
// first. Rows are written by the apply paths (never by implication
// fan-out) and die with their image_tags row via the
// trg_image_tags_sources_ad trigger.

// TagSource is one ledger row for an image's tag.
type TagSource struct {
	Source    string
	CreatedAt string
}

// RecordTagSourceTx records that source applied or confirmed tagID on
// imageID. An empty source is the anonymous UI add and is stored as
// 'user'. Exported for the apply paths that write image_tags outside
// this package (internal/tagger's direct-SQL batch).
func RecordTagSourceTx(tx *sql.Tx, imageID, tagID int64, source string) error {
	source = cmp.Or(source, "user")
	_, err := tx.Exec(
		`INSERT OR IGNORE INTO image_tag_sources (image_id, tag_id, source) VALUES (?, ?, ?)`,
		imageID, tagID, source,
	)
	return err
}

// UsedByLabels returns every source that has applied a tag, sorted, for
// the /tags Used-by filter. Free at any catalog size: SQLite skips ahead
// per distinct value over idx_image_tag_sources_source rather than
// walking the ledger.
func (s *Service) UsedByLabels() ([]string, error) {
	return db.QueryStrings(s.db.Read, `SELECT DISTINCT source FROM image_tag_sources ORDER BY source`)
}

// UsedByForTags reports which of labels applied each of tagIDs, keyed by
// tag id, for the /tags Used-by column. One EXISTS probe per (tag,
// label) pair: grouping the ledger by (tag_id, source) would instead
// walk every row a heavily-applied tag carries.
func (s *Service) UsedByForTags(tagIDs []int64, labels []string) (map[int64][]string, error) {
	out := make(map[int64][]string, len(tagIDs))
	if len(tagIDs) == 0 || len(labels) == 0 {
		return out, nil
	}
	labelValues := strings.TrimSuffix(strings.Repeat("(?),", len(labels)), ",")
	labelArgs := make([]any, 0, len(labels))
	for _, l := range labels {
		labelArgs = append(labelArgs, l)
	}
	err := db.Chunked(tagIDs, 500, func(batch []int64) error {
		placeholders, args := db.InPlaceholders(batch)
		rows, err := s.db.Read.Query(
			`WITH src(label) AS (VALUES `+labelValues+`)
			 SELECT t.id, src.label
			 FROM tags t, src
			 WHERE t.id IN (`+placeholders+`)
			   AND EXISTS (SELECT 1 FROM image_tag_sources s WHERE s.source = src.label AND s.tag_id = t.id)
			 ORDER BY t.id, src.label`,
			append(append([]any{}, labelArgs...), args...)...,
		)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			var label string
			if err := rows.Scan(&id, &label); err != nil {
				return err
			}
			out[id] = append(out[id], label)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TagSourcesForImage returns the ledger rows for one image keyed by
// tag id, each tag's sources in recording order.
func (s *Service) TagSourcesForImage(imageID int64) (map[int64][]TagSource, error) {
	rows, err := s.db.Read.Query(
		`SELECT tag_id, source, created_at FROM image_tag_sources
		 WHERE image_id = ? ORDER BY tag_id, created_at, source`, imageID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64][]TagSource{}
	for rows.Next() {
		var tagID int64
		var ts TagSource
		if err := rows.Scan(&tagID, &ts.Source, &ts.CreatedAt); err != nil {
			return nil, err
		}
		out[tagID] = append(out[tagID], ts)
	}
	return out, rows.Err()
}
