// Package relations manages the operator-declared graph between images:
// duplicate groups, alternate groups, directed version chains, directed
// derivative trees, and the "not related" rejection set.
//
// The Service is the only thing the rest of the codebase calls to mutate
// the graph; each mutation runs inside a single transaction so partial
// states never surface.
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

// FriendlyError carries an operator-facing message and the HTTP status
// code a transport-layer error writer should surface for one of the
// Service's typed errors. Wraps the original so callers can still
// errors.Is() against the sentinel.
type FriendlyError struct {
	Inner   error
	Status  int    // HTTP status the caller should write (400, 409, ...)
	Code    string // short identifier for JSON error envelopes
	Message string // the line the operator sees
}

func (e *FriendlyError) Error() string { return e.Inner.Error() }
func (e *FriendlyError) Unwrap() error { return e.Inner }

// FriendlyErrorFor maps a Service error to the operator-facing message
// shared by every transport. Returns nil when err is not one of the
// recognised sentinels so the caller can fall back to a generic 500.
func FriendlyErrorFor(err error) *FriendlyError {
	switch {
	case errors.Is(err, ErrSelfRelation):
		return &FriendlyError{Inner: err, Status: 400, Code: "invalid_request", Message: "Cannot relate an image to itself."}
	case errors.Is(err, ErrRelationConflict):
		return &FriendlyError{Inner: err, Status: 409, Code: "conflict", Message: "Pair already has a different relation; remove the existing one first."}
	case errors.Is(err, ErrVersionExists):
		return &FriendlyError{Inner: err, Status: 409, Code: "conflict", Message: "One of the images already has a version edge; remove it first."}
	case errors.Is(err, ErrDerivativeExists):
		return &FriendlyError{Inner: err, Status: 409, Code: "conflict", Message: "The chosen derivative already has a source; remove it first."}
	case errors.Is(err, ErrNotInGroup):
		return &FriendlyError{Inner: err, Status: 400, Code: "invalid_request", Message: "Image isn't a member of that group."}
	}
	return nil
}

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

// inWriteTx runs work inside a write transaction, committing on
// success and rolling back via defer on any error path. work's first
// error short-circuits the commit.
func (s *Service) inWriteTx(work func(*sql.Tx) error) error {
	tx, err := s.db.Write.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := work(tx); err != nil {
		return err
	}
	return tx.Commit()
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
	return s.inWriteTx(func(tx *sql.Tx) error {
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
		return pruneQueueForGroupTx(tx, "alt_group_members", a)
	})
}

// AddAlternate marks images a and b as alternates.
func (s *Service) AddAlternate(a, b int64) error {
	if a == b {
		return ErrSelfRelation
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		if conflict, err := pairHasOtherRelationTx(tx, a, b, "alternate"); err != nil {
			return err
		} else if conflict {
			return ErrRelationConflict
		}
		if err := mergeIntoAltGroupTx(tx, a, b); err != nil {
			return err
		}
		return pruneQueueForGroupTx(tx, "alt_group_members", a)
	})
}

// MaxVersionChainDepth caps how far AddVersionEdge walks the existing
// chain when checking for a cycle. Mirrors the implications walker's
// depth budget so a pathological chain can't loop indefinitely.
const MaxVersionChainDepth = 16

// AddVersionEdge declares child as the newer version of parent. The
// chain is strict: each image has at most one parent (PK on
// child_image_id) and at most one child (UNIQUE on parent_image_id), so
// the only forbidden configurations are (a) child already has a parent,
// (b) parent already has a child, or (c) adding the edge would close a
// loop with an existing ancestor chain.
func (s *Service) AddVersionEdge(parent, child int64) error {
	if parent == child {
		return ErrSelfRelation
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		if conflict, err := pairHasOtherRelationTx(tx, parent, child, "version"); err != nil {
			return err
		} else if conflict {
			return ErrRelationConflict
		}
		// Idempotent re-add: the same (parent, child) already on the
		// chain is a silent success so REST retries against a flaky
		// network don't have to distinguish "first call landed but
		// the response was lost" from a real cycle / direction
		// conflict.
		var exact int
		if err := tx.QueryRow(
			`SELECT 1 FROM version_edges WHERE child_image_id = ? AND parent_image_id = ?`,
			child, parent,
		).Scan(&exact); err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var n int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM version_edges WHERE child_image_id = ? OR parent_image_id = ?`,
			child, parent,
		).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrVersionExists
		}
		// Walk parent's ancestors; if child is anywhere up that chain, the
		// new edge would close a cycle.
		cur := parent
		for i := 0; i < MaxVersionChainDepth; i++ {
			var ancestor int64
			err := tx.QueryRow(`SELECT parent_image_id FROM version_edges WHERE child_image_id = ?`, cur).Scan(&ancestor)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return err
			}
			if ancestor == child {
				return ErrVersionExists
			}
			cur = ancestor
		}
		_, err := tx.Exec(
			`INSERT INTO version_edges (child_image_id, parent_image_id, created_at) VALUES (?, ?, ?)`,
			child, parent, nowISO(),
		)
		return err
	})
}

// AddDerivativeEdge declares derivative was made from source. A source
// can carry many derivatives (tree); each derivative has exactly one
// source. Refuses when the derivative already has a source or when
// adding the edge would close a cycle with an existing source chain.
func (s *Service) AddDerivativeEdge(source, derivative int64) error {
	if source == derivative {
		return ErrSelfRelation
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		if conflict, err := pairHasOtherRelationTx(tx, source, derivative, "derivative"); err != nil {
			return err
		} else if conflict {
			return ErrRelationConflict
		}
		// Idempotent re-add: the same (source, derivative) already
		// declared returns silent success so retries don't have to
		// distinguish a same-edge replay from a real source-conflict.
		var exact int
		if err := tx.QueryRow(
			`SELECT 1 FROM derivative_edges WHERE derivative_image_id = ? AND source_image_id = ?`,
			derivative, source,
		).Scan(&exact); err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
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
		// Walk source's source-chain; if derivative is anywhere up that
		// chain, the new edge would close a cycle. Same depth budget as
		// the version chain so a pathological tree can't loop indefinitely.
		cur := source
		for i := 0; i < MaxVersionChainDepth; i++ {
			var ancestor int64
			err := tx.QueryRow(`SELECT source_image_id FROM derivative_edges WHERE derivative_image_id = ?`, cur).Scan(&ancestor)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return err
			}
			if ancestor == derivative {
				return ErrDerivativeExists
			}
			cur = ancestor
		}
		_, err := tx.Exec(
			`INSERT INTO derivative_edges (derivative_image_id, source_image_id, created_at) VALUES (?, ?, ?)`,
			derivative, source, nowISO(),
		)
		return err
	})
}

// AddNotRelated records the canonicalised pair so it never surfaces in
// the find-pairs queue again.
func (s *Service) AddNotRelated(a, b int64) error {
	if a == b {
		return ErrSelfRelation
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		if conflict, err := pairHasOtherRelationTx(tx, a, b, "not_related"); err != nil {
			return err
		} else if conflict {
			return ErrRelationConflict
		}
		lo, hi := canonicalPair(a, b)
		_, err := tx.Exec(
			`INSERT OR IGNORE INTO not_related_pairs (a_image_id, b_image_id, created_at) VALUES (?, ?, ?)`,
			lo, hi, nowISO(),
		)
		return err
	})
}

// RemoveDupMember unlinks one image from its duplicate group. Idempotent:
// a no-op when the image isn't in a group. If the removal would leave
// the group with a single member, the group is dissolved. If the removed
// image was the original, the largest remaining member is promoted.
func (s *Service) RemoveDupMember(imageID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error { return removeDupMemberTx(tx, imageID) })
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
	return s.inWriteTx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM dup_group_members WHERE group_id = ? AND image_id = ?`, groupID, imageID,
		).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotInGroup
		}
		_, err := tx.Exec(`UPDATE dup_groups SET original_image_id = ? WHERE id = ?`, imageID, groupID)
		return err
	})
}

// RemoveAltMember unlinks one image from its alternate group.
// Idempotent. Dissolves the group when reduced to a singleton.
func (s *Service) RemoveAltMember(imageID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error { return removeAltMemberTx(tx, imageID) })
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

// MergeAltGroups consolidates N alt groups into one. The lowest id is
// the survivor; every alt_group_members.group_id pointing at the
// others is repointed at the survivor; the now-empty alt_groups rows
// are deleted. Idempotent on a single-group input.
func (s *Service) MergeAltGroups(groupIDs []int64) error {
	groupIDs = dedupAndSortInt64(groupIDs)
	if len(groupIDs) <= 1 {
		return nil
	}
	return s.inWriteTx(func(tx *sql.Tx) error { return mergeAltGroupsTx(tx, groupIDs) })
}

// MergeDupGroups consolidates N dup groups into one. The lowest id is
// the survivor; member rows from the others are repointed; the
// survivor's original_image_id is taken from the group named by
// keepOriginalFrom. Pass 0 to keep the survivor's existing original.
// Idempotent on a single-group input.
func (s *Service) MergeDupGroups(groupIDs []int64, keepOriginalFrom int64) error {
	groupIDs = dedupAndSortInt64(groupIDs)
	if len(groupIDs) <= 1 {
		return nil
	}
	return s.inWriteTx(func(tx *sql.Tx) error { return mergeDupGroupsTx(tx, groupIDs, keepOriginalFrom) })
}

// dedupAndSortInt64 returns the input sorted ascending with duplicates
// removed. The merge primitives use this so the lowest id is always at
// index 0 (the survivor) regardless of caller argument order.
func dedupAndSortInt64(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	out := make([]int64, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	// Insertion-sort style for tiny N (UI sends at most a handful).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// mergeAltGroupsTx implements MergeAltGroups inside an existing
// transaction. groupIDs must be deduplicated and sorted ascending.
func mergeAltGroupsTx(tx *sql.Tx, groupIDs []int64) error {
	if len(groupIDs) <= 1 {
		return nil
	}
	survivor := groupIDs[0]
	others := groupIDs[1:]
	for _, gid := range others {
		if _, err := tx.Exec(
			`UPDATE alt_group_members SET group_id = ? WHERE group_id = ?`, survivor, gid,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM alt_groups WHERE id = ?`, gid); err != nil {
			return err
		}
	}
	return nil
}

// mergeDupGroupsTx implements MergeDupGroups inside an existing
// transaction. groupIDs must be deduplicated and sorted ascending.
// keepOriginalFrom names which group's original_image_id is copied
// onto the survivor; 0 means "keep the survivor's existing original".
func mergeDupGroupsTx(tx *sql.Tx, groupIDs []int64, keepOriginalFrom int64) error {
	if len(groupIDs) <= 1 {
		return nil
	}
	survivor := groupIDs[0]
	others := groupIDs[1:]
	if keepOriginalFrom != 0 && keepOriginalFrom != survivor {
		// Caller asked to inherit a non-survivor's original. Copy it onto
		// the survivor row before we delete the source group.
		valid := false
		for _, gid := range others {
			if gid == keepOriginalFrom {
				valid = true
				break
			}
		}
		if valid {
			var original int64
			if err := tx.QueryRow(
				`SELECT original_image_id FROM dup_groups WHERE id = ?`, keepOriginalFrom,
			).Scan(&original); err != nil {
				return err
			}
			if _, err := tx.Exec(
				`UPDATE dup_groups SET original_image_id = ? WHERE id = ?`, original, survivor,
			); err != nil {
				return err
			}
		}
	}
	for _, gid := range others {
		if _, err := tx.Exec(
			`UPDATE dup_group_members SET group_id = ? WHERE group_id = ?`, survivor, gid,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM dup_groups WHERE id = ?`, gid); err != nil {
			return err
		}
	}
	return nil
}

// RemoveVersionEdge deletes the edge between parent and child if one
// exists, regardless of which side is which. The schema stores a
// directed (parent, child) row but the operator-facing UI labels both
// "earlier" and "later" buttons with the same form, so a hand-crafted
// post that swaps the sides still drops the edge the operator clicked
// on. Idempotent on a missing edge.
func (s *Service) RemoveVersionEdge(a, b int64) error {
	_, err := s.db.Write.Exec(
		`DELETE FROM version_edges
		 WHERE (parent_image_id = ? AND child_image_id = ?)
		    OR (parent_image_id = ? AND child_image_id = ?)`,
		a, b, b, a,
	)
	return err
}

// ReverseVersionEdge swaps the parent/child of the named edge in one
// transaction so the chain points the other way. Idempotent on a
// missing edge. The new (child=parent, parent=child) row must not
// collide with the per-side uniqueness of an adjacent chain entry; if
// it would (mid-chain reversal), the function returns ErrVersionExists
// so writeRelationError surfaces the operator-facing "remove the
// adjacent edge first" message rather than the raw SQLite constraint
// error.
func (s *Service) ReverseVersionEdge(parent, child int64) error {
	if parent == child {
		return ErrSelfRelation
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`DELETE FROM version_edges WHERE parent_image_id = ? AND child_image_id = ?`, parent, child,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		// After the delete, the swapped row (parent, child) -> (child, parent)
		// must not clash with the schema's per-side UNIQUE constraints. Either
		// side already standing on the new role means an adjacent edge would
		// block the insert.
		var blocked int
		if err := tx.QueryRow(
			`SELECT EXISTS (
				SELECT 1 FROM version_edges WHERE child_image_id = ?
				UNION ALL
				SELECT 1 FROM version_edges WHERE parent_image_id = ?
			)`, parent, child,
		).Scan(&blocked); err != nil {
			return err
		}
		if blocked != 0 {
			return ErrVersionExists
		}
		_, err = tx.Exec(
			`INSERT INTO version_edges (child_image_id, parent_image_id, created_at) VALUES (?, ?, ?)`,
			parent, child, nowISO(),
		)
		return err
	})
}

// RemoveDerivativeEdge deletes the edge between the two images if one
// exists, regardless of which side is the source and which the
// derivative. A hand-crafted post that swaps the sides still drops the
// edge the operator clicked on. Idempotent on a missing edge.
func (s *Service) RemoveDerivativeEdge(a, b int64) error {
	_, err := s.db.Write.Exec(
		`DELETE FROM derivative_edges
		 WHERE (source_image_id = ? AND derivative_image_id = ?)
		    OR (source_image_id = ? AND derivative_image_id = ?)`,
		a, b, b, a,
	)
	return err
}

// DissolveVersionChain drops every version_edge in the chain that
// contains anyMember. Walks up via child_image_id to the root, then
// down via parent_image_id collecting every member, then DELETEs in
// one statement using `parent_image_id IN (...) OR child_image_id IN
// (...)`. Idempotent on an image with no edges. Depth-capped at
// MaxVersionChainDepth on each side so a malformed cycle can't loop.
func (s *Service) DissolveVersionChain(anyMember int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		members, err := collectVersionChainMembersTx(tx, anyMember)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return nil
		}
		return deleteEdgesByEndpointsTx(tx, "version_edges", "parent_image_id", "child_image_id", members)
	})
}

// DissolveDerivativeTree drops every derivative_edge in the tree that
// contains anyMember. Walks up via derivative_image_id to the root,
// then DFSes down via source_image_id collecting every member, then
// DELETEs in one statement using `source_image_id IN (...) OR
// derivative_image_id IN (...)`. Idempotent on an image with no edges.
func (s *Service) DissolveDerivativeTree(anyMember int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		members, err := collectDerivativeTreeMembersTx(tx, anyMember)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return nil
		}
		return deleteEdgesByEndpointsTx(tx, "derivative_edges", "source_image_id", "derivative_image_id", members)
	})
}

// collectVersionChainMembersTx walks the chain containing anyMember
// and returns every member id, or nil when anyMember sits on no
// version edge. Up-walk and down-walk each run at most
// MaxVersionChainDepth steps so a malformed cycle in the data can't
// spin indefinitely.
func collectVersionChainMembersTx(tx *sql.Tx, anyMember int64) ([]int64, error) {
	var has int
	if err := tx.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM version_edges WHERE parent_image_id = ? OR child_image_id = ?)`,
		anyMember, anyMember,
	).Scan(&has); err != nil {
		return nil, err
	}
	if has == 0 {
		return nil, nil
	}
	root := anyMember
	for i := 0; i < MaxVersionChainDepth; i++ {
		var parent int64
		err := tx.QueryRow(`SELECT parent_image_id FROM version_edges WHERE child_image_id = ?`, root).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		root = parent
	}
	members := []int64{root}
	cur := root
	for i := 0; i < MaxVersionChainDepth; i++ {
		var child int64
		err := tx.QueryRow(`SELECT child_image_id FROM version_edges WHERE parent_image_id = ?`, cur).Scan(&child)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		members = append(members, child)
		cur = child
	}
	return members, nil
}

// collectDerivativeTreeMembersTx walks the derivative tree containing
// anyMember and returns every member id, or nil when anyMember sits on
// no derivative edge. Up-walk is depth-capped; the DFS down collects
// every descendant in arbitrary order.
func collectDerivativeTreeMembersTx(tx *sql.Tx, anyMember int64) ([]int64, error) {
	var has int
	if err := tx.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM derivative_edges WHERE source_image_id = ? OR derivative_image_id = ?)`,
		anyMember, anyMember,
	).Scan(&has); err != nil {
		return nil, err
	}
	if has == 0 {
		return nil, nil
	}
	root := anyMember
	for i := 0; i < MaxVersionChainDepth; i++ {
		var src int64
		err := tx.QueryRow(`SELECT source_image_id FROM derivative_edges WHERE derivative_image_id = ?`, root).Scan(&src)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		root = src
	}
	// BFS by tree level so MaxVersionChainDepth bounds genuine depth.
	// A previous DFS implementation incremented depth per stack pop, so
	// a wide tree (single source with >256 derivatives) silently
	// truncated at the 256th child; the cap should describe the tree's
	// vertical reach, not its fan-out.
	members := []int64{root}
	frontier := []int64{root}
	for level := 0; level < MaxVersionChainDepth && len(frontier) > 0; level++ {
		var next []int64
		for _, parent := range frontier {
			rows, err := tx.Query(`SELECT derivative_image_id FROM derivative_edges WHERE source_image_id = ?`, parent)
			if err != nil {
				return nil, err
			}
			ids, scanErr := db.ScanIDs(rows)
			rows.Close()
			if scanErr != nil {
				return nil, scanErr
			}
			next = append(next, ids...)
		}
		members = append(members, next...)
		frontier = next
	}
	return members, nil
}

// deleteEdgesByEndpointsTx removes every row in `table` whose `colA` or
// `colB` is one of `ids`. Used by the version-chain and derivative-tree
// dissolve methods to drop every edge between any pair of chain
// members in one statement.
func deleteEdgesByEndpointsTx(tx *sql.Tx, table, colA, colB string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders, idArgs := db.InPlaceholders(ids)
	args := append(append([]any{}, idArgs...), idArgs...)
	q := fmt.Sprintf(`DELETE FROM %s WHERE %s IN (%s) OR %s IN (%s)`, table, colA, placeholders, colB, placeholders)
	_, err := tx.Exec(q, args...)
	return err
}

// ReverseDerivativeEdge swaps the source and derivative sides of the
// named edge in one transaction. Idempotent on a missing edge. If the
// would-be new derivative side already has another source, the
// function returns ErrDerivativeExists so writeRelationError surfaces
// the operator-facing message.
func (s *Service) ReverseDerivativeEdge(source, derivative int64) error {
	if source == derivative {
		return ErrSelfRelation
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`DELETE FROM derivative_edges WHERE source_image_id = ? AND derivative_image_id = ?`, source, derivative,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		// The swapped row makes `source` the new derivative. PK on
		// derivative_image_id makes that collide with any existing edge
		// where source is already a derivative of another image.
		var blocked int
		if err := tx.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM derivative_edges WHERE derivative_image_id = ?)`, source,
		).Scan(&blocked); err != nil {
			return err
		}
		if blocked != 0 {
			return ErrDerivativeExists
		}
		_, err = tx.Exec(
			`INSERT INTO derivative_edges (derivative_image_id, source_image_id, created_at) VALUES (?, ?, ?)`,
			source, derivative, nowISO(),
		)
		return err
	})
}

// RemoveNotRelated forgets a previously-rejected pair so it becomes
// eligible to resurface in find-pairs again. Idempotent.
func (s *Service) RemoveNotRelated(a, b int64) error {
	lo, hi := canonicalPair(a, b)
	_, err := s.db.Write.Exec(`DELETE FROM not_related_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi)
	return err
}

// ClearVersionEdgeConflictsFor drops only the version_edge rows that
// would block an AddVersionEdge(parent, child) insert: the row where
// `child` is already a child (second parent for it) and the row where
// `parent` is already a parent (second child for it). Edges between
// either endpoint and a third image that don't violate the per-row
// uniqueness keep standing, so the operator's "Replace existing
// version edge" click only sacrifices the directly-conflicting links
// and the rest of the chain stays intact.
func (s *Service) ClearVersionEdgeConflictsFor(parent, child int64) error {
	_, err := s.db.Write.Exec(
		`DELETE FROM version_edges WHERE child_image_id = ? OR parent_image_id = ?`,
		child, parent,
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
	return s.inWriteTx(func(tx *sql.Tx) error {
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
		_, err := tx.Exec(`DELETE FROM not_related_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi)
		return err
	})
}

// CopyTagsFromDuplicatesToOriginal inserts every image_tag carried by
// a non-original member of groupID onto the original. Rating tags are
// excluded (the rating system has highest-wins semantics, so a copy
// would silently bump the original's level). INSERT OR IGNORE makes
// the operation idempotent. Returns the count of newly added rows
// across the group. Runs in one transaction so the per-tag usage_count
// refresh stays consistent.
func (s *Service) CopyTagsFromDuplicatesToOriginal(groupID int64) (int, error) {
	var added int64
	err := s.inWriteTx(func(tx *sql.Tx) error {
		var original int64
		if err := tx.QueryRow(`SELECT original_image_id FROM dup_groups WHERE id = ?`, groupID).Scan(&original); err != nil {
			return err
		}
		// Find the rating category id once; we use it to exclude rating
		// tags from the copy (highest-wins semantics handles them already).
		var ratingCatID sql.NullInt64
		if err := tx.QueryRow(`SELECT id FROM tag_categories WHERE name = 'rating'`).Scan(&ratingCatID); err != nil && err != sql.ErrNoRows {
			return err
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
			return err
		}
		added, _ = res.RowsAffected()
		return nil
	})
	return int(added), err
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
	if err := s.inWriteTx(func(tx *sql.Tx) error {
		if err := handleDupGroupOnDeleteTx(tx, imageID); err != nil {
			return err
		}
		return handleAltGroupOnDeleteTx(tx, imageID)
	}); err != nil {
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

// mergeIntoDupGroupTx is the dup-group five-case merge. When both sides
// already belong to distinct dup groups, the two are folded together
// through mergeDupGroupsTx; the lower-id group survives and keeps its
// existing original_image_id. The operator can flip that from the
// browse-groups Merge dialog when a different original is wanted.
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
		return mergeDupGroupsTx(tx, []int64{groupA.Int64, groupB.Int64}, 0)
	}
	return nil
}

// mergeIntoAltGroupTx is the alt-group equivalent. Alt groups carry no
// original_image_id, so the singleton-start case just creates a row;
// distinct-group merges defer to mergeAltGroupsTx so the same survivor-
// id contract holds.
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
		return mergeAltGroupsTx(tx, []int64{groupA.Int64, groupB.Int64})
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
		return mergeAltGroupsTx(tx, []int64{groupA.Int64, groupB.Int64})
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
