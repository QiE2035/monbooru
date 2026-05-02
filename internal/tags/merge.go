package tags

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/leqwin/monbooru/internal/models"
)

// ErrAliasNameInUse is returned when CreateAlias is asked to bind a name
// that already names a non-alias tag carrying image_tags rows. The
// caller is meant to fall back to the merge flow, which moves those
// rows onto the canonical before installing the alias.
var ErrAliasNameInUse = errors.New("a tag with this name already has image_tags rows; merge it instead")

// CreateAlias declares that name (in categoryID) resolves to canonicalID.
// The cheap path - alias name doesn't yet exist - inserts a fresh
// is_alias=1 row at zero usage. When a tag with the same (name,
// category) already exists, three branches:
//
//   - same id as canonical → reject (self-alias).
//   - already an alias → repoint (UPDATE canonical_tag_id).
//   - non-alias with image_tags rows → ErrAliasNameInUse so the UI can
//     route through MergeTags, which moves the rows onto the canonical.
//   - non-alias with zero usage → upgrade in place (set is_alias=1,
//     canonical_tag_id=canonical, usage_count=0).
//
// Mirrors MergeTags's rating-category and target-not-alias guards so
// the resolver invariants hold the same way.
func (s *Service) CreateAlias(name string, categoryID, canonicalID int64) (*models.Tag, error) {
	normalized, err := ValidateTagName(name)
	if err != nil {
		return nil, err
	}
	if s.ratingCatID != 0 && categoryID == s.ratingCatID {
		return nil, ErrRatingTagImmutable
	}
	if s.isRatingTag(canonicalID) {
		return nil, ErrRatingTagImmutable
	}

	tx, err := s.db.Write.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var canonIsAlias int
	if err := tx.QueryRow(`SELECT is_alias FROM tags WHERE id = ?`, canonicalID).Scan(&canonIsAlias); err == sql.ErrNoRows {
		return nil, ErrTagNotFound
	} else if err != nil {
		return nil, err
	}
	if canonIsAlias == 1 {
		return nil, fmt.Errorf("cannot alias to a tag that is itself an alias")
	}

	var existingID int64
	var existingIsAlias int
	var existingUsage int
	err = tx.QueryRow(
		`SELECT id, is_alias, usage_count FROM tags WHERE name = ? AND category_id = ?`,
		normalized, categoryID,
	).Scan(&existingID, &existingIsAlias, &existingUsage)
	switch {
	case err == sql.ErrNoRows:
		var id int64
		if err := tx.QueryRow(
			`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id, usage_count) VALUES (?, ?, 1, ?, 0) RETURNING id`,
			normalized, categoryID, canonicalID,
		).Scan(&id); err != nil {
			return nil, fmt.Errorf("inserting alias: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return s.GetTag(id)
	case err != nil:
		return nil, err
	}
	if existingID == canonicalID {
		return nil, fmt.Errorf("cannot alias a tag to itself")
	}
	if existingIsAlias == 1 {
		if _, err := tx.Exec(
			`UPDATE tags SET canonical_tag_id = ? WHERE id = ?`, canonicalID, existingID,
		); err != nil {
			return nil, err
		}
	} else if existingUsage > 0 {
		return nil, ErrAliasNameInUse
	} else {
		if _, err := tx.Exec(
			`UPDATE tags SET is_alias = 1, canonical_tag_id = ?, usage_count = 0 WHERE id = ?`,
			canonicalID, existingID,
		); err != nil {
			return nil, err
		}
	}
	// Same alias-keyed-rows invariant MergeTags maintains: a tag
	// becoming (or repointed as) an alias must not be left as parent or
	// implied in tag_implications. Zero-usage means there are no
	// orphan image_tags rows, but a future fan-out hitting a dangling
	// edge would still misbehave.
	if err := repointImplicationsToCanonicalTx(tx, existingID, canonicalID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetTag(existingID)
}

// MergeTags makes aliasID an alias of canonicalID, moving all
// image_tags rows from alias to canonical and marking the alias row.
func (s *Service) MergeTags(aliasID, canonicalID int64) error {
	if aliasID == canonicalID {
		return fmt.Errorf("cannot merge a tag into itself")
	}
	if s.isRatingTag(aliasID) || s.isRatingTag(canonicalID) {
		return ErrRatingTagImmutable
	}

	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Refuse merge-into-alias: the alias resolver only follows one hop
	// (COALESCE(canonical_tag_id, id) and GetOrCreateTag's single lookup),
	// so a two-hop chain would silently drop rows.
	var targetIsAlias int
	if err := tx.QueryRow(`SELECT is_alias FROM tags WHERE id = ?`, canonicalID).Scan(&targetIsAlias); err != nil {
		return fmt.Errorf("target tag not found")
	}
	if targetIsAlias == 1 {
		return fmt.Errorf("cannot merge into a tag that is itself an alias")
	}

	// (a) Move image_tags from alias to canonical in three set-based
	// statements instead of three statements per row. The previous
	// loop paid 3 round-trips per image_tags row through the single
	// writer; on a popular alias against the documented 10M-image_tags
	// ceiling that meant tens of millions of statements per merge.
	//
	// Step 1: insert canonical-side rows for images that don't already
	// have one. INSERT OR IGNORE keeps the existing canonical when the
	// image already had it, so no double-count.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name)
		 SELECT image_id, ?, is_auto, is_implied, confidence, tagger_name
		 FROM image_tags WHERE tag_id = ?`,
		canonicalID, aliasID,
	); err != nil {
		return fmt.Errorf("merge insert canonical rows: %w", err)
	}
	// Step 2: where the canonical was on the image only as an implied
	// row but the alias was user-owned, promote canonical to user-
	// owned. Mirrors addTagToImageTxReportingDup's promotion shape so
	// the next removal of an unrelated parent does not leave the image
	// with no user-owned tags. is_auto / confidence / tagger_name are
	// inherited from the alias row by the same convention.
	if _, err := tx.Exec(
		`UPDATE image_tags AS c
		 SET is_implied = 0, is_auto = a.is_auto, confidence = a.confidence, tagger_name = a.tagger_name
		 FROM image_tags AS a
		 WHERE c.image_id = a.image_id
		   AND c.tag_id = ?
		   AND a.tag_id = ?
		   AND c.is_implied = 1
		   AND a.is_implied = 0`,
		canonicalID, aliasID,
	); err != nil {
		return fmt.Errorf("merge promote canonical rows: %w", err)
	}
	// Step 3: drop every alias-keyed row.
	if _, err := tx.Exec(
		`DELETE FROM image_tags WHERE tag_id = ?`, aliasID,
	); err != nil {
		return fmt.Errorf("merge drop alias rows: %w", err)
	}

	// (b) Mark aliasID as an alias of canonicalID.
	if _, err := tx.Exec(
		`UPDATE tags SET is_alias = 1, canonical_tag_id = ?, usage_count = 0 WHERE id = ?`,
		canonicalID, aliasID,
	); err != nil {
		return err
	}

	// (c) Move tag_implications edges off the alias onto the canonical.
	// AddImplication refuses aliases on either side, so a row keyed on
	// aliasID after the flip would mean the implied closure walked from
	// the canonical never sees it - leaving image_tags rows that were
	// inserted as is_implied=1 because of the old alias-keyed edge with
	// no parent on the image to justify them on later removal.
	if err := repointImplicationsToCanonicalTx(tx, aliasID, canonicalID); err != nil {
		return err
	}

	// (d) Recount canonical's usage_count from non-missing images, the
	// same convention RecalcDB enforces, so a merge doesn't inflate the
	// count past what the next recalc would emit.
	if _, err := tx.Exec(
		`UPDATE tags SET usage_count = (
			SELECT COUNT(*) FROM image_tags it
			JOIN images i ON i.id = it.image_id
			WHERE it.tag_id = ? AND i.is_missing = 0
		) WHERE id = ?`,
		canonicalID, canonicalID,
	); err != nil {
		return err
	}

	return tx.Commit()
}

// repointImplicationsToCanonicalTx rewrites every tag_implications row
// that names aliasID as parent or implied to name canonicalID instead,
// then drops the alias-keyed rows. INSERT OR IGNORE collapses edges
// the canonical already held; the != canonicalID filters skip the
// would-be self-edges that an alias-into-its-own-implier merge could
// otherwise create. A merge can still synthesize a cycle that
// AddImplication would have refused at create time; the bounded
// MaxImplicationDepth walk used by every consumer absorbs that.
func repointImplicationsToCanonicalTx(tx *sql.Tx, aliasID, canonicalID int64) error {
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO tag_implications (parent_tag_id, implied_tag_id)
		 SELECT ?, implied_tag_id FROM tag_implications
		 WHERE parent_tag_id = ? AND implied_tag_id != ?`,
		canonicalID, aliasID, canonicalID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO tag_implications (parent_tag_id, implied_tag_id)
		 SELECT parent_tag_id, ? FROM tag_implications
		 WHERE implied_tag_id = ? AND parent_tag_id != ?`,
		canonicalID, aliasID, canonicalID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM tag_implications WHERE parent_tag_id = ? OR implied_tag_id = ?`,
		aliasID, aliasID,
	); err != nil {
		return err
	}
	return nil
}
