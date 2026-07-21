package tags

import (
	"database/sql"
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
	if source == "" {
		source = "user"
	}
	_, err := tx.Exec(
		`INSERT OR IGNORE INTO image_tag_sources (image_id, tag_id, source) VALUES (?, ?, ?)`,
		imageID, tagID, source,
	)
	return err
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
