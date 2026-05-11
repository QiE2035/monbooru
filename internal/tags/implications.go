package tags

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/leqwin/monbooru/internal/models"
)

// MaxImplicationDepth bounds transitive closure walks. Real-world booru
// implication graphs sit well under ten levels; the cap is a runtime
// belt-and-braces guard so a future cycle that slipped past the
// create-time check can't spin forever.
const MaxImplicationDepth = 16

// ErrImplicationCycle is returned when AddImplication would create a
// path from ImpliedID back to ParentID through the existing graph.
var ErrImplicationCycle = errors.New("implication would form a cycle")

// ListImplications returns every direct implication whose parent is
// parentID, with display fields joined for the /tags dialog.
func (s *Service) ListImplications(parentID int64) ([]models.Implication, error) {
	rows, err := s.db.Read.Query(
		`SELECT ti.parent_tag_id, ti.implied_tag_id,
		        p.name, pc.name, pc.color,
		        i.name, ic.name, ic.color,
		        ti.created_at
		 FROM tag_implications ti
		 JOIN tags p ON p.id = ti.parent_tag_id
		 JOIN tag_categories pc ON pc.id = p.category_id
		 JOIN tags i ON i.id = ti.implied_tag_id
		 JOIN tag_categories ic ON ic.id = i.category_id
		 WHERE ti.parent_tag_id = ?
		 ORDER BY i.name`, parentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Implication
	for rows.Next() {
		var im models.Implication
		var created string
		if err := rows.Scan(
			&im.ParentID, &im.ImpliedID,
			&im.ParentName, &im.ParentCategoryName, &im.ParentCategoryColor,
			&im.ImpliedName, &im.ImpliedCategoryName, &im.ImpliedCategoryColor,
			&created,
		); err != nil {
			return nil, err
		}
		im.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, im)
	}
	return out, rows.Err()
}

// ImplicationsForParents returns the direct implications keyed by
// parent id, with display fields joined for the /tags listing. One
// query per call regardless of input size (chunked at the SQLite
// parameter cap).
func (s *Service) ImplicationsForParents(parentIDs []int64) (map[int64][]models.Implication, error) {
	out := make(map[int64][]models.Implication, len(parentIDs))
	if len(parentIDs) == 0 {
		return out, nil
	}
	const chunk = 500
	for start := 0; start < len(parentIDs); start += chunk {
		end := start + chunk
		if end > len(parentIDs) {
			end = len(parentIDs)
		}
		batch := parentIDs[start:end]
		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.db.Read.Query(
			`SELECT ti.parent_tag_id, ti.implied_tag_id,
			        i.name, ic.name, ic.color
			 FROM tag_implications ti
			 JOIN tags i ON i.id = ti.implied_tag_id
			 JOIN tag_categories ic ON ic.id = i.category_id
			 WHERE ti.parent_tag_id IN (`+placeholders+`)
			 ORDER BY i.name`,
			args...,
		)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var im models.Implication
			if err := rows.Scan(
				&im.ParentID, &im.ImpliedID,
				&im.ImpliedName, &im.ImpliedCategoryName, &im.ImpliedCategoryColor,
			); err != nil {
				rows.Close()
				return nil, err
			}
			out[im.ParentID] = append(out[im.ParentID], im)
		}
		rows.Close()
	}
	return out, nil
}

// AddImplication declares parent -> implied. Refuses self-implication,
// alias rows on either side (alias resolution is name-only and would
// silently bypass the link), and any edge that would close a cycle
// through the existing graph. Rating tags are allowed on either side
// because the implication graph doesn't mutate the rating vocabulary -
// the tag row itself is still immutable. The returned bool reports
// whether the row was new; false means the edge already existed.
func (s *Service) AddImplication(parentID, impliedID int64) (bool, error) {
	if parentID == impliedID {
		return false, fmt.Errorf("cannot imply a tag from itself")
	}

	tx, err := s.db.Write.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	for _, id := range [2]int64{parentID, impliedID} {
		var isAlias int
		if err := tx.QueryRow(`SELECT is_alias FROM tags WHERE id = ?`, id).Scan(&isAlias); err == sql.ErrNoRows {
			return false, ErrTagNotFound
		} else if err != nil {
			return false, err
		}
		if isAlias == 1 {
			return false, fmt.Errorf("cannot involve an alias in an implication; use its canonical")
		}
	}

	// Cycle check: walk the existing graph from impliedID; if we reach
	// parentID, the new edge closes a loop.
	if reaches, err := implicationReachesTx(tx, impliedID, parentID); err != nil {
		return false, err
	} else if reaches {
		return false, ErrImplicationCycle
	}

	res, err := tx.Exec(
		`INSERT OR IGNORE INTO tag_implications (parent_tag_id, implied_tag_id) VALUES (?, ?)`,
		parentID, impliedID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// RemoveImplication deletes the parent -> implied edge. Image-side
// cleanup (removing implied rows that were only there because of this
// edge) is the caller's responsibility - it lives in the propagation
// job rather than the synchronous DELETE so the user's click returns
// fast on libraries with millions of image_tags.
func (s *Service) RemoveImplication(parentID, impliedID int64) error {
	res, err := s.db.Write.Exec(
		`DELETE FROM tag_implications WHERE parent_tag_id = ? AND implied_tag_id = ?`,
		parentID, impliedID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("implication not found")
	}
	return nil
}

// ImpliedTagIDs returns the transitive set implied by the input parents
// (parents themselves excluded). Depth-bounded by MaxImplicationDepth.
func (s *Service) ImpliedTagIDs(parents []int64) ([]int64, error) {
	if len(parents) == 0 {
		return nil, nil
	}
	seen := make(map[int64]struct{}, len(parents))
	for _, p := range parents {
		seen[p] = struct{}{}
	}
	frontier := append([]int64(nil), parents...)
	var out []int64
	for depth := 0; depth < MaxImplicationDepth && len(frontier) > 0; depth++ {
		next, err := s.directImplied(frontier)
		if err != nil {
			return nil, err
		}
		frontier = frontier[:0]
		for _, id := range next {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
			frontier = append(frontier, id)
		}
	}
	return out, nil
}

// directImplied returns the union of implied_tag_id rows for the given
// parents. Chunks the IN-list to stay under SQLite's parameter cap.
func (s *Service) directImplied(parents []int64) ([]int64, error) {
	const chunk = 500
	var out []int64
	for start := 0; start < len(parents); start += chunk {
		end := start + chunk
		if end > len(parents) {
			end = len(parents)
		}
		batch := parents[start:end]
		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.db.Read.Query(
			`SELECT DISTINCT implied_tag_id FROM tag_implications WHERE parent_tag_id IN (`+placeholders+`)`,
			args...,
		)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, id)
		}
		rows.Close()
	}
	return out, nil
}

// implicationReachesTx returns whether a directed path from start
// reaches target through tag_implications. Used for cycle detection
// inside AddImplication's transaction.
func implicationReachesTx(tx *sql.Tx, start, target int64) (bool, error) {
	seen := map[int64]struct{}{start: {}}
	frontier := []int64{start}
	for depth := 0; depth < MaxImplicationDepth && len(frontier) > 0; depth++ {
		placeholders := strings.Repeat("?,", len(frontier))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(frontier))
		for i, id := range frontier {
			args[i] = id
		}
		rows, err := tx.Query(
			`SELECT DISTINCT implied_tag_id FROM tag_implications WHERE parent_tag_id IN (`+placeholders+`)`,
			args...,
		)
		if err != nil {
			return false, err
		}
		var next []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return false, err
			}
			if id == target {
				rows.Close()
				return true, nil
			}
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				next = append(next, id)
			}
		}
		rows.Close()
		frontier = next
	}
	return false, nil
}

// ApplyImpliedFanoutTx fans out implications on an open transaction so
// callers outside the tags package (the auto-tagger insert path and
// the propagation job) get the same is_implied=1 rows the tags-service
// write path produces. The is_auto value is the parent's; implied rows
// inherit it so the detail-page source grouping keeps tracking origin.
// ratingCatID, if non-zero, gates a pruneLowerRatingsTx pass after the
// fan-out so an implication whose implied side is a rating tag doesn't
// leave the image with multiple rating rows.
func ApplyImpliedFanoutTx(tx *sql.Tx, imageID, parentID, ratingCatID int64, isAuto bool) error {
	isAutoInt := 0
	if isAuto {
		isAutoInt = 1
	}
	return fanOutImpliedTxImpl(tx, imageID, parentID, ratingCatID, isAutoInt)
}

// fanOutImpliedTxImpl is the package-internal twin shared between the
// service's addTagToImageTxReportingDup and the public ApplyImpliedFanoutTx
// entrypoint. Kept private so the call shape stays a single audit point.
func fanOutImpliedTxImpl(tx *sql.Tx, imageID, parentID, ratingCatID int64, isAutoInt int) error {
	implied, err := transitiveImpliedTx(tx, []int64{parentID})
	if err != nil {
		return err
	}
	insertedRating := false
	for _, id := range implied {
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO image_tags (image_id, tag_id, is_auto, is_implied, confidence, tagger_name)
			 VALUES (?, ?, ?, 1, NULL, NULL)`,
			imageID, id, isAutoInt,
		)
		if err != nil {
			return fmt.Errorf("inserting implied tag %d: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE tags SET usage_count = usage_count + 1
			 WHERE id = ? AND (SELECT is_missing FROM images WHERE id = ?) = 0`,
			id, imageID,
		); err != nil {
			return err
		}
		// If this newly-inserted implied tag is a rating, mark for the
		// post-fanout prune so the image keeps the highest-wins
		// invariant the executor's fast counts depend on.
		if ratingCatID != 0 && !insertedRating {
			var catID int64
			if err := tx.QueryRow(`SELECT category_id FROM tags WHERE id = ?`, id).Scan(&catID); err == nil && catID == ratingCatID {
				insertedRating = true
			}
		}
	}
	if insertedRating && ratingCatID != 0 {
		if err := pruneLowerRatingsTx(tx, ratingCatID, imageID); err != nil {
			return fmt.Errorf("prune lower ratings after implied fan-out: %w", err)
		}
	}
	return nil
}

// implicationParentsOnImageExcluding returns the tag ids on the image
// that still imply impliedID, optionally excluding one parent (the one
// being removed). Used by the propagation cleanup job and by
// removeTagFromImageTx to decide whether an implied row should stay.
func implicationParentsOnImageExcluding(tx *sql.Tx, imageID, impliedID, excludeParent int64) ([]int64, error) {
	rows, err := tx.Query(
		`SELECT ti.parent_tag_id
		 FROM tag_implications ti
		 JOIN image_tags it ON it.tag_id = ti.parent_tag_id
		 WHERE ti.implied_tag_id = ? AND it.image_id = ? AND ti.parent_tag_id != ?`,
		impliedID, imageID, excludeParent,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
