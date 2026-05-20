package web

import (
	"crypto/rand"
	"encoding/binary"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/search"
	"github.com/leqwin/monbooru/internal/tagger"
)

// galleryHiddenIndicatorBudget caps the time the first gallery
// search.Execute may spend before the handler skips the second
// no-ceiling COUNT that drives the "N hidden" indicator. The render
// degrades to the existing matches-only line instead of paying a
// second slow pass when the first already ate the search budget.
const galleryHiddenIndicatorBudget = 300 * time.Millisecond

type galleryData struct {
	baseData
	Query             string
	Sort              string
	Order             string
	RandomSeed        int64
	Page              int
	TotalPages        int
	Result            *models.SearchResult
	SidebarTags       []models.Tag
	FolderTree        []gallery.FolderNode
	SourceCounts      gallery.SourceCounts
	SeriesCounts      []gallery.SeriesCount
	SourceLabelCounts []gallery.SourceLabelCount
	FavoritedCount    int
	NonFavoritedCount int
	NonInboxCount     int
	SavedSearches     []models.SavedSearch
	SidebarURL        string                // populated on full-page renders so the placeholder can lazy-load the sidebar
	EnabledTaggers    []tagger.TaggerStatus // gates the gallery's Auto-tag controls; mirrors detailData.EnabledTaggers
}

func (s *Server) galleryHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	queryStr := q.Get("q")
	sortStr := q.Get("sort")
	if sortStr == "" {
		sortStr = "newest"
	}
	orderStr := q.Get("order")
	if orderStr == "" {
		orderStr = "desc"
	}
	pageStr := q.Get("page")
	page := 1
	pageNonPositive := false
	if pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil {
			if p > 0 {
				page = p
			} else {
				// `?page=0` or `?page=-1` would render page 1 but leave
				// the URL pointing at the bogus value; flag for the
				// clamp+redirect path below so bookmark coherence matches
				// the past-end branch.
				pageNonPositive = true
			}
		}
	}

	// For random sort, use a stable seed so the order doesn't change on every reload.
	// Generate a seed if not present in the URL, and redirect to add it (or set HX-Push-Url).
	var randomSeed int64
	if sortStr == "random" {
		if seedStr := q.Get("seed"); seedStr != "" {
			if s, err := strconv.ParseInt(seedStr, 10, 64); err == nil && s != 0 {
				randomSeed = s
			}
		}
		if randomSeed == 0 {
			seedBytes := make([]byte, 8)
			if _, err := rand.Read(seedBytes); err == nil {
				// Clamp to 31 bits (and force odd) so SQLite's int64
				// arithmetic on `(i.id * seed) & 2147483647` stays
				// inside int64 for any reasonable image id and the
				// low bits of the product remain uniform. A 63-bit
				// seed overflows on multiplication; the result coerces
				// to REAL and the low-bit mask becomes near-monotonic
				// in id, surfacing as identity-ordered "random" pages.
				randomSeed = int64(binary.BigEndian.Uint32(seedBytes) | 1)
			} else {
				randomSeed = time.Now().UnixNano() & 0x7FFFFFFF
			}
			if randomSeed < 0 {
				randomSeed = -randomSeed
			}
			newQ := r.URL.Query()
			newQ.Set("seed", strconv.FormatInt(randomSeed, 10))
			if isHTMXRequest(r) {
				// Push URL with seed so the next poll keeps the same order.
				w.Header().Set("HX-Push-Url", "/?"+newQ.Encode())
			} else {
				http.Redirect(w, r, "/?"+newQ.Encode(), http.StatusSeeOther)
				return
			}
		}
	}

	expr, parseErr := search.Parse(queryStr)
	if parseErr != nil {
		logx.Warnf("gallery search parse: %v", parseErr)
	}
	ceiling := resolveCeiling(r, s.Active())
	expr = ceiling.Apply(expr)
	sq := search.Query{
		Expr:       expr,
		Sort:       sortStr,
		Order:      orderStr,
		RandomSeed: randomSeed,
		Page:       page,
		Limit:      s.cfg.UI.PageSize,
		CacheKey:   search.BuildAdjacencyCacheKey(s.activeName, queryStr, sortStr, orderStr, randomSeed, ceiling.Level()),
	}
	// Unfiltered browse hits the full-visible count on every page; serve it
	// from the per-gallery cache to skip the O(N) index scan. The cache
	// counts every visible image, so it overcounts when a ceiling is on -
	// fall back to fastCountCeiling in that case.
	if expr == nil {
		if cx := s.Active(); cx != nil {
			if n, err := cx.VisibleCount(); err == nil {
				sq.PresetTotal = &n
			}
		}
	}

	htmxGridTarget := isHTMXRequest(r) && r.Header.Get("HX-Target") == "gallery-grid"

	firstStart := time.Now()
	result, err := search.Execute(s.db(), sq)
	if err != nil {
		logx.Errorf("gallery search: %v", err)
		http.Error(w, "search error", http.StatusInternalServerError)
		return
	}
	firstElapsed := time.Since(firstStart)

	totalPages := 1
	if s.cfg.UI.PageSize > 0 {
		totalPages = (result.Total + s.cfg.UI.PageSize - 1) / s.cfg.UI.PageSize
	}

	// If a concurrent ingestion or delete shrank the result set out from under
	// the user's current page (e.g. the auto-refresh re-fetches page 3 after
	// deletions dropped the total to 1 page), re-run at the last valid page
	// so the grid isn't empty while "N images" still shows a non-zero count.
	// Sync the URL so a bookmark of the clamped view doesn't keep replaying
	// the bogus page number. `?page=0` / `?page=-1` ride the same path so
	// the past-end and below-1 branches share the redirect.
	if (result.Total > 0 && page > totalPages) || pageNonPositive {
		if page > totalPages {
			page = totalPages
			sq.Page = page
			result, err = search.Execute(s.db(), sq)
			if err != nil {
				logx.Errorf("gallery search (clamped): %v", err)
				http.Error(w, "search error", http.StatusInternalServerError)
				return
			}
		}
		clampedQ := r.URL.Query()
		clampedQ.Set("page", strconv.Itoa(page))
		clampedURL := "/?" + clampedQ.Encode()
		if isHTMXRequest(r) {
			w.Header().Set("HX-Push-Url", clampedURL)
		} else {
			http.Redirect(w, r, clampedURL, http.StatusSeeOther)
			return
		}
	}

	// Compute the "N hidden" indicator: unfiltered total minus the
	// ceiling-aware total. Filtered queries probe the bare-expr
	// adjacency cache, then fall back to a COUNT-only Execute. The
	// budget guard skips the fallback when the first Execute already
	// burned the search budget so the render degrades gracefully.
	hiddenByCeiling := 0
	if ceiling.IsActive() {
		rawTotal := -1
		bareExpr, _ := search.Parse(queryStr)
		switch {
		case bareExpr == nil:
			if cx := s.Active(); cx != nil {
				if n, err := cx.VisibleCount(); err == nil {
					rawTotal = n
				}
			}
		case firstElapsed < galleryHiddenIndicatorBudget:
			bareKey := search.BuildAdjacencyCacheKey(s.activeName, queryStr, sortStr, orderStr, randomSeed, "")
			if cachedIDs, ok := search.AdjacencyCacheGet(bareKey); ok {
				rawTotal = len(cachedIDs)
			} else {
				rawResult, err := search.Execute(s.db(), search.Query{
					Expr: bareExpr, Sort: sortStr, Order: orderStr,
					RandomSeed: randomSeed, Page: 1, Limit: 1,
				})
				if err == nil {
					rawTotal = rawResult.Total
				}
			}
		}
		if rawTotal > result.Total {
			hiddenByCeiling = rawTotal - result.Total
		}
	}

	// Full-page renders ship the sidebar as a placeholder that lazy-loads via
	// GET /internal/sidebar, so first paint isn't blocked on the folder-tree
	// aggregation. Search/pagination HTMX responses still need the sidebar
	// content in the same payload because gallery_htmx.html OOB-swaps it into
	// the live page.
	ids := make([]int64, 0, len(result.Results))
	for _, img := range result.Results {
		ids = append(ids, img.ID)
	}

	var sb sidebarBundle
	if htmxGridTarget {
		sb = s.sidebarLoad(ids, ceiling)
	}

	data := galleryData{
		baseData:          s.base(r, "gallery", "Images - Monbooru"),
		Query:             queryStr,
		Sort:              sortStr,
		Order:             orderStr,
		RandomSeed:        randomSeed,
		Page:              page,
		TotalPages:        totalPages,
		Result:            result,
		SidebarTags:       sb.Tags,
		FolderTree:        sb.Folders,
		SourceCounts:      sb.Sources,
		SeriesCounts:      sb.Series,
		SourceLabelCounts: sb.SourceLabels,
		FavoritedCount:    sb.Favorited,
		NonFavoritedCount: sb.NonFavorited,
		NonInboxCount:     sb.NonInbox,
		SavedSearches:     sb.Saved,
		EnabledTaggers:    tagger.EnabledTaggersForGallery(s.cfg, s.activeName),
	}
	data.HiddenByCeiling = hiddenByCeiling
	// The footer InboxCount stays ceiling-blind (it's a true gallery
	// shape number), but the toolbar tooltip "Inbox (N)" promises the
	// post-click match count - the operator clicks and lands on the
	// ceiling-filtered list. Patch data.InboxCount with the
	// ceiling-aware cached count so the parenthesised number matches.
	if cx := s.Active(); cx != nil {
		if n, err := cx.InboxCountUnder(ceiling); err == nil {
			data.InboxCount = n
		}
	}

	if htmxGridTarget {
		s.renderTemplate(w, "partials/gallery_htmx.html", data)
		return
	}
	data.SidebarURL = buildSidebarURL(queryStr, sortStr, orderStr, pageStr, q.Get("seed"), ids)
	s.renderTemplate(w, "gallery.html", data)
}

// sidebarBundle is the parallel-fetched payload that populates the
// gallery sidebar - tags from the current page, folder tree, AI source
// breakdown, top series + source labels, inbox / favourite tallies,
// saved searches. Bundling them in one struct keeps the goroutine fan
// at sidebarLoad readable as the count grows.
type sidebarBundle struct {
	Tags         []models.Tag
	Folders      []gallery.FolderNode
	Sources      gallery.SourceCounts
	Series       []gallery.SeriesCount
	SourceLabels []gallery.SourceLabelCount
	Favorited    int
	NonFavorited int
	NonInbox     int
	Saved        []models.SavedSearch
}

// sidebarLoad runs the reads that populate the gallery sidebar.
// Two background goroutines cover the work that always touches the
// DB - the per-page tag aggregation against image_tags and the
// saved_searches scan. Everything else reads the per-cx atomic
// caches that warmCaches primes at gallery open, so it runs inline
// instead of fanning out a goroutine per sub-query (each grabbing a
// slot against the read pool under c>1, which doubles the cheap-
// shape sidebar latency). On cold cache the inline reads pay the
// query cost sequentially - rare enough (sidebar warmup runs at
// gallery open and after every cache invalidation) that the warm-
// path simplification is the right tradeoff.
//
// ceiling drives the per-image aggregates so the sidebar reflects what
// the operator sees in the gallery. A nil or inactive ceiling reads
// the existing blind caches, leaving the no-ceiling steady state
// untouched.
func (s *Server) sidebarLoad(pageImageIDs []int64, ceiling *Ceiling) sidebarBundle {
	var sb sidebarBundle
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sb.Tags, _ = search.SidebarTagsWithGlobalCount(s.db(), pageImageIDs)
	}()
	go func() {
		defer wg.Done()
		ssRows, ssErr := s.db().Read.Query(`SELECT id, name, query, sort, sort_order, seed FROM saved_searches ORDER BY name`)
		if ssErr != nil {
			return
		}
		defer ssRows.Close()
		for ssRows.Next() {
			var ss models.SavedSearch
			if err := ssRows.Scan(&ss.ID, &ss.Name, &ss.Query, &ss.Sort, &ss.Order, &ss.Seed); err != nil {
				logx.Warnf("sidebar saved searches scan: %v", err)
				continue
			}
			sb.Saved = append(sb.Saved, ss)
		}
	}()
	if cx := s.Active(); cx != nil {
		sb.Folders, _ = cx.FolderTreeUnder(ceiling)
		sb.Sources, _ = cx.SourceCountsUnder(ceiling)
		sb.Series, _ = cx.SeriesCountsUnder(ceiling)
		sb.SourceLabels, _ = cx.SourceLabelCountsUnder(ceiling)
		visible, _ := cx.VisibleCountUnder(ceiling)
		inbox, _ := cx.InboxCountUnder(ceiling)
		fav, _ := cx.FavoritedCountUnder(ceiling)
		sb.Favorited = fav
		sb.NonFavorited = visible - fav
		if sb.NonFavorited < 0 {
			sb.NonFavorited = 0
		}
		sb.NonInbox = visible - inbox
		if sb.NonInbox < 0 {
			sb.NonInbox = 0
		}
	}
	wg.Wait()
	return sb
}

// gallerySidebar renders the gallery sidebar partial on demand. Initial
// full-page gallery renders ship an empty #sidebar-inner placeholder that
// hx-gets this endpoint on load - same pattern as the detail page's
// related-images lazy fetch.
func (s *Server) gallerySidebar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	queryStr := q.Get("q")
	ceiling := resolveCeiling(r, s.Active())

	// galleryHandler piggy-backs the page's image IDs on the lazy-load URL
	// so we don't re-run search.Execute just to enumerate them. A direct
	// hit (no ids param at all) falls back to the search call so the
	// endpoint still works on its own.
	var ids []int64
	if q.Has("ids") {
		if raw := q.Get("ids"); raw != "" {
			ids = make([]int64, 0, strings.Count(raw, ",")+1)
			for _, s := range strings.Split(raw, ",") {
				if id, err := strconv.ParseInt(s, 10, 64); err == nil {
					ids = append(ids, id)
				}
			}
		}
	} else {
		sortStr := q.Get("sort")
		if sortStr == "" {
			sortStr = "newest"
		}
		orderStr := q.Get("order")
		if orderStr == "" {
			orderStr = "desc"
		}
		page := 1
		if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
			page = p
		}
		var randomSeed int64
		if sortStr == "random" {
			if seed, err := strconv.ParseInt(q.Get("seed"), 10, 64); err == nil {
				randomSeed = seed
			}
		}
		expr, _ := search.Parse(queryStr)
		expr = ceiling.Apply(expr)
		sq := search.Query{
			Expr:       expr,
			Sort:       sortStr,
			Order:      orderStr,
			RandomSeed: randomSeed,
			Page:       page,
			Limit:      s.cfg.UI.PageSize,
			SkipCount:  true,
		}
		result, err := search.Execute(s.db(), sq)
		if err != nil {
			logx.Errorf("sidebar search: %v", err)
			http.Error(w, "search error", http.StatusInternalServerError)
			return
		}
		ids = make([]int64, 0, len(result.Results))
		for _, img := range result.Results {
			ids = append(ids, img.ID)
		}
	}

	sb := s.sidebarLoad(ids, ceiling)
	inboxCount := 0
	if cx := s.Active(); cx != nil {
		inboxCount, _ = cx.InboxCountUnder(ceiling)
	}

	s.renderTemplate(w, "partials/sidebar_content.html", map[string]any{
		"Query":             queryStr,
		"CSRFToken":         s.csrfToken(sessionFromContext(r.Context())),
		"SidebarTags":       sb.Tags,
		"FolderTree":        sb.Folders,
		"SourceCounts":      sb.Sources,
		"SeriesCounts":      sb.Series,
		"SourceLabelCounts": sb.SourceLabels,
		"FavoritedCount":    sb.Favorited,
		"NonFavoritedCount": sb.NonFavorited,
		"InboxCount":        inboxCount,
		"NonInboxCount":     sb.NonInbox,
		"SavedSearches":     sb.Saved,
	})
}

// sidebarBrowse renders the folder/source/saved-searches sections only -
// no per-page tag groups. Lazy-loaded from the detail page so its sidebar
// gets the same browse shortcuts the gallery sidebar does without paying
// the folder-tree aggregation cost on first paint.
func (s *Server) sidebarBrowse(w http.ResponseWriter, r *http.Request) {
	queryStr := r.URL.Query().Get("q")
	ceiling := resolveCeiling(r, s.Active())

	var (
		folders        []gallery.FolderNode
		sources        gallery.SourceCounts
		series         []gallery.SeriesCount
		sourceLabels   []gallery.SourceLabelCount
		visible, inbox int
		fav            int
		saved          []models.SavedSearch
	)
	var wg sync.WaitGroup
	wg.Add(6)
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			folders, _ = cx.FolderTreeUnder(ceiling)
		}
	}()
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			sources, _ = cx.SourceCountsUnder(ceiling)
		}
	}()
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			series, _ = cx.SeriesCountsUnder(ceiling)
		}
	}()
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			sourceLabels, _ = cx.SourceLabelCountsUnder(ceiling)
		}
	}()
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			visible, _ = cx.VisibleCountUnder(ceiling)
			inbox, _ = cx.InboxCountUnder(ceiling)
			fav, _ = cx.FavoritedCountUnder(ceiling)
		}
	}()
	go func() {
		defer wg.Done()
		rows, err := s.db().Read.Query(`SELECT id, name, query, sort, sort_order, seed FROM saved_searches ORDER BY name`)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var ss models.SavedSearch
			if err := rows.Scan(&ss.ID, &ss.Name, &ss.Query, &ss.Sort, &ss.Order, &ss.Seed); err != nil {
				logx.Warnf("sidebar-browse saved searches scan: %v", err)
				continue
			}
			saved = append(saved, ss)
		}
	}()
	wg.Wait()
	nonFav := visible - fav
	if nonFav < 0 {
		nonFav = 0
	}
	nonInbox := visible - inbox
	if nonInbox < 0 {
		nonInbox = 0
	}

	s.renderTemplate(w, "partials/sidebar_browse.html", map[string]any{
		"Query":             queryStr,
		"CSRFToken":         s.csrfToken(sessionFromContext(r.Context())),
		"FolderTree":        folders,
		"SourceCounts":      sources,
		"SeriesCounts":      series,
		"SourceLabelCounts": sourceLabels,
		"InboxCount":        inbox,
		"NonInboxCount":     nonInbox,
		"FavoritedCount":    fav,
		"NonFavoritedCount": nonFav,
		"SavedSearches":     saved,
	})
}

// buildSidebarURL constructs the URL the #sidebar-inner placeholder hx-gets
// on full-page gallery renders, mirroring buildGalleryURL's encoding style
// so the sidebar handler sees the same q/sort/order/page/seed the page does.
// ids carries the page's image IDs as a comma-separated list so
// gallerySidebar can skip re-running search.Execute. The param is always
// set (even when empty) because absence is the signal for a direct URL hit
// that must fall back to the search call.
func buildSidebarURL(q, sort, order, page, seed string, ids []int64) string {
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if sort != "" {
		v.Set("sort", sort)
	}
	if order != "" {
		v.Set("order", order)
	}
	if page != "" {
		v.Set("page", page)
	}
	if seed != "" {
		v.Set("seed", seed)
	}
	var sb strings.Builder
	for i, id := range ids {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatInt(id, 10))
	}
	v.Set("ids", sb.String())
	return "/internal/sidebar?" + v.Encode()
}

// buildGalleryURL constructs a properly URL-encoded gallery redirect URL.
func buildGalleryURL(q, sort, order, page, seed string) string {
	if q == "" && sort == "" && order == "" && page == "" && seed == "" {
		return "/"
	}
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if sort != "" {
		v.Set("sort", sort)
	}
	if order != "" {
		v.Set("order", order)
	}
	if page != "" {
		v.Set("page", page)
	}
	if seed != "" {
		v.Set("seed", seed)
	}
	return "/?" + v.Encode()
}
