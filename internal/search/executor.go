package search

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/searchkw"
	"github.com/leqwin/monbooru/internal/tags"
)

// Query holds a parsed query and pagination parameters.
type Query struct {
	Expr       Expr
	Sort       string // "newest" | "filesize" | "random"
	Order      string // "asc" | "desc"
	RandomSeed int64  // used when Sort=="random" for stable ordering
	Page       int    // 1-based
	Limit      int
	// PresetTotal lets a caller that already knows the match count
	// (e.g. cached visible-image count for an unfiltered render) skip
	// the COUNT(*) pass.
	PresetTotal *int
	// SkipCount drops COUNT(*) entirely; result.Total is 0. For callers
	// like the sidebar that consume Results.IDs but never surface Total.
	SkipCount bool
	// CacheKey, when set, ties Execute's match-id list to ExecuteAdjacent's
	// prev/next lookup: the gallery populates the cache when its page-1
	// result holds the full match set, and the detail page reads it
	// instead of refetching. Empty disables both sides.
	CacheKey string
}

// Execute runs the query against the DB and returns paginated results.
func Execute(database *db.DB, q Query) (*models.SearchResult, error) {
	page := q.Page
	if page < 1 {
		page = 1
	}
	limit := q.Limit
	if limit < 1 {
		limit = 40
	}

	// Cache fast path: when the gallery's match-id list is already in the
	// adjacency cache, slice it for the requested page and reread row data
	// by primary key. Skips driver-leg picking, COUNT, and the sorted data
	// SELECT entirely. PresetTotal/SkipCount callers (unfiltered visible
	// path, sidebar) keep their tighter shapes - they don't carry a key in
	// practice but the guard keeps the contract explicit. Stale membership
	// is bounded by the cache TTL; row fields are always fresh.
	if q.CacheKey != "" && !q.SkipCount && q.PresetTotal == nil {
		if ids, ok := AdjacencyCacheGet(q.CacheKey); ok {
			return executeFromCachedIDs(database, ids, page, limit)
		}
	}

	driverLegs, _ := pickAndDriverTag(database, q.Expr, q.Sort == "random")

	// Push a recent-id bound into each multi-leg INTERSECT subquery for
	// newest-DESC pages: id is monotonic with ingested_at on the default
	// ingest path, so the top-(page*limit) rows ordered by ingested_at
	// DESC live within the most recent (page*limit)*driverIDBoundMargin
	// visible images. The bound caps each leg's materialisation by
	// orders of magnitude on a populous tag.
	//
	// Gated on intersection density >= 1/driverIDBoundDensityCutoff so a
	// sparse-AND case (where the top of the result set may not lie in the
	// recent slice) keeps the unbounded INTERSECT - the slow path was
	// already fast there. containsMissingFilter excludes shapes whose
	// match set isn't bounded by the visible (is_missing=0) carrier.
	// Order=asc is excluded because the bound is the recent end of the
	// id range; under ASC the user wants the oldest matches, whose ids
	// sit below the bound and would be filtered out entirely.
	if len(driverLegs) >= 2 &&
		(q.Sort == "" || q.Sort == "newest") &&
		q.Order != "asc" &&
		!containsMissingFilter(q.Expr) {
		if total, ok := fastTagTotal(database, q.Expr); ok {
			if visible, vOk := fastVisibleCount(database); vOk &&
				total*driverIDBoundDensityCutoff >= visible {
				targetOffset := (page * limit) * driverIDBoundMargin
				var bound int64
				err := database.Read.QueryRow(
					`SELECT id FROM images INDEXED BY idx_images_ingested_visible
					 WHERE is_missing = 0
					 ORDER BY ingested_at DESC, id DESC
					 LIMIT 1 OFFSET ?`, targetOffset,
				).Scan(&bound)
				if err == nil {
					for i := range driverLegs {
						driverLegs[i].idBound = bound
					}
				}
				// ErrNoRows: library smaller than offset; full INTERSECT
				// is already cheap, no bound needed.
			}
		}
	}

	where, args, hasMissingFilter := buildWhereDBDriver(q.Expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)

	if !hasMissingFilter {
		if where == "" {
			where = "i.is_missing = 0"
		} else {
			where = where + " AND i.is_missing = 0"
		}
	}

	orderClause := buildOrder(q.Sort, q.Order, q.RandomSeed)

	offset := (page - 1) * limit

	var total int
	fastEmpty := false
	switch {
	case q.SkipCount:
	case q.PresetTotal != nil:
		total = *q.PresetTotal
	default:
		// usage_count is maintained as the visible-image count for the
		// canonical, so a single positive literal tag matches COUNT(*)
		// exactly without scanning images.
		if !hasMissingFilter {
			if n, ok := fastTagTotal(database, q.Expr); ok {
				total = n
				fastEmpty = n == 0
				break
			}
		}
		countSQL := "SELECT COUNT(*) FROM images i WHERE " + where
		if err := database.Read.QueryRow(countSQL, args...).Scan(&total); err != nil {
			return nil, fmt.Errorf("count query: %w", err)
		}
	}

	if fastEmpty {
		return &models.SearchResult{Page: page, Limit: limit, Total: 0}, nil
	}

	// Pin the partial sort index when nothing in the query has its own
	// more-selective index. Without the hint SQLite picks
	// idx_images_missing and materialises a temp B-tree for ORDER BY.
	indexHint := ""
	if !hasMissingFilter && (q.Expr == nil || isPureTagExpr(q.Expr)) {
		switch q.Sort {
		case "filesize":
			indexHint = " INDEXED BY idx_images_filesize_visible"
		case "", "newest":
			indexHint = " INDEXED BY idx_images_ingested_visible"
		}
	}

	dataSQL := fmt.Sprintf(
		`SELECT i.id, i.sha256, i.canonical_path, i.folder_path, i.file_type,
		        i.width, i.height, i.file_size, i.is_missing, i.is_favorited,
		        i.is_inbox, i.auto_tagged_at, i.source_type, i.origin, i.source, i.url, i.page_count, i.series, i.series_order, i.ingested_at
		 FROM images i%s
		 WHERE %s
		 %s
		 LIMIT ? OFFSET ?`,
		indexHint, where, orderClause,
	)

	dataArgs := make([]any, len(args), len(args)+2)
	copy(dataArgs, args)
	dataArgs = append(dataArgs, limit, offset)
	rows, err := database.Read.Query(dataSQL, dataArgs...)
	if err != nil {
		return nil, fmt.Errorf("data query: %w", err)
	}
	defer rows.Close()

	var images []models.Image
	for rows.Next() {
		var img models.Image
		var isMissing, isFav, isInbox int
		var width, height, pageCount, seriesOrder *int
		var autoTaggedAt *string
		var ingestedAt string

		if err := rows.Scan(
			&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType,
			&width, &height, &img.FileSize, &isMissing, &isFav,
			&isInbox, &autoTaggedAt, &img.SourceType, &img.Origin, &img.Source, &img.URL, &pageCount, &img.Series, &seriesOrder, &ingestedAt,
		); err != nil {
			return nil, err
		}
		img.IsMissing = isMissing == 1
		img.IsFavorited = isFav == 1
		img.IsInbox = isInbox == 1
		img.Width = width
		img.Height = height
		img.PageCount = pageCount
		img.SeriesOrder = seriesOrder
		if autoTaggedAt != nil {
			t, _ := time.Parse(time.RFC3339, *autoTaggedAt)
			img.AutoTaggedAt = &t
		}
		img.IngestedAt, _ = time.Parse(time.RFC3339, ingestedAt)
		images = append(images, img)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Seed the cache so subsequent gallery pages and detail prev/next
	// ride the cached id slice instead of re-running the sorted SELECT.
	// Above adjacencyCacheMaxIDs the entry would be partial against an
	// unknown total; skip and let the slow path serve.
	//
	// Single-page case (len(images) == total): ids are already in hand,
	// seed synchronously - free.
	//
	// Multi-page case: a separate id-only fan fetches up to the cap.
	// Run it in the background so page 1 returns at the speed of its
	// data SELECT alone; page 2 either gets the cache hit when the fan
	// finishes, or falls through to a fresh Execute. Captured args are
	// not mutated by the caller after Execute returns; the fan reads
	// the same WHERE shape against the read pool, which is goroutine-
	// safe.
	if q.CacheKey != "" && total > 0 && total <= adjacencyCacheMaxIDs {
		if len(images) == total {
			ids := make([]int64, len(images))
			for i, img := range images {
				ids[i] = img.ID
			}
			AdjacencyCacheSet(q.CacheKey, ids)
		} else if AdjacencyCacheTryAcquireFan(q.CacheKey) {
			cacheKey, hint, w, fanArgs, order, t := q.CacheKey, indexHint, where, args, orderClause, total
			go func() {
				defer AdjacencyCacheReleaseFan(cacheKey)
				ids := fetchSortedMatchIDs(database, hint, w, fanArgs, order, t)
				if len(ids) > 0 {
					AdjacencyCacheSet(cacheKey, ids)
				}
			}()
		}
	}

	return &models.SearchResult{
		Page:    page,
		Limit:   limit,
		Total:   total,
		Results: images,
	}, nil
}

// executeFromCachedIDs builds a SearchResult from a cached, sorted
// match-id list: slice for the requested page, fan a single primary-key
// IN-fetch, and re-emit in the cached order. Image rows are always read
// fresh so favorite, tag, and missing-flag mutations surface
// immediately on the next render. Rows returned out of order by the
// planner are reordered in Go to match the cache's sort.
func executeFromCachedIDs(database *db.DB, ids []int64, page, limit int) (*models.SearchResult, error) {
	total := len(ids)
	offset := (page - 1) * limit
	if offset >= total {
		return &models.SearchResult{Page: page, Limit: limit, Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	pageIDs := ids[offset:end]

	placeholders := strings.Repeat("?,", len(pageIDs)-1) + "?"
	args := make([]any, len(pageIDs))
	for i, id := range pageIDs {
		args[i] = id
	}
	sql := fmt.Sprintf(
		`SELECT i.id, i.sha256, i.canonical_path, i.folder_path, i.file_type,
		        i.width, i.height, i.file_size, i.is_missing, i.is_favorited,
		        i.is_inbox, i.auto_tagged_at, i.source_type, i.origin, i.source, i.url, i.page_count, i.series, i.series_order, i.ingested_at
		 FROM images i WHERE i.id IN (%s)`, placeholders,
	)
	rows, err := database.Read.Query(sql, args...)
	if err != nil {
		return nil, fmt.Errorf("cached id fetch: %w", err)
	}
	defer rows.Close()

	byID := make(map[int64]models.Image, len(pageIDs))
	for rows.Next() {
		var img models.Image
		var isMissing, isFav, isInbox int
		var width, height, pageCount, seriesOrder *int
		var autoTaggedAt *string
		var ingestedAt string
		if err := rows.Scan(
			&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType,
			&width, &height, &img.FileSize, &isMissing, &isFav,
			&isInbox, &autoTaggedAt, &img.SourceType, &img.Origin, &img.Source, &img.URL, &pageCount, &img.Series, &seriesOrder, &ingestedAt,
		); err != nil {
			return nil, err
		}
		img.IsMissing = isMissing == 1
		img.IsFavorited = isFav == 1
		img.IsInbox = isInbox == 1
		img.Width = width
		img.Height = height
		img.PageCount = pageCount
		img.SeriesOrder = seriesOrder
		if autoTaggedAt != nil {
			t, _ := time.Parse(time.RFC3339, *autoTaggedAt)
			img.AutoTaggedAt = &t
		}
		img.IngestedAt, _ = time.Parse(time.RFC3339, ingestedAt)
		byID[img.ID] = img
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]models.Image, 0, len(pageIDs))
	for _, id := range pageIDs {
		if img, ok := byID[id]; ok {
			out = append(out, img)
		}
	}
	return &models.SearchResult{
		Page:    page,
		Limit:   limit,
		Total:   total,
		Results: out,
	}, nil
}

// fetchSortedMatchIDs runs the same WHERE/ORDER BY shape as Execute's
// data SELECT but selects only ids and stops at adjacencyCacheMaxIDs.
// Used by Execute to seed the cache when total exceeds a single page,
// so subsequent page-flips and detail prev/next ride the cache. Errors
// degrade to a nil slice; the caller skips populate and the next render
// retries.
func fetchSortedMatchIDs(database *db.DB, indexHint, where string, args []any, orderClause string, total int) []int64 {
	n := total
	if n > adjacencyCacheMaxIDs {
		n = adjacencyCacheMaxIDs
	}
	sql := fmt.Sprintf(
		`SELECT i.id FROM images i%s WHERE %s %s LIMIT ?`,
		indexHint, where, orderClause,
	)
	qargs := make([]any, len(args), len(args)+1)
	copy(qargs, args)
	qargs = append(qargs, n)
	rows, err := database.Read.Query(sql, qargs...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	ids := make([]int64, 0, n)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	return ids
}

// isPureTagExpr reports whether expr is composed only of tag-style
// predicates - TagExpr leaves and the cat:/rating:/tagged:/autotagged:/
// category-qualified FilterExpr leaves. Mixed expressions (fav, source,
// folder, ...) have their own selective column indexes that the
// planner picks. tagged:/autotagged: emit an image_tags EXISTS shape
// the planner short-circuits per row, so pinning idx_images_ingested_
// visible drops the TEMP B-TREE the planner otherwise builds for ORDER
// BY ingested_at on libraries where is_missing=0 has near-zero
// selectivity.
func isPureTagExpr(expr Expr) bool {
	switch e := expr.(type) {
	case AndExpr:
		return isPureTagExpr(e.Left) && isPureTagExpr(e.Right)
	case OrExpr:
		return isPureTagExpr(e.Left) && isPureTagExpr(e.Right)
	case NotExpr:
		return isPureTagExpr(e.Expr)
	case TagExpr:
		return true
	case FilterExpr:
		switch e.Key {
		case "cat", "rating", "tagged", "autotagged":
			return true
		}
		// Category-qualified leaves and the colon-tag fallback emit
		// image_tags EXISTS too; everything else (fav, source, folder,
		// width, ...) has its own column index.
		return !searchkw.IsKeyword(e.Key)
	}
	return false
}

// containsMissingFilter reports whether expr carries a `missing:` filter
// anywhere in the AST. Used to gate optimisations whose density
// estimates assume `is_missing = 0` (the implicit visibility filter).
func containsMissingFilter(expr Expr) bool {
	switch e := expr.(type) {
	case AndExpr:
		return containsMissingFilter(e.Left) || containsMissingFilter(e.Right)
	case OrExpr:
		return containsMissingFilter(e.Left) || containsMissingFilter(e.Right)
	case NotExpr:
		return containsMissingFilter(e.Expr)
	case FilterExpr:
		return e.Key == "missing"
	}
	return false
}

// containsTagPredicate reports whether expr carries a node whose
// match set is unbounded under the cursor walk: tag-shaped EXISTS
// predicates and folder-prefix LIKEs that match a large fraction of
// the library on popular roots. Drives the random-sort bucket gate
// in ExecuteAdjacent.
func containsTagPredicate(expr Expr) bool {
	switch e := expr.(type) {
	case AndExpr:
		return containsTagPredicate(e.Left) || containsTagPredicate(e.Right)
	case OrExpr:
		return containsTagPredicate(e.Left) || containsTagPredicate(e.Right)
	case NotExpr:
		return containsTagPredicate(e.Expr)
	case TagExpr:
		return true
	case FilterExpr:
		switch e.Key {
		case "cat", "tagged", "autotagged", "folder", "folderonly":
			return true
		}
		return !searchkw.IsKeyword(e.Key)
	}
	return false
}

// fastTagTotal returns a visible-image count for an Expr by reading
// tags.usage_count instead of EXISTS-scanning image_tags. ok=false
// falls back to COUNT(*) for shapes the helper can't bound.
//
// Recognised shapes (each delegates to a fastCount* helper):
//   - TagExpr literal, no wildcard, single canonical - exact.
//   - TagExpr wildcard - sum over canonicals; upper bound when an image
//     carries more than one matching tag.
//   - NotExpr{TagExpr literal, no wildcard} - exact.
//   - AndExpr/OrExpr of recognised sub-shapes - min/sum, both upper bounds.
//   - FilterExpr{cat:X} - sum over category; upper bound.
//   - FilterExpr where Key is a real tag category and Val is the tag
//     name (e.g. character:miku) - exact.
//
// The upper-bound shapes (wildcard, And, Or, cat:X) are gated by
// fastApproxThreshold: below it the slow EXISTS COUNT finishes within
// budget on the documented large fixture and is exact, so this helper
// only short-circuits when the slow path would actually be slow.
// Pagination's totalPages may over-shoot when the bound is loose;
// rendered pages past the actual end come back empty.
func fastTagTotal(database *db.DB, expr Expr) (int, bool) {
	if n, ok := fastCountCeiling(database, expr); ok {
		return n, true
	}
	switch e := expr.(type) {
	case TagExpr:
		return fastCountTag(database, e)
	case NotExpr:
		return fastCountNot(database, e)
	case AndExpr:
		return fastCountAnd(database, e)
	case OrExpr:
		return fastCountOr(database, e)
	case FilterExpr:
		return fastCountFilter(database, e)
	}
	return 0, false
}

// adjacencyTotalEstimate returns an upper-bound row count for expr
// without applying the fastApproxThreshold gate that fastCountAnd /
// fastCountOr use to fall back to the slow exact COUNT. ExecuteAdjacent's
// bucket decision needs to know whether the candidate set is small
// regardless of size: the gate inside fastTagTotal hides exactly the
// case the bucket harms (sparse intersections that fit in a fast
// unbounded cursor but get capped to 0-1 matches per id-bucket).
//
// AND/OR are walked here so the recursion never crosses the gated
// fastCountAnd / fastCountOr; leaves and NotExpr delegate to
// fastTagTotal, which carries its own leaf-level gates - those handle
// shapes (multi-canonical wildcard sum, cat: sum) where a loose bound
// is still preferable to firing the bucket on a borderline case.
func adjacencyTotalEstimate(database *db.DB, expr Expr) (int, bool) {
	switch e := expr.(type) {
	case AndExpr:
		l, lok := adjacencyTotalEstimate(database, e.Left)
		if !lok {
			return 0, false
		}
		r, rok := adjacencyTotalEstimate(database, e.Right)
		if !rok {
			return 0, false
		}
		if l < r {
			return l, true
		}
		return r, true
	case OrExpr:
		l, lok := adjacencyTotalEstimate(database, e.Left)
		if !lok {
			return 0, false
		}
		r, rok := adjacencyTotalEstimate(database, e.Right)
		if !rok {
			return 0, false
		}
		sum := l + r
		if v, vok := fastVisibleCount(database); vok && sum > v {
			sum = v
		}
		return sum, true
	}
	return fastTagTotal(database, expr)
}

// fastCountCeiling matches the cookie-ceiling AST shape: a chain of
// NotExpr{FilterExpr{Key:"rating"}} ANDed together, optionally wrapped
// as AndExpr{userExpr, chain} when the cookie is combined with a user
// search. The chain bound is visible_count minus the sum of usage_count
// over the excluded rating tags. Exact when each image carries at most
// one rating tag (the durable invariant PruneLowerRatingsTx now upholds
// on every write); a lower bound on the chain count if pre-existing
// data violates it, in which case pagination drops a trailing edge -
// same off-by-N direction the rest of fastTagTotal already accepts.
//
// The sum-of-usage form replaces the prior COUNT(DISTINCT image_id)
// over `tag_id IN (excluded) AND is_missing = 0`, which scanned ~900k
// image_tags rows on the audit fixture. Reading four tags rows is
// constant time.
func fastCountCeiling(database *db.DB, expr Expr) (int, bool) {
	user, excluded, ok := extractCeilingShape(expr)
	if !ok || len(excluded) == 0 {
		return 0, false
	}
	rows, err := database.Read.Query(
		`SELECT t.name, t.usage_count FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.is_alias = 0
		   AND t.name IN ('general','sensitive','questionable','explicit')`,
	)
	if err != nil {
		return 0, false
	}
	usageByName := make(map[string]int, 4)
	for rows.Next() {
		var name string
		var usage int
		if err := rows.Scan(&name, &usage); err == nil {
			usageByName[name] = usage
		}
	}
	rows.Close()

	hidden := 0
	for _, name := range excluded {
		hidden += usageByName[name]
	}
	visible, ok := fastVisibleCount(database)
	if !ok {
		return 0, false
	}
	chainBound := visible - hidden
	if chainBound < 0 {
		chainBound = 0
	}
	if user == nil {
		return chainBound, true
	}
	// Both userCount and chainBound are valid upper bounds on
	// count(userExpr AND chain), so min(userCount, chainBound) is too -
	// but the bound has no relationship to the actual intersection
	// density. A general-category tag carried mostly by NSFW images
	// under a SFW ceiling, or a SFW visible count dwarfed by a popular
	// tag, both produce an order-of-magnitude overshoot that
	// pagination then advertises as phantom trailing pages. Defer to
	// the slow exact COUNT; cost is bounded by the user's match
	// cardinality via idx_image_tags_tag_image, paid once per cache
	// miss, and the resulting total seeds the adjacency cache so
	// subsequent renders ride the fast path with no SQL.
	return 0, false
}

// extractCeilingShape splits an AST into a userExpr remainder and the
// list of excluded rating levels carried by a NotExpr{FilterExpr{
// Key:"rating"}} chain ANDed onto it. Recognises:
//   - A pure chain (no userExpr; user is nil).
//   - AndExpr{userExpr, chain} - applyRatingCeiling's wrapped shape.
//   - Chains nested arbitrarily inside AndExpr nodes; the non-rating
//     leaves recombine into the returned userExpr.
//
// Anything outside an AndExpr/NotExpr/FilterExpr{rating} branch (Or, a
// non-rating Filter or Tag at the top level) is treated as part of the
// userExpr remainder. ok=false only when the entire tree contains zero
// ceiling-rating leaves.
func extractCeilingShape(expr Expr) (Expr, []string, bool) {
	if expr == nil {
		return nil, nil, false
	}
	var levels []string
	user := peelCeilingChain(expr, &levels)
	if len(levels) == 0 {
		return nil, nil, false
	}
	return user, levels, true
}

// peelCeilingChain walks expr, appending each NotExpr{FilterExpr{
// Key:"rating", Val:X}} encountered along AndExpr branches into out
// and returning the non-chain remainder. Anything that is not an
// AndExpr or a chain leaf is returned as-is.
func peelCeilingChain(expr Expr, out *[]string) Expr {
	switch v := expr.(type) {
	case NotExpr:
		if f, ok := v.Expr.(FilterExpr); ok && f.Key == "rating" {
			*out = append(*out, strings.ToLower(f.Val))
			return nil
		}
		return expr
	case AndExpr:
		left := peelCeilingChain(v.Left, out)
		right := peelCeilingChain(v.Right, out)
		switch {
		case left == nil && right == nil:
			return nil
		case left == nil:
			return right
		case right == nil:
			return left
		default:
			return AndExpr{Left: left, Right: right}
		}
	}
	return expr
}

// fastApproxThreshold gates the fast-path approximations on the slow
// path actually being slow. The slow EXISTS-AND-EXISTS / per-row scan
// is bounded by the smallest matching tag's image_tags rows, so for
// counts under this cap the slow path finishes inside the audit's
// per-query budget and remains exact. Above the cap (popular tags on
// large libraries) the upper-bound short-circuit kicks in.
const fastApproxThreshold = 50000

// andDriverThreshold caps how many image_tags rows the AND-driver shape
// is willing to materialise as a non-correlated IN(...) subquery. The
// driver replaces the smallest ANDed tag's correlated EXISTS with a
// pre-bounded image_id set so the planner stops walking the
// ingested-at index row by row and instead joins against that set.
// Above the cap, materialising the driver costs more than it saves;
// the slow path stays as is.
const andDriverThreshold = 50000

// driverIDBoundMargin is the safety multiplier applied when picking the
// recent-id bound for multi-leg INTERSECT under newest sort. The bound
// covers the (page*limit)*driverIDBoundMargin most recent visible
// images so even with NTP-scale clock skew or a sparse intersection
// inside the recent slice the page is fully populated. 100x absorbs
// drift up to several hours of ingestion volume on typical libraries.
const driverIDBoundMargin = 100

// driverIDBoundDensityCutoff gates the recent-id bound on the AND
// intersection being dense enough that the bound has plenty of
// matches. Bound applies when total*cutoff >= visibleCount, i.e.
// density >= 1/cutoff. The bound's safety margin produces (page*limit)*
// driverIDBoundMargin candidate rows; with density >= 1/20 = 5% that
// floor stays well above the page size.
const driverIDBoundDensityCutoff = 20

// collectAndedTags returns the positive TagExpr leaves (literal or
// wildcard) reachable from the root through AndExpr nodes only. Leaves
// under OrExpr, NotExpr, or any FilterExpr are skipped because dropping
// their EXISTS in favour of a top-level IN(...) driver would flip
// semantics.
func collectAndedTags(expr Expr) []TagExpr {
	var out []TagExpr
	var walk func(Expr)
	walk = func(e Expr) {
		switch v := e.(type) {
		case AndExpr:
			walk(v.Left)
			walk(v.Right)
		case TagExpr:
			if v.Tag != "" {
				out = append(out, v)
			}
		}
	}
	walk(expr)
	return out
}

// andDriverLeg pairs an ANDed TagExpr leaf with the canonical tag IDs
// the driver materialises for it. Multiple legs feed an INTERSECT chain
// in applyAndDriver; a single leg uses the simpler IN form. idBound,
// when > 0, is pushed into each leg's IN subquery as
// `AND image_id >= ?` so the materialisation is capped to the recent
// id range covering the requested page.
type andDriverLeg struct {
	leaf    TagExpr
	ids     []int64
	idBound int64
}

// pickAndDriverTag chooses one or more ANDed TagExpr leaves to feed the
// driver as a non-correlated `i.id IN (SELECT image_id FROM image_tags
// WHERE tag_id IN (...))` predicate that bounds the candidate set
// before the outer query runs. Each chosen leaf has its correlated
// EXISTS suppressed in buildWhereDBDriver so the predicate isn't paid
// twice. Returns ok=false when nothing can be picked.
//
// Two shapes:
//   - Single leg. The smallest leaf has usage <= andDriverThreshold,
//     so materialising its image set is cheap and the outer EXISTS
//     scan against the bounded candidate set is the win. This covers
//     the rare-tag-wins shape (sparse 3-AND) and any AND that includes
//     a wildcard whose LIST SUBQUERY would otherwise rescan tags per
//     EXISTS evaluation.
//   - Multi-leg INTERSECT. Every leaf is above the threshold (the
//     popular-AND shape). The slow path runs three nested EXISTS over
//     a ~1 M visible-image scan; INTERSECTing each leaf's image_id
//     stream off `idx_image_tags_tag` produces the candidate set in
//     O(sum of leaf sizes) sorted-merge.
//
// Single-literal-at-root is the one shape the helper still bails on
// by default: the planner already handles a single EXISTS via
// idx_images_missing or the partial idx_images_ingested_visible (when
// isPureTagExpr) and materialising would just shift the same work.
// allowSingleLiteral=true overrides this for random sort, where there
// is no covering index for the synthetic random key and the slow path
// has to TEMP-B-TREE every visible row carrying the predicate.
func pickAndDriverTag(database *db.DB, expr Expr, allowSingleLiteral bool) ([]andDriverLeg, bool) {
	if database == nil {
		return nil, false
	}
	leaves := collectAndedTags(expr)
	if len(leaves) == 0 {
		return nil, false
	}
	hasWildcard := false
	for _, leaf := range leaves {
		if leaf.Wildcard != "" {
			hasWildcard = true
			break
		}
	}
	// A single literal at root rides one EXISTS - the planner already
	// handles it well for indexed sorts, materialising would just shift
	// the same work. Wildcards alone are different: their LIST SUBQUERY
	// scans every tag row and the planner can't always cache the
	// result, so even one wildcard benefits from materialisation.
	// Random sort overrides this since the random key has no covering
	// index; the materialised set bounds the temp-sort input.
	if !hasWildcard && len(leaves) < 2 && !allowSingleLiteral {
		return nil, false
	}

	type resolved struct {
		leaf  TagExpr
		ids   []int64
		usage int64
	}
	seen := make(map[TagExpr]bool, len(leaves))
	var legs []resolved
	for _, leaf := range leaves {
		if seen[leaf] {
			continue
		}
		seen[leaf] = true
		ids, usage, ok := resolveDriverCanonicals(database, leaf)
		if !ok {
			return nil, false
		}
		if len(ids) == 0 {
			// Unknown name (or wildcard with no matching canonicals);
			// the slow path's EXISTS would return zero matches anyway.
			// Don't pick it as driver - the caller still needs an
			// empty-result fast exit elsewhere.
			continue
		}
		legs = append(legs, resolved{leaf: leaf, ids: ids, usage: usage})
	}
	if len(legs) == 0 {
		return nil, false
	}

	smallestUsage := legs[0].usage
	smallestIdx := 0
	for i, leg := range legs {
		if leg.usage < smallestUsage {
			smallestUsage = leg.usage
			smallestIdx = i
		}
	}

	// Single-leg path when the smallest leaf is cheap enough to feed
	// the IN-driver alone. Materialising one ~ small set + outer EXISTS
	// for the rest is the win the rare-tag-wins shape rides on.
	if smallestUsage <= andDriverThreshold {
		return []andDriverLeg{{leaf: legs[smallestIdx].leaf, ids: legs[smallestIdx].ids}}, true
	}

	// Multi-leg INTERSECT path: every leaf is above the threshold, so
	// the slow EXISTS scan walks visible images for every candidate
	// the outer cursor visits. INTERSECTing each leaf's image_id stream
	// off idx_image_tags_tag (sorted by image_id) reduces the candidate
	// set to the actual intersection in sorted-merge time. Skip when
	// there's only one leg above threshold (no INTERSECT to do; same
	// fall-through case the prior single-leg-or-bail logic took).
	if len(legs) < 2 {
		return nil, false
	}
	// Cap at the two least-popular leaves. Each additional materialised
	// leg adds ~smallestUsage rows to the read pool's working set; under
	// c>1 contention every thread pays that cost in parallel and the
	// extra narrowing past two legs no longer offsets it. Leaves dropped
	// here keep their correlated EXISTS via buildWhereDBDriver, which
	// runs against the candidate set the INTERSECT already bounded.
	sort.Slice(legs, func(i, j int) bool { return legs[i].usage < legs[j].usage })
	const maxIntersectLegs = 2
	if len(legs) > maxIntersectLegs {
		legs = legs[:maxIntersectLegs]
	}
	out := make([]andDriverLeg, len(legs))
	for i, l := range legs {
		out[i] = andDriverLeg{leaf: l.leaf, ids: l.ids}
	}
	return out, true
}

// resolveDriverCanonicals reads the canonical tag IDs and the sum of
// their usage_count for a TagExpr leaf. Literal exact-name uses the
// `t.name = ?` path (matches buildTagExpr's literal branch); wildcards
// ride the LIKE pattern from buildTagExpr's prefix/substring branches.
// Returns ok=false on a query error, ok=true with an empty slice when
// the name (or pattern) matches no canonicals.
func resolveDriverCanonicals(database *db.DB, leaf TagExpr) ([]int64, int64, bool) {
	var pred string
	var arg any
	switch leaf.Wildcard {
	case "":
		pred = "t.name = ?"
		arg = leaf.Tag
	case "prefix":
		pred = `t.name LIKE ? ESCAPE '\'`
		arg = escapeLike(leaf.Tag) + "%"
	case "substring":
		pred = `t.name LIKE ? ESCAPE '\'`
		arg = "%" + escapeLike(leaf.Tag) + "%"
	default:
		return nil, 0, false
	}
	rows, err := database.Read.Query(
		`SELECT canon.id, canon.usage_count
		 FROM tags t
		 JOIN tags canon ON canon.id = COALESCE(t.canonical_tag_id, t.id)
		 WHERE `+pred,
		arg,
	)
	if err != nil {
		return nil, 0, false
	}
	defer rows.Close()
	seen := make(map[int64]bool)
	var ids []int64
	var usage int64
	for rows.Next() {
		var id, count int64
		if err := rows.Scan(&id, &count); err != nil {
			return nil, 0, false
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		usage += count
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false
	}
	return ids, usage, true
}

// applyAndDriver prepends a non-correlated image_tags filter to where
// using the leaf canonical IDs picked by pickAndDriverTag. The driver
// replaces the matched leaves' correlated EXISTS, which the where
// builder has already skipped via driverLeaves.
//
// One leg: `i.id IN (SELECT image_id FROM image_tags WHERE tag_id
// IN (...))`. Multiple legs: each leg's image_id stream is INTERSECTed
// so the bound is the matching intersection of every materialised
// leaf - the popular-AND shape's path off the slow per-row EXISTS scan.
//
// idBound, when set per-leg, appends `AND image_id >= ?` to that leg's
// subquery so the materialisation is capped to the recent id range. The
// caller computes the bound from the partial idx_images_ingested_visible
// for newest-sort pages where id is monotonic with ingested_at; the
// bounded INTERSECT keeps just the recent slice that the LIMIT page is
// drawn from.
func applyAndDriver(where string, args []any, legs []andDriverLeg) (string, []any) {
	if len(legs) == 0 {
		return where, args
	}
	driverArgs := make([]any, 0)
	parts := make([]string, len(legs))
	for i, leg := range legs {
		placeholders := strings.Repeat("?,", len(leg.ids))
		placeholders = placeholders[:len(placeholders)-1]
		parts[i] = "SELECT image_id FROM image_tags WHERE tag_id IN (" + placeholders + ")"
		for _, id := range leg.ids {
			driverArgs = append(driverArgs, id)
		}
		if leg.idBound > 0 {
			parts[i] += " AND image_id >= ?"
			driverArgs = append(driverArgs, leg.idBound)
		}
	}
	var driverWhere string
	if len(parts) == 1 {
		driverWhere = "i.id IN (" + parts[0] + ")"
	} else {
		driverWhere = "i.id IN (" + strings.Join(parts, " INTERSECT ") + ")"
	}
	if where == "" || where == "1=1" {
		return driverWhere, driverArgs
	}
	return driverWhere + " AND " + where, append(driverArgs, args...)
}

// randomAdjacencyBucketSize caps the id range ExecuteAdjacent scans
// when Sort=="random" carries a tag predicate dense enough to make the
// unbounded temp-sort blow the detail-page budget. The random key has
// no index, so the cursor's ORDER BY temp-sorts every matching row;
// bounding the outer scan to a fixed id-range bucket keeps that sort
// proportional to the bucket. The chain ends at bucket boundaries when
// the gate fires - skipped for candidate sets below fastApproxThreshold
// where the bucket would only ever hold currentID itself.
const randomAdjacencyBucketSize = 2000

// andAdjacencyBucketSize caps the id range ExecuteAdjacent scans for
// newest/filesize sorts when the back_q expression carries 3+ ANDed
// tag predicates. The cursor on `(ingested_at, id)` (or file_size, id)
// otherwise walks past arbitrarily many non-matching rows before
// finding the next match - 7-8 s p95 for a sparse-intersection 3-AND
// late in the result set on the audit fixture. Bucketing by id caps
// the worst case to a fixed window even when the intersection is
// sparse. Sized larger than randomAdjacencyBucketSize because newest/
// filesize are the common navigation sorts; users expect prev/next to
// reach further than they do under random. Same fastApproxThreshold
// skip applies as random sort: candidate sets below it ride the
// AND-driver's single-leg path unbounded.
const andAdjacencyBucketSize = 10000

func fastCountTag(database *db.DB, t TagExpr) (int, bool) {
	if t.Tag == "" {
		return 0, false
	}
	var pred string
	var arg any
	switch t.Wildcard {
	case "":
		pred = "name = ?"
		arg = t.Tag
	case "prefix":
		pred = `name LIKE ? ESCAPE '\'`
		arg = escapeLike(t.Tag) + "%"
	case "substring":
		pred = `name LIKE ? ESCAPE '\'`
		arg = "%" + escapeLike(t.Tag) + "%"
	default:
		return 0, false
	}
	rows, err := database.Read.Query(
		`SELECT DISTINCT COALESCE(canonical_tag_id, id) FROM tags WHERE `+pred,
		arg,
	)
	if err != nil {
		return 0, false
	}
	var canonIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, false
		}
		canonIDs = append(canonIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, false
	}
	if len(canonIDs) == 0 {
		return 0, true
	}
	if len(canonIDs) == 1 {
		var n int
		if err := database.Read.QueryRow(
			`SELECT usage_count FROM tags WHERE id = ?`, canonIDs[0],
		).Scan(&n); err != nil {
			return 0, false
		}
		return n, true
	}
	// Multi-canonical exact-name (same name in two categories): summing
	// would over-count images carrying both, and the user typed an exact
	// name so they expect an exact answer. Fall back.
	if t.Wildcard == "" {
		return 0, false
	}
	placeholders := strings.Repeat("?,", len(canonIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(canonIDs))
	for i, id := range canonIDs {
		args[i] = id
	}
	var n int
	if err := database.Read.QueryRow(
		`SELECT COALESCE(SUM(usage_count), 0) FROM tags WHERE id IN (`+placeholders+`)`,
		args...,
	).Scan(&n); err != nil {
		return 0, false
	}
	if n < fastApproxThreshold {
		return 0, false
	}
	return n, true
}

// fastCountNot only handles NOT of a single literal tag. count(!E) is
// visible_count - count(E); applied to an upper-bound count(E) it
// would under-shoot, leaving pagination unable to reach actually-
// existing pages. Restricting to the exact-count case keeps the
// upper-bound invariant of fastTagTotal.
func fastCountNot(database *db.DB, e NotExpr) (int, bool) {
	inner, ok := e.Expr.(TagExpr)
	if !ok || inner.Wildcard != "" || inner.Tag == "" {
		return 0, false
	}
	used, ok := fastCountTag(database, inner)
	if !ok {
		return 0, false
	}
	visible, ok := fastVisibleCount(database)
	if !ok {
		return 0, false
	}
	n := visible - used
	if n < 0 {
		n = 0
	}
	return n, true
}

func fastCountAnd(database *db.DB, e AndExpr) (int, bool) {
	l, ok := fastTagTotal(database, e.Left)
	if !ok {
		return 0, false
	}
	r, ok := fastTagTotal(database, e.Right)
	if !ok {
		return 0, false
	}
	minN := l
	if r < minN {
		minN = r
	}
	if minN < fastApproxThreshold {
		return 0, false
	}
	return minN, true
}

func fastCountOr(database *db.DB, e OrExpr) (int, bool) {
	l, ok := fastTagTotal(database, e.Left)
	if !ok {
		return 0, false
	}
	r, ok := fastTagTotal(database, e.Right)
	if !ok {
		return 0, false
	}
	sum := l + r
	if sum < fastApproxThreshold {
		return 0, false
	}
	v, ok := fastVisibleCount(database)
	if !ok {
		return 0, false
	}
	if sum > v {
		sum = v
	}
	return sum, true
}

func fastCountFilter(database *db.DB, e FilterExpr) (int, bool) {
	if e.Val == "" {
		return 0, false
	}
	if e.Key == "cat" {
		// Sum usage_count over non-alias tags in the category. Aliases
		// have usage_count=0 after merge so they don't actually
		// contribute, but the explicit filter mirrors the slow path's
		// canonical-only image_tags rows.
		var n int
		if err := database.Read.QueryRow(
			`SELECT COALESCE(SUM(usage_count), 0) FROM tags
			 WHERE is_alias = 0
			   AND category_id = (SELECT id FROM tag_categories WHERE name = ?)`,
			e.Val,
		).Scan(&n); err != nil {
			return 0, false
		}
		if n < fastApproxThreshold {
			return 0, false
		}
		return n, true
	}
	if e.Key == "generated" {
		return fastCountGenerated(database, e)
	}
	if e.Key == "rating" {
		return fastCountRating(database, e)
	}
	if e.Key == "tagged" || e.Key == "autotagged" {
		return fastCountTagged(database, e)
	}
	if e.Key == "inbox" {
		return fastCountInbox(database, e)
	}
	if e.Key == "ai" {
		return fastCountAI(database, e)
	}
	if e.Key == "folder" {
		return fastCountFolder(database, e)
	}
	if searchkw.IsKeyword(e.Key) {
		// Other filter keywords (fav, ai, source, folder, ...) have their
		// own selective indexes that the planner picks; no fast path
		// shortcut is needed.
		return 0, false
	}
	// Category-qualified single tag (e.g. character:miku) or a
	// literal-tag fallback (e.g. nier:automata). Match buildFilterExpr's
	// categoryExists branch by looking the category up first.
	var catID int64
	if err := database.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = ?`, e.Key,
	).Scan(&catID); err != nil {
		// Not a real category; the slow path falls back to a literal-
		// tag-name match for the whole "key:val" string. Bail.
		return 0, false
	}
	var n int
	err := database.Read.QueryRow(
		`SELECT canon.usage_count FROM tags t
		 JOIN tags canon ON canon.id = COALESCE(t.canonical_tag_id, t.id)
		 WHERE t.name = ? AND t.category_id = ?
		 LIMIT 1`,
		e.Val, catID,
	).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, true
	}
	if err != nil {
		return 0, false
	}
	return n, true
}

// fastCountRating returns the rating tag's usage_count as the bound
// for `rating:LEVEL`. Exact for the highest level (explicit) since
// nothing higher can hide a carrier; an upper bound for lower levels
// when the highest-rank-wins write rule is held end-to-end (the manual-
// add path and the autotagger both uphold it via PruneLowerRatingsTx).
// Rows that violate the invariant over-shoot here in the same direction
// fastCountAnd / fastCountOr already do - pagination renders an empty
// trailing page rather than dropping a real one.
//
// fastApproxThreshold gates the lower-level upper bound so small
// fixtures with multi-rated images keep the slow path's exact count.
// The highest level skips the gate because its bound is exact.
func fastCountRating(database *db.DB, e FilterExpr) (int, bool) {
	if e.Key != "rating" || e.Val == "" {
		return 0, false
	}
	level := strings.ToLower(e.Val)
	rank := ratingRank(level)
	if rank < 0 {
		// Out-of-vocabulary level matches no rows; the slow-path
		// `1=0` short-circuit returns 0 too.
		return 0, true
	}
	var n int
	err := database.Read.QueryRow(
		`SELECT t.usage_count FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.is_alias = 0 AND t.name = ?`,
		level,
	).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, true
	}
	if err != nil {
		return 0, false
	}
	// An empty rating level (usage_count == 0) and the highest level
	// (explicit, no higher to hide it) are both exact regardless of
	// fixture size; everything else gates so test/small libraries stay
	// on the slow path's exact count.
	if n == 0 || rank == len(ratingLevels)-1 {
		return n, true
	}
	if n < fastApproxThreshold {
		return 0, false
	}
	return n, true
}

// fastCountFolder answers `folder:X` counts (the recursive form) via
// two seeks against the partial idx_images_folder_visible: one for the
// folder itself, one half-open range for paths beneath it. Same match
// set as the slow path's `(folder = ? OR folder LIKE ? ESCAPE '\')`.
//
// Folder paths in monbooru are stored as POSIX-style relative paths
// without a trailing slash (e.g. "anime/girls"). Subdirs sort
// lexicographically as `X || '/' || ...`. The half-open range
// `path >= X || '/' AND path < X || '0'` covers exactly those entries
// because '0' (0x30) is the codepoint immediately after '/' (0x2f),
// so any string `X/...` falls in the range and any string `X[?]...`
// where `?` is a non-`/` character does not. Quoted forms reach the
// builder with quotes stripped, so the same range works.
//
// Empty value falls through; the slow path emits `1=1` for `folder:`
// alone (the recursive root, equivalent to "no filter").
func fastCountFolder(database *db.DB, e FilterExpr) (int, bool) {
	if e.Key != "folder" || e.Val == "" {
		return 0, false
	}
	rangeLo := e.Val + "/"
	rangeHi := e.Val + "0"
	var n int
	if err := database.Read.QueryRow(
		`SELECT (
		     (SELECT COUNT(*) FROM images
		        WHERE folder_path = ? AND is_missing = 0)
		   + (SELECT COUNT(*) FROM images
		        WHERE folder_path >= ? AND folder_path < ? AND is_missing = 0)
		 )`,
		e.Val, rangeLo, rangeHi,
	).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// fastCountAI answers `ai:VAL` counts via two index-pinned queries:
// list distinct source_type values (a tiny set on real data;
// models.SourceType* names four of them), filter that set against the
// same four LIKE patterns buildFilterExpr uses, then COUNT(*) WHERE
// source_type IN (matching) hitting idx_images_source_type. Same
// matches as the slow path; the difference is the slow path's count
// phase walks visible images evaluating an OR-of-LIKE that no index
// pins, while this helper rides idx_images_source_type for both
// phases.
//
// The bare-equality and the bare-ai aliases (`sd`, `any`, `none`)
// keep the slow path: the bare ones are fast already (single equality
// pinning idx_images_source_type), and `any` is a fixed three-element
// OR the planner handles directly. Empty value is the no-op slow path.
func fastCountAI(database *db.DB, e FilterExpr) (int, bool) {
	if e.Key != "ai" || e.Val == "" {
		return 0, false
	}
	val := e.Val
	if val == "sd" {
		val = "a1111"
	}
	if val == "any" || val == "none" {
		return 0, false
	}
	if !strings.Contains(val, ",") {
		// Single-token value already pins idx_images_source via the
		// bare equality emitted by buildFilterExpr. The slow path is
		// not slow here.
		return 0, false
	}
	rows, err := database.Read.Query(`SELECT DISTINCT source_type FROM images`)
	if err != nil {
		return 0, false
	}
	var present []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return 0, false
		}
		present = append(present, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, false
	}

	// Match buildFilterExpr's 4-LIKE shape: equality, prefix-of-list,
	// suffix-of-list, middle-of-list. Decide membership in app code
	// against the small `present` set so each can ride a comma boundary.
	prefix := val + ","
	suffix := "," + val
	middle := "," + val + ","
	var matching []string
	for _, s := range present {
		if s == val ||
			strings.HasPrefix(s, prefix) ||
			strings.HasSuffix(s, suffix) ||
			strings.Contains(s, middle) {
			matching = append(matching, s)
		}
	}
	if len(matching) == 0 {
		return 0, true
	}
	placeholders := strings.Repeat("?,", len(matching))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(matching))
	for i, s := range matching {
		args[i] = s
	}
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images WHERE source_type IN (`+placeholders+`) AND is_missing = 0`,
		args...,
	).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// fastCountTagged returns the exact fast-path count for tagged:true and
// autotagged:true. Computes visible_total - untagged_visible: untagged
// is the exact slow path's :false count (inexpensive at typical scale -
// the EXISTS subquery rides the partial inbox-visible index), and the
// difference is the partition-correct tagged-visible count. Falls back
// to (0, false) on any DB error so the slow path takes over.
func fastCountTagged(database *db.DB, e FilterExpr) (int, bool) {
	if e.Key != "tagged" && e.Key != "autotagged" {
		return 0, false
	}
	if e.Val != "true" {
		return 0, false
	}
	visible, ok := fastVisibleCount(database)
	if !ok {
		return 0, false
	}
	var untagged int
	q := `SELECT COUNT(*) FROM images i WHERE is_missing = 0 AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id)`
	if e.Key == "autotagged" {
		q = `SELECT COUNT(*) FROM images i WHERE is_missing = 0 AND NOT EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id AND it.is_auto = 1)`
	}
	if err := database.Read.QueryRow(q).Scan(&untagged); err != nil {
		return 0, false
	}
	return visible - untagged, true
}

// fastCountInbox returns the visible count for inbox:true / inbox:false
// off idx_images_inbox_visible. Exact at every fixture size: the partial
// index covers the (is_missing = 0, is_inbox = ?) seek directly with no
// row fetch, so the slow path's full visible scan is the wrong tradeoff
// even on small libraries.
func fastCountInbox(database *db.DB, e FilterExpr) (int, bool) {
	if e.Key != "inbox" {
		return 0, false
	}
	var target int
	switch e.Val {
	case "true":
		target = 1
	case "false":
		target = 0
	default:
		return 0, false
	}
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images WHERE is_missing = 0 AND is_inbox = ?`, target,
	).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// fastCountGenerated answers generated:HASH counts from the metadata
// side. The partial idx_sd_metadata_genhash / idx_comfyui_metadata_genhash
// seek directly on the hash, replacing the slow path's per-row EXISTS
// probe over every visible image. UNION dedups image_ids carrying both
// sd and comfy metadata for the same hash.
func fastCountGenerated(database *db.DB, e FilterExpr) (int, bool) {
	if e.Key != "generated" || e.Val == "" {
		return 0, false
	}
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM (
		     SELECT sm.image_id FROM sd_metadata sm
		       JOIN images i ON i.id = sm.image_id
		       WHERE sm.generation_hash = ? AND i.is_missing = 0
		     UNION
		     SELECT cm.image_id FROM comfyui_metadata cm
		       JOIN images i ON i.id = cm.image_id
		       WHERE cm.generation_hash = ? AND i.is_missing = 0
		 )`,
		e.Val, e.Val,
	).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

func fastVisibleCount(database *db.DB) (int, bool) {
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images WHERE is_missing = 0`,
	).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// ExecuteAdjacent returns the image IDs immediately before and after
// currentID under q's sort and filter. Uses cursor-style LIMIT 1
// queries so cost is O(log n) via the ingested_at / file_size indexes,
// not O(matches). Random sort has no key index; for popular tag-
// predicate queries the scan is bounded to a fixed id-range bucket
// containing currentID (see randomAdjacencyBucketSize). Sparse
// candidate sets skip the gate so prev/next reaches every match
// instead of dying at a bucket edge holding only currentID.
func ExecuteAdjacent(database *db.DB, q Query, currentID int64) (*int64, *int64, error) {
	// Cache fast path: when the gallery handed us the sorted match list,
	// prev/next is a slice scan and no SQL fires. Empty key or cache miss
	// falls through to the cursor logic below.
	if ids, ok := AdjacencyCacheGet(q.CacheKey); ok {
		prev, next := findInAdjacencyList(ids, currentID)
		return prev, next, nil
	}

	var ingestedAt string
	var fileSize int64
	if err := database.Read.QueryRow(
		`SELECT ingested_at, file_size FROM images WHERE id = ?`, currentID,
	).Scan(&ingestedAt, &fileSize); err != nil {
		return nil, nil, nil
	}

	// Decide the bucket gate ahead of the AND-driver pick so the driver
	// doesn't materialise legs the bucket would render redundant. With
	// the bucket bounding the candidate range to a fixed window
	// (2k rows under random, 10k under newest/filesize), a per-row
	// correlated EXISTS finishes in tens of ms; an INTERSECT of two
	// popular leaves materialises hundreds of thousands of image_tags
	// rows ahead of the BETWEEN bound and dwarfs the bucket's cap.
	//
	// Skip the gate when the candidate set is provably small: a sparse
	// multi-tag intersection scatters its matches across id-space at
	// densities far below one per bucket, so prev/next would terminate
	// on every click. Below fastApproxThreshold the AND-driver's
	// single-leg path keeps the outer cursor scan in budget without
	// bucketing - same threshold the rest of the count helpers gate on.
	smallCandidate := false
	if total, ok := adjacencyTotalEstimate(database, q.Expr); ok && total < fastApproxThreshold {
		smallCandidate = true
	}

	bucketLo, bucketHi := int64(0), int64(0)
	bucketed := false
	switch {
	case smallCandidate:
	case q.Sort == "random" && containsTagPredicate(q.Expr):
		bucketLo = (currentID / randomAdjacencyBucketSize) * randomAdjacencyBucketSize
		bucketHi = bucketLo + randomAdjacencyBucketSize - 1
		bucketed = true
	case (q.Sort == "" || q.Sort == "newest" || q.Sort == "filesize") &&
		len(collectAndedTags(q.Expr)) >= 3:
		// Sparse multi-AND adjacency: bound the cursor's outer walk to a
		// fixed id window so a sparse intersection late in the result
		// set can't force a multi-second scan. prev/next stops at the
		// bucket boundary when the bound has neighbours to give; the
		// smallCandidate gate above lifts the cap when matches are
		// scattered too thin to bucket usefully.
		bucketLo = (currentID / andAdjacencyBucketSize) * andAdjacencyBucketSize
		bucketHi = bucketLo + andAdjacencyBucketSize - 1
		bucketed = true
	}

	var driverLegs []andDriverLeg
	if !bucketed {
		driverLegs, _ = pickAndDriverTag(database, q.Expr, q.Sort == "random")
	}
	where, args, hasMissingFilter := buildWhereDBDriver(q.Expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)

	// Pure folder predicate: substitute the cursor's WHERE with a
	// seekable equality-or-range form so SQLite hits
	// idx_images_folder_visible instead of scanning
	// idx_images_ingested_visible past unmatched rows under the OR/LIKE
	// form. Splits into the folder itself plus the half-open subfolder
	// range [val+"/", val+"0"); '0' is the codepoint immediately after
	// '/'. A bare [val, val+"0") would leak siblings sharing the prefix
	// followed by an ASCII char below '/' - "anime-2024" and "anime "
	// both sit between "anime" and "anime0" lexicographically. Mirrors
	// fastCountFolder's split.
	if f, ok := q.Expr.(FilterExpr); ok && f.Key == "folder" && f.Val != "" {
		where = "(i.folder_path = ? OR (i.folder_path >= ? AND i.folder_path < ?))"
		args = []any{f.Val, f.Val + "/", f.Val + "0"}
	}

	if !hasMissingFilter {
		if where == "" {
			where = "i.is_missing = 0"
		} else {
			where = where + " AND i.is_missing = 0"
		}
	}

	if bucketed {
		where = where + " AND i.id BETWEEN ? AND ?"
		args = append(args, bucketLo, bucketHi)
	}

	var keyCol string
	var keyVal any
	switch q.Sort {
	case "random":
		if q.RandomSeed == 0 {
			return nil, nil, nil
		}
		// SAFETY: q.RandomSeed is generated server-side via crypto/rand
		// and never sourced from user input. %d only produces digits, so
		// the literal interpolation is safe from SQL injection.
		keyCol = fmt.Sprintf("((i.id * %d) & 2147483647)", q.RandomSeed)
		keyVal = (currentID * q.RandomSeed) & 2147483647
	case "filesize":
		keyCol = "i.file_size"
		keyVal = fileSize
	default: // "newest"
		keyCol = "i.ingested_at"
		keyVal = ingestedAt
	}

	// In desc order prev is the next-larger neighbour; in asc/random it's
	// the next-smaller one. Row-value comparison `(A, id) < (?, ?)`
	// seek-prunes against the (A, id) index; the equivalent OR shape
	// does not.
	var prevCmp, nextCmp, prevSort, nextSort string
	if q.Order == "asc" || q.Sort == "random" {
		prevCmp = fmt.Sprintf("(%s, i.id) < (?, ?)", keyCol)
		nextCmp = fmt.Sprintf("(%s, i.id) > (?, ?)", keyCol)
		prevSort = fmt.Sprintf("ORDER BY %s DESC, i.id DESC", keyCol)
		nextSort = fmt.Sprintf("ORDER BY %s ASC, i.id ASC", keyCol)
	} else {
		prevCmp = fmt.Sprintf("(%s, i.id) > (?, ?)", keyCol)
		nextCmp = fmt.Sprintf("(%s, i.id) < (?, ?)", keyCol)
		prevSort = fmt.Sprintf("ORDER BY %s ASC, i.id ASC", keyCol)
		nextSort = fmt.Sprintf("ORDER BY %s DESC, i.id DESC", keyCol)
	}

	// Pin the partial sort index when nothing in the query has its own
	// more-selective column index, otherwise the planner can pick
	// idx_images_missing and emit a TEMP B-TREE FOR ORDER BY on libraries
	// where is_missing=0 has near-zero selectivity. Mirrors the hint in
	// Execute.
	indexHint := ""
	if !hasMissingFilter && (q.Expr == nil || isPureTagExpr(q.Expr)) {
		switch q.Sort {
		case "filesize":
			indexHint = " INDEXED BY idx_images_filesize_visible"
		case "", "newest":
			indexHint = " INDEXED BY idx_images_ingested_visible"
		}
	}

	lookup := func(cursorCmp, sort string) *int64 {
		qargs := make([]any, 0, len(args)+2)
		qargs = append(qargs, args...)
		qargs = append(qargs, keyVal, currentID)
		sql := fmt.Sprintf("SELECT i.id FROM images i%s WHERE %s AND %s %s LIMIT 1",
			indexHint, where, cursorCmp, sort)
		var id int64
		if err := database.Read.QueryRow(sql, qargs...).Scan(&id); err != nil {
			return nil
		}
		return &id
	}
	return lookup(prevCmp, prevSort), lookup(nextCmp, nextSort), nil
}

// RankInQuery returns the 0-indexed position currentID would occupy in
// q's sorted result set, computed as a single COUNT against the same
// WHERE shape Execute uses. Callers turn the rank into a 1-indexed
// page via floor(rank / pageSize) + 1. Use it as a cold-path fallback
// for the detail handler's back-link page when AdjacencyCacheGet
// misses; warm calls should hit the cache and skip this helper. Cost
// scales with the WHERE's match cardinality, so spawn it in parallel
// with other detail reads and pass a deadline-bound context - on a
// large fixture a popular-tag back_q can otherwise pin the COUNT for
// seconds while the user waits on the page.
//
// Returns (-1, nil) when the helper can't usefully answer (random
// sort with seed=0, ctx cancelled). The caller degrades to whatever
// back_page came in on the URL.
func RankInQuery(ctx context.Context, database *db.DB, q Query, currentID int64) (int, error) {
	// Skip when the result set won't fit the adjacency cache anyway. A
	// popular-tag back_q is the worst case here - the COUNT walks the
	// EXISTS chain against every newer-than-currentID visible row, paid
	// in seconds on a 1M-image fixture. The cache wouldn't have helped
	// either (Execute would have refused to seed past the cap), so the
	// rank is structurally unrecoverable; the handler degrades to the
	// URL's back_page rather than blocking the page render.
	if total, ok := fastTagTotal(database, q.Expr); ok && total > adjacencyCacheMaxIDs {
		return -1, nil
	}

	var ingestedAt string
	var fileSize int64
	if err := database.Read.QueryRowContext(ctx,
		`SELECT ingested_at, file_size FROM images WHERE id = ?`, currentID,
	).Scan(&ingestedAt, &fileSize); err != nil {
		return -1, err
	}

	driverLegs, _ := pickAndDriverTag(database, q.Expr, q.Sort == "random")
	where, args, hasMissingFilter := buildWhereDBDriver(q.Expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)

	// Pure folder predicate gets the same seekable shape Execute and
	// ExecuteAdjacent use so the planner hits idx_images_folder_visible.
	if f, ok := q.Expr.(FilterExpr); ok && f.Key == "folder" && f.Val != "" {
		where = "(i.folder_path = ? OR (i.folder_path >= ? AND i.folder_path < ?))"
		args = []any{f.Val, f.Val + "/", f.Val + "0"}
	}

	if !hasMissingFilter {
		if where == "" {
			where = "i.is_missing = 0"
		} else {
			where = where + " AND i.is_missing = 0"
		}
	}

	var keyCol string
	var keyVal any
	switch q.Sort {
	case "random":
		if q.RandomSeed == 0 {
			return -1, nil
		}
		// SAFETY: q.RandomSeed is generated server-side via crypto/rand
		// and never sourced from user input. %d only produces digits, so
		// the literal interpolation is safe from SQL injection.
		keyCol = fmt.Sprintf("((i.id * %d) & 2147483647)", q.RandomSeed)
		keyVal = (currentID * q.RandomSeed) & 2147483647
	case "filesize":
		keyCol = "i.file_size"
		keyVal = fileSize
	default: // "newest"
		keyCol = "i.ingested_at"
		keyVal = ingestedAt
	}

	// Match ExecuteAdjacent's prev-direction comparison: under DESC
	// order the rows that come before currentID in the result are the
	// ones with a larger (key, id); under ASC / random they're the
	// ones with a smaller (key, id).
	var beforeCmp string
	if q.Order == "asc" || q.Sort == "random" {
		beforeCmp = fmt.Sprintf("(%s, i.id) < (?, ?)", keyCol)
	} else {
		beforeCmp = fmt.Sprintf("(%s, i.id) > (?, ?)", keyCol)
	}

	indexHint := ""
	if !hasMissingFilter && (q.Expr == nil || isPureTagExpr(q.Expr)) {
		switch q.Sort {
		case "filesize":
			indexHint = " INDEXED BY idx_images_filesize_visible"
		case "", "newest":
			indexHint = " INDEXED BY idx_images_ingested_visible"
		}
	}

	sql := fmt.Sprintf(
		"SELECT COUNT(*) FROM images i%s WHERE %s AND %s",
		indexHint, where, beforeCmp,
	)
	qargs := make([]any, 0, len(args)+2)
	qargs = append(qargs, args...)
	qargs = append(qargs, keyVal, currentID)

	var rank int
	if err := database.Read.QueryRowContext(ctx, sql, qargs...).Scan(&rank); err != nil {
		return -1, err
	}
	return rank, nil
}

// DeleteTarget is the minimum bulk-delete needs from a row.
type DeleteTarget struct {
	ID            int64
	CanonicalPath string
	FolderPath    string
	IsMissing     bool
}

// ExecuteForDeleteStream invokes visit for each matching row, streaming
// directly off the cursor so very large result sets never materialise.
// visit returning a non-nil error aborts iteration.
func ExecuteForDeleteStream(database *db.DB, expr Expr, visit func(DeleteTarget) error) error {
	driverLegs, _ := pickAndDriverTag(database, expr, false)
	where, args, hasMissingFilter := buildWhereDBDriver(expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)
	if !hasMissingFilter {
		if where == "" {
			where = "i.is_missing = 0"
		} else {
			where = where + " AND i.is_missing = 0"
		}
	}

	rows, err := database.Read.Query(
		"SELECT i.id, i.canonical_path, i.folder_path, i.is_missing FROM images i WHERE "+where+" ORDER BY i.id",
		args...,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var t DeleteTarget
		var isMissing int
		if err := rows.Scan(&t.ID, &t.CanonicalPath, &t.FolderPath, &isMissing); err != nil {
			return err
		}
		t.IsMissing = isMissing == 1
		if err := visit(t); err != nil {
			return err
		}
	}
	return rows.Err()
}

// sidebarMaxPerCategory caps the sidebar tag list per category so the
// tree stays legible on long-tail libraries.
const sidebarMaxPerCategory = 25

// SidebarTagsWithGlobalCount returns the top N tags per category for the
// given image IDs. Tags are ranked by per-page count; UsageCount carries
// the global tags.usage_count so the sidebar badge reflects total
// occurrences across the library. A ROW_NUMBER() window caps each
// category server-side.
func SidebarTagsWithGlobalCount(database *db.DB, imageIDs []int64) ([]models.Tag, error) {
	if len(imageIDs) == 0 {
		return nil, nil
	}

	placeholders := strings.Repeat("?,", len(imageIDs))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]any, len(imageIDs))
	for i, id := range imageIDs {
		args[i] = id
	}

	rows, err := database.Read.Query(
		fmt.Sprintf(
			`WITH tag_counts AS (
			     SELECT t.id AS tag_id, t.name AS tag_name, tc.name AS cat_name,
			            tc.color AS cat_color, t.usage_count,
			            COUNT(DISTINCT it.image_id) AS page_count
			     FROM image_tags it
			     JOIN tags t ON t.id = it.tag_id
			     JOIN tag_categories tc ON tc.id = t.category_id
			     WHERE it.image_id IN (%s) AND t.is_alias = 0
			     GROUP BY t.id
			 )
			 SELECT tag_id, tag_name, cat_name, cat_color, usage_count
			 FROM (
			     SELECT tag_id, tag_name, cat_name, cat_color, usage_count, page_count,
			            ROW_NUMBER() OVER (PARTITION BY cat_name
			                               ORDER BY page_count DESC, tag_name ASC) AS rn
			     FROM tag_counts
			 )
			 WHERE rn <= ?
			 ORDER BY page_count DESC, tag_name ASC`,
			placeholders,
		),
		append(args, sidebarMaxPerCategory)...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Tag
	for rows.Next() {
		var t models.Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &t.UsageCount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// suggestCandidateCap bounds how many prefix/substring-matching tags are
// considered before computing the per-tag combination count. On 100k+
// galleries a popular prefix like "re" can match thousands of tags;
// without a cap the executor joined image_tags ⋈ images for every match
// just to discard all but the top 10. Bounding by global usage_count is
// safe because COUNT(DISTINCT i.id) ≤ tags.usage_count, so a candidate
// outside the cap cannot outrank one inside it on combo count. The
// dropdown surfaces 10 results, so 2.5x headroom absorbs the prefix-then-
// substring de-dup pass without dragging tail candidates into the join.
// The per-candidate image_tags probe scales the with-context shape under
// concurrency; halving the cap halves the join work each request pays.
const suggestCandidateCap = 25

// suggestContextCap bounds the materialised context-image set. The combo
// count for hot context tags becomes a lower bound past the cap, but
// the relative ordering of suggestions is preserved because tied
// candidates fall through to global usage_count for tie-breaking. 1000
// is the working point: under c=5 the per-worker join through
// image_tags x context fits the page cache; bumping to 5000 amplifies
// the autocomplete latency 3.5x without changing the visible top 10.
const suggestContextCap = 1000

// SuggestTagsWithFilter returns up to limit tags matching prefix that
// also co-occur with at least one image matching expr. UsageCount on
// each returned tag carries the combination count (expr AND the
// suggested tag), not the global one. categoryName, when set, restricts
// suggestions to that category.
func SuggestTagsWithFilter(database *db.DB, expr Expr, prefix, categoryName string, limit int) ([]models.Tag, error) {
	// No preceding context: the combination count collapses to the tag's
	// global usage count, so skip the image_tags ⋈ images join entirely.
	if expr == nil {
		return tags.SuggestUsageRanked(database, prefix, categoryName, true, limit)
	}

	where, args, hasMissingFilter := buildWhereDB(expr, database)
	if !hasMissingFilter {
		if where == "" {
			where = "i.is_missing = 0"
		} else {
			where = where + " AND i.is_missing = 0"
		}
	}

	// Two-pass: prefix matches first (ranked by combo count), then
	// substring matches until limit is hit. Each pass first picks up to
	// suggestCandidateCap tags by global usage_count, then computes the
	// combination count only for that bounded set.
	prefixPat := prefix + "%"
	substrPat := "%" + prefix + "%"

	// ctx materialises the context-image set once via the same WHERE
	// clause Execute uses; each candidate then probes image_tags filtered
	// by `image_id IN ctx` instead of joining images and running an
	// EXISTS subquery per row. The image_tags PK makes (image_id, tag_id)
	// unique, so COUNT(it.image_id) within a group fixed at one tag_id
	// equals the original COUNT(DISTINCT i.id).
	//
	// suggestContextCap keeps the per-candidate join bounded; combo
	// counts become a lower bound past the cap.
	baseSQL := `WITH ctx AS (
	                SELECT i.id AS image_id FROM images i WHERE %s LIMIT ?
	            ),
	            cand AS (
	                SELECT id, category_id, usage_count
	                FROM tags
	                WHERE is_alias = 0
	                  AND name LIKE ?
	                  %s
	                ORDER BY usage_count DESC
	                LIMIT ?
	            )
	            SELECT c.id, t.name, tc.name, tc.color, COUNT(it.image_id) AS combo
	            FROM cand c
	            JOIN tags t ON t.id = c.id
	            JOIN tag_categories tc ON tc.id = c.category_id
	            JOIN image_tags it ON it.tag_id = c.id
	                              AND it.image_id IN (SELECT image_id FROM ctx)
	            GROUP BY c.id
	            HAVING combo > 0
	            ORDER BY combo DESC, c.usage_count DESC
	            LIMIT ?`

	catClause := ""
	catArgs := []any{}
	if categoryName != "" {
		catClause = "AND category_id = (SELECT id FROM tag_categories WHERE name = ?)"
		catArgs = []any{categoryName}
	}

	run := func(pat string, prior []models.Tag, remaining int, nameNotLike string) ([]models.Tag, error) {
		extra := catClause
		qargs := make([]any, 0, 5+len(args)+len(catArgs))
		qargs = append(qargs, args...)
		qargs = append(qargs, suggestContextCap)
		qargs = append(qargs, pat)
		qargs = append(qargs, catArgs...)
		if nameNotLike != "" {
			extra = extra + " AND name NOT LIKE ?"
			qargs = append(qargs, nameNotLike)
		}
		qargs = append(qargs, suggestCandidateCap)
		qargs = append(qargs, remaining)
		rows, err := database.Read.Query(fmt.Sprintf(baseSQL, where, extra), qargs...)
		if err != nil {
			return prior, err
		}
		defer rows.Close()
		seen := map[int64]bool{}
		for _, t := range prior {
			seen[t.ID] = true
		}
		for rows.Next() {
			var t models.Tag
			var combo int
			if err := rows.Scan(&t.ID, &t.Name, &t.CategoryName, &t.CategoryColor, &combo); err != nil {
				return prior, err
			}
			if seen[t.ID] {
				continue
			}
			t.UsageCount = combo
			prior = append(prior, t)
			seen[t.ID] = true
		}
		return prior, rows.Err()
	}

	out, err := run(prefixPat, nil, limit, "")
	if err != nil {
		return nil, err
	}
	if len(out) < limit {
		out, err = run(substrPat, out, limit-len(out), prefixPat)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func buildOrder(sort, order string, randomSeed int64) string {
	switch sort {
	case "filesize":
		dir := "DESC"
		if order == "asc" {
			dir = "ASC"
		}
		return "ORDER BY i.file_size " + dir + ", i.id " + dir
	case "order":
		// Group by series alphabetically, then by within-series position
		// with NULLs last in both directions (a series with most rows
		// unordered should still sit next to its ordered ones), then
		// fall back to ingest order so untagged rows have a stable seat
		// and pagination has a total order. ASC and DESC flip every
		// axis except the NULLs-last bias.
		dir := "ASC"
		if order == "desc" {
			dir = "DESC"
		}
		return "ORDER BY i.series " + dir + ", i.series_order IS NULL, i.series_order " + dir + ", i.id " + dir
	case "random":
		if randomSeed != 0 {
			// Deterministic pseudo-random order, stable across page
			// loads. The 31-bit masked product can collide, so i.id is
			// the tiebreaker for a total order (otherwise pagination
			// can repeat or skip images).
			// SAFETY: randomSeed comes from crypto/rand in galleryHandler,
			// never user input; %d only produces digits.
			return fmt.Sprintf("ORDER BY ((i.id * %d) & 2147483647), i.id", randomSeed)
		}
		return "ORDER BY RANDOM(), i.id"
	default: // "newest"
		dir := "DESC"
		if order == "asc" {
			dir = "ASC"
		}
		return "ORDER BY i.ingested_at " + dir + ", i.id " + dir
	}
}

type whereBuilder struct {
	parts            []string
	args             []any
	hasMissingFilter bool
	// db, when non-nil, lets FilterExpr's default branch check whether
	// an unknown `prefix:value` key matches a real tag category. On
	// miss the whole token is matched as a literal tag so names like
	// `nier:automata` remain searchable. A nil db (test path) keeps
	// the always-category-qualified behaviour.
	db *db.DB
	// ratingIDs caches the four canonical rating tag IDs for the
	// duration of one buildExpr walk. Resolved on first `rating:`
	// encounter so a query without rating predicates pays nothing.
	// Keyed by tag name; `ratingResolved` tracks "queried, cache is
	// authoritative even if some entries are missing". ratingUsage
	// carries usage_count for the same rows so a positive `rating:X`
	// against a level no image yet carries can short-circuit instead
	// of paying a full image scan to find zero matches.
	ratingIDs      map[string]int64
	ratingUsage    map[string]int64
	ratingResolved bool
	// driverLeaves names the ANDed TagExpr leaves (literal or wildcard)
	// whose correlated EXISTS is suppressed because the caller has
	// prepended a non-correlated `i.id IN (...)` driver covering the
	// same rows. The set is keyed on (Tag, Wildcard) exactly so a
	// literal `blue` does not silence a `blue*` leaf in the same
	// expression. Multi-entry sets correspond to the popular-AND
	// INTERSECT path; single-entry to the rare-tag-wins single-leg path.
	driverLeaves map[TagExpr]bool
}

// resolveRatingIDs queries the four canonical rating tag rows and caches
// the result on the builder. A row missing from the result (e.g. the tag
// was pruned at runtime) leaves the entry absent from the map; callers
// fall back to a no-match predicate when their target name is unmapped.
func (b *whereBuilder) resolveRatingIDs() {
	if b.ratingResolved {
		return
	}
	b.ratingResolved = true
	if b.db == nil {
		return
	}
	rows, err := b.db.Read.Query(
		`SELECT t.name, t.id, t.usage_count FROM tags t
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE tc.name = 'rating' AND t.is_alias = 0
		   AND t.name IN ('general','sensitive','questionable','explicit')`,
	)
	if err != nil {
		return
	}
	defer rows.Close()
	ids := make(map[string]int64, 4)
	usage := make(map[string]int64, 4)
	for rows.Next() {
		var name string
		var id, count int64
		if err := rows.Scan(&name, &id, &count); err == nil {
			ids[name] = id
			usage[name] = count
		}
	}
	b.ratingIDs = ids
	b.ratingUsage = usage
}

// imageIDExists builds an EXISTS predicate against the per-image-ids
// subquery FROM <fromBody> WHERE <where>. alias qualifies image_id;
// where omits the alias.image_id = i.id link, which the helper supplies.
func (b *whereBuilder) imageIDExists(fromBody, alias, where string, negate bool) string {
	op := "EXISTS"
	if negate {
		op = "NOT EXISTS"
	}
	if where == "" {
		return fmt.Sprintf("%s (SELECT 1 FROM %s WHERE %s.image_id = i.id)", op, fromBody, alias)
	}
	return fmt.Sprintf("%s (SELECT 1 FROM %s WHERE %s.image_id = i.id AND %s)", op, fromBody, alias, where)
}

// imageTagsPredicate is imageIDExists shorthand for the common
// `image_tags it`-only shape used by tag and tagged/autotagged filters.
func (b *whereBuilder) imageTagsPredicate(where string, negate bool) string {
	return b.imageIDExists("image_tags it", "it", where, negate)
}

func buildWhere(expr Expr) (string, []any, bool) {
	return buildWhereDB(expr, nil)
}

// buildWhereDBDriver is buildWhereDB with a driver-leaves hint: TagExpr
// leaves present in the set emit no SQL, because the caller has
// prepended a non-correlated IN(...) (or IN INTERSECT) predicate
// covering the same rows. An empty/nil set leaves the regular build
// path untouched.
func buildWhereDBDriver(expr Expr, database *db.DB, legs []andDriverLeg) (string, []any, bool) {
	var leaves map[TagExpr]bool
	if len(legs) > 0 {
		leaves = make(map[TagExpr]bool, len(legs))
		for _, l := range legs {
			leaves[l.leaf] = true
		}
	}
	b := &whereBuilder{db: database, driverLeaves: leaves}
	if expr != nil {
		part := b.buildExpr(expr)
		if part != "" {
			b.parts = append(b.parts, part)
		}
	}
	where := strings.Join(b.parts, " AND ")
	if where == "" {
		where = "1=1"
	}
	return where, b.args, b.hasMissingFilter
}

// categoryExists reports whether name matches a tag_categories row.
// Returns true on a nil-db (test) builder so the caller's old behaviour
// is preserved.
func (b *whereBuilder) categoryExists(name string) bool {
	if b.db == nil {
		return true
	}
	var n int
	if err := b.db.Read.QueryRow(
		`SELECT 1 FROM tag_categories WHERE name = ? LIMIT 1`, name,
	).Scan(&n); err != nil {
		return false
	}
	return true
}

func buildWhereDB(expr Expr, database *db.DB) (string, []any, bool) {
	b := &whereBuilder{db: database}
	if expr != nil {
		part := b.buildExpr(expr)
		if part != "" {
			b.parts = append(b.parts, part)
		}
	}
	where := strings.Join(b.parts, " AND ")
	if where == "" {
		where = "1=1"
	}
	return where, b.args, b.hasMissingFilter
}

func (b *whereBuilder) buildExpr(expr Expr) string {
	switch e := expr.(type) {
	case AndExpr:
		left := b.buildExpr(e.Left)
		right := b.buildExpr(e.Right)
		if left == "" {
			return right
		}
		if right == "" {
			return left
		}
		return "(" + left + " AND " + right + ")"

	case OrExpr:
		left := b.buildExpr(e.Left)
		right := b.buildExpr(e.Right)
		return "(" + left + " OR " + right + ")"

	case NotExpr:
		inner := b.buildExpr(e.Expr)
		return "NOT (" + inner + ")"

	case TagExpr:
		return b.buildTagExpr(e)

	case FilterExpr:
		return b.buildFilterExpr(e)
	}
	return ""
}

func (b *whereBuilder) buildTagExpr(e TagExpr) string {
	// COALESCE(canonical_tag_id, id) collapses alias rows onto their
	// canonical so a search for the alias name still hits image_tags
	// rows that were re-pointed at the canonical.
	//
	// Wildcard branches escape `_` and `%` in the user-supplied portion
	// so a tag literal containing those characters (legal: `[a-z0-9_…]`)
	// matches itself instead of acting as a LIKE wildcard.
	if b.driverLeaves[e] {
		// Caller prepended a non-correlated IN(...) (or IN INTERSECT)
		// covering this leaf. Returning "" lets AndExpr collapse this
		// branch (only AND-only paths from root were eligible to be
		// marked as a driver leaf, so the empty result never lands
		// inside an OR or NOT).
		return ""
	}
	switch e.Wildcard {
	case "prefix":
		b.args = append(b.args, escapeLike(e.Tag)+"%")
		return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE name LIKE ? ESCAPE '\')`, false)
	case "substring":
		b.args = append(b.args, "%"+escapeLike(e.Tag)+"%")
		return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE name LIKE ? ESCAPE '\')`, false)
	default:
		b.args = append(b.args, e.Tag)
		return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE name = ?)`, false)
	}
}

// escapeLike escapes the SQLite LIKE metacharacters (`_`, `%`) and the
// escape character itself (`\`) so user-supplied input matches literally
// when concatenated with `%`/`_` wildcards. Callers must pair this with
// `ESCAPE '\'` on the LIKE clause.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`)
	return r.Replace(s)
}

// ratingLevels is the canonical rating vocabulary, ordered low to high.
// Highest-wins resolution and the cookie ceiling rely on this order.
var ratingLevels = []string{"general", "sensitive", "questionable", "explicit"}

func ratingRank(name string) int {
	for i, l := range ratingLevels {
		if l == name {
			return i
		}
	}
	return -1
}

func (b *whereBuilder) buildFilterExpr(e FilterExpr) string {
	switch e.Key {
	case "fav":
		if e.Val == "true" {
			return "i.is_favorited = 1"
		}
		return "i.is_favorited = 0"

	case "inbox":
		if e.Val == "true" {
			return "i.is_inbox = 1"
		}
		return "i.is_inbox = 0"

	case "ai":
		// Accept comma-separated source_type and the legacy "sd" alias.
		val := e.Val
		if val == "sd" {
			val = "a1111"
		}
		// "any" matches any image carrying a1111 and/or comfyui metadata.
		if val == "any" {
			return "(i.source_type = 'a1111' OR i.source_type = 'comfyui' OR i.source_type = 'a1111,comfyui')"
		}
		b.args = append(b.args, val, "%,"+val, val+",%", "%,"+val+",%")
		return "(i.source_type = ? OR i.source_type LIKE ? OR i.source_type LIKE ? OR i.source_type LIKE ?)"

	case "source":
		// Exact-match against the operator-edited images.source label.
		// Empty value matches images that carry no source - common for
		// freshly-ingested files - so the user can triage them with
		// `source:""`. The bare token form `source:` (no value) is also
		// useful as the empty-string predicate.
		b.args = append(b.args, e.Val)
		return "i.source = ?"

	case "cat":
		b.args = append(b.args, e.Val)
		return b.imageIDExists("image_tags it JOIN tags t ON it.tag_id = t.id JOIN tag_categories tc ON tc.id = t.category_id", "it", "tc.name = ?", false)

	case "width":
		op, n, ok := parseIntComp(e.Val)
		if !ok {
			return "1=0"
		}
		b.args = append(b.args, n)
		return fmt.Sprintf("i.width %s ?", op)

	case "height":
		op, n, ok := parseIntComp(e.Val)
		if !ok {
			return "1=0"
		}
		b.args = append(b.args, n)
		return fmt.Sprintf("i.height %s ?", op)

	case "date":
		return b.buildDateFilter(e.Val)

	case "missing":
		// Any explicit `missing:` opts out of the default
		// `AND is_missing = 0`. Without this flag, `-missing:false`
		// collapses to `NOT (is_missing = 0) AND is_missing = 0` and
		// returns nothing.
		b.hasMissingFilter = true
		if e.Val == "true" {
			return "i.is_missing = 1"
		}
		return "i.is_missing = 0"

	case "type":
		// Comma-separated union of named file-type buckets:
		//   image     -> jpeg / png / webp / gif / mp4 / webm
		//   archive   -> cbz (cbz and zip archives of images; the
		//                ingest collapses both extensions onto the
		//                'cbz' file_type)
		//   animated  -> gif / mp4 / webm (subset of image)
		// `-type:animated` is the inverse via the parser's NotExpr; no
		// dedicated `animated:false` keyword exists.
		buckets := map[string][]string{
			"image":    {"jpeg", "png", "webp", "gif", "mp4", "webm"},
			"archive":  {"cbz"},
			"animated": {"gif", "mp4", "webm"},
		}
		all := map[string]bool{
			"jpeg": true, "png": true, "webp": true, "gif": true,
			"mp4": true, "webm": true, "cbz": true,
		}
		seen := map[string]bool{}
		for _, v := range strings.Split(strings.ToLower(e.Val), ",") {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			fts, ok := buckets[v]
			if !ok {
				continue
			}
			for _, ft := range fts {
				seen[ft] = true
			}
		}
		if len(seen) == 0 {
			return "1=0"
		}
		if len(seen) == len(all) {
			return "1=1"
		}
		quoted := make([]string, 0, len(seen))
		for ft := range seen {
			quoted = append(quoted, "'"+ft+"'")
		}
		sort.Strings(quoted)
		return "i.file_type IN (" + strings.Join(quoted, ", ") + ")"

	case "collection":
		// Operator-edited per-row collection label (the comic / manga
		// "series" surface, generalised for plain image groupings).
		// Schema column kept as `series` for backwards compatibility
		// with existing databases; only the user-facing keyword and
		// payload field names carry the new vocabulary.
		b.args = append(b.args, e.Val)
		return "i.series = ?"

	case "pages":
		op, n, ok := parseIntComp(e.Val)
		if !ok {
			return "1=0"
		}
		b.args = append(b.args, n)
		// COALESCE so non-manga rows (NULL page_count) compare as 0;
		// matches the spec contract that `pages:>=1` excludes images.
		return fmt.Sprintf("COALESCE(i.page_count, 0) %s ?", op)

	case "tagged":
		return b.imageTagsPredicate("", e.Val != "true")

	case "autotagged":
		return b.imageTagsPredicate("it.is_auto = 1", e.Val != "true")

	case "folder":
		if e.Val == "" {
			// `folder:` alone is recursive root - every non-missing
			// image lives at or below the gallery root. Use
			// `folderonly:` with an empty value for "root directly".
			return "1=1"
		}
		// Recursive match: this folder or anywhere beneath it. Escape
		// LIKE metacharacters so a folder named `foo_bar` only matches
		// itself (not `fooXbar`).
		b.args = append(b.args, e.Val, escapeLike(e.Val)+"/%")
		return `(i.folder_path = ? OR i.folder_path LIKE ? ESCAPE '\')`

	case "folderonly":
		if e.Val == "" {
			return "i.folder_path = ''"
		}
		b.args = append(b.args, e.Val)
		return "i.folder_path = ?"

	case "generated":
		b.args = append(b.args, e.Val, e.Val)
		sm := b.imageIDExists("sd_metadata sm", "sm", "sm.generation_hash = ?", false)
		cm := b.imageIDExists("comfyui_metadata cm", "cm", "cm.generation_hash = ?", false)
		return "(" + sm + " OR " + cm + ")"

	case "rating":
		// Highest-wins: an image matches `rating:X` only when it carries X
		// AND no rating ranked above X. Self uses EXISTS, the strictly-
		// higher levels are NOT EXISTS, all keyed on the cached rating
		// tag IDs so the predicates hit idx_image_tags_image directly.
		val := strings.ToLower(e.Val)
		rank := ratingRank(val)
		if rank < 0 {
			return "1=0"
		}
		b.resolveRatingIDs()
		selfID, ok := b.ratingIDs[val]
		if !ok {
			return "1=0"
		}
		// No image carries this level yet (fresh install state). Skip the
		// EXISTS predicate so the LIMIT-bounded data path stops on the
		// ingested-at index instead of scanning every visible row to find
		// zero matches.
		if b.ratingUsage[val] == 0 {
			return "1=0"
		}
		b.args = append(b.args, selfID)
		parts := []string{b.imageTagsPredicate("it.tag_id = ?", false)}
		for i := rank + 1; i < len(ratingLevels); i++ {
			higherID, ok := b.ratingIDs[ratingLevels[i]]
			if !ok {
				continue
			}
			b.args = append(b.args, higherID)
			parts = append(parts, b.imageTagsPredicate("it.tag_id = ?", true))
		}
		if len(parts) == 1 {
			return parts[0]
		}
		return "(" + strings.Join(parts, " AND ") + ")"

	default:
		// Unknown key is either a category-qualified tag search
		// ("character:cat") or a literal colon-bearing tag name
		// ("nier:automata", ":3"). If the key matches a real category
		// we split; otherwise the whole "key:val" is matched as a
		// literal tag name.
		if e.Val == "" {
			return "1=1"
		}
		if b.categoryExists(e.Key) {
			b.args = append(b.args, e.Val, e.Key)
			return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(t.canonical_tag_id, t.id) FROM tags t JOIN tag_categories tc ON tc.id = t.category_id WHERE t.name = ? AND tc.name = ?)`, false)
		}
		b.args = append(b.args, e.Key+":"+e.Val)
		return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE name = ?)`, false)
	}
}

// dateFilterRe matches the documented date filter shapes: YYYY,
// YYYY-MM, or YYYY-MM-DD. The HELP.md examples show YYYY-MM ranges
// (`date:2024-01..2024-06`) which lexicographically string-compare
// correctly against the ISO-8601 ingested_at column.
// `buildDateFilter` accepts each component (after stripping the
// optional comparison or range syntax) and rejects malformed input
// with `1=0` rather than passing it into a SQL comparison verbatim,
// which produced silent zero-result answers indistinguishable from a
// real "no images on that date" result.
var dateFilterRe = regexp.MustCompile(`^\d{4}(-\d{2}(-\d{2})?)?$`)

func (b *whereBuilder) buildDateFilter(val string) string {
	if strings.HasPrefix(val, ">") {
		date := val[1:]
		if !dateFilterRe.MatchString(date) {
			return "1=0"
		}
		b.args = append(b.args, date)
		return "i.ingested_at > ?"
	}
	if strings.HasPrefix(val, "<") {
		date := val[1:]
		if !dateFilterRe.MatchString(date) {
			return "1=0"
		}
		b.args = append(b.args, date)
		return "i.ingested_at < ?"
	}
	if idx := strings.Index(val, ".."); idx >= 0 {
		from := val[:idx]
		to := val[idx+2:]
		if !dateFilterRe.MatchString(from) || !dateFilterRe.MatchString(to) {
			return "1=0"
		}
		b.args = append(b.args, from, to)
		return "i.ingested_at BETWEEN ? AND ?"
	}
	if !dateFilterRe.MatchString(val) {
		return "1=0"
	}
	b.args = append(b.args, val, val+"T23:59:59Z")
	return "i.ingested_at BETWEEN ? AND ?"
}

func parseCompOp(val string) (string, string) {
	for _, op := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(val, op) {
			return op, val[len(op):]
		}
	}
	return "=", val
}

// parseIntComp wraps parseCompOp with strict int parsing so non-numeric
// values like `width:>=abc` produce ok=false (and an explicit empty
// result via `1=0`) instead of SQLite silently coercing the operand to
// 0 and returning everything wider than 0.
func parseIntComp(val string) (string, int64, bool) {
	op, raw := parseCompOp(val)
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return op, 0, false
	}
	return op, n, true
}
