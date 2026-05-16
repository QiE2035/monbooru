// Package relations manages the operator-declared graph between images:
// duplicate groups, alternate groups, directed version chains, directed
// derivative trees, and the "not related" rejection set.
//
// Merge invariants and the per-type teardown rules are documented in
// project/RELATIONS.md §3 and §6.4. The Service is the only thing the
// rest of the codebase calls to mutate the graph; each mutation runs
// inside a single transaction so partial states never surface.
package relations

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/leqwin/monbooru/internal/db"
)

var (
	// ErrSelfRelation is returned when both ends of a relation point at
	// the same image.
	ErrSelfRelation = errors.New("relations: pair refers to a single image")

	// ErrRelationConflict is returned when a pair already carries a
	// relation of a different type. The operator must explicitly
	// remove the existing relation before declaring a new one.
	ErrRelationConflict = errors.New("relations: pair already has a different relation")

	// ErrVersionExists is returned when adding a version edge would
	// give a child a second parent or a parent a second child. Strict
	// chains, not trees.
	ErrVersionExists = errors.New("relations: version edge already exists on one side")

	// ErrDerivativeExists is returned when adding a derivative edge
	// would assign a derivative a second source.
	ErrDerivativeExists = errors.New("relations: derivative already has a source")

	// ErrNotInGroup is returned by PromoteToOriginal when the named
	// image isn't currently a member of the target dup group.
	ErrNotInGroup = errors.New("relations: image is not a member of the group")
)

// Service is the transactional boundary for relations mutations.
type Service struct {
	db *db.DB
}

// New returns a Service backed by the provided database.
func New(database *db.DB) *Service {
	return &Service{db: database}
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// canonicalPair returns (min, max) so symmetric relations
// (not_related, in particular) live as a single canonical row
// regardless of caller argument order.
func canonicalPair(a, b int64) (int64, int64) {
	if a < b {
		return a, b
	}
	return b, a
}

// AddDuplicate marks images a and b as duplicates. Handles the five
// cases from §6.4 in a single transaction: both singletons, one
// existing member, the other existing member, same group already, and
// two different groups (merge). The same transaction merges the
// operator's alternate-group state when needed because duplicate-of-A
// implies alternate-of-everything-A-is-alternate-of.
func (s *Service) AddDuplicate(a, b int64) error {
	if a == b {
		return ErrSelfRelation
	}
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if conflict, err := pairHasOtherRelationTx(tx, a, b, "duplicate"); err != nil {
		return err
	} else if conflict {
		return ErrRelationConflict
	}
	if err := mergeIntoDupGroupTx(tx, a, b); err != nil {
		return err
	}
	if err := propagateAltOnDuplicateTx(tx, a, b); err != nil {
		return err
	}
	if err := pruneQueueForGroupTx(tx, "dup_group_members", a); err != nil {
		return err
	}
	if err := pruneQueueForGroupTx(tx, "alt_group_members", a); err != nil {
		return err
	}
	return tx.Commit()
}

// AddAlternate marks images a and b as alternates.
func (s *Service) AddAlternate(a, b int64) error {
	if a == b {
		return ErrSelfRelation
	}
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if conflict, err := pairHasOtherRelationTx(tx, a, b, "alternate"); err != nil {
		return err
	} else if conflict {
		return ErrRelationConflict
	}
	if err := mergeIntoAltGroupTx(tx, a, b); err != nil {
		return err
	}
	if err := pruneQueueForGroupTx(tx, "alt_group_members", a); err != nil {
		return err
	}
	return tx.Commit()
}

// AddVersionEdge declares child as the newer version of parent. Refuses
// if either side is already part of a version edge - the chain is
// strict.
func (s *Service) AddVersionEdge(parent, child int64) error {
	if parent == child {
		return ErrSelfRelation
	}
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if conflict, err := pairHasOtherRelationTx(tx, parent, child, "version"); err != nil {
		return err
	} else if conflict {
		return ErrRelationConflict
	}
	var n int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM version_edges WHERE child_image_id IN (?, ?) OR parent_image_id IN (?, ?)`,
		parent, child, parent, child,
	).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrVersionExists
	}
	if _, err := tx.Exec(
		`INSERT INTO version_edges (child_image_id, parent_image_id, created_at) VALUES (?, ?, ?)`,
		child, parent, nowISO(),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// AddDerivativeEdge declares derivative was made from source. A source
// can carry many derivatives (tree); each derivative has exactly one
// source.
func (s *Service) AddDerivativeEdge(source, derivative int64) error {
	if source == derivative {
		return ErrSelfRelation
	}
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if conflict, err := pairHasOtherRelationTx(tx, source, derivative, "derivative"); err != nil {
		return err
	} else if conflict {
		return ErrRelationConflict
	}
	var n int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM derivative_edges WHERE derivative_image_id = ?`, derivative,
	).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrDerivativeExists
	}
	if _, err := tx.Exec(
		`INSERT INTO derivative_edges (derivative_image_id, source_image_id, created_at) VALUES (?, ?, ?)`,
		derivative, source, nowISO(),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// AddNotRelated records the canonicalised pair so it never surfaces in
// the find-pairs queue again.
func (s *Service) AddNotRelated(a, b int64) error {
	if a == b {
		return ErrSelfRelation
	}
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if conflict, err := pairHasOtherRelationTx(tx, a, b, "not_related"); err != nil {
		return err
	} else if conflict {
		return ErrRelationConflict
	}
	lo, hi := canonicalPair(a, b)
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO not_related_pairs (a_image_id, b_image_id, created_at) VALUES (?, ?, ?)`,
		lo, hi, nowISO(),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveDupMember unlinks one image from its duplicate group. Idempotent:
// a no-op when the image isn't in a group. If the removal would leave
// the group with a single member, the group is dissolved. If the removed
// image was the original, the largest remaining member is promoted.
func (s *Service) RemoveDupMember(imageID int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := removeDupMemberTx(tx, imageID); err != nil {
		return err
	}
	return tx.Commit()
}

func removeDupMemberTx(tx *sql.Tx, imageID int64) error {
	gid, err := lookupGroupIDTx(tx, "dup_group_members", imageID)
	if err != nil {
		return err
	}
	if !gid.Valid {
		return nil
	}
	var memberCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM dup_group_members WHERE group_id = ?`, gid.Int64).Scan(&memberCount); err != nil {
		return err
	}
	if memberCount <= 2 {
		_, err := tx.Exec(`DELETE FROM dup_groups WHERE id = ?`, gid.Int64)
		return err
	}
	var current int64
	if err := tx.QueryRow(`SELECT original_image_id FROM dup_groups WHERE id = ?`, gid.Int64).Scan(&current); err != nil {
		return err
	}
	if current == imageID {
		var newOriginal int64
		err := tx.QueryRow(`
			SELECT m.image_id FROM dup_group_members m
			JOIN images i ON i.id = m.image_id
			WHERE m.group_id = ? AND m.image_id != ?
			ORDER BY i.file_size DESC, m.image_id DESC
			LIMIT 1`, gid.Int64, imageID,
		).Scan(&newOriginal)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE dup_groups SET original_image_id = ? WHERE id = ?`, newOriginal, gid.Int64); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`DELETE FROM dup_group_members WHERE image_id = ?`, imageID)
	return err
}

// DissolveDupGroup drops the entire duplicate group. CASCADE clears
// every member row. Idempotent.
func (s *Service) DissolveDupGroup(groupID int64) error {
	_, err := s.db.Write.Exec(`DELETE FROM dup_groups WHERE id = ?`, groupID)
	return err
}

// NextOriginalIfRemoved returns the id removeDupMemberTx would promote
// to original if `removeID` left `groupID`. Same ORDER BY as the
// promotion itself so the preview the UI shows the operator matches
// what the unlink will commit. Returns (0, nil) when the group has
// fewer than three members - the group dissolves and there is no
// new original to name.
func (s *Service) NextOriginalIfRemoved(groupID, removeID int64) (int64, error) {
	var n int
	if err := s.db.Read.QueryRow(
		`SELECT COUNT(*) FROM dup_group_members WHERE group_id = ?`, groupID,
	).Scan(&n); err != nil {
		return 0, err
	}
	if n < 3 {
		return 0, nil
	}
	var nextID int64
	err := s.db.Read.QueryRow(`
		SELECT m.image_id FROM dup_group_members m
		JOIN images i ON i.id = m.image_id
		WHERE m.group_id = ? AND m.image_id != ?
		ORDER BY i.file_size DESC, m.image_id DESC
		LIMIT 1`, groupID, removeID,
	).Scan(&nextID)
	if err != nil {
		return 0, err
	}
	return nextID, nil
}

// PromoteToOriginal sets imageID as the original of groupID. Errors
// with ErrNotInGroup when imageID isn't a member of the group.
func (s *Service) PromoteToOriginal(groupID, imageID int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM dup_group_members WHERE group_id = ? AND image_id = ?`, groupID, imageID,
	).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotInGroup
	}
	if _, err := tx.Exec(`UPDATE dup_groups SET original_image_id = ? WHERE id = ?`, imageID, groupID); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveAltMember unlinks one image from its alternate group.
// Idempotent. Dissolves the group when reduced to a singleton.
func (s *Service) RemoveAltMember(imageID int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := removeAltMemberTx(tx, imageID); err != nil {
		return err
	}
	return tx.Commit()
}

func removeAltMemberTx(tx *sql.Tx, imageID int64) error {
	gid, err := lookupGroupIDTx(tx, "alt_group_members", imageID)
	if err != nil {
		return err
	}
	if !gid.Valid {
		return nil
	}
	var memberCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM alt_group_members WHERE group_id = ?`, gid.Int64).Scan(&memberCount); err != nil {
		return err
	}
	if memberCount <= 2 {
		_, err := tx.Exec(`DELETE FROM alt_groups WHERE id = ?`, gid.Int64)
		return err
	}
	_, err = tx.Exec(`DELETE FROM alt_group_members WHERE image_id = ?`, imageID)
	return err
}

// DissolveAltGroup drops the entire alternate group. CASCADE clears
// every member row. Idempotent.
func (s *Service) DissolveAltGroup(groupID int64) error {
	_, err := s.db.Write.Exec(`DELETE FROM alt_groups WHERE id = ?`, groupID)
	return err
}

// RemoveVersionEdge deletes the parent -> child edge if it exists.
// Idempotent.
func (s *Service) RemoveVersionEdge(parent, child int64) error {
	_, err := s.db.Write.Exec(
		`DELETE FROM version_edges WHERE parent_image_id = ? AND child_image_id = ?`, parent, child,
	)
	return err
}

// RemoveDerivativeEdge deletes the source -> derivative edge if it
// exists. Idempotent.
func (s *Service) RemoveDerivativeEdge(source, derivative int64) error {
	_, err := s.db.Write.Exec(
		`DELETE FROM derivative_edges WHERE source_image_id = ? AND derivative_image_id = ?`, source, derivative,
	)
	return err
}

// RemoveNotRelated forgets a previously-rejected pair so it becomes
// eligible to resurface in find-pairs again. Idempotent.
func (s *Service) RemoveNotRelated(a, b int64) error {
	lo, hi := canonicalPair(a, b)
	_, err := s.db.Write.Exec(`DELETE FROM not_related_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi)
	return err
}

// ClearVersionEdgesInvolving drops every version_edge that names a or
// b on either side, so a subsequent AddVersionEdge can land a fresh
// edge in the operator-chosen direction. Used by the detail-page
// "Replace existing version edge" affordance when the conflict is
// with a third image (which ClearBetween wouldn't touch).
func (s *Service) ClearVersionEdgesInvolving(a, b int64) error {
	_, err := s.db.Write.Exec(
		`DELETE FROM version_edges WHERE child_image_id IN (?, ?) OR parent_image_id IN (?, ?)`,
		a, b, a, b,
	)
	return err
}

// ClearDerivativeSourceOf drops the derivative_edge that names
// `derivative` as its derivative side. The schema allows only one
// source per derivative, so this is the single row that blocks a
// re-source. Used by the detail-page "Replace existing source"
// affordance.
func (s *Service) ClearDerivativeSourceOf(derivative int64) error {
	_, err := s.db.Write.Exec(
		`DELETE FROM derivative_edges WHERE derivative_image_id = ?`,
		derivative,
	)
	return err
}

// ClearBetween drops every relation row that connects a and b, in one
// transaction. Group-shaped relations (duplicate, alternate) keep the
// rest of the group intact - only b's membership goes if the two
// shared a group. Used by the detail-page "Overwrite" affordance so a
// follow-up Add* succeeds without first asking the operator to unlink
// the previous relation by hand.
func (s *Service) ClearBetween(a, b int64) error {
	if a == b {
		return ErrSelfRelation
	}
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if share, err := pairShareGroupTx(tx, "dup_group_members", a, b); err != nil {
		return err
	} else if share {
		if err := removeDupMemberTx(tx, b); err != nil {
			return err
		}
	}
	if share, err := pairShareGroupTx(tx, "alt_group_members", a, b); err != nil {
		return err
	} else if share {
		if err := removeAltMemberTx(tx, b); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`DELETE FROM version_edges WHERE (child_image_id = ? AND parent_image_id = ?) OR (child_image_id = ? AND parent_image_id = ?)`,
		a, b, b, a,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM derivative_edges WHERE (derivative_image_id = ? AND source_image_id = ?) OR (derivative_image_id = ? AND source_image_id = ?)`,
		a, b, b, a,
	); err != nil {
		return err
	}
	lo, hi := canonicalPair(a, b)
	if _, err := tx.Exec(`DELETE FROM not_related_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi); err != nil {
		return err
	}
	return tx.Commit()
}

// CopyTagsFromDuplicatesToOriginal inserts every image_tag carried by
// a non-original member of groupID onto the original. Rating tags are
// excluded (the rating system has highest-wins semantics, so a copy
// would silently bump the original's level). INSERT OR IGNORE makes
// the operation idempotent. Returns the count of newly added rows
// across the group.
//
// Used by the "Copy tags from duplicates to original" affordance on
// the Relations page's duplicate-group card and at session-end
// (RELATIONS.md §6.6). Runs in one transaction so the per-tag
// usage_count refresh stays consistent.
func (s *Service) CopyTagsFromDuplicatesToOriginal(groupID int64) (int, error) {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var original int64
	if err := tx.QueryRow(`SELECT original_image_id FROM dup_groups WHERE id = ?`, groupID).Scan(&original); err != nil {
		return 0, err
	}
	// Find the rating category id once; we use it to exclude rating
	// tags from the copy (highest-wins semantics handles them already).
	var ratingCatID sql.NullInt64
	if err := tx.QueryRow(`SELECT id FROM tag_categories WHERE name = 'rating'`).Scan(&ratingCatID); err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	res, err := tx.Exec(`
		INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name, created_at)
		SELECT ?, it.tag_id, 0, 0, NULL, NULL, ?
		FROM image_tags it
		JOIN dup_group_members m ON m.image_id = it.image_id
		LEFT JOIN tags t ON t.id = it.tag_id
		WHERE m.group_id = ? AND m.image_id != ?
		  AND (? IS NULL OR t.category_id != ?)`,
		original, nowISO(), groupID, original, ratingCatID, ratingCatID,
	)
	if err != nil {
		return 0, err
	}
	added, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(added), nil
}

// OnImageDelete fixes up dup_groups.original_image_id (no FK CASCADE)
// and dissolves singleton groups so the caller's subsequent
// `DELETE FROM images WHERE id = ?` doesn't fail on the NOT NULL FK.
// Called from gallery.DeleteImage right before the image row is
// removed; the FK CASCADE on the member tables takes care of the
// dependent rows. Also drops the image from this gallery's in-memory
// BK-tree (if one is built) so subsequent phash queries don't surface
// a stale id.
func (s *Service) OnImageDelete(imageID int64) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := handleDupGroupOnDeleteTx(tx, imageID); err != nil {
		return err
	}
	if err := handleAltGroupOnDeleteTx(tx, imageID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if tree := DefaultRegistry.Lookup(s.db); tree != nil && tree.Built() {
		tree.Remove(imageID)
	}
	return nil
}

// lookupGroupIDTx returns the dup_group_members.group_id or
// alt_group_members.group_id for imageID. sql.NullInt64{Valid:false}
// when the image isn't currently in any group of that type.
func lookupGroupIDTx(tx *sql.Tx, table string, imageID int64) (sql.NullInt64, error) {
	var gid sql.NullInt64
	q := fmt.Sprintf(`SELECT group_id FROM %s WHERE image_id = ?`, table)
	err := tx.QueryRow(q, imageID).Scan(&gid)
	if err == sql.ErrNoRows {
		return sql.NullInt64{}, nil
	}
	if err != nil {
		return sql.NullInt64{}, err
	}
	return gid, nil
}

// pairHasOtherRelationTx reports whether the pair already carries any
// declared relation outside of `ignore` ("duplicate", "alternate",
// "version", "derivative", "not_related"). Used by every Add* method
// to short-circuit before mutating - the spec demands at most one
// type per pair.
func pairHasOtherRelationTx(tx *sql.Tx, a, b int64, ignore string) (bool, error) {
	if ignore != "duplicate" {
		ok, err := pairShareGroupTx(tx, "dup_group_members", a, b)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	if ignore != "alternate" {
		ok, err := pairShareGroupTx(tx, "alt_group_members", a, b)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	if ignore != "version" {
		var n int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM version_edges
			 WHERE (child_image_id = ? AND parent_image_id = ?)
			    OR (child_image_id = ? AND parent_image_id = ?)`,
			a, b, b, a,
		).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	if ignore != "derivative" {
		var n int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM derivative_edges
			 WHERE (derivative_image_id = ? AND source_image_id = ?)
			    OR (derivative_image_id = ? AND source_image_id = ?)`,
			a, b, b, a,
		).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	if ignore != "not_related" {
		lo, hi := canonicalPair(a, b)
		var n int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM not_related_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi,
		).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

// pruneQueueForGroupTx deletes potential_relation_pairs rows whose
// endpoints are both members of the group that `anchor` now belongs
// to. Resolving one pair in a group can make other queue rows
// redundant (their endpoints land in the same group via the merge);
// this sweeps them so the session UI never asks the operator to
// re-decide a pair that is already inside a declared group. `anchor`
// being a non-member is a quiet no-op.
func pruneQueueForGroupTx(tx *sql.Tx, table string, anchor int64) error {
	gid, err := lookupGroupIDTx(tx, table, anchor)
	if err != nil {
		return err
	}
	if !gid.Valid {
		return nil
	}
	q := fmt.Sprintf(`
		DELETE FROM potential_relation_pairs
		WHERE a_image_id IN (SELECT image_id FROM %s WHERE group_id = ?)
		  AND b_image_id IN (SELECT image_id FROM %s WHERE group_id = ?)`, table, table)
	_, err = tx.Exec(q, gid.Int64, gid.Int64)
	return err
}

// pairShareGroupTx reports whether a and b sit in the same group of
// the given membership table (dup_group_members or alt_group_members).
func pairShareGroupTx(tx *sql.Tx, table string, a, b int64) (bool, error) {
	q := fmt.Sprintf(`
		SELECT COUNT(*) FROM %s m1
		JOIN %s m2 ON m1.group_id = m2.group_id
		WHERE m1.image_id = ? AND m2.image_id = ?`, table, table)
	var n int
	if err := tx.QueryRow(q, a, b).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// mergeIntoDupGroupTx is the dup-group five-case merge.
func mergeIntoDupGroupTx(tx *sql.Tx, a, b int64) error {
	groupA, err := lookupGroupIDTx(tx, "dup_group_members", a)
	if err != nil {
		return err
	}
	groupB, err := lookupGroupIDTx(tx, "dup_group_members", b)
	if err != nil {
		return err
	}
	switch {
	case !groupA.Valid && !groupB.Valid:
		// The caller has already decided which side is the original by
		// passing it first; the session UI puts the bigger-filesize image
		// in slot `a` by default. Existing-group cases below preserve
		// whichever original is already in place.
		var gid int64
		if err := tx.QueryRow(
			`INSERT INTO dup_groups (original_image_id, created_at) VALUES (?, ?) RETURNING id`,
			a, nowISO(),
		).Scan(&gid); err != nil {
			return err
		}
		now := nowISO()
		if _, err := tx.Exec(
			`INSERT INTO dup_group_members (image_id, group_id, created_at) VALUES (?, ?, ?), (?, ?, ?)`,
			a, gid, now, b, gid, now,
		); err != nil {
			return err
		}
	case groupA.Valid && !groupB.Valid:
		if _, err := tx.Exec(
			`INSERT INTO dup_group_members (image_id, group_id, created_at) VALUES (?, ?, ?)`,
			b, groupA.Int64, nowISO(),
		); err != nil {
			return err
		}
	case !groupA.Valid && groupB.Valid:
		if _, err := tx.Exec(
			`INSERT INTO dup_group_members (image_id, group_id, created_at) VALUES (?, ?, ?)`,
			a, groupB.Int64, nowISO(),
		); err != nil {
			return err
		}
	case groupA.Int64 == groupB.Int64:
		// Already in the same group - idempotent no-op.
	default:
		if _, err := tx.Exec(
			`UPDATE dup_group_members SET group_id = ? WHERE group_id = ?`, groupA.Int64, groupB.Int64,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM dup_groups WHERE id = ?`, groupB.Int64); err != nil {
			return err
		}
	}
	return nil
}

// mergeIntoAltGroupTx is the alt-group equivalent. Alt groups carry no
// original_image_id, so the singleton-start case just creates a row.
func mergeIntoAltGroupTx(tx *sql.Tx, a, b int64) error {
	groupA, err := lookupGroupIDTx(tx, "alt_group_members", a)
	if err != nil {
		return err
	}
	groupB, err := lookupGroupIDTx(tx, "alt_group_members", b)
	if err != nil {
		return err
	}
	switch {
	case !groupA.Valid && !groupB.Valid:
		var gid int64
		if err := tx.QueryRow(`INSERT INTO alt_groups (created_at) VALUES (?) RETURNING id`, nowISO()).Scan(&gid); err != nil {
			return err
		}
		now := nowISO()
		if _, err := tx.Exec(
			`INSERT INTO alt_group_members (image_id, group_id, created_at) VALUES (?, ?, ?), (?, ?, ?)`,
			a, gid, now, b, gid, now,
		); err != nil {
			return err
		}
	case groupA.Valid && !groupB.Valid:
		if _, err := tx.Exec(
			`INSERT INTO alt_group_members (image_id, group_id, created_at) VALUES (?, ?, ?)`,
			b, groupA.Int64, nowISO(),
		); err != nil {
			return err
		}
	case !groupA.Valid && groupB.Valid:
		if _, err := tx.Exec(
			`INSERT INTO alt_group_members (image_id, group_id, created_at) VALUES (?, ?, ?)`,
			a, groupB.Int64, nowISO(),
		); err != nil {
			return err
		}
	case groupA.Int64 == groupB.Int64:
		// Same group; no-op.
	default:
		if _, err := tx.Exec(
			`UPDATE alt_group_members SET group_id = ? WHERE group_id = ?`, groupA.Int64, groupB.Int64,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM alt_groups WHERE id = ?`, groupB.Int64); err != nil {
			return err
		}
	}
	return nil
}

// propagateAltOnDuplicateTx applies the §6.4 corollary: when a and b
// become duplicates, any alternate-group state propagates so the new
// duplicate set carries the same alternates. If one side is in an alt
// group and the other is not, the other joins. If both are in
// different alt groups, those groups merge.
func propagateAltOnDuplicateTx(tx *sql.Tx, a, b int64) error {
	groupA, err := lookupGroupIDTx(tx, "alt_group_members", a)
	if err != nil {
		return err
	}
	groupB, err := lookupGroupIDTx(tx, "alt_group_members", b)
	if err != nil {
		return err
	}
	switch {
	case !groupA.Valid && !groupB.Valid:
		// Neither is in an alt group - nothing to propagate.
	case groupA.Valid && !groupB.Valid:
		if _, err := tx.Exec(
			`INSERT INTO alt_group_members (image_id, group_id, created_at) VALUES (?, ?, ?)`,
			b, groupA.Int64, nowISO(),
		); err != nil {
			return err
		}
	case !groupA.Valid && groupB.Valid:
		if _, err := tx.Exec(
			`INSERT INTO alt_group_members (image_id, group_id, created_at) VALUES (?, ?, ?)`,
			a, groupB.Int64, nowISO(),
		); err != nil {
			return err
		}
	case groupA.Int64 == groupB.Int64:
		// Already in the same alt group.
	default:
		if _, err := tx.Exec(
			`UPDATE alt_group_members SET group_id = ? WHERE group_id = ?`, groupA.Int64, groupB.Int64,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM alt_groups WHERE id = ?`, groupB.Int64); err != nil {
			return err
		}
	}
	return nil
}

// handleDupGroupOnDeleteTx promotes a new original or dissolves the
// group when the image leaves. The membership row itself is cleared by
// the FK CASCADE that fires on the caller's subsequent
// `DELETE FROM images`.
func handleDupGroupOnDeleteTx(tx *sql.Tx, imageID int64) error {
	gid, err := lookupGroupIDTx(tx, "dup_group_members", imageID)
	if err != nil {
		return err
	}
	if !gid.Valid {
		return nil
	}
	var memberCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM dup_group_members WHERE group_id = ?`, gid.Int64).Scan(&memberCount); err != nil {
		return err
	}
	if memberCount <= 2 {
		// Removing this image leaves at most one member. Drop the group;
		// CASCADE clears both member rows when the image and the group
		// go away.
		if _, err := tx.Exec(`DELETE FROM dup_groups WHERE id = ?`, gid.Int64); err != nil {
			return err
		}
		return nil
	}
	var current int64
	if err := tx.QueryRow(`SELECT original_image_id FROM dup_groups WHERE id = ?`, gid.Int64).Scan(&current); err != nil {
		return err
	}
	if current != imageID {
		return nil
	}
	var newOriginal int64
	err = tx.QueryRow(`
		SELECT m.image_id FROM dup_group_members m
		JOIN images i ON i.id = m.image_id
		WHERE m.group_id = ? AND m.image_id != ?
		ORDER BY i.file_size DESC, m.image_id DESC
		LIMIT 1`, gid.Int64, imageID,
	).Scan(&newOriginal)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE dup_groups SET original_image_id = ? WHERE id = ?`, newOriginal, gid.Int64); err != nil {
		return err
	}
	return nil
}

// handleAltGroupOnDeleteTx dissolves the alt group when the image
// leaves and the group would shrink to a singleton. Otherwise the
// CASCADE handles the membership-row drop.
func handleAltGroupOnDeleteTx(tx *sql.Tx, imageID int64) error {
	gid, err := lookupGroupIDTx(tx, "alt_group_members", imageID)
	if err != nil {
		return err
	}
	if !gid.Valid {
		return nil
	}
	var memberCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM alt_group_members WHERE group_id = ?`, gid.Int64).Scan(&memberCount); err != nil {
		return err
	}
	if memberCount <= 2 {
		if _, err := tx.Exec(`DELETE FROM alt_groups WHERE id = ?`, gid.Int64); err != nil {
			return err
		}
	}
	return nil
}
