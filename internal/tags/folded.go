package tags

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
)

// Folded-duplicate detection pairs a pre-widening folded tag with the richer
// spelling that superseded it, so the operator can merge the old into the new.

var (
	// legacyDisallowedChars is the pre-widening tag charset (lowercase ASCII
	// plus a little punctuation). LegacyFold projects a stored name back to the
	// spelling that charset would have produced, so a tag folded before the
	// widening is recognised against its richer counterpart. Kept in lockstep
	// with monloader's mapping.LegacyFoldTag.
	legacyDisallowedChars = regexp.MustCompile(`[^a-z0-9_()!@#$.~+:?<>=^-]+`)
	underscoreRuns        = regexp.MustCompile(`_+`)
)

// LegacyFold returns the pre-widening projection of a tag name: runs outside
// the old charset collapse to `_`, underscore runs merge, ends trim. It is
// idempotent on a name already in the old form.
func LegacyFold(name string) string {
	name = legacyDisallowedChars.ReplaceAllString(name, "_")
	name = underscoreRuns.ReplaceAllString(name, "_")
	return strings.Trim(name, "_")
}

// ScanFoldedDuplicates recomputes folded_tag_pairs and returns the number of
// distinct folded originals found. For each non-alias tag B carrying a
// character the old charset had no room for, if a non-alias tag A named
// LegacyFold(B) exists in the same category, it records A (the old fold) -> B
// (the corrected spelling). When more than one B folds onto the same A, all of
// A's rows are flagged ambiguous so the merge leaves them for manual
// resolution.
func (s *Service) ScanFoldedDuplicates() (int, error) {
	rows, err := s.db.Read.Query(`SELECT id, name, category_id FROM tags WHERE is_alias = 0`)
	if err != nil {
		return 0, err
	}
	type tagRow struct {
		id   int64
		name string
		cat  int64
	}
	var all []tagRow
	byNameCat := map[int64]map[string]int64{}
	for rows.Next() {
		var tr tagRow
		if err := rows.Scan(&tr.id, &tr.name, &tr.cat); err != nil {
			_ = rows.Close()
			return 0, err
		}
		all = append(all, tr)
		if byNameCat[tr.cat] == nil {
			byNameCat[tr.cat] = map[string]int64{}
		}
		byNameCat[tr.cat][tr.name] = tr.id
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	type pair struct{ old, new, cat int64 }
	var pairs []pair
	countByOld := map[int64]int{}
	for _, b := range all {
		fold := LegacyFold(b.name)
		if fold == "" || fold == b.name {
			continue // b is already in the folded form (a candidate A, not a B)
		}
		// LegacyFold also collapses underscore runs, so a scrape artifact like
		// girls__frontline differs from its fold without having gained
		// anything; pairing it would retire the clean spelling.
		if !legacyDisallowedChars.MatchString(b.name) {
			continue
		}
		aID, ok := byNameCat[b.cat][fold]
		if !ok || aID == b.id {
			continue
		}
		pairs = append(pairs, pair{old: aID, new: b.id, cat: b.cat})
		countByOld[aID]++
	}

	err = s.inWriteTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM folded_tag_pairs`); err != nil {
			return err
		}
		for _, p := range pairs {
			ambiguous := 0
			if countByOld[p.old] > 1 {
				ambiguous = 1
			}
			if _, err := tx.Exec(
				`INSERT OR IGNORE INTO folded_tag_pairs (old_id, new_id, category_id, ambiguous) VALUES (?, ?, ?, ?)`,
				p.old, p.new, p.cat, ambiguous,
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(countByOld), nil
}

// FoldedDuplicatesCount returns the number of folded originals recorded by the
// last scan, for the /tags Folded-duplicates badge and the Maintenance
// diagnostic.
func (s *Service) FoldedDuplicatesCount() (int, error) {
	var n int
	err := s.db.Read.QueryRow(`SELECT COUNT(DISTINCT old_id) FROM folded_tag_pairs`).Scan(&n)
	return n, err
}

// MergeFolded merges each given folded original into its corrected spelling,
// resolving the target from folded_tag_pairs and skipping ambiguous ones or any
// whose pair no longer holds. ctx aborts between merges, leaving the ones
// already committed in place. Returns merged and skipped counts.
func (s *Service) MergeFolded(ctx context.Context, oldIDs []int64) (merged, skipped int, cancelled bool, err error) {
	for _, oldID := range oldIDs {
		if ctx.Err() != nil {
			return merged, skipped, true, nil
		}
		var newID int64
		e := s.db.Read.QueryRow(
			`SELECT new_id FROM folded_tag_pairs WHERE old_id = ? AND ambiguous = 0`, oldID,
		).Scan(&newID)
		if e == sql.ErrNoRows {
			skipped++
			continue
		}
		if e != nil {
			return merged, skipped, false, e
		}
		if e := s.MergeTags(oldID, newID); e != nil {
			skipped++
			continue
		}
		// The old spelling is now a zero-usage alias; drop its pair so the
		// folded view reflects the merge before the next scan.
		_, _ = s.db.Write.Exec(`DELETE FROM folded_tag_pairs WHERE old_id = ?`, oldID)
		merged++
	}
	return merged, skipped, false, nil
}
