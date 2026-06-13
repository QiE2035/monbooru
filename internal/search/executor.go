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
	"github.com/leqwin/monbooru/internal/relations"
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

// imageRowColumns is the canonical SELECT list shared by Execute and
// executeFromCachedIDs. Keeping the column order frozen here means
// scanImageRow's Scan-and-coerce stays the single source of truth for
// the image row shape.
const imageRowColumns = `i.id, i.sha256, i.canonical_path, i.folder_path, i.file_type,
	        i.width, i.height, i.file_size, i.is_missing, i.is_favorited,
	        i.is_inbox, i.auto_tagged_at, i.source_type, i.origin, i.source, i.url, i.page_count, i.duration_seconds, i.series, i.series_order, i.phash, i.ingested_at, i.upload_batch`

// scanImageRow reads one row in the imageRowColumns shape and folds the
// int-as-bool flags + RFC3339 timestamps back onto the typed Image
// struct.
func scanImageRow(rows *sql.Rows) (models.Image, error) {
	var img models.Image
	var isMissing, isFav, isInbox int
	var width, height, pageCount, seriesOrder *int
	var durationSec *float64
	var autoTaggedAt *string
	var phash *int64
	var ingestedAt string
	if err := rows.Scan(
		&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType,
		&width, &height, &img.FileSize, &isMissing, &isFav,
		&isInbox, &autoTaggedAt, &img.SourceType, &img.Origin, &img.Source, &img.URL, &pageCount, &durationSec, &img.Series, &seriesOrder, &phash, &ingestedAt, &img.UploadBatch,
	); err != nil {
		return models.Image{}, err
	}
	img.IsMissing = isMissing == 1
	img.IsFavorited = isFav == 1
	img.IsInbox = isInbox == 1
	img.Width = width
	img.Height = height
	img.PageCount = pageCount
	img.DurationSec = durationSec
	img.SeriesOrder = seriesOrder
	img.Phash = phash
	if autoTaggedAt != nil {
		t, _ := time.Parse(time.RFC3339, *autoTaggedAt)
		img.AutoTaggedAt = &t
	}
	img.IngestedAt, _ = time.Parse(time.RFC3339, ingestedAt)
	return img, nil
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

	where, args, hasMissingFilter, ceilingRewrote := buildWhereDBDriverFull(q.Expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)

	where = andDefaultVisible(where, hasMissingFilter)

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
	indexHint := sortIndexHint(q.Expr, q.Sort, hasMissingFilter, ceilingRewrote)

	dataSQL := fmt.Sprintf(
		"SELECT "+imageRowColumns+`
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
	defer func() { _ = rows.Close() }()

	var images []models.Image
	for rows.Next() {
		img, scanErr := scanImageRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
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
	// Multi-page case: page 1 fans synchronously so the immediate next
	// request hits a populated cache. A previous async fan made page 1
	// look faster on a one-shot bench, but the very next request (the
	// real-user page-flip or detail prev/next) raced the fan and missed
	// the cache. The synchronous fan adds one id-only cursor walk to
	// the page-1 wall, capped at adjacencyCacheMaxIDs and read off the
	// read pool. Pages > 1 still skip the fan because the cache either
	// settled on page 1's request or the operator jumped past it.
	if q.CacheKey != "" && total > 0 && total <= adjacencyCacheMaxIDs {
		if len(images) == total {
			ids := make([]int64, len(images))
			for i, img := range images {
				ids[i] = img.ID
			}
			AdjacencyCacheSet(q.CacheKey, ids)
		} else if page == 1 && AdjacencyCacheTryAcquireFan(q.CacheKey) {
			defer AdjacencyCacheReleaseFan(q.CacheKey)
			ids := fetchSortedMatchIDs(database, indexHint, where, args, orderClause, total)
			if len(ids) > 0 {
				AdjacencyCacheSet(q.CacheKey, ids)
			}
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

	placeholders, args := db.InPlaceholders(pageIDs)
	sql := fmt.Sprintf(
		"SELECT "+imageRowColumns+" FROM images i WHERE i.id IN (%s)", placeholders,
	)
	rows, err := database.Read.Query(sql, args...)
	if err != nil {
		return nil, fmt.Errorf("cached id fetch: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byID := make(map[int64]models.Image, len(pageIDs))
	for rows.Next() {
		img, scanErr := scanImageRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
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
	defer func() { _ = rows.Close() }()
	ids, err := db.ScanIDs(rows)
	if err != nil {
		return nil
	}
	return ids
}

// isPureTagExpr reports whether expr's data SELECT should pin
// idx_images_ingested_visible (or _filesize_visible) instead of
// letting the planner pick. Tag leaves and the cat:/rating:/tagged:/
// autotagged:/inbox: filter keywords qualify because their per-row
// EXISTS rides idx_image_tags_image cleanly. The v1.7.2 metadata
// keywords without a covering column index (width, height, date,
// ratio, pages, tagcount) also qualify: each evaluates to a per-row
// column read or a small predicate, so walking the partial sort
// index in (ingested_at, id) order with the predicate as a filter
// beats the fall-back to idx_images_missing + USE TEMP B-TREE FOR
// ORDER BY. fav / source / source_type (ai) / folder / file_type
// (mime/type) / file_size / collection / hash / duration /
// origin (via) / sd-metadata-backed (name / prompt / model /
// sampler / seed) all have their own selective column or partial
// index so the planner picks fine on its own; pinning the sort
// index there forces a 1 M-row scan that the seek would otherwise
// short-circuit.
// andDefaultVisible appends the default `i.is_missing = 0` predicate
// unless the caller's expression already pinned an explicit missing:
// filter (in which case `hasMissingFilter` is true and `where` is
// returned unchanged).
func andDefaultVisible(where string, hasMissingFilter bool) string {
	if hasMissingFilter {
		return where
	}
	if where == "" {
		return "i.is_missing = 0"
	}
	return where + " AND i.is_missing = 0"
}

// detectPureFolder returns the three-leg split of a root-level
// `folder:<val>` predicate: the equality leg, plus the half-open
// subfolder range [val+"/", val+"0") which mirrors buildFilterExpr's
// folder handling and fastCountFolder's split. '0' is the codepoint
// immediately after '/'; a bare [val, val+"0") would leak siblings
// sharing the prefix followed by an ASCII char below '/' ("anime-2024"
// and "anime " both sit between "anime" and "anime0" lexicographically).
// active is false when expr isn't a bare folder filter and the caller
// should stay on the per-image where + args it already built.
func detectPureFolder(expr Expr) (active bool, eq, lo, hi string) {
	f, ok := expr.(FilterExpr)
	if !ok || f.Key != "folder" || f.Val == "" {
		return false, "", "", ""
	}
	return true, f.Val, f.Val + "/", f.Val + "0"
}

// sortIndexHint returns the ` INDEXED BY ...` clause to pin the partial
// sort index when the planner would otherwise pick idx_images_missing
// and materialise a temp B-tree for ORDER BY. Returns "" when a more
// selective per-column hint should win, or when the query isn't a pure
// tag predicate. A non-empty columnFilterIndexHint always overrides.
func sortIndexHint(expr Expr, sort string, hasMissingFilter, ceilingRewrote bool) string {
	if h := columnFilterIndexHint(expr, sort); h != "" {
		return h
	}
	if hasMissingFilter || (expr != nil && !isPureTagExpr(expr)) {
		return ""
	}
	switch sort {
	case "filesize":
		if ceilingRewrote {
			return " INDEXED BY idx_images_filesize_rating_visible"
		}
		return " INDEXED BY idx_images_filesize_visible"
	case "", "newest":
		if ceilingRewrote {
			return " INDEXED BY idx_images_ingested_rating_visible"
		}
		return " INDEXED BY idx_images_ingested_visible"
	}
	return ""
}

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
		case "cat", "rating", "tagged", "autotagged", "inbox",
			"width", "height", "date", "ratio", "pages", "tagcount":
			return true
		}
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

// columnFilterIndexHint returns the INDEXED BY clause for a single
// column-filter at the root of expr where a partial visible index
// would be more selective than the planner's default fallback to
// idx_images_missing + TEMP B-TREE FOR ORDER BY. Returns "" when
// the expression isn't a single root-filter that needs the hint.
//
// The COLLATE NOCASE equality predicates (`source = ? COLLATE
// NOCASE`, `series = ? COLLATE NOCASE`, `folder_path = ? COLLATE
// NOCASE`) cannot ride a BINARY-collated index, so without the hint
// the planner skips the wider partial idx_images_source / series /
// folder_visible indexes and walks the visibility-only set. The
// matching NOCASE-collated partials let the seek run directly;
// pinning them avoids the planner's selectivity-based fall-back when
// stats predict a large match set.
//
// The sort axis is passed in so high-cardinality IN-list filters
// (type:image, mime:<broad>) can pin the matching sort-visible index
// and walk in order, sidestepping the temp sort that an
// idx_images_missing scan would otherwise force.
func columnFilterIndexHint(expr Expr, sort string) string {
	f, ok := expr.(FilterExpr)
	if !ok || f.Val == "" {
		return ""
	}
	sortHint := func() string {
		switch sort {
		case "filesize":
			return " INDEXED BY idx_images_filesize_visible"
		default:
			return " INDEXED BY idx_images_ingested_visible"
		}
	}
	switch f.Key {
	case "fav":
		// idx_images_favorited_visible is partial WHERE is_favorited = 1;
		// only the positive polarity rides the covering seek. fav:false
		// falls through to the planner's default plan.
		if strings.EqualFold(f.Val, "true") {
			return " INDEXED BY idx_images_favorited_visible"
		}
	case "source":
		return " INDEXED BY idx_images_source_nocase_visible"
	case "collection":
		return " INDEXED BY idx_images_series_nocase"
	case "type", "mime":
		// High-cardinality IN-lists (type:image covers ~80% of rows,
		// mime:png+jpeg about 60%) defeat the partial file-type
		// indexes - the planner sees too many matches and falls back
		// on idx_images_missing + a temp sort over the visible set.
		// Pinning the sort-axis index walks the result in sorted
		// order and pays a cheap per-row file_type IN-list test.
		return sortHint()
	case "ai":
		// ai:none matches the schema default and runs against most
		// rows on a typical library. Pin the sort index so the data
		// SELECT walks (ingested_at, id) order and stops at LIMIT
		// instead of seeking idx_images_source_type_visible and
		// temp-sorting every visible row by ingested_at.
		if strings.ToLower(f.Val) == "none" {
			return sortHint()
		}
	}
	return ""
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
// search. For the bare chain it counts the visible images whose
// effective rating passes the ceiling straight off the maintained
// images.rating_rank column (the highest rank an image carries, -1 when
// unrated). Counting that single per-image rank stays exact when an
// image carries more than one rating tag - summing the excluded tags'
// usage_count would subtract such a row once per excluded level it
// carries and under-report the total. The render path filters the same
// `rating_rank <= ?` predicate, so the count and the rendered page agree.
//
// A user search ANDed onto the ceiling defers to the slow exact COUNT:
// the chain count says nothing about how the user predicate intersects
// it, and a loose bound would advertise phantom trailing pages. The
// slow COUNT then seeds the adjacency cache so later renders ride the
// fast path with no SQL.
func fastCountCeiling(database *db.DB, expr Expr) (int, bool) {
	user, excluded, ok := extractCeilingShape(expr)
	if !ok || len(excluded) == 0 {
		return 0, false
	}
	if user != nil {
		return 0, false
	}
	rank := ceilingRankFromExcluded(excluded)
	if rank < -1 {
		return 0, false
	}
	// Pin idx_images_rating_rank_visible: rating_rank has only five
	// distinct values, so the sampled sqlite_stat1 (analysis_limit=400)
	// underestimates its per-value cardinality and the planner otherwise
	// counts through the wider idx_images_missing, reading every visible
	// images row (seconds on a cold million-row library). The covering
	// partial index answers the range from a few MB of index pages.
	var total int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images INDEXED BY idx_images_rating_rank_visible
		 WHERE is_missing = 0 AND rating_rank <= ?`, rank,
	).Scan(&total); err != nil {
		return 0, false
	}
	return total, true
}

// extractCeilingShape splits an AST into a userExpr remainder and the
// list of excluded rating levels carried by a NotExpr{FilterExpr{
// Key:"rating"}} chain ANDed onto it. Recognises:
//   - A pure chain (no userExpr; user is nil).
//   - AndExpr{userExpr, chain} - the wrapped shape Ceiling.Apply emits.
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

// ceilingRankFromExcluded resolves the highest rank that should pass
// the cookie ceiling, given the rating levels the ceiling chain
// excludes. Returns -2 when no documented level is excluded (the
// caller should skip the rewrite and fall through to the
// per-leaf NOT EXISTS path). Documented levels are general=0,
// sensitive=1, questionable=2, explicit=3; unknown level names get
// filtered so a typo doesn't push the ceiling rank to the floor.
func ceilingRankFromExcluded(levels []string) int {
	minRank := -1
	for _, name := range levels {
		r := ratingRank(name)
		if r < 0 {
			continue
		}
		if minRank < 0 || r < minRank {
			minRank = r
		}
	}
	if minRank < 0 {
		return -2
	}
	return minRank - 1
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
// counts under this cap the slow path finishes inside the search
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

// rankInQueryCountSkip caps the matched-set size RankInQuery will
// run a COUNT against. Popular tags (>50 k carriers) walk newer-than-
// currentID rows in numbers that peg the detail handler's 500 ms ctx
// ceiling even with the AND-driver inline. Below the cap the cursor
// terminates inside the budget; above it the helper short-circuits
// to -1 so the detail page degrades to the URL's back_page without
// blocking the render.
const rankInQueryCountSkip = 50000

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

// collectAndedFilterLeaves returns the FilterExpr leaves whose Key
// matches a real tag category (e.g. `character:miku`) reachable from
// the root through AndExpr nodes. Same suppression rules as
// collectAndedTags: leaves under OrExpr / NotExpr / non-tag-category
// FilterExpr are skipped. Category recognition delegates to the where
// builder's b.categoryExists helper at resolve time so a nil db
// (test path) doesn't break this collector; here we just filter out
// obvious non-tag keywords.
func collectAndedFilterLeaves(expr Expr) []FilterExpr {
	var out []FilterExpr
	var walk func(Expr)
	walk = func(e Expr) {
		switch v := e.(type) {
		case AndExpr:
			walk(v.Left)
			walk(v.Right)
		case FilterExpr:
			if v.Val == "" || searchkw.IsKeyword(v.Key) {
				return
			}
			out = append(out, v)
		}
	}
	walk(expr)
	return out
}

// andDriverLeg pairs an ANDed leaf with the canonical tag IDs the
// driver materialises for it. The leaf is a TagExpr or a
// category-qualified FilterExpr (`character:miku`) - the where builder
// checks driverLeaves on both shapes so the per-row EXISTS the leaf
// would otherwise emit is suppressed. Multiple legs feed an INTERSECT
// chain in applyAndDriver; a single leg uses the simpler IN form.
// idBound, when > 0, is pushed into each leg's IN subquery as
// `AND image_id >= ?` so the materialisation is capped to the recent
// id range covering the requested page.
type andDriverLeg struct {
	leaf      Expr
	ids       []int64
	idBound   int64
	idBoundHi int64
}

// pickAndDriverTag chooses one or more ANDed leaves (TagExpr or
// category-qualified FilterExpr) to feed the driver as a non-correlated
// `i.id IN (SELECT image_id FROM image_tags WHERE tag_id IN (...))`
// predicate that bounds the candidate set before the outer query runs.
// Each chosen leaf has its correlated EXISTS suppressed in
// buildWhereDBDriver so the predicate isn't paid twice. Returns
// ok=false when nothing can be picked.
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
// allowSingleLiteral=true overrides this for the random-sort and the
// COUNT-cursor callers, where there is no covering index for the
// synthetic random key (random sort) or no LIMIT to short-circuit the
// per-row EXISTS scan (rank COUNT).
func pickAndDriverTag(database *db.DB, expr Expr, allowSingleLiteral bool) ([]andDriverLeg, bool) {
	if database == nil {
		return nil, false
	}
	tagLeaves := collectAndedTags(expr)
	filterLeaves := collectAndedFilterLeaves(expr)
	if len(tagLeaves)+len(filterLeaves) == 0 {
		return nil, false
	}
	hasWildcard := false
	for _, leaf := range tagLeaves {
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
	// index; the materialised set bounds the temp-sort input. RankInQuery
	// also flips the override on because its COUNT cursor has no LIMIT
	// to short-circuit the per-row EXISTS.
	if !hasWildcard && (len(tagLeaves)+len(filterLeaves)) < 2 && !allowSingleLiteral {
		return nil, false
	}

	type resolved struct {
		leaf  Expr
		ids   []int64
		usage int64
	}
	seenTag := make(map[TagExpr]bool, len(tagLeaves))
	seenFilter := make(map[FilterExpr]bool, len(filterLeaves))
	var legs []resolved
	for _, leaf := range tagLeaves {
		if seenTag[leaf] {
			continue
		}
		seenTag[leaf] = true
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
	for _, leaf := range filterLeaves {
		if seenFilter[leaf] {
			continue
		}
		seenFilter[leaf] = true
		ids, usage, ok := resolveFilterDriverCanonicals(database, leaf)
		if !ok {
			// Key isn't a real tag category, or the lookup failed -
			// fall back on the slow EXISTS shape (already correct).
			continue
		}
		if len(ids) == 0 {
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

	// Random sort with a single popular leaf: the data SELECT's
	// ORDER BY ((id * mixed) & ...) has no covering index, so the slow
	// path TEMP-B-TREEs every visible row carrying the predicate. The
	// IN-driver materialisation walks the same row count via
	// idx_image_tags_tag_image and feeds the temp sort a bounded id
	// stream instead of EXISTS-probing every visible image - the gate
	// stays scoped to random sort so indexed-sort callers keep their
	// existing planner choice on a single popular literal.
	if allowSingleLiteral && len(legs) == 1 {
		return []andDriverLeg{{leaf: legs[0].leaf, ids: legs[0].ids}}, true
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
		arg = db.EscapeLike(leaf.Tag) + "%"
	case "suffix":
		pred = `t.name LIKE ? ESCAPE '\'`
		arg = "%" + db.EscapeLike(leaf.Tag)
	case "substring":
		pred = `t.name LIKE ? ESCAPE '\'`
		arg = "%" + db.EscapeLike(leaf.Tag) + "%"
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
	defer func() { _ = rows.Close() }()
	return drainCanonicalUsage(rows)
}

// resolveFilterDriverCanonicals reads the canonical tag IDs and the
// sum of their usage_count for a category-qualified FilterExpr leaf
// (e.g. `character:miku`). Same query shape as the whereBuilder's
// resolveCategoryTagByName, with usage_count added so the leg can
// participate in the smallest-first pick. Returns ok=false when the
// key isn't a real tag_categories row.
func resolveFilterDriverCanonicals(database *db.DB, leaf FilterExpr) ([]int64, int64, bool) {
	if leaf.Key == "" || leaf.Val == "" {
		return nil, 0, false
	}
	rows, err := database.Read.Query(
		`SELECT DISTINCT COALESCE(t.canonical_tag_id, t.id), canon.usage_count
		   FROM tags t
		   JOIN tag_categories tc ON tc.id = t.category_id
		   JOIN tags canon ON canon.id = COALESCE(t.canonical_tag_id, t.id)
		  WHERE t.name = ? AND tc.name = ?`,
		strings.ToLower(leaf.Val), leaf.Key,
	)
	if err != nil {
		return nil, 0, false
	}
	defer func() { _ = rows.Close() }()
	return drainCanonicalUsage(rows)
}

// drainCanonicalUsage scans (canonical_id, usage_count) rows into a
// deduped id slice and the usage sum. ok=false on any read error so
// callers fall back to the un-driven plan.
func drainCanonicalUsage(rows *sql.Rows) ([]int64, int64, bool) {
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
// idBound / idBoundHi, when set per-leg, append `AND image_id >= ?`
// (and optionally `AND image_id <= ?`) to that leg's subquery so the
// materialisation is capped to the caller-supplied id range. The
// newest-sort callers use the lower bound to keep just the recent
// slice; ExecuteAdjacent's bucket gate uses both bounds to keep the
// materialised set inside the bucket window so the per-row EXISTS
// it would otherwise pay collapses to a small IN check.
func applyAndDriver(where string, args []any, legs []andDriverLeg) (string, []any) {
	if len(legs) == 0 {
		return where, args
	}
	driverArgs := make([]any, 0)
	parts := make([]string, len(legs))
	for i, leg := range legs {
		placeholders, idArgs := db.InPlaceholders(leg.ids)
		parts[i] = "SELECT image_id FROM image_tags WHERE tag_id IN (" + placeholders + ")"
		driverArgs = append(driverArgs, idArgs...)
		if leg.idBound > 0 {
			parts[i] += " AND image_id >= ?"
			driverArgs = append(driverArgs, leg.idBound)
		}
		if leg.idBoundHi > 0 {
			parts[i] += " AND image_id <= ?"
			driverArgs = append(driverArgs, leg.idBoundHi)
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
// late in the result set at large-fixture scale. Bucketing by id caps
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
		arg = db.EscapeLike(t.Tag) + "%"
	case "suffix":
		pred = `name LIKE ? ESCAPE '\'`
		arg = "%" + db.EscapeLike(t.Tag)
	case "substring":
		pred = `name LIKE ? ESCAPE '\'`
		arg = "%" + db.EscapeLike(t.Tag) + "%"
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
	canonIDs, scanErr := db.ScanIDs(rows)
	_ = rows.Close()
	if scanErr != nil {
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
	placeholders, args := db.InPlaceholders(canonIDs)
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
		strings.ToLower(e.Val), catID,
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
// two seeks against the partial idx_images_folder_nocase_visible: one
// for the folder itself, one half-open range for paths beneath it.
// Same match set as the slow path's `(folder = ? OR folder LIKE ?
// ESCAPE '\')` shape.
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
// COLLATE NOCASE matches the slow path so `folder:Characters` resolves
// against operator-edited folder names regardless of case.
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
		        WHERE folder_path = ? COLLATE NOCASE AND is_missing = 0)
		   + (SELECT COUNT(*) FROM images INDEXED BY idx_images_folder_nocase_visible
		        WHERE folder_path >= ? COLLATE NOCASE
		          AND folder_path < ? COLLATE NOCASE
		          AND is_missing = 0)
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
			_ = rows.Close()
			return 0, false
		}
		present = append(present, s)
	}
	_ = rows.Close()
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
	placeholders, args := db.InPlaceholders(matching)
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images WHERE source_type IN (`+placeholders+`) AND is_missing = 0`,
		args...,
	).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// parseBoolVal converts the documented boolean filter values plus the
// common aliases (yes/no, y/n, on/off, 1/0) into a canonical bool.
// ok=false signals the value wasn't a recognised boolean: callers in
// buildFilterExpr emit 1=0 so a typo like `inbox:maybe` produces an
// explicit empty result instead of silently flipping to the false
// cohort the user did not ask for.
func parseBoolVal(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "y", "on", "1":
		return true, true
	case "false", "no", "n", "off", "0":
		return false, true
	}
	return false, false
}

// fastCountTagged returns the exact fast-path count for tagged:true and
// autotagged:true. Computes visible_total - untagged_visible: the
// untagged subtrahend is a NOT-EXISTS walk over image_tags that hits
// multi-second p95 on a million-row library, so it rides the DB-level
// count cache that InvalidateCachedCounts drops on every membership
// write. Falls back to (0, false) on any DB error so the slow path
// takes over.
func fastCountTagged(database *db.DB, e FilterExpr) (int, bool) {
	if e.Key != "tagged" && e.Key != "autotagged" {
		return 0, false
	}
	val, ok := parseBoolVal(e.Val)
	if !ok || !val {
		return 0, false
	}
	visible, ok := fastVisibleCount(database)
	if !ok {
		return 0, false
	}
	var untagged int
	if e.Key == "autotagged" {
		untagged, ok = database.AutoUntaggedVisibleCount()
	} else {
		untagged, ok = database.UntaggedVisibleCount()
	}
	if !ok {
		return 0, false
	}
	n := visible - untagged
	if n < 0 {
		n = 0
	}
	return n, true
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
	val, ok := parseBoolVal(e.Val)
	if !ok {
		// Unparseable boolean: the slow path emits 1=0 (no rows match)
		// so the count is exactly zero. Mark as known so Execute's
		// fastEmpty path short-circuits the data SELECT.
		return 0, true
	}
	target := 0
	if val {
		target = 1
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

// orderCursor builds the cursor predicates and sort clauses for sort=order,
// matching buildOrder's (series, series_order NULLS-last, id) total order.
// before/after match the rows positioned before/after the current row; fwd
// and rev are the forward and reversed ORDER BY for the LIMIT-1 seeks.
func orderCursor(series string, order sql.NullInt64, id int64, desc bool) (before, after, fwd, rev string, beforeArgs, afterArgs []any) {
	if desc {
		fwd = "ORDER BY i.series DESC, i.series_order IS NULL, i.series_order DESC, i.id DESC"
		rev = "ORDER BY i.series ASC, i.series_order IS NULL DESC, i.series_order ASC, i.id ASC"
		if order.Valid {
			before = "(i.series > ? OR (i.series = ? AND i.series_order IS NOT NULL AND (i.series_order, i.id) > (?, ?)))"
			after = "(i.series < ? OR (i.series = ? AND (i.series_order IS NULL OR (i.series_order, i.id) < (?, ?))))"
			beforeArgs = []any{series, series, order.Int64, id}
			afterArgs = []any{series, series, order.Int64, id}
		} else {
			before = "(i.series > ? OR (i.series = ? AND (i.series_order IS NOT NULL OR (i.series_order IS NULL AND i.id > ?))))"
			after = "(i.series < ? OR (i.series = ? AND i.series_order IS NULL AND i.id < ?))"
			beforeArgs = []any{series, series, id}
			afterArgs = []any{series, series, id}
		}
		return
	}
	fwd = "ORDER BY i.series ASC, i.series_order IS NULL, i.series_order ASC, i.id ASC"
	rev = "ORDER BY i.series DESC, i.series_order IS NULL DESC, i.series_order DESC, i.id DESC"
	if order.Valid {
		before = "(i.series < ? OR (i.series = ? AND i.series_order IS NOT NULL AND (i.series_order, i.id) < (?, ?)))"
		after = "(i.series > ? OR (i.series = ? AND (i.series_order IS NULL OR (i.series_order, i.id) > (?, ?))))"
		beforeArgs = []any{series, series, order.Int64, id}
		afterArgs = []any{series, series, order.Int64, id}
	} else {
		before = "(i.series < ? OR (i.series = ? AND (i.series_order IS NOT NULL OR (i.series_order IS NULL AND i.id < ?))))"
		after = "(i.series > ? OR (i.series = ? AND i.series_order IS NULL AND i.id > ?))"
		beforeArgs = []any{series, series, id}
		afterArgs = []any{series, series, id}
	}
	return
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
	var series string
	var seriesOrder sql.NullInt64
	if err := database.Read.QueryRow(
		`SELECT ingested_at, file_size, series, series_order FROM images WHERE id = ?`, currentID,
	).Scan(&ingestedAt, &fileSize, &series, &seriesOrder); err != nil {
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
	} else {
		// Bucket gate fired. Pre-resolve any wildcard tag predicate
		// to its canonical id list and bound the materialisation to
		// the bucket window so the per-row EXISTS the slow path would
		// pay (one image_tags seek per matching tag_id, per bucket
		// row) collapses to a small IN check on the cursor's outer
		// walk. A popular substring or prefix wildcard at root would
		// otherwise pay ~30 tag_id seeks per each of 2 000 bucket
		// rows; with the bucket bound the materialised set drops to
		// whatever lives inside that 2 000-id window.
		driverLegs, _ = pickAndDriverTag(database, q.Expr, q.Sort == "random")
		for i := range driverLegs {
			driverLegs[i].idBound = bucketLo
			driverLegs[i].idBoundHi = bucketHi
		}
	}
	where, args, hasMissingFilter, ceilingRewrote := buildWhereDBDriverFull(q.Expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)

	// On a pure folder predicate the lookup builds two seekable legs as
	// a UNION ALL on idx_images_folder_nocase_visible; the per-image
	// where is dropped so the legs don't double-count the placeholders.
	folderActive, folderEq, folderRangeLo, folderRangeHi := detectPureFolder(q.Expr)
	// sort=order has no single key column for the folder UNION-ALL legs.
	if q.Sort == "order" {
		folderActive = false
	}
	if folderActive {
		where = ""
		args = args[:0]
	}

	where = andDefaultVisible(where, hasMissingFilter)

	if bucketed {
		where = where + " AND i.id BETWEEN ? AND ?"
		args = append(args, bucketLo, bucketHi)
	}

	var keyCol string
	var keyVal any
	var prevCmp, nextCmp, prevSort, nextSort string
	var prevArgs, nextArgs []any
	if q.Sort == "order" {
		before, after, fwd, rev, bArgs, aArgs := orderCursor(series, seriesOrder, currentID, q.Order == "desc")
		prevCmp, prevSort, prevArgs = before, rev, bArgs
		nextCmp, nextSort, nextArgs = after, fwd, aArgs
	} else {
		switch q.Sort {
		case "random":
			if q.RandomSeed == 0 {
				return nil, nil, nil
			}
			// SAFETY: %d only produces digits; literal seed interpolation
			// is injection-safe. db.RandomSortKey mirrors random_key()'s
			// hash so the cursor compares Go-computed and SQLite-computed
			// keys against the same scrambled space.
			keyCol = fmt.Sprintf("random_key(i.id, %d)", q.RandomSeed)
			keyVal = int64(db.RandomSortKey(currentID, q.RandomSeed))
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
		prevArgs = []any{keyVal, currentID}
		nextArgs = []any{keyVal, currentID}
	}

	// Pin the partial sort index when nothing in the query has its own
	// more-selective column index, otherwise the planner can pick
	// idx_images_missing and emit a TEMP B-TREE FOR ORDER BY on libraries
	// where is_missing=0 has near-zero selectivity. Mirrors the hint in
	// Execute.
	indexHint := sortIndexHint(q.Expr, q.Sort, hasMissingFilter, ceilingRewrote)

	lookup := func(cursorCmp, sort string, cursorArgs []any) *int64 {
		var sql string
		var qargs []any
		if folderActive {
			// UNION ALL of the equality and range legs, each pinned to
			// idx_images_folder_nocase_visible so the planner runs two
			// tight seeks instead of one OR-of-(equality, range) that
			// SQLite resolves with a full index scan + TEMP B-TREE
			// sort. The outer SELECT picks the closer of the two leg
			// winners under the shared cursor sort.
			outer := strings.ReplaceAll(strings.ReplaceAll(sort, keyCol, "k"), "i.id", "id")
			legSQL := "SELECT i.id AS id, " + keyCol + " AS k FROM images i INDEXED BY idx_images_folder_nocase_visible WHERE %s AND " + where + " AND " + cursorCmp + " " + sort + " LIMIT 1"
			sql = "SELECT id FROM (SELECT * FROM (" + fmt.Sprintf(legSQL, "i.folder_path = ? COLLATE NOCASE") +
				") UNION ALL SELECT * FROM (" + fmt.Sprintf(legSQL, "i.folder_path >= ? COLLATE NOCASE AND i.folder_path < ? COLLATE NOCASE") +
				")) " + outer + " LIMIT 1"
			qargs = make([]any, 0, len(args)+8)
			// Leg 1 (equality): folder val, plus the shared where/cursor args
			qargs = append(qargs, folderEq)
			qargs = append(qargs, args...)
			qargs = append(qargs, cursorArgs...)
			// Leg 2 (range): folder lo, folder hi, plus the shared args
			qargs = append(qargs, folderRangeLo, folderRangeHi)
			qargs = append(qargs, args...)
			qargs = append(qargs, cursorArgs...)
		} else {
			qargs = make([]any, 0, len(args)+len(cursorArgs))
			qargs = append(qargs, args...)
			qargs = append(qargs, cursorArgs...)
			sql = fmt.Sprintf("SELECT i.id FROM images i%s WHERE %s AND %s %s LIMIT 1",
				indexHint, where, cursorCmp, sort)
		}
		var id int64
		if err := database.Read.QueryRow(sql, qargs...).Scan(&id); err != nil {
			return nil
		}
		return &id
	}
	return lookup(prevCmp, prevSort, prevArgs), lookup(nextCmp, nextSort, nextArgs), nil
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
	// Skip when the rank COUNT would walk a popular-tag candidate set
	// large enough to peg the ctx ceiling. The COUNT cursor walks every
	// matched row newer-than-currentID; for a 50 k-row materialised
	// set the natural cost lands near the 500 ms budget even with the
	// AND-driver inline, so the handler falls back to the URL's
	// back_page rather than blocking the page render.
	if total, ok := fastTagTotal(database, q.Expr); ok && total > rankInQueryCountSkip {
		return -1, nil
	}

	var ingestedAt string
	var fileSize int64
	var series string
	var seriesOrder sql.NullInt64
	if err := database.Read.QueryRowContext(ctx,
		`SELECT ingested_at, file_size, series, series_order FROM images WHERE id = ?`, currentID,
	).Scan(&ingestedAt, &fileSize, &series, &seriesOrder); err != nil {
		return -1, err
	}

	// allowSingleLiteral=true regardless of sort: this COUNT has no
	// LIMIT to short-circuit the per-row EXISTS scan a single-literal
	// at root would otherwise pay. Execute and ExecuteAdjacent gate on
	// random sort because the planner's default newest-sort plan
	// terminates at LIMIT; here the cursor walks newer-than-current
	// rows to completion.
	driverLegs, _ := pickAndDriverTag(database, q.Expr, true)
	where, args, hasMissingFilter, ceilingRewrote := buildWhereDBDriverFull(q.Expr, database, driverLegs)
	where, args = applyAndDriver(where, args, driverLegs)

	// Mirrors ExecuteAdjacent: a pure folder predicate runs as a
	// COUNT-UNION-ALL of equality and range legs, sidestepping the
	// full idx_images_folder_nocase_visible scan the OR-of-(equality,
	// range) shape forced.
	folderActive, folderEq, folderRangeLo, folderRangeHi := detectPureFolder(q.Expr)
	// sort=order has no single key column for the folder UNION-ALL legs.
	if q.Sort == "order" {
		folderActive = false
	}
	if folderActive {
		where = ""
		args = args[:0]
	}

	where = andDefaultVisible(where, hasMissingFilter)

	var keyCol string
	var keyVal any
	var beforeCmp string
	var beforeArgs []any
	if q.Sort == "order" {
		before, _, _, _, bArgs, _ := orderCursor(series, seriesOrder, currentID, q.Order == "desc")
		beforeCmp = before
		beforeArgs = bArgs
	} else {
		switch q.Sort {
		case "random":
			if q.RandomSeed == 0 {
				return -1, nil
			}
			// SAFETY: %d only produces digits; literal seed interpolation
			// is injection-safe. db.RandomSortKey mirrors random_key()
			// so the rank COUNT compares Go-computed and SQLite-computed
			// keys against the same scrambled space.
			keyCol = fmt.Sprintf("random_key(i.id, %d)", q.RandomSeed)
			keyVal = int64(db.RandomSortKey(currentID, q.RandomSeed))
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
		if q.Order == "asc" || q.Sort == "random" {
			beforeCmp = fmt.Sprintf("(%s, i.id) < (?, ?)", keyCol)
		} else {
			beforeCmp = fmt.Sprintf("(%s, i.id) > (?, ?)", keyCol)
		}
		beforeArgs = []any{keyVal, currentID}
	}

	indexHint := sortIndexHint(q.Expr, q.Sort, hasMissingFilter, ceilingRewrote)

	var sql string
	var qargs []any
	if folderActive {
		legSQL := "SELECT 1 FROM images i INDEXED BY idx_images_folder_nocase_visible WHERE %s AND " + where + " AND " + beforeCmp
		sql = "SELECT COUNT(*) FROM (" +
			fmt.Sprintf(legSQL, "i.folder_path = ? COLLATE NOCASE") +
			" UNION ALL " +
			fmt.Sprintf(legSQL, "i.folder_path >= ? COLLATE NOCASE AND i.folder_path < ? COLLATE NOCASE") +
			")"
		qargs = make([]any, 0, len(args)*2+8)
		qargs = append(qargs, folderEq)
		qargs = append(qargs, args...)
		qargs = append(qargs, beforeArgs...)
		qargs = append(qargs, folderRangeLo, folderRangeHi)
		qargs = append(qargs, args...)
		qargs = append(qargs, beforeArgs...)
	} else {
		sql = fmt.Sprintf(
			"SELECT COUNT(*) FROM images i%s WHERE %s AND %s",
			indexHint, where, beforeCmp,
		)
		qargs = make([]any, 0, len(args)+len(beforeArgs))
		qargs = append(qargs, args...)
		qargs = append(qargs, beforeArgs...)
	}

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
	where = andDefaultVisible(where, hasMissingFilter)

	rows, err := database.Read.Query(
		"SELECT i.id, i.canonical_path, i.folder_path, i.is_missing FROM images i WHERE "+where+" ORDER BY i.id",
		args...,
	)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

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

	placeholders, args := db.InPlaceholders(imageIDs)

	rows, err := database.Read.Query(
		fmt.Sprintf(
			`WITH tag_counts AS (
			     SELECT t.id AS tag_id, t.name AS tag_name, tc.name AS cat_name,
			            tc.color AS cat_color, t.usage_count,
			            COUNT(DISTINCT it.image_id) AS page_count
			     FROM image_tags it INDEXED BY idx_image_tags_image
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
	defer func() { _ = rows.Close() }()

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
	where = andDefaultVisible(where, hasMissingFilter)

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
		defer func() { _ = rows.Close() }()
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
			// loads. random_key() applies a SplitMix64-style hash to
			// (id, seed) so consecutive ids end up at unrelated
			// positions even for small seeds. id remains the
			// tiebreaker for a total order so pagination doesn't
			// repeat or skip.
			// SAFETY: %d only produces digits; literal interpolation
			// of the seed is injection-safe.
			return fmt.Sprintf("ORDER BY random_key(i.id, %d), i.id", randomSeed)
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
	// driverLeaves names the ANDed leaves whose correlated EXISTS is
	// suppressed because the caller has prepended a non-correlated
	// `i.id IN (...)` driver covering the same rows. Keys are either
	// TagExpr (literal or wildcard) or FilterExpr (category-qualified).
	// Interface equality compares (concrete type, field values) so a
	// literal `blue` does not silence a `blue*` leaf, nor does
	// `character:miku` silence `artist:miku`. Multi-entry sets
	// correspond to the popular-AND INTERSECT path; single-entry to
	// the rare-tag-wins single-leg path.
	driverLeaves map[Expr]bool
	// ceilingRewrote records that peelCeilingForColumnRewrite swapped
	// the NOT EXISTS chain for an `i.rating_rank <= ?` predicate.
	// Callers consult buildResult to pick the rating-aware partial
	// sort index instead of the bare ingested / filesize partials so
	// the deep-page cursor stays covering.
	ceilingRewrote bool
	// relPresence caches per-source "has any row?" results so
	// repeated relation: predicates inside one builder walk share
	// the lookups. Populated lazily on first relation: encounter;
	// the relPresenceResolved bool gates re-query.
	relPresence         relationPresence
	relPresenceResolved bool
}

// relationPresence is the per-source existence answer used to
// short-circuit relation: predicates against an empty source. A nil
// db (test path) leaves every field false and the builder falls back
// to the regular EXISTS / NOT-IN shape.
type relationPresence struct {
	dup        bool
	alt        bool
	version    bool
	derivative bool
	series     bool
}

// resolveRatingIDs queries the four canonical rating tag rows and caches
// the result on the builder. A row missing from the result (e.g. the tag
// was pruned at runtime) leaves the entry absent from the map; callers
// fall back to a no-match predicate when their target name is unmapped.
func (b *whereBuilder) resolveRatingIDs() {
	if b.ratingResolved {
		return
	}
	if b.db == nil {
		b.ratingResolved = true
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
	defer func() { _ = rows.Close() }()
	ids := make(map[string]int64, 4)
	usage := make(map[string]int64, 4)
	for rows.Next() {
		var name string
		var id, count int64
		if err := rows.Scan(&name, &id, &count); err != nil {
			return
		}
		ids[name] = id
		usage[name] = count
	}
	// Only latch the cache as authoritative on a clean read - a torn
	// cursor must not leave a partial map that turns rating:X into 1=0.
	if err := rows.Err(); err != nil {
		return
	}
	b.ratingIDs = ids
	b.ratingUsage = usage
	b.ratingResolved = true
}

// resolveCategoryTagByName reads the canonical tag_ids that match
// the named category-qualified tag name (e.g. character:hatsune_miku
// resolves to {miku} plus any aliases that point at it). Returns
// ok=false on a nil-db builder or on a query error so callers fall
// back to the 2-table join shape.
func (b *whereBuilder) resolveCategoryTagByName(category, name string) ([]int64, bool) {
	if b.db == nil || category == "" || name == "" {
		return nil, false
	}
	rows, err := b.db.Read.Query(
		`SELECT DISTINCT COALESCE(t.canonical_tag_id, t.id)
		   FROM tags t
		   JOIN tag_categories tc ON tc.id = t.category_id
		  WHERE t.name = ? AND tc.name = ?`,
		name, category,
	)
	if err != nil {
		return nil, false
	}
	defer func() { _ = rows.Close() }()
	ids, err := db.ScanIDs(rows)
	if err != nil {
		return nil, false
	}
	return ids, true
}

// inlineImageTagsTagIDExists emits an EXISTS predicate against
// image_tags that checks the per-image's tag_id is in the supplied
// id list. The ids are inlined as %d literals so the planner sees a
// constant predicate it can short-circuit, and so the predicate
// rides idx_image_tags_image (image_id, tag_id) without dragging
// tags / tag_categories into the per-row evaluation.
func inlineImageTagsTagIDExists(ids []int64) string {
	if len(ids) == 0 {
		return "1=0"
	}
	var b strings.Builder
	b.WriteString("EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = i.id AND it.tag_id IN (")
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", id)
	}
	b.WriteString("))")
	return b.String()
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

// buildWhereDBDriver is buildWhereDB with a driver-leaves hint: leaves
// (TagExpr or category-qualified FilterExpr) present in the set emit
// no SQL, because the caller has prepended a non-correlated IN(...)
// (or IN INTERSECT) predicate covering the same rows. An empty/nil
// set leaves the regular build path untouched.
func buildWhereDBDriver(expr Expr, database *db.DB, legs []andDriverLeg) (string, []any, bool) {
	where, args, hasMissing, _ := buildWhereDBDriverFull(expr, database, legs)
	return where, args, hasMissing
}

// buildWhereDBDriverFull mirrors buildWhereDBDriver but exposes the
// ceiling-rewrote signal. Callers that pick a sort-axis INDEXED BY
// hint check the bool to switch to the rating-aware partial covering
// index (idx_images_ingested_rating_visible /
// idx_images_filesize_rating_visible) so the deep-page cursor walks
// the rating-rank-filtered set entirely off the index.
func buildWhereDBDriverFull(expr Expr, database *db.DB, legs []andDriverLeg) (string, []any, bool, bool) {
	var leaves map[Expr]bool
	if len(legs) > 0 {
		leaves = make(map[Expr]bool, len(legs))
		for _, l := range legs {
			leaves[l.leaf] = true
		}
	}
	b := &whereBuilder{db: database, driverLeaves: leaves}
	expr = b.peelCeilingForColumnRewrite(expr)
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
	return where, b.args, b.hasMissingFilter, b.ceilingRewrote
}

// peelCeilingForColumnRewrite swaps the Ceiling.Apply chain of
// `NOT EXISTS (rating:LEVEL)` for an `i.rating_rank <= ?` predicate so
// the deep-gallery cursor walks the partial
// idx_images_rating_rank_visible covering index instead of three per-
// row correlated subqueries. The remainder (the userExpr part of the
// AST minus the chain) is returned for the regular buildExpr walk.
// Skips on nil db (test path without the column) and on shapes that
// don't carry a recognisable chain.
func (b *whereBuilder) peelCeilingForColumnRewrite(expr Expr) Expr {
	if b.db == nil {
		return expr
	}
	user, levels, ok := extractCeilingShape(expr)
	if !ok {
		return expr
	}
	rank := ceilingRankFromExcluded(levels)
	if rank < 0 {
		// Unknown levels only - leave the chain alone so the slow path
		// keeps the strict-but-correct NOT EXISTS shape.
		return expr
	}
	b.parts = append(b.parts, "i.rating_rank <= ?")
	b.args = append(b.args, rank)
	b.ceilingRewrote = true
	return user
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
	where, args, hasMissing, _ := buildWhereDBDriverFull(expr, database, nil)
	return where, args, hasMissing
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
		b.args = append(b.args, db.EscapeLike(e.Tag)+"%")
		return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE name LIKE ? ESCAPE '\')`, false)
	case "suffix":
		b.args = append(b.args, "%"+db.EscapeLike(e.Tag))
		return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE name LIKE ? ESCAPE '\')`, false)
	case "substring":
		b.args = append(b.args, "%"+db.EscapeLike(e.Tag)+"%")
		return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE name LIKE ? ESCAPE '\')`, false)
	default:
		b.args = append(b.args, e.Tag)
		return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE name = ?)`, false)
	}
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

// scalarComp emits template with op spliced in and n bound. ok=false
// collapses to "1=0" so each scalar filter case stays one expression.
func (b *whereBuilder) scalarComp(template, op string, n any, ok bool) string {
	if !ok {
		return "1=0"
	}
	b.args = append(b.args, n)
	return fmt.Sprintf(template, op)
}

func (b *whereBuilder) dualMetadataLike(sdCol, comfyCol, val string) string {
	if val == "" {
		return "1=0"
	}
	pat := "%" + db.EscapeLike(val) + "%"
	b.args = append(b.args, pat, pat)
	sm := b.imageIDExists("sd_metadata sm", "sm", "sm."+sdCol+` LIKE ? ESCAPE '\'`, false)
	cm := b.imageIDExists("comfyui_metadata cm", "cm", "cm."+comfyCol+` LIKE ? ESCAPE '\'`, false)
	return "(" + sm + " OR " + cm + ")"
}

// fileTypeInClause emits `i.file_type IN (...)`. Pass tautologyCap=nil
// to skip the every-bucket short-circuit - the type: aliases (image /
// archive / animated) want it, the mime: filter doesn't.
func fileTypeInClause(seen, tautologyCap map[string]bool) string {
	if len(seen) == 0 {
		return "1=0"
	}
	if tautologyCap != nil && len(seen) == len(tautologyCap) {
		return "1=1"
	}
	quoted := make([]string, 0, len(seen))
	for ft := range seen {
		quoted = append(quoted, "'"+ft+"'")
	}
	sort.Strings(quoted)
	return "i.file_type IN (" + strings.Join(quoted, ", ") + ")"
}

// filterBuilders dispatches FilterExpr.Key to the per-key builder.
// Unknown keys fall through to buildDefaultFilter (category-qualified
// tag searches plus literal colon-bearing tag names).
var filterBuilders = map[string]func(*whereBuilder, FilterExpr) string{
	"system":     (*whereBuilder).buildSystemFilter,
	"fav":        (*whereBuilder).buildFavFilter,
	"inbox":      (*whereBuilder).buildInboxFilter,
	"ai":         (*whereBuilder).buildAIFilter,
	"source":     (*whereBuilder).buildSourceFilter,
	"cat":        (*whereBuilder).buildCatFilter,
	"width":      (*whereBuilder).buildWidthFilter,
	"height":     (*whereBuilder).buildHeightFilter,
	"date":       func(b *whereBuilder, e FilterExpr) string { return b.buildDateFilter(e.Val) },
	"missing":    (*whereBuilder).buildMissingFilter,
	"type":       (*whereBuilder).buildTypeFilter,
	"collection": (*whereBuilder).buildCollectionFilter,
	"pages":      (*whereBuilder).buildPagesFilter,
	"name":       (*whereBuilder).buildNameFilter,
	"size":       (*whereBuilder).buildSizeFilter,
	"mime":       (*whereBuilder).buildMimeFilter,
	"ratio":      (*whereBuilder).buildRatioFilter,
	"tagcount":   (*whereBuilder).buildTagcountFilter,
	"duration":   (*whereBuilder).buildDurationFilter,
	"hash":       (*whereBuilder).buildHashFilter,
	"id":         (*whereBuilder).buildIDFilter,
	"phash":      (*whereBuilder).buildPhashFilter,
	"relation":   (*whereBuilder).buildRelationFilter,
	"prompt":     (*whereBuilder).buildPromptFilter,
	"model":      (*whereBuilder).buildModelFilter,
	"sampler":    (*whereBuilder).buildSamplerFilter,
	"seed":       (*whereBuilder).buildSeedFilter,
	"via":        (*whereBuilder).buildViaFilter,
	"tagged":     (*whereBuilder).buildTaggedFilter,
	"autotagged": (*whereBuilder).buildAutotaggedFilter,
	"folder":     (*whereBuilder).buildFolderFilter,
	"folderonly": (*whereBuilder).buildFolderonlyFilter,
	"generated":  (*whereBuilder).buildGeneratedFilter,
	"rating":     (*whereBuilder).buildRatingFilter,
}

func (b *whereBuilder) buildFilterExpr(e FilterExpr) string {
	if h, ok := filterBuilders[e.Key]; ok {
		return h(b, e)
	}
	return b.buildDefaultFilter(e)
}

// buildSystemFilter is the autocomplete-only cheat-sheet trigger; a
// bare `system:` query must not fall into buildDefaultFilter's
// match-all branch.
func (b *whereBuilder) buildSystemFilter(_ FilterExpr) string {
	return "1=0"
}

// boolColumnFilter parses a bool from val and returns "col = 1" /
// "col = 0", or "1=0" on a parse failure (no row matches, so the
// AND with the rest of the WHERE short-circuits).
func boolColumnFilter(col, val string) string {
	b, ok := parseBoolVal(val)
	if !ok {
		return "1=0"
	}
	if b {
		return col + " = 1"
	}
	return col + " = 0"
}

func (b *whereBuilder) buildFavFilter(e FilterExpr) string {
	return boolColumnFilter("i.is_favorited", e.Val)
}

func (b *whereBuilder) buildInboxFilter(e FilterExpr) string {
	return boolColumnFilter("i.is_inbox", e.Val)
}

// buildAIFilter accepts comma-separated source_type and the legacy
// "sd" alias. "any" matches any image carrying a1111 and/or comfyui
// metadata. "none" is the schema default for non-AI images and is
// never combined with another tool in source_type, so it collapses to
// a single-column equality the partial source_type_visible index can
// seek - the four-LIKE shape below would force the planner past
// idx_images_source_type onto idx_images_missing.
func (b *whereBuilder) buildAIFilter(e FilterExpr) string {
	val := e.Val
	if val == "sd" {
		val = "a1111"
	}
	if val == "any" {
		return "(i.source_type = 'a1111' OR i.source_type = 'comfyui' OR i.source_type = 'a1111,comfyui')"
	}
	if val == "none" {
		return "i.source_type = 'none'"
	}
	b.args = append(b.args, val, "%,"+val, val+",%", "%,"+val+",%")
	return "(i.source_type = ? OR i.source_type LIKE ? OR i.source_type LIKE ? OR i.source_type LIKE ?)"
}

// buildSourceFilter does exact-match against the operator-edited
// images.source label. Empty value matches images that carry no
// source - common for freshly-ingested files - so the user can triage
// them with `source:""`. The bare token form `source:` (no value) is
// also useful as the empty-string predicate. NOCASE so a user who
// wrote "Pixiv" once and types `source:pixiv` later still finds the row.
func (b *whereBuilder) buildSourceFilter(e FilterExpr) string {
	b.args = append(b.args, e.Val)
	return "i.source = ? COLLATE NOCASE"
}

func (b *whereBuilder) buildCatFilter(e FilterExpr) string {
	b.args = append(b.args, e.Val)
	return b.imageIDExists("image_tags it JOIN tags t ON it.tag_id = t.id JOIN tag_categories tc ON tc.id = t.category_id", "it", "tc.name = ?", false)
}

func (b *whereBuilder) buildWidthFilter(e FilterExpr) string {
	if s, ok := b.tryRangeComp("i.width %s ?", e.Val, parseIntValue); ok {
		return s
	}
	op, n, ok := parseIntComp(e.Val)
	return b.scalarComp("i.width %s ?", op, n, ok)
}

func (b *whereBuilder) buildHeightFilter(e FilterExpr) string {
	if s, ok := b.tryRangeComp("i.height %s ?", e.Val, parseIntValue); ok {
		return s
	}
	op, n, ok := parseIntComp(e.Val)
	return b.scalarComp("i.height %s ?", op, n, ok)
}

// buildMissingFilter sets a flag so any explicit `missing:` opts out
// of the default `AND is_missing = 0`. Without this flag,
// `-missing:false` collapses to `NOT (is_missing = 0) AND
// is_missing = 0` and returns nothing.
func (b *whereBuilder) buildMissingFilter(e FilterExpr) string {
	b.hasMissingFilter = true
	return boolColumnFilter("i.is_missing", e.Val)
}

// buildTypeFilter emits a comma-separated union of named file-type
// buckets:
//
//	image     -> jpeg / png / webp / gif / mp4 / webm
//	archive   -> cbz (cbz and zip archives of images; the ingest
//	             collapses both extensions onto the 'cbz' file_type)
//	animated  -> gif / mp4 / webm (subset of image)
//
// `-type:animated` is the inverse via the parser's NotExpr; no
// dedicated `animated:false` keyword exists.
func (b *whereBuilder) buildTypeFilter(e FilterExpr) string {
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
	return fileTypeInClause(seen, all)
}

// buildCollectionFilter matches the operator-edited per-row collection
// label (the comic / manga "series" surface, generalised for plain
// image groupings). Schema column kept as `series` for backwards
// compatibility with existing databases; only the user-facing keyword
// and payload field names carry the new vocabulary. NOCASE so a label
// saved as "My Comic Series" still matches the user typing
// `collection:"my comic series"`.
func (b *whereBuilder) buildCollectionFilter(e FilterExpr) string {
	b.args = append(b.args, e.Val)
	return "i.series = ? COLLATE NOCASE"
}

func (b *whereBuilder) buildPagesFilter(e FilterExpr) string {
	if s, ok := b.tryRangeComp("COALESCE(i.page_count, 0) %s ?", e.Val, parseIntValue); ok {
		return s
	}
	op, n, ok := parseIntComp(e.Val)
	// COALESCE so non-manga rows (NULL page_count) compare as 0;
	// matches the spec contract that `pages:>=1` excludes images.
	return b.scalarComp("COALESCE(i.page_count, 0) %s ?", op, n, ok)
}

// buildNameFilter does substring match against the filename segment
// after the last "/", so a folder named "vacation" doesn't match every
// file inside it. Empty value matches nothing (a bare `name:` is
// unlikely to be useful and would otherwise alias to "any").
// images.basename_lower is the indexed VIRTUAL column over
// lower(basename(canonical_path)); reading it directly avoids a
// per-row basename() call.
//
// SHA-256 duplicates land in the gallery as additional `image_paths`
// alias rows; the canonical_path-only match would miss any image
// found-but-renamed under a second filename even though the detail
// page lists the alias. The EXISTS clause keeps search-side parity
// with the single-image GET so typing `name:<alias-basename>` finds
// the image whose alias carries that name.
func (b *whereBuilder) buildNameFilter(e FilterExpr) string {
	if e.Val == "" {
		return "1=0"
	}
	// FTS5 trigram MATCH seek requires at least 3 characters of
	// overlap to produce a usable token. Inputs shorter than that
	// fall back to the LIKE shape - the planner has no faster path
	// for a one- or two-character substring regardless of index.
	if len([]rune(e.Val)) >= 3 {
		// Quote the user value as a single FTS5 phrase so spaces and
		// punctuation are searched literally instead of being parsed
		// as boolean operators. Double-quote escaping is "".
		ftsQuery := `"` + strings.ReplaceAll(strings.ToLower(e.Val), `"`, `""`) + `"`
		b.args = append(b.args, ftsQuery, ftsQuery)
		return `(i.id IN (SELECT rowid FROM image_basename_canonical_fts WHERE image_basename_canonical_fts MATCH ?) ` +
			`OR i.id IN (SELECT image_id FROM image_basename_alias_fts WHERE image_basename_alias_fts MATCH ?))`
	}
	pat := "%" + db.EscapeLike(strings.ToLower(e.Val)) + "%"
	// image_paths.basename_lower is the VIRTUAL twin of
	// images.basename_lower. The INDEXED BY hint pins the partial
	// `is_canonical = 0` index so the EXISTS subquery rides a seek
	// over the small alias subset rather than `idx_image_paths_image`
	// (which carries every row and pays a per-row is_canonical
	// filter); the basename_lower column drop replaces a per-row
	// lower(basename(ip.path)) function call.
	b.args = append(b.args, pat, pat)
	return `(i.basename_lower LIKE ? ESCAPE '\' ` +
		`OR EXISTS (SELECT 1 FROM image_paths ip INDEXED BY idx_image_paths_aliases WHERE ip.image_id = i.id AND ip.is_canonical = 0 AND ip.basename_lower LIKE ? ESCAPE '\'))`
}

func (b *whereBuilder) buildSizeFilter(e FilterExpr) string {
	if s, ok := b.tryRangeComp("i.file_size %s ?", e.Val, parseSizeValueAny); ok {
		return s
	}
	op, n, ok := parseSizeComp(e.Val)
	return b.scalarComp("i.file_size %s ?", op, n, ok)
}

// buildMimeFilter accepts either the bare file_type bucket ("png") or
// the `image/png` / `video/webm` form. Anything else falls through to
// the empty result. Multiple values comma-separated like `mime:png,jpeg`
// build an IN list. nil tautologyCap to fileTypeInClause so a
// `mime:png,jpeg,...` listing every bucket still emits the literal IN
// list - the 1=1 shortcut belongs to the type: aliases (image /
// archive / animated) where the semantic is "any media", not "the
// union of the listed buckets".
func (b *whereBuilder) buildMimeFilter(e FilterExpr) string {
	val := strings.TrimPrefix(strings.ToLower(e.Val), "image/")
	val = strings.TrimPrefix(val, "video/")
	if val == "" {
		return "1=0"
	}
	allowed := map[string]bool{
		"jpeg": true, "png": true, "webp": true, "gif": true,
		"mp4": true, "webm": true, "cbz": true,
	}
	seen := map[string]bool{}
	for _, v := range strings.Split(val, ",") {
		v = strings.TrimSpace(v)
		if allowed[v] {
			seen[v] = true
		}
	}
	return fileTypeInClause(seen, nil)
}

func (b *whereBuilder) buildRatioFilter(e FilterExpr) string {
	tmpl := "(CAST(i.width AS REAL) / NULLIF(i.height, 0)) %s ?"
	if s, ok := b.tryRangeComp(tmpl, e.Val, parseFloatValue); ok {
		return s
	}
	op, n, ok := parseFloatComp(e.Val)
	// Width and height are nullable on edge cases (cbz cover failed to
	// decode); guard against division-by-zero with NULLIF so the row
	// drops out instead of erroring.
	return b.scalarComp(tmpl, op, n, ok)
}

func (b *whereBuilder) buildTagcountFilter(e FilterExpr) string {
	if s, ok := b.tryRangeComp("i.tag_count %s ?", e.Val, parseIntValue); ok {
		return s
	}
	op, n, ok := parseIntComp(e.Val)
	// images.tag_count is a stored column maintained by triggers on
	// image_tags (db.Bootstrap). The indexed range seek over
	// idx_images_tag_count_visible is one primary-table read per
	// visible row.
	return b.scalarComp("i.tag_count %s ?", op, n, ok)
}

func (b *whereBuilder) buildDurationFilter(e FilterExpr) string {
	tmpl := "(i.duration_seconds IS NOT NULL AND i.duration_seconds %s ?)"
	if s, ok := b.tryRangeComp(tmpl, e.Val, parseFloatValue); ok {
		return s
	}
	op, n, ok := parseFloatComp(e.Val)
	// NULL duration_seconds (non-videos and pre-migration rows) drops
	// out of any comparison via the IS NOT NULL guard; the COALESCE
	// form pages: uses would force them into "0 seconds" matches,
	// which silently advertises every image as a 0-second clip.
	return b.scalarComp(tmpl, op, n, ok)
}

func (b *whereBuilder) buildHashFilter(e FilterExpr) string {
	if e.Val == "" {
		return "1=0"
	}
	b.args = append(b.args, strings.ToLower(e.Val))
	return "i.sha256 = ?"
}

func (b *whereBuilder) buildIDFilter(e FilterExpr) string {
	n, err := strconv.ParseInt(strings.TrimSpace(e.Val), 10, 64)
	if err != nil {
		return "1=0"
	}
	b.args = append(b.args, n)
	return "i.id = ?"
}

// buildPromptFilter is substring match across both SD and ComfyUI
// metadata tables; either is enough for the row to qualify. Mirrors
// the generated: filter's UNION-of-tables shape.
func (b *whereBuilder) buildPromptFilter(e FilterExpr) string {
	return b.dualMetadataLike("prompt", "prompt", e.Val)
}

func (b *whereBuilder) buildModelFilter(e FilterExpr) string {
	return b.dualMetadataLike("model", "model_checkpoint", e.Val)
}

func (b *whereBuilder) buildSamplerFilter(e FilterExpr) string {
	return b.dualMetadataLike("sampler", "sampler", e.Val)
}

// buildSeedFilter takes a 64-bit int seed in both metadata tables.
// Anything else matches nothing. The IN-subquery shape lets the
// planner answer the seek through the partial idx_sd_metadata_seed /
// idx_comfyui_metadata_seed indexes; an EXISTS form would walk images
// first and probe the metadata tables by image_id rowid, missing the
// seed indexes.
func (b *whereBuilder) buildSeedFilter(e FilterExpr) string {
	seed, err := strconv.ParseInt(strings.TrimSpace(e.Val), 10, 64)
	if err != nil {
		return "1=0"
	}
	b.args = append(b.args, seed, seed)
	return "(i.id IN (SELECT image_id FROM sd_metadata WHERE seed = ?) OR i.id IN (SELECT image_id FROM comfyui_metadata WHERE seed = ?))"
}

// buildViaFilter: origin is operator-supplied free text (app name,
// scraper label, ...). NOCASE so a row written by `via:ScraperBot`
// still surfaces when the operator types `via:scraperbot` in the
// search bar, matching the help promise that all searches are
// case-insensitive.
func (b *whereBuilder) buildViaFilter(e FilterExpr) string {
	if e.Val == "" {
		return "1=0"
	}
	b.args = append(b.args, e.Val)
	return "i.origin = ? COLLATE NOCASE"
}

func (b *whereBuilder) buildTaggedFilter(e FilterExpr) string {
	val, ok := parseBoolVal(e.Val)
	if !ok {
		return "1=0"
	}
	return b.imageTagsPredicate("", !val)
}

func (b *whereBuilder) buildAutotaggedFilter(e FilterExpr) string {
	val, ok := parseBoolVal(e.Val)
	if !ok {
		return "1=0"
	}
	return b.imageTagsPredicate("it.is_auto = 1", !val)
}

// buildFolderFilter does a recursive match: this folder or anywhere
// beneath it. `folder:` alone is the recursive root - every
// non-missing image lives at or below the gallery root. Use
// `folderonly:` with an empty value for "root directly". Escape LIKE
// metacharacters so a folder named `foo_bar` only matches itself (not
// `fooXbar`). NOCASE on both halves so the help promise of
// case-insensitive search holds for operator-edited folder paths the
// same way it holds for tag names.
func (b *whereBuilder) buildFolderFilter(e FilterExpr) string {
	if e.Val == "" {
		return "1=1"
	}
	b.args = append(b.args, e.Val, db.EscapeLike(e.Val)+"/%")
	return `(i.folder_path = ? COLLATE NOCASE OR i.folder_path LIKE ? ESCAPE '\' COLLATE NOCASE)`
}

func (b *whereBuilder) buildFolderonlyFilter(e FilterExpr) string {
	if e.Val == "" {
		return "i.folder_path = ''"
	}
	b.args = append(b.args, e.Val)
	return "i.folder_path = ? COLLATE NOCASE"
}

func (b *whereBuilder) buildGeneratedFilter(e FilterExpr) string {
	b.args = append(b.args, e.Val, e.Val)
	sm := b.imageIDExists("sd_metadata sm", "sm", "sm.generation_hash = ?", false)
	cm := b.imageIDExists("comfyui_metadata cm", "cm", "cm.generation_hash = ?", false)
	return "(" + sm + " OR " + cm + ")"
}

// buildRatingFilter encodes the highest-wins rule: an image matches
// `rating:X` only when it carries X AND no rating ranked above X. Self
// uses EXISTS, the strictly-higher levels are NOT EXISTS, all keyed on
// the cached rating tag IDs so the predicates hit
// idx_image_tags_image directly.
func (b *whereBuilder) buildRatingFilter(e FilterExpr) string {
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
}

// buildDefaultFilter handles unknown keys: either a category-qualified
// tag search ("character:cat") or a literal colon-bearing tag name
// ("nier:automata", ":3"). If the key matches a real category we
// split; otherwise the whole "key:val" is matched as a literal tag
// name. Tag names round-trip through the create path lowercased, so
// the value half gets the same treatment before the equality compare -
// otherwise `character:Asuka` misses a row whose tag was stored as
// `asuka`. A bare `<category>:` with no value collapses to "match any
// image carrying a tag in that category", mirroring `cat:<name>`;
// without this rewrite the gallery surface silently matched every
// image.
func (b *whereBuilder) buildDefaultFilter(e FilterExpr) string {
	if b.categoryExists(e.Key) {
		if e.Val == "" {
			b.args = append(b.args, e.Key)
			return b.imageIDExists("image_tags it JOIN tags t ON it.tag_id = t.id JOIN tag_categories tc ON tc.id = t.category_id", "it", "tc.name = ?", false)
		}
		// Caller prepended a non-correlated IN(...) (or INTERSECT)
		// covering this category-qualified leaf. Returning "" lets
		// AndExpr collapse this branch the same way buildTagExpr does
		// for matched tag leaves.
		if b.driverLeaves[e] {
			return ""
		}
		// Pre-resolve `<category>:<name>` to canonical tag IDs so the
		// per-row predicate is one image_tags seek + small IN check
		// instead of a 2-table join evaluated under every outer cursor
		// iter. ExecuteAdjacent's bucket walk pays this 2 000 times per
		// random-cat back_q render and the resolver result is constant
		// across the walk.
		if ids, ok := b.resolveCategoryTagByName(e.Key, strings.ToLower(e.Val)); ok {
			if len(ids) == 0 {
				return "1=0"
			}
			return inlineImageTagsTagIDExists(ids)
		}
		b.args = append(b.args, strings.ToLower(e.Val), e.Key)
		return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(t.canonical_tag_id, t.id) FROM tags t JOIN tag_categories tc ON tc.id = t.category_id WHERE t.name = ? AND tc.name = ?)`, false)
	}
	if e.Val == "" {
		return "1=1"
	}
	b.args = append(b.args, strings.ToLower(e.Key+":"+e.Val))
	return b.imageTagsPredicate(`it.tag_id IN (SELECT COALESCE(canonical_tag_id, id) FROM tags WHERE name = ?)`, false)
}

// dateFilterRe matches the documented date filter shapes: YYYY,
// YYYY-MM, YYYY-MM-DD, plus the optional time component used by the
// inbox-cluster links: YYYY-MM-DDTHH, YYYY-MM-DDTHH:MM, and
// YYYY-MM-DDTHH:MM:SS. The HELP.md examples show YYYY-MM ranges
// (`date:2024-01..2024-06`) which lexicographically string-compare
// correctly against the ISO-8601 ingested_at column. `buildDateFilter`
// accepts each component (after stripping the optional comparison or
// range syntax) and rejects malformed input with `1=0` rather than
// passing it into a SQL comparison verbatim, which produced silent
// zero-result answers indistinguishable from a real "no images on
// that date" result.
var dateFilterRe = regexp.MustCompile(`^\d{4}(-\d{2}(-\d{2}(T\d{2}(:\d{2}(:\d{2})?)?)?)?)?$`)

// endOfPrecisionISO returns the lexicographically-largest ingested_at
// value that still belongs to the truncated precision the caller named.
// `2026-05-22` -> `2026-05-22T23:59:59Z` (end of day, matches the
// historical contract), `2026-05-22T06` -> `2026-05-22T06:59:59Z`
// (end of hour), `2026-05-22T19:23` -> `2026-05-22T19:23:59Z` (end of
// minute), `2026-05-22T19:23:42` -> `2026-05-22T19:23:42Z` (the exact
// second). Bare years and months keep the historical end-of-day
// append since `2026T23:59:59Z` still sorts lex-larger than any
// `2026-MM-DD...` timestamp.
func endOfPrecisionISO(val string) string {
	tIdx := strings.Index(val, "T")
	if tIdx < 0 {
		return val + "T23:59:59Z"
	}
	switch strings.Count(val[tIdx+1:], ":") {
	case 0:
		return val + ":59:59Z"
	case 1:
		return val + ":59Z"
	case 2:
		return val + "Z"
	}
	return val + "T23:59:59Z"
}

func (b *whereBuilder) buildDateFilter(val string) string {
	// Two-character operators must be checked before their one-character
	// prefixes so `>=2026-05-14` doesn't match the `>` arm and strip a
	// single char, leaving an unparseable `=2026-05-14`.
	//
	// `ingested_at` is stored as `YYYY-MM-DDTHH:MM:SSZ`; a bare day
	// payload like `2026-05-14` is shorter than every real timestamp,
	// so a plain lexicographic compare to it would treat the day as
	// "the moment just before 00:00:00Z" and exclude every row from
	// that day under `<=` (and include every row under `>`). The
	// inclusive operators (`<=`, `>=`) extend the payload to the
	// end-of-day (or end-of-minute / second when the caller supplied
	// time precision) so `date:<=2026-05-14` actually catches the last
	// image ingested at 23:59. The exclusive `>` does the same
	// (matches: "ingested strictly after day X"); `<` keeps the bare
	// day so it means "before midnight of day X".
	for _, op := range []string{">=", "<=", ">", "<"} {
		if !strings.HasPrefix(val, op) {
			continue
		}
		date := val[len(op):]
		if !dateFilterRe.MatchString(date) {
			return "1=0"
		}
		bound := date
		if op == "<=" || op == ">" {
			bound = endOfPrecisionISO(date)
		}
		b.args = append(b.args, bound)
		return "i.ingested_at " + op + " ?"
	}
	if idx := strings.Index(val, ".."); idx >= 0 {
		from := val[:idx]
		to := val[idx+2:]
		// Bare `..` is meaningless on its own; both halves empty is the
		// silent-zero shape callers hit when they delete the wrong end
		// of an existing range.
		if from == "" && to == "" {
			return "1=0"
		}
		if from != "" && !dateFilterRe.MatchString(from) {
			return "1=0"
		}
		if to != "" && !dateFilterRe.MatchString(to) {
			return "1=0"
		}
		// Open-ended forms collapse to a single inclusive bound: `..X`
		// is `<=X` (every day up to and including X), `X..` is `>=X`
		// (every day from X forward). Mirrors the level-2 cheat-sheet
		// hint that includes `..` next to `>=` / `<=`.
		switch {
		case from == "":
			b.args = append(b.args, endOfPrecisionISO(to))
			return "i.ingested_at <= ?"
		case to == "":
			b.args = append(b.args, from)
			return "i.ingested_at >= ?"
		}
		b.args = append(b.args, from, endOfPrecisionISO(to))
		return "i.ingested_at BETWEEN ? AND ?"
	}
	// `=YYYY-MM-DD` is the explicit form of the bare `date:YYYY-MM-DD`
	// shape - the user types the operator the sibling filters (size:=,
	// pages:=, tagcount:=) all accept. Strip the leading `=` and fall
	// through to the bare-form BETWEEN.
	val = strings.TrimPrefix(val, "=")
	if !dateFilterRe.MatchString(val) {
		return "1=0"
	}
	b.args = append(b.args, val, endOfPrecisionISO(val))
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

// parseFloatComp is the float-arg twin of parseIntComp. Used by ratio:
// and duration:. Rejects empty / non-numeric input the same way.
func parseFloatComp(val string) (string, float64, bool) {
	op, raw := parseCompOp(val)
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return op, 0, false
	}
	return op, n, true
}

// parseSizeComp parses a size comparison value like ">=10MB", "<2GiB",
// "= 500K", "1024" (bare bytes). Suffixes are case-insensitive and
// accept either the SI form (KB, MB, GB, TB) or the binary form (KiB,
// MiB, ...). All resolve to powers of 1024 for parity with the rest of
// the UI (humanBytes uses 1024-based MiB). Bare numbers are bytes.
// Returns ok=false on parse failures so callers emit `1=0`.
func parseSizeComp(val string) (string, int64, bool) {
	op, raw := parseCompOp(val)
	n, ok := parseSizeValue(raw)
	return op, n, ok
}

// parseSizeValue parses the bare numeric-plus-unit half of a size:
// filter value (no operator prefix). Shared between parseSizeComp and
// the X..Y range path.
func parseSizeValue(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	i := 0
	for i < len(raw) {
		c := raw[i]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' {
			i++
			continue
		}
		break
	}
	numStr := raw[:i]
	unit := strings.TrimSpace(strings.ToLower(raw[i:]))
	n, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, false
	}
	var mult int64
	switch unit {
	case "", "b":
		mult = 1
	case "k", "kb", "kib":
		mult = 1 << 10
	case "m", "mb", "mib":
		mult = 1 << 20
	case "g", "gb", "gib":
		mult = 1 << 30
	case "t", "tb", "tib":
		mult = 1 << 40
	default:
		return 0, false
	}
	return int64(n * float64(mult)), true
}

// tryRangeComp detects the `..` range form in val and emits the matching
// BETWEEN / >= / <= clause through the same `template %s ?` shape
// scalarComp uses. Returns ok=false when val has no `..`, so the caller
// falls through to its comparison helper. Parsing failures still return
// ok=true with a `1=0` clause so the silent-zero shape stays explicit.
// `..X` collapses to `<= X`, `X..` to `>= X`, `X..Y` to `BETWEEN X AND Y`;
// bare `..` is the meaningless 1=0 form date: also emits.
func (b *whereBuilder) tryRangeComp(template, val string, parse func(string) (any, bool)) (string, bool) {
	idx := strings.Index(val, "..")
	if idx < 0 {
		return "", false
	}
	fromS := strings.TrimSpace(val[:idx])
	toS := strings.TrimSpace(val[idx+2:])
	if fromS == "" && toS == "" {
		return "1=0", true
	}
	switch {
	case fromS == "":
		toV, ok := parse(toS)
		if !ok {
			return "1=0", true
		}
		b.args = append(b.args, toV)
		return fmt.Sprintf(template, "<="), true
	case toS == "":
		fromV, ok := parse(fromS)
		if !ok {
			return "1=0", true
		}
		b.args = append(b.args, fromV)
		return fmt.Sprintf(template, ">="), true
	}
	fromV, ok := parse(fromS)
	if !ok {
		return "1=0", true
	}
	toV, ok := parse(toS)
	if !ok {
		return "1=0", true
	}
	b.args = append(b.args, fromV, toV)
	return fmt.Sprintf(template, "BETWEEN ? AND"), true
}

// parseIntValue and parseFloatValue are the bare-numeric counterparts of
// parseIntComp / parseFloatComp - tryRangeComp wraps them as the per-half
// parser for X..Y, ..X, and X.. on integer and float filters.
func parseIntValue(s string) (any, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return nil, false
	}
	return n, true
}

func parseFloatValue(s string) (any, bool) {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return nil, false
	}
	return n, true
}

func parseSizeValueAny(s string) (any, bool) {
	n, ok := parseSizeValue(s)
	if !ok {
		return nil, false
	}
	return n, true
}

// buildPhashFilter handles the three phash: forms:
//
//   - bare `phash:` matches every image that has a phash (was once
//     ingested with a decodable thumbnail). The negation `-phash:`
//     is the IS NULL inverse.
//   - `phash:<16hex>` is an exact-equality lookup on idx_images_phash.
//   - `phash:<16hex>~<d>` matches every image within Hamming distance
//     d. When a BK-tree is wired for the gallery it answers the lookup
//     directly; otherwise the SQL popcount fallback scans phashes.
//
// Malformed input collapses to `1=0` so a typo never bleeds into the
// rest of the query as an empty-match.
func (b *whereBuilder) buildPhashFilter(e FilterExpr) string {
	val := strings.TrimSpace(e.Val)
	if val == "" {
		return "i.phash IS NOT NULL"
	}
	hexPart := val
	distance := -1
	if idx := strings.IndexByte(val, '~'); idx >= 0 {
		hexPart = val[:idx]
		d, err := strconv.Atoi(strings.TrimSpace(val[idx+1:]))
		if err != nil || d < 0 || d > 64 {
			return "1=0"
		}
		distance = d
	}
	hexPart = strings.TrimSpace(hexPart)
	if len(hexPart) != 16 {
		return "1=0"
	}
	u, err := strconv.ParseUint(hexPart, 16, 64)
	if err != nil {
		return "1=0"
	}
	phash := int64(u)
	if distance < 0 {
		b.args = append(b.args, phash)
		return "i.phash = ?"
	}
	if b.db != nil {
		if tree := relations.DefaultRegistry.Lookup(b.db); tree != nil {
			if err := tree.EnsureBuilt(b.db); err == nil {
				ids := tree.SearchWithinDistance(phash, distance)
				if len(ids) == 0 {
					return "1=0"
				}
				placeholders, idArgs := db.InPlaceholders(ids)
				b.args = append(b.args, idArgs...)
				return "i.id IN (" + placeholders + ")"
			}
		}
	}
	// Fallback: SQL-side hammingdist scalar. Slower than the BK-tree on
	// a 1M-row library but correct.
	b.args = append(b.args, phash, distance)
	return "(i.phash IS NOT NULL AND hammingdist(i.phash, ?) <= ?)"
}

// buildRelationFilter maps each closed `relation:` vocabulary value to
// an EXISTS / NOT-EXISTS over the matching relations table; every
// clause rides a covering index, so cost is dominated by the outer
// sort. `any` is the union, `none` is the NOT-any inverse.
//
// resolveRelationPresence is consulted first so a gallery with no
// declared relations skips the per-row EXISTS / NOT IN entirely: an
// empty source means the predicate's answer is constant across every
// visible row, and we emit `1=0` (positive sense) or `1=1` (NOT) up
// front. The cold relation:none scan on a 1M-row visible set used to
// land in seconds; here it folds into the outer sort with no
// per-row work.
func (b *whereBuilder) buildRelationFilter(e FilterExpr) string {
	val := strings.ToLower(strings.TrimSpace(e.Val))
	b.resolveRelationPresence()
	switch val {
	case "duplicate":
		if !b.relPresence.dup {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM dup_group_members m WHERE m.image_id = i.id)"
	case "original":
		if !b.relPresence.dup {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM dup_groups g WHERE g.original_image_id = i.id)"
	case "alternate":
		if !b.relPresence.alt {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM alt_group_members m WHERE m.image_id = i.id)"
	case "version":
		if !b.relPresence.version {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM version_edges v WHERE v.child_image_id = i.id OR v.parent_image_id = i.id)"
	case "derivative":
		if !b.relPresence.derivative {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM derivative_edges d WHERE d.derivative_image_id = i.id)"
	case "source":
		if !b.relPresence.derivative {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM derivative_edges d WHERE d.source_image_id = i.id)"
	case "collection":
		if !b.relPresence.series {
			return "1=0"
		}
		return "i.series IS NOT NULL AND i.series != ''"
	case "any":
		if !b.anyRelationPresent() {
			return "1=0"
		}
		return b.relationAnyClauseForPresence()
	case "none":
		if !b.anyRelationPresent() {
			return "1=1"
		}
		return b.relationNoneClauseForPresence()
	}
	return "1=0"
}

// resolveRelationPresence runs a single COUNT(EXISTS) probe per
// relations source (5 cheap lookups, each <0.1 ms on a warm DB) and
// caches the booleans on the builder. Skipped on a nil db (test path)
// so the regular EXISTS / NOT IN shapes still surface there.
func (b *whereBuilder) resolveRelationPresence() {
	if b.relPresenceResolved {
		return
	}
	b.relPresenceResolved = true
	if b.db == nil {
		return
	}
	probe := func(query string) bool {
		var has int
		if err := b.db.Read.QueryRow(query).Scan(&has); err != nil {
			// Treat probe failure as "may have rows" so the slow but
			// correct path runs - errors here only surface in degraded
			// modes, not on the empty-table fast path this resolver
			// targets.
			return true
		}
		return has > 0
	}
	b.relPresence = relationPresence{
		dup:        probe(`SELECT EXISTS (SELECT 1 FROM dup_group_members)`),
		alt:        probe(`SELECT EXISTS (SELECT 1 FROM alt_group_members)`),
		version:    probe(`SELECT EXISTS (SELECT 1 FROM version_edges)`),
		derivative: probe(`SELECT EXISTS (SELECT 1 FROM derivative_edges)`),
		series:     probe(`SELECT EXISTS (SELECT 1 FROM images WHERE series IS NOT NULL AND series != '' LIMIT 1)`),
	}
}

func (b *whereBuilder) anyRelationPresent() bool {
	p := &b.relPresence
	return p.dup || p.alt || p.version || p.derivative || p.series
}

// relationAnyClauseForPresence returns the union with each subquery
// dropped when its source is known to be empty. Strict-empty branches
// can't contribute matches, so trimming them yields a cheaper plan
// (often a single EXISTS or a series-IS-NOT-NULL leg).
func (b *whereBuilder) relationAnyClauseForPresence() string {
	p := &b.relPresence
	parts := make([]string, 0, 5)
	if p.dup {
		parts = append(parts,
			"EXISTS (SELECT 1 FROM dup_group_members m WHERE m.image_id = i.id)",
		)
	}
	if p.alt {
		parts = append(parts,
			"EXISTS (SELECT 1 FROM alt_group_members m WHERE m.image_id = i.id)",
		)
	}
	if p.version {
		parts = append(parts,
			"EXISTS (SELECT 1 FROM version_edges v WHERE v.child_image_id = i.id OR v.parent_image_id = i.id)",
		)
	}
	if p.derivative {
		parts = append(parts,
			"EXISTS (SELECT 1 FROM derivative_edges d WHERE d.derivative_image_id = i.id OR d.source_image_id = i.id)",
		)
	}
	if p.series {
		parts = append(parts, "(i.series IS NOT NULL AND i.series != '')")
	}
	if len(parts) == 0 {
		return "1=0"
	}
	return strings.Join(parts, " OR ")
}

// relationNoneClauseForPresence rewrites the NOT IN (UNION ...) so
// only the populated relation sources contribute. An empty union
// would walk every visible row checking against nothing; dropping
// the empty branches keeps the materialised set tight to whatever
// the operator actually has. Series carriers stay on the AND-leg
// per the original shape.
func (b *whereBuilder) relationNoneClauseForPresence() string {
	p := &b.relPresence
	var unions []string
	if p.dup {
		unions = append(unions, "SELECT image_id FROM dup_group_members")
	}
	if p.alt {
		unions = append(unions, "SELECT image_id FROM alt_group_members")
	}
	if p.version {
		unions = append(unions,
			"SELECT child_image_id FROM version_edges",
			"SELECT parent_image_id FROM version_edges",
		)
	}
	if p.derivative {
		unions = append(unions,
			"SELECT derivative_image_id FROM derivative_edges",
			"SELECT source_image_id FROM derivative_edges",
		)
	}
	if !p.series && len(unions) == 0 {
		return "1=1"
	}
	// Fold the series-empty check into the NOT IN so SQLite uses the
	// partial idx_images_series to materialise the carriers cheaply
	// (the index covers WHERE series != ''). The bare LIKE on series
	// would otherwise force a per-row column read on every visible
	// image even when zero carriers exist.
	if p.series {
		unions = append(unions, "SELECT id FROM images WHERE series IS NOT NULL AND series != ''")
	}
	if len(unions) == 0 {
		return "1=1"
	}
	return "i.id NOT IN (\n\t\t" + strings.Join(unions, "\n\t\tUNION ") + "\n\t)"
}
