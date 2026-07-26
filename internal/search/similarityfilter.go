package search

import (
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/tags"
)

// buildSimilarFilter handles the two similar: forms:
//
//   - `similar:<id>` matches every image sharing at least one counted
//     tag with the seed. Sharing one counted tag is already score > 0,
//     so the bare form skips scoring entirely and rides a plain
//     membership test on idx_image_tags_tag_image.
//   - `similar:<id>~<score>` matches images scoring at least <score>.
//
// Weights are resolved in Go (see tags.LoadSimilaritySeed) and inlined
// as a CASE ladder, so the aggregate never joins tags nor recomputes
// rarity per candidate row. Malformed input collapses to `1=0` like
// id: does.
func (b *whereBuilder) buildSimilarFilter(e FilterExpr) string {
	seedID, threshold, ok := parseSimilarValue(e.Val)
	if !ok {
		return "1=0"
	}
	seed, ok := b.similaritySeed(seedID)
	if !ok || len(seed.Tags) == 0 {
		return "1=0"
	}
	placeholders, idArgs := db.InPlaceholders(seed.TagIDs())
	if threshold < 0 {
		b.args = append(b.args, idArgs...)
		b.args = append(b.args, seedID)
		return "i.id IN (SELECT it.image_id FROM image_tags it" +
			" WHERE it.tag_id IN (" + placeholders + ") AND it.image_id != ?)"
	}
	weightCase, weightArgs := seed.WeightCase()
	b.args = append(b.args, idArgs...)
	b.args = append(b.args, seedID)
	b.args = append(b.args, weightArgs...)
	b.args = append(b.args, seed.Norm, threshold)
	return "i.id IN (SELECT it.image_id FROM image_tags it" +
		" JOIN images im ON im.id = it.image_id" +
		" WHERE it.tag_id IN (" + placeholders + ") AND it.image_id != ?" +
		" GROUP BY it.image_id" +
		" HAVING min(1.0, sum(" + weightCase + ") / sqrt(? * im.tag_count)) >= ?)"
}

// parseSimilarValue splits `<id>` and `<id>~<score>`. threshold is -1
// for the bare form; a `~` with nothing usable after it is malformed
// rather than a silent fallback to the bare form.
func parseSimilarValue(val string) (seedID int64, threshold float64, ok bool) {
	val = strings.TrimSpace(val)
	idPart, scorePart := val, ""
	tilde := strings.IndexByte(val, '~')
	if tilde >= 0 {
		idPart, scorePart = val[:tilde], strings.TrimSpace(val[tilde+1:])
	}
	id, err := strconv.ParseInt(strings.TrimSpace(idPart), 10, 64)
	if err != nil || id <= 0 {
		return 0, 0, false
	}
	if tilde < 0 {
		return id, -1, true
	}
	s, err := strconv.ParseFloat(scorePart, 64)
	if err != nil || s < 0 || s > 1 {
		return 0, 0, false
	}
	return id, s, true
}

// similarityOrderClause ranks by score against the seed. Score is not
// a column, so it rides a correlated subquery the same way the
// collection sort reads its position. An image with no tags sums to
// NULL and sorts to the tail.
func similarityOrderClause(seed tags.SimilaritySeed, order string) (string, []any) {
	dir := "DESC"
	if order == "asc" {
		dir = "ASC"
	}
	weightCase, args := seed.WeightCase()
	args = append(args, seed.Norm)
	sub := "(SELECT min(1.0, sum(" + weightCase + ") / sqrt(? * i.tag_count))" +
		" FROM image_tags it WHERE it.image_id = i.id)"
	return "ORDER BY " + sub + " " + dir + ", i.id " + dir, args
}

// similarityRankSeed resolves the seed the similarity sort ranks
// against: the leftmost positive similar: term in expr. Returns false
// when there is none or it has nothing to match on, and the caller
// keeps the default order.
func similarityRankSeed(database *db.DB, expr Expr) (tags.SimilaritySeed, bool) {
	if database == nil {
		return tags.SimilaritySeed{}, false
	}
	id, ok := leftmostSimilarSeedID(expr)
	if !ok {
		return tags.SimilaritySeed{}, false
	}
	seed, err := tags.LoadSimilaritySeed(database, id)
	if err != nil || len(seed.Tags) == 0 {
		return tags.SimilaritySeed{}, false
	}
	return seed, true
}

// leftmostSimilarSeedID returns the seed id of the first positive
// similar: term in reading order. Negated terms are skipped: ranking
// by a seed the operator asked to exclude is never what they meant.
func leftmostSimilarSeedID(expr Expr) (int64, bool) {
	switch e := expr.(type) {
	case AndExpr:
		if id, ok := leftmostSimilarSeedID(e.Left); ok {
			return id, true
		}
		return leftmostSimilarSeedID(e.Right)
	case OrExpr:
		if id, ok := leftmostSimilarSeedID(e.Left); ok {
			return id, true
		}
		return leftmostSimilarSeedID(e.Right)
	case FilterExpr:
		if e.Key != "similar" {
			return 0, false
		}
		id, _, ok := parseSimilarValue(e.Val)
		return id, ok
	}
	return 0, false
}

// SimilaritySeedID returns the seed the query ranks against: the
// leftmost positive similar: term. The gallery handler reads it to
// default the sort to similarity, the way a collection: term defaults
// it to collection order, and to score the page it is about to render.
func SimilaritySeedID(expr Expr) (int64, bool) {
	return leftmostSimilarSeedID(expr)
}

// HasSimilarTerm reports whether expr carries a positive similar: term.
func HasSimilarTerm(expr Expr) bool {
	_, ok := leftmostSimilarSeedID(expr)
	return ok
}

// similarityMatchIDs runs the ranked id-only SELECT that the cold
// prev/next and back-page paths read from. The similarity sort has no
// key column to seek on, so both resolve their answer by position in
// this list instead of a cursor comparison.
func similarityMatchIDs(database *db.DB, q Query) []int64 {
	seed, ok := similarityRankSeed(database, q.Expr)
	if !ok {
		return nil
	}
	driverLegs, _ := pickAndDriverTag(database, q.Expr, false)
	where, args, hasMissingFilter, _ := buildWhereDBDriverFull(q.Expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)
	where = andDefaultVisible(where, hasMissingFilter)
	orderClause, orderArgs := similarityOrderClause(seed, q.Order)
	return fetchSortedMatchIDs(database, "", where, args, orderClause, orderArgs, adjacencyCacheMaxIDs)
}
