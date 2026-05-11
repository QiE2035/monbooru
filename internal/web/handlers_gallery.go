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

type galleryData struct {
	baseData
	Query          string
	Sort           string
	Order          string
	RandomSeed     int64
	Page           int
	TotalPages     int
	Result         *models.SearchResult
	SidebarTags    []models.Tag
	FolderTree     []gallery.FolderNode
	SourceCounts   gallery.SourceCounts
	SeriesCounts   []gallery.SeriesCount
	SavedSearches  []models.SavedSearch
	SidebarURL     string                // populated on full-page renders so the placeholder can lazy-load the sidebar
	EnabledTaggers []tagger.TaggerStatus // gates the gallery's Auto-tag controls; mirrors detailData.EnabledTaggers
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
	if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
		page = p
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
	ceiling := ratingCeilingFromRequest(r)
	expr = applyRatingCeiling(expr, ceiling)
	sq := search.Query{
		Expr:       expr,
		Sort:       sortStr,
		Order:      orderStr,
		RandomSeed: randomSeed,
		Page:       page,
		Limit:      s.cfg.UI.PageSize,
		CacheKey:   search.BuildAdjacencyCacheKey(s.activeName, queryStr, sortStr, orderStr, randomSeed, ceiling),
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

	result, err := search.Execute(s.db(), sq)
	if err != nil {
		logx.Errorf("gallery search: %v", err)
		http.Error(w, "search error", http.StatusInternalServerError)
		return
	}

	totalPages := 1
	if s.cfg.UI.PageSize > 0 {
		totalPages = (result.Total + s.cfg.UI.PageSize - 1) / s.cfg.UI.PageSize
	}

	// If a concurrent ingestion or delete shrank the result set out from under
	// the user's current page (e.g. the auto-refresh re-fetches page 3 after
	// deletions dropped the total to 1 page), re-run at the last valid page
	// so the grid isn't empty while "N images" still shows a non-zero count.
	// Sync the URL so a bookmark of the clamped view doesn't keep replaying
	// the bogus page number.
	if result.Total > 0 && page > totalPages {
		page = totalPages
		sq.Page = page
		result, err = search.Execute(s.db(), sq)
		if err != nil {
			logx.Errorf("gallery search (clamped): %v", err)
			http.Error(w, "search error", http.StatusInternalServerError)
			return
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

	// Full-page renders ship the sidebar as a placeholder that lazy-loads via
	// GET /internal/sidebar, so first paint isn't blocked on the folder-tree
	// aggregation. Search/pagination HTMX responses still need the sidebar
	// content in the same payload because gallery_htmx.html OOB-swaps it into
	// the live page.
	ids := make([]int64, 0, len(result.Results))
	for _, img := range result.Results {
		ids = append(ids, img.ID)
	}

	var (
		sidebarTags   []models.Tag
		folderTree    []gallery.FolderNode
		sourceCounts  gallery.SourceCounts
		seriesCounts  []gallery.SeriesCount
		savedSearches []models.SavedSearch
	)
	if htmxGridTarget {
		sidebarTags, folderTree, sourceCounts, seriesCounts, savedSearches = s.sidebarLoad(ids)
	}

	data := galleryData{
		baseData:       s.base(r, "gallery", "Images - Monbooru"),
		Query:          queryStr,
		Sort:           sortStr,
		Order:          orderStr,
		RandomSeed:     randomSeed,
		Page:           page,
		TotalPages:     totalPages,
		Result:         result,
		SidebarTags:    sidebarTags,
		FolderTree:     folderTree,
		SourceCounts:   sourceCounts,
		SeriesCounts:   seriesCounts,
		SavedSearches:  savedSearches,
		EnabledTaggers: tagger.EnabledTaggersForGallery(s.cfg, s.activeName),
	}

	if htmxGridTarget {
		s.renderTemplate(w, "partials/gallery_htmx.html", data)
		return
	}
	data.SidebarURL = buildSidebarURL(queryStr, sortStr, orderStr, pageStr, q.Get("seed"), ids)
	s.renderTemplate(w, "gallery.html", data)
}

// sidebarLoad runs the reads that populate the gallery sidebar - current-page
// tags, folder tree, source counts, series counts, saved searches - in parallel
// across the read pool. Called from galleryHandler on HTMX grid swaps (to keep
// the OOB sidebar update) and from gallerySidebar (lazy-load on full-page render).
func (s *Server) sidebarLoad(pageImageIDs []int64) ([]models.Tag, []gallery.FolderNode, gallery.SourceCounts, []gallery.SeriesCount, []models.SavedSearch) {
	var (
		tags    []models.Tag
		folders []gallery.FolderNode
		sources gallery.SourceCounts
		series  []gallery.SeriesCount
		saved   []models.SavedSearch
	)
	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		tags, _ = search.SidebarTagsWithGlobalCount(s.db(), pageImageIDs)
	}()
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			folders, _ = cx.FolderTree()
		}
	}()
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			sources, _ = cx.SourceCounts()
		}
	}()
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			series, _ = cx.SeriesCounts()
		}
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
			saved = append(saved, ss)
		}
	}()
	wg.Wait()
	return tags, folders, sources, series, saved
}

// gallerySidebar renders the gallery sidebar partial on demand. Initial
// full-page gallery renders ship an empty #sidebar-inner placeholder that
// hx-gets this endpoint on load - same pattern as the detail page's
// related-images lazy fetch.
func (s *Server) gallerySidebar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	queryStr := q.Get("q")

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
		expr = applyRatingCeiling(expr, ratingCeilingFromRequest(r))
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

	sidebarTags, folderTree, sourceCounts, seriesCounts, savedSearches := s.sidebarLoad(ids)

	s.renderTemplate(w, "partials/sidebar_content.html", map[string]any{
		"Query":         queryStr,
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
		"SidebarTags":   sidebarTags,
		"FolderTree":    folderTree,
		"SourceCounts":  sourceCounts,
		"SeriesCounts":  seriesCounts,
		"SavedSearches": savedSearches,
	})
}

// sidebarBrowse renders the folder/source/saved-searches sections only -
// no per-page tag groups. Lazy-loaded from the detail page so its sidebar
// gets the same browse shortcuts the gallery sidebar does without paying
// the folder-tree aggregation cost on first paint.
func (s *Server) sidebarBrowse(w http.ResponseWriter, r *http.Request) {
	queryStr := r.URL.Query().Get("q")

	var (
		folders []gallery.FolderNode
		sources gallery.SourceCounts
		series  []gallery.SeriesCount
		saved   []models.SavedSearch
	)
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			folders, _ = cx.FolderTree()
		}
	}()
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			sources, _ = cx.SourceCounts()
		}
	}()
	go func() {
		defer wg.Done()
		if cx := s.Active(); cx != nil {
			series, _ = cx.SeriesCounts()
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

	s.renderTemplate(w, "partials/sidebar_browse.html", map[string]any{
		"Query":         queryStr,
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
		"FolderTree":    folders,
		"SourceCounts":  sources,
		"SeriesCounts":  series,
		"SavedSearches": saved,
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
