package web

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
	"github.com/leqwin/monbooru/internal/logx"
	meta "github.com/leqwin/monbooru/internal/metadata"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/search"
	"github.com/leqwin/monbooru/internal/searchkw"
	"github.com/leqwin/monbooru/internal/tagger"
	"github.com/leqwin/monbooru/internal/tags"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Auth.EnablePassword {
		// Render the login form with an inline notice and the input/submit
		// disabled instead of silently redirecting - a user who bookmarked
		// /login after disabling auth otherwise gets no explanation for why
		// the page vanished, and leaving the fields live makes it look like
		// the 'login' somehow worked when the server just redirects to /.
		s.renderTemplate(w, "login.html", map[string]any{
			"CSRFToken":    s.csrfToken("anon"),
			"Error":        "Password authentication is disabled. Enable it from Settings → Authentication.",
			"AuthDisabled": true,
		})
		return
	}
	s.renderTemplate(w, "login.html", map[string]any{
		"CSRFToken": s.csrfToken("anon"),
	})
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Auth.EnablePassword {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	ip := clientIP(r)
	if !s.loginRL.check(ip) {
		logx.Warnf("login rate-limited from %s", ip)
		s.renderTemplate(w, "login.html", map[string]any{
			"Error":     "Too many attempts. Please wait before trying again.",
			"CSRFToken": s.csrfToken("anon"),
		})
		return
	}

	password := r.FormValue("password")
	if err := bcrypt.CompareHashAndPassword(
		[]byte(s.cfg.Auth.PasswordHash), []byte(password),
	); err != nil {
		s.loginRL.recordFailure(ip)
		logx.Warnf("login failed from %s", ip)
		s.renderTemplate(w, "login.html", map[string]any{
			"Error":     "Invalid password",
			"CSRFToken": s.csrfToken("anon"),
		})
		return
	}
	s.loginRL.recordSuccess(ip)
	logx.Infof("login success from %s", ip)

	sessID, err := s.sessions.NewSession(s.cfg.Auth.SessionLifetimeDays)
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "monbooru_session",
		Value:    sessID,
		Path:     "/",
		MaxAge:   s.cfg.Auth.SessionLifetimeDays * 86400,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logoutPost(w http.ResponseWriter, r *http.Request) {
	sessID := sessionFromContext(r.Context())
	s.sessions.DeleteSession(sessID)
	http.SetCookie(w, &http.Cookie{
		Name:   "monbooru_session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

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
	if result.Total > 0 && page > totalPages {
		page = totalPages
		sq.Page = page
		result, err = search.Execute(s.db(), sq)
		if err != nil {
			logx.Errorf("gallery search (clamped): %v", err)
			http.Error(w, "search error", http.StatusInternalServerError)
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
		savedSearches []models.SavedSearch
	)
	if htmxGridTarget {
		sidebarTags, folderTree, sourceCounts, savedSearches = s.sidebarLoad(ids)
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
		SavedSearches:  savedSearches,
		EnabledTaggers: tagger.EnabledTaggers(s.cfg),
	}

	if htmxGridTarget {
		s.renderTemplate(w, "partials/gallery_htmx.html", data)
		return
	}
	data.SidebarURL = buildSidebarURL(queryStr, sortStr, orderStr, pageStr, q.Get("seed"), ids)
	s.renderTemplate(w, "gallery.html", data)
}

// sidebarLoad runs the reads that populate the gallery sidebar - current-page
// tags, folder tree, source counts, saved searches - in parallel across the
// read pool. Called from galleryHandler on HTMX grid swaps (to keep the OOB
// sidebar update) and from gallerySidebar (lazy-load on full-page render).
func (s *Server) sidebarLoad(pageImageIDs []int64) ([]models.Tag, []gallery.FolderNode, gallery.SourceCounts, []models.SavedSearch) {
	var (
		tags    []models.Tag
		folders []gallery.FolderNode
		sources gallery.SourceCounts
		saved   []models.SavedSearch
	)
	var wg sync.WaitGroup
	wg.Add(4)
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
		ssRows, ssErr := s.db().Read.Query(`SELECT id, name, query FROM saved_searches ORDER BY name`)
		if ssErr != nil {
			return
		}
		defer ssRows.Close()
		for ssRows.Next() {
			var ss models.SavedSearch
			if err := ssRows.Scan(&ss.ID, &ss.Name, &ss.Query); err != nil {
				logx.Warnf("sidebar saved searches scan: %v", err)
				continue
			}
			saved = append(saved, ss)
		}
	}()
	wg.Wait()
	return tags, folders, sources, saved
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

	sidebarTags, folderTree, sourceCounts, savedSearches := s.sidebarLoad(ids)

	s.renderTemplate(w, "partials/sidebar_content.html", map[string]any{
		"Query":         queryStr,
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
		"SidebarTags":   sidebarTags,
		"FolderTree":    folderTree,
		"SourceCounts":  sourceCounts,
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
		saved   []models.SavedSearch
	)
	var wg sync.WaitGroup
	wg.Add(3)
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
		rows, err := s.db().Read.Query(`SELECT id, name, query FROM saved_searches ORDER BY name`)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var ss models.SavedSearch
			if err := rows.Scan(&ss.ID, &ss.Name, &ss.Query); err != nil {
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

type detailData struct {
	baseData
	Image          models.Image
	Filename       string // basename of the canonical path, shown on the detail page topbar
	ImageTags      []models.ImageTag
	SDMeta         *models.SDMetadata
	ComfyMeta      *models.ComfyUIMetadata
	ComfyNodes     []models.ComfyNode
	GenericMeta    []models.SDParam
	ImagePaths     []models.ImagePath
	ThumbnailURL   string
	PrevID         *int64
	NextID         *int64
	Position       int    // 1-based rank of Image within the referring search; 0 = unknown (no back-* context)
	PositionTotal  int    // total matching rows in the referring search
	RefURL         string // predecessor detail URL when the user arrived via a Similar-images click; drives the "← Previous image" back link and Escape
	Ref            string // raw ref=<sourceID> value when valid; forwarded on the delete button so the post-delete redirect returns to the source instead of an arbitrary neighbour
	BackQuery      string
	BackSort       string
	BackOrder      string
	BackPage       string
	BackSeed       string
	EnabledTaggers []tagger.TaggerStatus // enabled+available taggers offered in the auto-tag control
	ImageTaggers   []string              // distinct tagger names currently on this image's auto-tags
	HasUserTags    bool                  // true when at least one manual (non-auto) tag is on this image
	Aliases        []models.Tag          // alias rows pointing at any non-implied tag on this image, flattened for display
}

func (s *Server) detailHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		s.notFoundHandler(w, r)
		return
	}

	ctx := r.Context()
	img, err := loadImage(ctx, s.db(), id)
	if err != nil {
		s.notFoundHandler(w, r)
		return
	}

	// Prev/next navigation is only computed when the referring gallery context
	// is carried through via back_* query params. Resolve the values now so
	// the parallel block below can launch the adjacency lookup alongside the
	// other reads instead of after them.
	backQ := r.URL.Query().Get("back_q")
	backSort := r.URL.Query().Get("back_sort")
	backOrder := r.URL.Query().Get("back_order")
	backPage := r.URL.Query().Get("back_page")
	backSeed := r.URL.Query().Get("back_seed")

	// A "ref" query param points at the detail page the user just came from
	// (a Similar-images click). When set and valid, the gallery-context UI
	// (X/Y counter, prev/next arrows, "← Images" back link) is suppressed
	// because the user just switched contexts - the current image may not
	// even be in the referring search. back_* still flows through so the
	// rebuilt refURL lands the user back on the source with its original
	// gallery context when they click "← Previous image".
	refURL := ""
	refStrValid := ""
	if refStr := r.URL.Query().Get("ref"); refStr != "" {
		if refID, err := strconv.ParseInt(refStr, 10, 64); err == nil && refID != id {
			refURL = buildDetailURL(refID, backQ, backSort, backOrder, backPage, backSeed)
			refStrValid = strconv.FormatInt(refID, 10)
		}
	}

	wantAdjacent := refURL == "" && (backSort != "" || backQ != "")
	if wantAdjacent {
		if backSort == "" {
			backSort = "newest"
		}
		if backOrder == "" {
			backOrder = "desc"
		}
	}
	ceiling := ratingCeilingFromRequest(r)

	// The remaining reads are independent of each other and target different
	// tables (or the filesystem for ExtractGeneric). Run them in parallel
	// across the read pool. Related images are fetched lazily via
	// /images/{id}/related so the page paints before that join finishes -
	// on libraries with millions of image_tags rows it was the single
	// largest contributor to detail-page latency. comfyNodes parsing stays
	// sequential - it's pure CPU on the comfyMeta payload and only matters
	// once that read returns.
	var (
		imageTags     []models.ImageTag
		sdMeta        *models.SDMetadata
		comfyMeta     *models.ComfyUIMetadata
		genericMeta   []models.SDParam
		imagePaths    []models.ImagePath
		prevID        *int64
		nextID        *int64
		position      int
		positionTotal int
	)
	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); imageTags, _ = s.tagSvc().GetImageTags(id) }()
	go func() { defer wg.Done(); sdMeta = loadSDMeta(ctx, s.db(), id) }()
	go func() { defer wg.Done(); comfyMeta = loadComfyMeta(ctx, s.db(), id) }()
	go func() { defer wg.Done(); imagePaths = loadImagePaths(ctx, s.db(), id) }()
	go func() {
		defer wg.Done()
		genericMeta = meta.ExtractGeneric(img.CanonicalPath, img.FileType)
	}()
	if wantAdjacent {
		wg.Add(2)
		go func() {
			defer wg.Done()
			prevID, nextID = s.findAdjacentImages(id, backQ, backSort, backOrder, backSeed, ceiling)
		}()
		go func() {
			defer wg.Done()
			position, positionTotal = s.findImagePosition(id, backQ, backSort, backOrder, backSeed, ceiling)
		}()
	}
	wg.Wait()

	var comfyNodes []models.ComfyNode
	if comfyMeta != nil && comfyMeta.RawWorkflow != "" {
		comfyNodes = meta.ParseComfyWorkflowNodes(comfyMeta.RawWorkflow)
	}

	enabledTaggers := tagger.EnabledTaggers(s.cfg)
	imageTaggers := distinctAutoTaggerNames(imageTags)
	hasUserTags := false
	for _, t := range imageTags {
		if !t.IsAuto {
			hasUserTags = true
			break
		}
	}

	baseName := filepath.Base(img.CanonicalPath)
	data := detailData{
		baseData:       s.base(r, "gallery", fmt.Sprintf("%s - Monbooru", baseName)),
		Image:          *img,
		Filename:       baseName,
		ImageTags:      imageTags,
		SDMeta:         sdMeta,
		ComfyMeta:      comfyMeta,
		ComfyNodes:     comfyNodes,
		GenericMeta:    genericMeta,
		ImagePaths:     imagePaths,
		ThumbnailURL:   fmt.Sprintf("/thumbnails/%s/%d.jpg", s.activeName, id),
		PrevID:         prevID,
		NextID:         nextID,
		Position:       position,
		PositionTotal:  positionTotal,
		RefURL:         refURL,
		Ref:            refStrValid,
		BackQuery:      backQ,
		BackSort:       backSort,
		BackOrder:      backOrder,
		BackPage:       backPage,
		BackSeed:       backSeed,
		EnabledTaggers: enabledTaggers,
		ImageTaggers:   imageTaggers,
		HasUserTags:    hasUserTags,
		Aliases:        s.aliasesForImageTags(imageTags),
	}
	s.renderTemplate(w, "detail.html", data)
}

// relatedImagesHandler returns the Similar-images mini-grid for an image,
// fetched lazily from the detail page so the initial render isn't blocked
// on the shared-tag aggregation over image_tags.
func (s *Server) relatedImagesHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	related, _ := s.tagSvc().RelatedImages(id, 9, ratingCeilingFromRequest(r))
	q := r.URL.Query()
	s.renderTemplate(w, "partials/related_images.html", map[string]any{
		"Images":        related,
		"ActiveGallery": s.activeName,
		// Each similar-image link carries ref=<current image> so the
		// destination detail page swaps "← Images" for "← Previous image"
		// (pointing back here) and Escape walks browser history. back_*
		// flow through so that "← Previous image" link restores this
		// page's own gallery context when clicked.
		"SourceID":  id,
		"BackQuery": q.Get("back_q"),
		"BackSort":  q.Get("back_sort"),
		"BackOrder": q.Get("back_order"),
		"BackPage":  q.Get("back_page"),
		"BackSeed":  q.Get("back_seed"),
	})
}

// distinctAutoTaggerNames returns the unique tagger names seen in the
// image's auto-tag rows, preserving the first-seen order from the sorted
// tag list.
func distinctAutoTaggerNames(tags []models.ImageTag) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		if !t.IsAuto || t.TaggerName == "" || seen[t.TaggerName] {
			continue
		}
		seen[t.TaggerName] = true
		out = append(out, t.TaggerName)
	}
	return out
}

// catTag pairs a resolved category ID with a tag name for creation/application.
type catTag struct {
	catID int64
	name  string
}

// parseTagInput parses multi-token tag input.
//
// Tokens are separated by whitespace. Each token becomes its own tag: a
// bare word, a "category:name" pair, or a double-quoted span whose
// internal spaces are collapsed to underscores (so `"red hair"` →
// `red_hair`). Quotes can follow a category prefix
// (`artist:"john doe"`).
//
// Examples:
//
//	red hair                 -> [{general, "red"}, {general, "hair"}]
//	"red hair" blue_eyes     -> [{general, "red_hair"}, {general, "blue_eyes"}]
//	artist:"john doe" 1girl  -> [{artist, "john_doe"}, {general, "1girl"}]
func (s *Server) parseTagInput(tagInput string) ([]catTag, string) {
	tokens, err := splitTagTokens(tagInput)
	if err != nil {
		return nil, err.Error()
	}

	// general category id is cached on galleryCtx at open time so this
	// hot path doesn't re-query the immutable built-in row.
	var generalID int64
	if cx := s.Active(); cx != nil {
		generalID = cx.GeneralCategoryID
	}

	var catTags []catTag
	for _, tok := range tokens {
		name := tok.name
		if idx := strings.Index(name, ":"); idx > 0 {
			catName := name[:idx]
			tagName := name[idx+1:]
			var catID int64
			if err := s.db().Read.QueryRow(
				`SELECT id FROM tag_categories WHERE name=?`, catName,
			).Scan(&catID); err == nil {
				if tagName != "" {
					catTags = append(catTags, catTag{catID, tagName})
				}
				continue
			}
			// Prefix isn't a known category; treat the whole token as a
			// literal general-category tag (e.g. "nier:automata").
		}
		catTags = append(catTags, catTag{generalID, name})
	}

	return catTags, ""
}

// parsedTagToken is one tokenizer output: its resolved name.
type parsedTagToken struct {
	name string
}

// splitTagTokens splits tag-input into whitespace-separated tokens while
// respecting double-quoted spans. Inside a quoted span, internal spaces
// are replaced with underscores. Quoted spans may be preceded by a
// category prefix (`artist:"john doe"`). Unterminated quotes return an
// error.
func splitTagTokens(s string) ([]parsedTagToken, error) {
	var tokens []parsedTagToken
	var buf strings.Builder
	quoted := false
	inToken := false

	flush := func() {
		if !inToken {
			return
		}
		tokens = append(tokens, parsedTagToken{name: buf.String()})
		buf.Reset()
		inToken = false
	}

	for _, r := range s {
		if r == '"' {
			quoted = !quoted
			inToken = true
			continue
		}
		if quoted {
			if r == ' ' || r == '\t' {
				buf.WriteRune('_')
			} else {
				buf.WriteRune(r)
			}
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' {
			flush()
			continue
		}
		buf.WriteRune(r)
		inToken = true
	}
	if quoted {
		return nil, fmt.Errorf("unterminated quote in tag input")
	}
	flush()
	return tokens, nil
}

func (s *Server) addTagToImage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	tagInput := strings.TrimSpace(r.FormValue("tag"))
	if tagInput == "" {
		http.Error(w, "tag required", http.StatusBadRequest)
		return
	}

	catTags, parseErrMsg := s.parseTagInput(tagInput)

	var rejected, dupes []string
	var promotedTokens []string
	addErrMsg := parseErrMsg
	mutated := false
	for _, ct := range catTags {
		tag, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
		if err != nil {
			logx.Warnf("add tag %q: %v", ct.name, err)
			rejected = append(rejected, ct.name+": "+err.Error())
			continue
		}
		// Single write-pool transaction does the INSERT OR IGNORE and
		// reports whether the row was actually new, so parallel adds
		// can't both claim they were the first writer.
		added, promoted, err := s.tagSvc().AddTagToImageReportingDup(id, tag.ID, false, nil, "")
		if err != nil {
			logx.Warnf("add tag %q to image %d: %v", ct.name, id, err)
			rejected = append(rejected, ct.name+": "+err.Error())
			continue
		}
		if added || promoted {
			mutated = true
		}
		if !added && !promoted {
			dupes = append(dupes, ct.name)
		}
		if promoted {
			promotedTokens = append(promotedTokens, ct.name)
		}
	}

	if mutated {
		s.Active().InvalidateCaches()
	}
	// Distinguish "everything went in" from "some tokens failed" so a
	// pasted multi-token input doesn't leave the user diffing the under-
	// image list against their string. The input is cleared on full
	// success and on a clean partial (some applied, some duplicates):
	// the user can read the live tag list to confirm what's there. It
	// stays populated only when at least one token was rejected, so the
	// user can edit and resubmit.
	switch {
	case len(rejected) > 0 && addErrMsg == "":
		addErrMsg = "rejected: " + strings.Join(rejected, "; ")
	case len(rejected) == 0 && len(dupes) > 0 && !mutated && parseErrMsg == "":
		// Whole submit hit only existing tags; preserve the prior
		// soft-error feedback so the user sees something happened.
		addErrMsg = "tag already on image: " + strings.Join(dupes, ", ")
	}
	addOkMsg := ""
	if len(promotedTokens) > 0 {
		addOkMsg = "promoted to user tag: " + strings.Join(promotedTokens, ", ")
	}
	s.renderTagListWithSidebar(w, r, id, addErrMsg, addOkMsg, len(rejected) == 0 && parseErrMsg == "")
}

// aliasesForImageTags returns the alias rows pointing at any non-implied
// tag carried by the image, ordered by name. Used by both the full
// detail render and the htmx tag-list refresh so the "Aliases" group at
// the bottom of the under-image list stays in sync.
func (s *Server) aliasesForImageTags(imageTags []models.ImageTag) []models.Tag {
	if len(imageTags) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(imageTags))
	for _, t := range imageTags {
		if t.IsImplied {
			continue
		}
		ids = append(ids, t.TagID)
	}
	if len(ids) == 0 {
		return nil
	}
	byCanon, err := s.tagSvc().AliasesForTagIDs(ids)
	if err != nil {
		logx.Warnf("AliasesForTagIDs: %v", err)
		return nil
	}
	var out []models.Tag
	for _, list := range byCanon {
		out = append(out, list...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// renderTagListWithSidebar renders the image tag list partial and always emits
// OOB swaps of the detail sidebar and danger zone so tag groups and remove-tag
// buttons stay in sync without a page reload.
// errMsg / okMsg are shown as inline flashes if non-empty; clearInput resets
// the add-tag input.
func (s *Server) renderTagListWithSidebar(w http.ResponseWriter, r *http.Request, id int64, errMsg, okMsg string, clearInput bool) {
	imageTags, _ := s.tagSvc().GetImageTags(id)
	hasUserTags := false
	for _, t := range imageTags {
		if !t.IsAuto {
			hasUserTags = true
			break
		}
	}
	var folderPath string
	_ = s.db().Read.QueryRow(`SELECT folder_path FROM images WHERE id = ?`, id).Scan(&folderPath)
	q := r.URL.Query()
	s.renderTemplate(w, "partials/tag_list.html", map[string]any{
		"ImageID":       id,
		"ImageTags":     imageTags,
		"Aliases":       s.aliasesForImageTags(imageTags),
		"SidebarTags":   true,
		"DangerZone":    true,
		"HasUserTags":   hasUserTags,
		"ImageTaggers":  distinctAutoTaggerNames(imageTags),
		"BackQuery":     q.Get("back_q"),
		"BackSort":      q.Get("back_sort"),
		"BackOrder":     q.Get("back_order"),
		"BackPage":      q.Get("back_page"),
		"BackSeed":      q.Get("back_seed"),
		"CSRFToken":     s.csrfToken(sessionFromContext(r.Context())),
		"EditMode":      true,
		"ErrMsg":        errMsg,
		"OkMsg":         okMsg,
		"ClearInput":    clearInput,
		"CurrentFolder": folderPath,
	})
}

// removeAutoTagsFromImageHandler removes auto-tagged rows from one image,
// optionally filtered by the caller-supplied `taggers` query parameter
// (comma-separated tagger names). Empty filter removes every auto-tag.
func (s *Server) removeAutoTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	raw := r.URL.Query().Get("taggers")
	var names []string
	for _, n := range strings.Split(raw, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if err := s.tagSvc().RemoveAutoTagsFromImage(id, names); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderTagListWithSidebar(w, r, id, "", "", false)
}

func (s *Server) removeUserTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().RemoveUserTagsFromImage(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderTagListWithSidebar(w, r, id, "", "", false)
}

func (s *Server) removeAllTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().RemoveAllTagsFromImage(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderTagListWithSidebar(w, r, id, "", "", false)
}

func (s *Server) removeTagFromImage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	tagIDStr := r.PathValue("tagID")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	tagID, err := strconv.ParseInt(tagIDStr, 10, 64)
	if err != nil {
		http.Error(w, "bad tagID", http.StatusBadRequest)
		return
	}

	if err := s.tagSvc().RemoveTagFromImage(id, tagID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderTagListWithSidebar(w, r, id, "", "", false)
}

// ratingCeilingFromRequest reads the monbooru_rating_ceiling cookie and
// returns the active level. Empty string and "explicit" both mean "no
// ceiling" - callers should treat them as a no-op. Anything outside the
// closed enum is dropped to "" so a stale or hand-crafted cookie can't
// inject arbitrary AST values.
func ratingCeilingFromRequest(r *http.Request) string {
	c, err := r.Cookie("monbooru_rating_ceiling")
	if err != nil {
		return ""
	}
	switch c.Value {
	case "general", "sensitive", "questionable", "explicit":
		return c.Value
	}
	return ""
}

// applyRatingCeiling AND-chains a NotExpr per rating level above the
// ceiling onto userExpr. An empty/unknown ceiling and "explicit" pass
// through unchanged.
func applyRatingCeiling(userExpr search.Expr, ceiling string) search.Expr {
	rank := -1
	for i, l := range tags.RatingLevels {
		if l == ceiling {
			rank = i
			break
		}
	}
	if rank < 0 || rank >= len(tags.RatingLevels)-1 {
		return userExpr
	}
	var ce search.Expr
	for i := rank + 1; i < len(tags.RatingLevels); i++ {
		not := search.NotExpr{Expr: search.FilterExpr{Key: "rating", Val: tags.RatingLevels[i]}}
		if ce == nil {
			ce = not
		} else {
			ce = search.AndExpr{Left: ce, Right: not}
		}
	}
	if userExpr == nil {
		return ce
	}
	return search.AndExpr{Left: userExpr, Right: ce}
}

// ratingCeilingPost sets or clears the monbooru_rating_ceiling cookie.
// Posting level=explicit (or any out-of-enum value) clears the cookie so
// the empty-storage steady state means "no ceiling".
func (s *Server) ratingCeilingPost(w http.ResponseWriter, r *http.Request) {
	level := r.URL.Query().Get("level")
	if level == "" {
		level = r.FormValue("level")
	}
	switch level {
	case "general", "sensitive", "questionable":
		http.SetCookie(w, &http.Cookie{
			Name:     "monbooru_rating_ceiling",
			Value:    level,
			Path:     "/",
			HttpOnly: true,
			MaxAge:   31_536_000,
			SameSite: http.SameSiteLaxMode,
		})
	default:
		// "explicit" or any unknown value clears the cookie.
		http.SetCookie(w, &http.Cookie{
			Name:   "monbooru_rating_ceiling",
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) toggleFavorite(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	// Toggle atomically and read the new value in a single statement using RETURNING,
	// avoiding a separate read query and any read-after-write race.
	var newFav int
	if err := s.db().Write.QueryRow(
		`UPDATE images SET is_favorited = 1 - is_favorited WHERE id = ? RETURNING is_favorited`, id,
	).Scan(&newFav); err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if newFav == 1 {
		w.Write([]byte(`<button type="submit" id="fav-btn" class="btn-fav active" title="Unfavorite">★</button>`))
	} else {
		w.Write([]byte(`<button type="submit" id="fav-btn" class="btn-fav" title="Favorite">☆</button>`))
	}
}

func (s *Server) deleteImage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}

	backQ := r.URL.Query().Get("back_q")
	backSort := r.URL.Query().Get("back_sort")
	backOrder := r.URL.Query().Get("back_order")
	backPage := r.URL.Query().Get("back_page")
	backSeed := r.URL.Query().Get("back_seed")

	// When the caller arrived via a Similar-images click the URL carries
	// ref=<sourceID> and the back_* params describe the source's gallery
	// context, not a search the current image is part of. Snapshotting the
	// valid source id up front keeps the post-delete redirect aimed at the
	// original image instead of jumping to an arbitrary neighbour of the
	// unrelated back_* search.
	var refID *int64
	if refStr := r.URL.Query().Get("ref"); refStr != "" {
		if parsed, err := strconv.ParseInt(refStr, 10, 64); err == nil && parsed != id {
			refID = &parsed
		}
	}

	// Compute the neighbour before the delete so we don't miss it once the
	// current row is gone. When the caller carried back_* params the detail
	// page had a search context; we keep the user in that stream by jumping
	// to the adjacent image instead of bouncing back to the gallery. Ref
	// takes precedence over adjacency: the current image may not even be in
	// the referring search.
	var prevID, nextID *int64
	if refID == nil && (backSort != "" || backQ != "") {
		sortStr := backSort
		if sortStr == "" {
			sortStr = "newest"
		}
		orderStr := backOrder
		if orderStr == "" {
			orderStr = "desc"
		}
		prevID, nextID = s.findAdjacentImages(id, backQ, sortStr, orderStr, backSeed, ratingCeilingFromRequest(r))
	}

	result, err := gallery.DeleteImage(s.db(), s.thumbnailsPath(), id, s.tagSvc().RemoveAllTagsFromImage)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.Active().InvalidateCaches()

	if !result.IsMissing {
		gallery.DeleteEmptyFolderIfEmpty(s.galleryPath(), result.FolderPath)
	}

	redirectURL := ""
	switch {
	case refID != nil:
		redirectURL = buildDetailURL(*refID, backQ, backSort, backOrder, backPage, backSeed)
	case nextID != nil:
		redirectURL = buildDetailURL(*nextID, backQ, backSort, backOrder, backPage, backSeed)
	case prevID != nil:
		redirectURL = buildDetailURL(*prevID, backQ, backSort, backOrder, backPage, backSeed)
	default:
		redirectURL = buildGalleryURL(backQ, backSort, backOrder, backPage, backSeed)
	}

	if isHTMXRequest(r) {
		// Ref case: the user arrived here via a Similar-images click, which
		// itself may be any depth into a chain. Redirecting to the source
		// would push a fresh history entry that drops the ref chain - the
		// post-delete source page then has no data-ref and Escape escapes
		// straight to the gallery. Fire a delete-go-back trigger instead so
		// the client can prefer history.back(), landing on the source's
		// original URL (with its own ref intact) and keeping the chain
		// walkable. The fallback URL handles the cold-load case where the
		// browser has no predecessor (direct link, bookmarked tab).
		if refID != nil {
			w.Header().Set("HX-Trigger", fmt.Sprintf(`{"delete-go-back":{"fallback":%q}}`, redirectURL))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("HX-Redirect", redirectURL)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

// buildDetailURL constructs a detail-page URL with back_* params so the
// destination page keeps the same gallery context (prev/next adjacency,
// back-link target) the user came in with.
func buildDetailURL(id int64, q, sort, order, page, seed string) string {
	base := fmt.Sprintf("/images/%d", id)
	v := url.Values{}
	if q != "" {
		v.Set("back_q", q)
	}
	if sort != "" {
		v.Set("back_sort", sort)
	}
	if order != "" {
		v.Set("back_order", order)
	}
	if page != "" {
		v.Set("back_page", page)
	}
	if seed != "" {
		v.Set("back_seed", seed)
	}
	if len(v) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

func (s *Server) promoteCanonical(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	newCanonical := r.FormValue("path")
	if newCanonical == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}

	// Refuse to promote anything that isn't already a tracked alias of this
	// image. Without this check, a user could write an arbitrary string
	// into images.canonical_path and coerce serveImageFile into serving a
	// sibling file whose path happens to live inside the gallery root.
	var aliasExists int
	if err := s.db().Read.QueryRow(
		`SELECT COUNT(*) FROM image_paths WHERE image_id = ? AND path = ?`,
		id, newCanonical,
	).Scan(&aliasExists); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if aliasExists == 0 {
		http.Error(w, "path is not an alias of this image", http.StatusBadRequest)
		return
	}

	newFolder := gallery.FolderPath(s.galleryPath(), newCanonical)

	tx, err := s.db().Write.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE image_paths SET is_canonical = 0 WHERE image_id = ?`, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(
		`UPDATE image_paths SET is_canonical = 1 WHERE image_id = ? AND path = ?`,
		id, newCanonical,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(
		`UPDATE images SET canonical_path = ?, folder_path = ? WHERE id = ?`,
		newCanonical, newFolder, id,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/images/"+idStr, http.StatusSeeOther)
}

func (s *Server) deleteAlias(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	pathIDStr := r.PathValue("pathID")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	pathID, err := strconv.ParseInt(pathIDStr, 10, 64)
	if err != nil {
		http.Error(w, "bad pathID", http.StatusBadRequest)
		return
	}

	// Refuse to remove the canonical path; callers must promote another alias
	// first, otherwise the image would lose its on-disk reference entirely.
	var isCanon int
	var aliasPath string
	if err := s.db().Read.QueryRow(
		`SELECT is_canonical, path FROM image_paths WHERE id = ? AND image_id = ?`, pathID, id,
	).Scan(&isCanon, &aliasPath); err != nil {
		http.Error(w, "alias path not found", http.StatusNotFound)
		return
	}
	if isCanon == 1 {
		http.Error(w, "cannot delete canonical path", http.StatusBadRequest)
		return
	}

	if _, err := s.db().Write.Exec(`DELETE FROM image_paths WHERE id = ?`, pathID); err != nil {
		logx.Warnf("delete alias row %d: %v", pathID, err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	if aliasPath != "" {
		if err := os.Remove(aliasPath); err != nil && !os.IsNotExist(err) {
			logx.Warnf("delete alias file %q: %v", aliasPath, err)
		}
	}

	if isHTMXRequest(r) {
		// Empty body for HTMX outerHTML swap - removes the row.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(""))
		return
	}
	http.Redirect(w, r, "/images/"+idStr, http.StatusSeeOther)
}

// tagsPageData embeds baseData so the layout template sees its fields as
// struct members (matching galleryData / detailData) and the tags template
// can reach its own state via direct field access.
type tagsPageData struct {
	baseData
	Tags         []models.Tag
	Categories   []models.TagCategory
	Implications map[int64][]models.Implication // direct implications keyed by parent tag id
	Total        int
	Page         int
	TotalPages   int
	CategoryID   string
	Prefix       string
	Sort         string
	Order        string
	Origin       string
	ShowZero     bool
	ZeroOnly     bool
}

func (s *Server) tagsHandler(w http.ResponseWriter, r *http.Request) {
	// The tags page reflects rapidly-changing state (category re-assignment,
	// merges). Opt out of browser caching so a reload after a mutation never
	// serves a stale render.
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	catIDStr := q.Get("cat")
	prefix := q.Get("q")
	sortStr := q.Get("sort")
	if sortStr == "" {
		sortStr = "usage"
	}
	orderStr := q.Get("order")
	if orderStr != "asc" && orderStr != "desc" {
		// Default to the natural reading direction per sort: most-used first
		// for usage, alphabetical A→Z for name.
		if sortStr == "usage" {
			orderStr = "desc"
		} else {
			orderStr = "asc"
		}
	}
	originStr := q.Get("origin")
	// show_zero is tri-state: empty/"1" → Show (default so freshly-declared
	// tags surface without a filter flip); "0" → Hide; "only" → only zero-
	// usage rows (triage view).
	zeroParam := q.Get("show_zero")
	zeroOnly := zeroParam == "only"
	showZero := zeroOnly || zeroParam != "0"
	page := 1
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
		page = p
	}

	filter := s.buildTagFilter(catIDStr, prefix, sortStr, orderStr, originStr, showZero, zeroOnly, page, 100)

	tagList, total, err := s.tagSvc().ListTags(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cats, _ := s.tagSvc().ListCategories()
	totalPages := (total + 99) / 100

	parentIDs := make([]int64, 0, len(tagList))
	for _, t := range tagList {
		if !t.IsAlias {
			parentIDs = append(parentIDs, t.ID)
		}
	}
	imps, err := s.tagSvc().ImplicationsForParents(parentIDs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := tagsPageData{
		baseData:     s.base(r, "tags", "Tags - Monbooru"),
		Tags:         tagList,
		Categories:   cats,
		Implications: imps,
		Total:        total,
		Page:         page,
		TotalPages:   totalPages,
		CategoryID:   catIDStr,
		Prefix:       prefix,
		Sort:         sortStr,
		Order:        orderStr,
		Origin:       originStr,
		ShowZero:     showZero,
		ZeroOnly:     zeroOnly,
	}
	s.renderTemplate(w, "tags.html", data)
}

func (s *Server) buildTagFilter(catIDStr, prefix, sortStr, orderStr, originStr string, showZero, zeroOnly bool, page, limit int) tags.TagFilter {
	f := tags.TagFilter{
		Prefix:    prefix,
		Sort:      sortStr,
		Order:     orderStr,
		PageIndex: page - 1,
		Limit:     limit,
		Origin:    originStr,
		ShowZero:  showZero,
		ZeroOnly:  zeroOnly,
	}
	if catIDStr != "" {
		if id, err := strconv.ParseInt(catIDStr, 10, 64); err == nil {
			f.CategoryID = &id
		}
	}
	return f
}

func (s *Server) mergeTagsPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	aliasIDStr := r.FormValue("alias_id")
	canonInput := strings.TrimSpace(r.FormValue("canonical_id"))

	aliasID, err := strconv.ParseInt(aliasIDStr, 10, 64)
	if err != nil {
		if isHTMXRequest(r) {
			// 200 + flash so htmx 1.9 swaps it into #merge-error;
			// the dialog's after-request hook detects the
			// flash-err class to stay open instead of closing.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<div class="flash flash-err">Invalid source tag.</div>`))
			return
		}
		http.Error(w, "bad alias id", http.StatusBadRequest)
		return
	}

	// Accept canonical_id as a tag ID, "category:name", or a plain name.
	// Plain names must be unique across categories.
	var canonID int64
	mergeErr := func(msg string) {
		if isHTMXRequest(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(msg) + `</div>`))
			return
		}
		http.Error(w, msg, http.StatusBadRequest)
	}
	if id, err := strconv.ParseInt(canonInput, 10, 64); err == nil {
		canonID = id
	} else if idx := strings.Index(canonInput, ":"); idx > 0 && s.categoryExists(canonInput[:idx]) {
		catName := canonInput[:idx]
		tagName := canonInput[idx+1:]
		if err := s.db().Read.QueryRow(
			`SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id
			 WHERE t.name = ? AND tc.name = ?`, tagName, catName,
		).Scan(&canonID); err != nil {
			mergeErr("Tag not found: " + canonInput)
			return
		}
	} else {
		rows, err := s.db().Read.Query(`SELECT id FROM tags WHERE name = ?`, canonInput)
		if err != nil {
			mergeErr("Tag lookup failed: " + err.Error())
			return
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				logx.Warnf("merge tags lookup scan: %v", err)
				continue
			}
			ids = append(ids, id)
		}
		rows.Close()
		switch len(ids) {
		case 0:
			mergeErr("Tag not found: " + canonInput)
			return
		case 1:
			canonID = ids[0]
		default:
			mergeErr("Tag name " + canonInput + " exists in multiple categories; use category:name or the tag ID")
			return
		}
	}

	if err := s.tagSvc().MergeTags(aliasID, canonID); err != nil {
		if isHTMXRequest(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()

	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/tags")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

// resolveCanonicalTag turns the merge-style canonical input (numeric id,
// "category:name", or plain name) into a tag id. The plain-name branch
// requires the name to live in a single category; ambiguity returns a
// human-readable error message the caller surfaces verbatim.
func (s *Server) resolveCanonicalTag(input string) (int64, string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, "Tag name is required."
	}
	if id, err := strconv.ParseInt(input, 10, 64); err == nil {
		return id, ""
	}
	if idx := strings.Index(input, ":"); idx > 0 && s.categoryExists(input[:idx]) {
		catName := input[:idx]
		tagName := input[idx+1:]
		var canonID int64
		if err := s.db().Read.QueryRow(
			`SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id
			 WHERE t.name = ? AND tc.name = ?`, tagName, catName,
		).Scan(&canonID); err != nil {
			return 0, "Tag not found: " + input
		}
		return canonID, ""
	}
	rows, err := s.db().Read.Query(`SELECT id FROM tags WHERE name = ?`, input)
	if err != nil {
		return 0, "Tag lookup failed: " + err.Error()
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			logx.Warnf("resolveCanonicalTag scan: %v", err)
			continue
		}
		ids = append(ids, id)
	}
	switch len(ids) {
	case 0:
		return 0, "Tag not found: " + input
	case 1:
		return ids[0], ""
	default:
		return 0, "Tag name " + input + " exists in multiple categories; use category:name or the tag ID"
	}
}

func (s *Server) createTagPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	catIDStr := r.FormValue("category_id")

	flashErr := func(msg string) {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(msg) + `</div>`))
			return
		}
		http.Error(w, msg, http.StatusBadRequest)
	}

	catID, err := strconv.ParseInt(catIDStr, 10, 64)
	if err != nil {
		flashErr("Invalid category.")
		return
	}
	if _, err := s.tagSvc().GetOrCreateTag(name, catID); err != nil {
		flashErr(err.Error())
		return
	}
	s.Active().InvalidateCaches()
	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/tags?q="+name)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

func (s *Server) createAliasPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	catIDStr := r.FormValue("category_id")
	canonInput := strings.TrimSpace(r.FormValue("canonical_id"))

	flashErr := func(msg string) {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(msg) + `</div>`))
			return
		}
		http.Error(w, msg, http.StatusBadRequest)
	}

	catID, err := strconv.ParseInt(catIDStr, 10, 64)
	if err != nil {
		flashErr("Invalid category.")
		return
	}
	canonID, msg := s.resolveCanonicalTag(canonInput)
	if msg != "" {
		flashErr(msg)
		return
	}

	if _, err := s.tagSvc().CreateAlias(name, catID, canonID); err != nil {
		flashErr(err.Error())
		return
	}
	s.Active().InvalidateCaches()

	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/tags?origin=alias&q="+name)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/tags?origin=alias", http.StatusSeeOther)
}

// implicationsDialogHandler renders the body of the implications dialog
// on the /tags page: one chip per direct implication with a delete
// button, plus a multi-tag input with autocomplete to declare new edges.
func (s *Server) implicationsDialogHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	parent, err := s.tagSvc().GetTag(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	imps, err := s.tagSvc().ListImplications(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Parent":       parent,
		"Implications": imps,
		"CSRFToken":    s.csrfToken(sessionFromContext(r.Context())),
	}
	s.renderTemplate(w, "partials/implications_dialog.html", data)
}

func (s *Server) addImplicationPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	parentID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	rawInput := strings.TrimSpace(r.FormValue("implied_id"))
	if rawInput == "" {
		w.Write([]byte(`<div class="flash flash-err">Tag name is required.</div>`))
		return
	}

	// Parse the same way the detail-page tag input does so users get
	// space-separated multi-add and "category:name" / quoted spans.
	catTags, parseErrMsg := s.parseTagInput(rawInput)
	if parseErrMsg != "" {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(parseErrMsg) + `</div>`))
		return
	}

	added := 0
	var failures []string
	for _, ct := range catTags {
		tag, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
		if err != nil {
			failures = append(failures, ct.name+": "+err.Error())
			continue
		}
		isNew, err := s.tagSvc().AddImplication(parentID, tag.ID)
		if err != nil {
			failures = append(failures, ct.name+": "+err.Error())
			continue
		}
		if isNew {
			added++
			s.startImplicationPropagation(parentID, tag.ID, "add")
		}
	}

	if added > 0 {
		// New implication targets may have been created via GetOrCreateTag,
		// so the cached tag count is stale until next render.
		s.Active().InvalidateCaches()
		// The dialog's after-request hook listens for this trigger and
		// re-fetches the body so newly-added rows render without a
		// full-page refresh that would close the modal underneath it.
		w.Header().Set("HX-Trigger", "implication-added")
	}
	switch {
	case len(failures) == 0 && added > 0:
		w.WriteHeader(http.StatusNoContent)
	case len(failures) == 0 && added == 0:
		w.Write([]byte(`<div class="flash flash-ok">Already declared.</div>`))
	case added > 0:
		w.Write([]byte(`<div class="flash flash-err">Added ` +
			strconv.Itoa(added) + `. Failed: ` +
			html.EscapeString(strings.Join(failures, "; ")) + `</div>`))
	default:
		w.Write([]byte(`<div class="flash flash-err">` +
			html.EscapeString(strings.Join(failures, "; ")) + `</div>`))
	}
}

func (s *Server) removeImplicationDelete(w http.ResponseWriter, r *http.Request) {
	parentID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	impliedID, err := strconv.ParseInt(r.PathValue("impliedID"), 10, 64)
	if err != nil {
		http.Error(w, "bad implied id", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().RemoveImplication(parentID, impliedID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.startImplicationPropagation(parentID, impliedID, "remove")
	w.WriteHeader(http.StatusNoContent)
}

// startImplicationPropagation kicks off the background job that fans
// out (op="add") or sweeps (op="remove") the parent → implied edge
// across every image carrying parent. Skipped when a job is already
// running; the user can re-trigger by editing the implication again
// (the in-DB edge is independent of this propagation, so search and
// future adds still see it through the synchronous transitive walk).
func (s *Server) startImplicationPropagation(parentID, impliedID int64, op string) {
	if err := s.jobs.Start("tag"); err != nil {
		logx.Warnf("implication %s skipped: %v", op, err)
		return
	}
	go s.runImplicationPropagation(parentID, impliedID, op)
}

func (s *Server) runImplicationPropagation(parentID, impliedID int64, op string) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	processed := 0

	rows, err := s.db().Read.QueryContext(ctx,
		`SELECT image_id FROM image_tags WHERE tag_id = ? ORDER BY image_id`, parentID,
	)
	if err != nil {
		s.jobs.Fail(err.Error())
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			s.jobs.Fail(err.Error())
			return
		}
		ids = append(ids, id)
	}
	rows.Close()

	total := len(ids)
	verb := "applying implication"
	if op == "remove" {
		verb = "removing implication"
	}
	s.jobs.Update(0, total, fmt.Sprintf("%s 0/%d…", verb, total))

	for start := 0; start < total; start += chunkSize {
		if ctx.Err() != nil {
			s.jobs.Complete(fmt.Sprintf("%s cancelled (%d/%d)", verb, processed, total))
			return
		}
		end := start + chunkSize
		if end > total {
			end = total
		}
		chunk := ids[start:end]
		tx, err := s.db().Write.Begin()
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		for _, imageID := range chunk {
			if op == "add" {
				if err := propagateAddImplication(tx, imageID, parentID); err != nil {
					tx.Rollback()
					s.jobs.Fail(err.Error())
					return
				}
			} else {
				if err := propagateRemoveImplication(tx, imageID, parentID, impliedID); err != nil {
					tx.Rollback()
					s.jobs.Fail(err.Error())
					return
				}
			}
		}
		if err := tx.Commit(); err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		processed = end
		s.jobs.Update(processed, total, fmt.Sprintf("%s %d/%d…", verb, processed, total))
	}

	if err := s.tagSvc().RecalcIDs([]int64{impliedID}); err != nil {
		logx.Warnf("implication propagation recalc: %v", err)
	}
	s.Active().InvalidateCaches()
	s.jobs.Complete(fmt.Sprintf("%s applied to %d image(s)", verb, processed))
}

// propagateAddImplication backfills implied rows for the parent on the
// given image, mirroring what addTagToImageTxReportingDup would have
// done if the implication had existed at the original add time.
// Existing rows are left alone; only fresh INSERTs get is_implied=1.
func propagateAddImplication(tx *sql.Tx, imageID, parentID int64) error {
	var isAuto int
	err := tx.QueryRow(
		`SELECT is_auto FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, parentID,
	).Scan(&isAuto)
	if err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	return tags.ApplyImpliedFanoutTx(tx, imageID, parentID, isAuto == 1)
}

// propagateRemoveImplication walks the implied tag (and its transitive
// dependents) on this image and drops any row whose only justification
// was the now-deleted edge. is_implied=0 rows (user-owned) and rows
// still implied by another parent on the image are preserved.
func propagateRemoveImplication(tx *sql.Tx, imageID, parentID, impliedID int64) error {
	// Closure under the now-gone edge: every tag that was implied via
	// parent → implied → ... must be reconsidered.
	closure, err := tags.TransitiveImpliedTx(tx, []int64{impliedID})
	if err != nil {
		return err
	}
	closure = append([]int64{impliedID}, closure...)
	for _, id := range closure {
		var rowImplied int
		err := tx.QueryRow(
			`SELECT is_implied FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, id,
		).Scan(&rowImplied)
		if err == sql.ErrNoRows {
			continue
		} else if err != nil {
			return err
		}
		if rowImplied != 1 {
			continue
		}
		// Still implied by another parent on the image? Keep it.
		var alt int64
		err = tx.QueryRow(
			`SELECT ti.parent_tag_id
			 FROM tag_implications ti
			 JOIN image_tags it ON it.tag_id = ti.parent_tag_id
			 WHERE ti.implied_tag_id = ? AND it.image_id = ?
			 LIMIT 1`,
			id, imageID,
		).Scan(&alt)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err := tx.Exec(
			`DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?`, imageID, id,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE tags SET usage_count = MAX(0, usage_count - 1) WHERE id = ?`, id,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) categoriesHandler(w http.ResponseWriter, r *http.Request) {
	cats, err := s.tagSvc().ListCategories()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	base := s.base(r, "categories", "Categories - Monbooru")
	data := map[string]any{
		"Title":         base.Title,
		"ActiveNav":     base.ActiveNav,
		"CSRFToken":     base.CSRFToken,
		"AuthEnabled":   base.AuthEnabled,
		"Degraded":      base.Degraded,
		"Version":       base.Version,
		"RepoURL":       base.RepoURL,
		"Variant":       base.Variant,
		"CustomCSS":     base.CustomCSS,
		"ActiveGallery": base.ActiveGallery,
		"Galleries":     s.galleryList(),
		"VisibleCount":  base.VisibleCount,
		"TagCount":      base.TagCount,
		"SavedCount":    base.SavedCount,
		"RatingLevels":  base.RatingLevels,
		"ActiveRating":  base.ActiveRating,
		"Categories":    cats,
	}
	s.renderTemplate(w, "categories.html", data)
}

// taggerRow is the per-template render shape for one row of the
// Auto-Tagger settings table. It unifies installed taggers and catalog
// ghosts so the template iterates a single list. Supported rows (i.e.
// in the catalog) carry precomputed host + docker install snippets so
// the Instructions cell can open a per-row dialog without the template
// touching shell quoting.
type taggerRow struct {
	Name                string
	Description         string
	Available           bool
	Reason              string
	Enabled             bool
	ConfidenceThreshold float64
	Installed           bool
	Supported           bool
	HostCommand         string
	DockerCommand       string
}

func (s *Server) settingsHandler(w http.ResponseWriter, r *http.Request) {
	base := s.base(r, "settings", "Settings - Monbooru")
	s.disableUnavailableTaggers()
	taggers := tagger.AvailableTaggers(s.cfg)
	// Build a unified row list: catalog-backed rows (installed-and-in-catalog
	// plus catalog entries whose subfolder isn't on disk yet) come first as
	// "supported"; user-only installed taggers (not in the catalog) come last
	// as "unsupported". The template renders a separator between the two
	// groups when both are non-empty.
	modelPath := s.cfg.Paths.ModelPath
	catalog := tagger.LoadCatalog(modelPath)
	catalogByName := map[string]tagger.CatalogEntry{}
	for _, e := range catalog {
		catalogByName[e.Name] = e
	}
	taggerNames := map[string]bool{}
	for _, t := range taggers {
		taggerNames[t.Name] = true
	}
	var supportedRows, unsupportedRows []taggerRow
	for _, t := range taggers {
		row := taggerRow{
			Name:                t.Name,
			Available:           t.Available,
			Reason:              t.Reason,
			Enabled:             t.Enabled,
			ConfidenceThreshold: t.ConfidenceThreshold,
			Installed:           true,
		}
		if e, ok := catalogByName[t.Name]; ok {
			row.Supported = true
			row.Description = e.Description
			row.HostCommand = e.HostCommand()
			row.DockerCommand = e.DockerCommand("monbooru")
			supportedRows = append(supportedRows, row)
		} else {
			unsupportedRows = append(unsupportedRows, row)
		}
	}
	for _, e := range catalog {
		if taggerNames[e.Name] {
			continue
		}
		supportedRows = append(supportedRows, taggerRow{
			Name:          e.Name,
			Description:   e.Description,
			Supported:     true,
			HostCommand:   e.HostCommand(),
			DockerCommand: e.DockerCommand("monbooru"),
		})
	}
	taggerRows := append(supportedRows, unsupportedRows...)
	data := map[string]any{
		"Title":            base.Title,
		"ActiveNav":        base.ActiveNav,
		"CSRFToken":        base.CSRFToken,
		"AuthEnabled":      base.AuthEnabled,
		"Degraded":         base.Degraded,
		"Version":          base.Version,
		"RepoURL":          base.RepoURL,
		"Variant":          base.Variant,
		"CustomCSS":        base.CustomCSS,
		"ActiveGallery":    base.ActiveGallery,
		"Galleries":        s.galleryRows(),
		"VisibleCount":     base.VisibleCount,
		"TagCount":         base.TagCount,
		"SavedCount":       base.SavedCount,
		"RatingLevels":     base.RatingLevels,
		"ActiveRating":     base.ActiveRating,
		"Config":           s.cfg,
		"Taggers":          taggers,
		"TaggerRows":       taggerRows,
		"SupportedCount":   len(supportedRows),
		"UnsupportedCount": len(unsupportedRows),
		"ScheduleStatus":   s.ScheduleStatus(),
	}
	s.renderTemplate(w, "settings.html", data)
}

func (s *Server) settingsSchedulePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	timeVal := strings.TrimSpace(r.FormValue("time"))
	if timeVal == "" {
		timeVal = "01:00"
	}
	if err := config.ValidateScheduleTime(timeVal); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">%s</div>`, html.EscapeString(err.Error()))
		return
	}
	s.cfgMu.Lock()
	s.cfg.Schedule.Time = timeVal
	s.cfg.Schedule.SyncGallery = r.FormValue("sync_gallery") == "on"
	s.cfg.Schedule.RemoveOrphans = r.FormValue("remove_orphans") == "on"
	s.cfg.Schedule.RunAutoTaggers = r.FormValue("run_auto_taggers") == "on"
	s.cfg.Schedule.MergeGeneralTags = r.FormValue("merge_general_tags") == "on"
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	select {
	case s.schedReload <- struct{}{}:
	default:
	}
	logx.Infof("settings: schedule updated (time=%s)", timeVal)
	w.Write([]byte(`<div class="flash flash-ok">Saved.</div>`))
}

// settingsGeneralPost saves the unified Settings → General form: the Files
// subsection (watch toggle + max file size) and the UI subsection (page
// size). One submit covers both so the page has a single Save button.
func (s *Server) settingsGeneralPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.cfgMu.Lock()
	s.cfg.Gallery.WatchEnabled = r.FormValue("watch_enabled") == "on"
	if n, err := strconv.Atoi(r.FormValue("max_file_size_mb")); err == nil {
		s.cfg.Gallery.MaxFileSizeMB = n
	}
	if n, err := strconv.Atoi(r.FormValue("page_size")); err == nil && n > 0 {
		s.cfg.UI.PageSize = n
	}
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: general updated")
	w.Write([]byte(`<div class="flash flash-ok">Saved.</div>`))
}

func (s *Server) settingsTaggerPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}

	// Parse per-tagger form fields: tagger_<name>_enabled and tagger_<name>_threshold.
	// The list of known tagger names is sent as hidden "tagger_names" fields so
	// unchecked checkboxes still produce a disabled entry.
	names := r.Form["tagger_names"]

	newUseCUDA := r.FormValue("use_cuda") == "on"
	// Probe CUDA before persisting the enable so the user sees any library/GPU
	// issue immediately instead of waiting for a tagger run to fail. ORT env
	// init is not re-entrant so refuse while a tagger job is holding it.
	if newUseCUDA && !s.cfg.Tagger.UseCUDA {
		if s.jobs.IsRunning() {
			w.Write([]byte(`<div class="flash flash-err">A job is running; try again when it finishes.</div>`))
			return
		}
		if err := tagger.CheckCUDAAvailable(); err != nil {
			fmt.Fprintf(w, `<div class="flash flash-err">Cannot enable GPU: %s</div>`, html.EscapeString(err.Error()))
			return
		}
	}

	s.cfgMu.Lock()

	cudaChanged := s.cfg.Tagger.UseCUDA != newUseCUDA
	s.cfg.Tagger.UseCUDA = newUseCUDA
	if n, err := strconv.Atoi(r.FormValue("parallel")); err == nil && n >= 1 {
		s.cfg.Tagger.Parallel = n
	}

	// Rebuild the taggers slice from submitted names so deleted subfolders drop out.
	// Seed from DiscoverTaggers so subfolders present on disk but not yet in TOML
	// keep their discovery defaults (Enabled=true) instead of collapsing to the
	// zero value when the form is saved.
	byName := map[string]config.TaggerInstance{}
	for _, t := range tagger.DiscoverTaggers(s.cfg) {
		byName[t.Name] = t.TaggerInstance
	}
	newList := make([]config.TaggerInstance, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		t := byName[name]
		t.Name = name
		// Enabled state is owned by the per-row Enable/Disable endpoints,
		// not the Save form, so preserve whatever the config already holds.
		if f, err := strconv.ParseFloat(r.FormValue("tagger_"+name+"_threshold"), 64); err == nil {
			t.ConfidenceThreshold = f
		}
		newList = append(newList, t)
	}
	s.cfg.Tagger.Taggers = newList
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	// Drop the cached ORT session so the freed RAM is visible immediately
	// rather than after idle_release_after_minutes elapses.
	if cudaChanged {
		tagger.ReleaseAll()
	}
	logx.Infof("settings: tagger updated (%d taggers, use_cuda=%t)", len(newList), s.cfg.Tagger.UseCUDA)
	w.Write([]byte(`<div class="flash flash-ok">Saved.</div>`))
	s.renderTemplate(w, "partials/tagger_mode_badge.html", map[string]any{
		"UseCUDA": s.cfg.Tagger.UseCUDA,
		"OOB":     true,
	})
}

// settingsTaggerEnablePost flips one tagger's enabled flag to true without
// going through the full tagger form. Mirrors settingsTaggerDisablePost.
func (s *Server) settingsTaggerEnablePost(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	s.cfgMu.Lock()
	found := false
	for i, t := range s.cfg.Tagger.Taggers {
		if t.Name == name {
			s.cfg.Tagger.Taggers[i].Enabled = true
			found = true
			break
		}
	}
	if !found {
		s.cfg.Tagger.Taggers = append(s.cfg.Tagger.Taggers, config.TaggerInstance{
			Name:                name,
			Enabled:             true,
			ConfidenceThreshold: tagger.DefaultConfidenceThreshold,
		})
	}
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: tagger %q enabled", name)
	w.Header().Set("HX-Refresh", "true")
	w.Write([]byte(`<div class="flash flash-ok">Tagger ` + html.EscapeString(name) + ` enabled.</div>`))
}

// settingsTaggerDisablePost flips one tagger's enabled flag to false without
// going through the full tagger form. An HX-Refresh header re-renders the
// settings page so the row's enabled state and Actions column reflect the
// new state.
func (s *Server) settingsTaggerDisablePost(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	s.cfgMu.Lock()
	found := false
	for i, t := range s.cfg.Tagger.Taggers {
		if t.Name == name {
			s.cfg.Tagger.Taggers[i].Enabled = false
			found = true
			break
		}
	}
	if !found {
		// Tagger existed on disk but had no TOML entry yet - add a disabled
		// one so the preference persists.
		s.cfg.Tagger.Taggers = append(s.cfg.Tagger.Taggers, config.TaggerInstance{
			Name:                name,
			Enabled:             false,
			ConfidenceThreshold: tagger.DefaultConfidenceThreshold,
		})
	}
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: tagger %q disabled", name)
	w.Header().Set("HX-Refresh", "true")
	w.Write([]byte(`<div class="flash flash-ok">Tagger ` + html.EscapeString(name) + ` disabled.</div>`))
}

// disableUnavailableTaggers flips Enabled to false on any configured tagger
// whose model files have gone missing on disk. Persists the result so a
// re-downloaded model has to be re-enabled deliberately rather than firing
// off a half-broken job.
func (s *Server) disableUnavailableTaggers() {
	available := map[string]bool{}
	for _, t := range tagger.DiscoverTaggers(s.cfg) {
		available[t.Name] = t.Available
	}
	s.cfgMu.Lock()
	changed := false
	for i, t := range s.cfg.Tagger.Taggers {
		if t.Enabled && !available[t.Name] {
			s.cfg.Tagger.Taggers[i].Enabled = false
			changed = true
			logx.Infof("settings: auto-disabled tagger %q (files missing)", t.Name)
		}
	}
	s.cfgMu.Unlock()
	if changed {
		if err := s.saveConfig(); err != nil {
			logx.Warnf("auto-disable taggers: save config: %v", err)
		}
	}
}

// settingsTaggerDeletePost removes a tagger entry from the config and wipes
// its subfolder under paths.model_path. Refused if the tagger is currently
// enabled (the UI hides the button in that case; this is the server gate).
// The name is validated so it can't escape model_path with `..` segments.
func (s *Server) settingsTaggerDeletePost(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	s.cfgMu.Lock()
	for _, t := range s.cfg.Tagger.Taggers {
		if t.Name == name && t.Enabled {
			s.cfgMu.Unlock()
			fmt.Fprintf(w, `<div class="flash flash-err">Disable tagger %s before deleting it.</div>`, html.EscapeString(name))
			return
		}
	}
	for i, t := range s.cfg.Tagger.Taggers {
		if t.Name == name {
			s.cfg.Tagger.Taggers = append(s.cfg.Tagger.Taggers[:i], s.cfg.Tagger.Taggers[i+1:]...)
			break
		}
	}
	dir := filepath.Join(s.cfg.Paths.ModelPath, name)
	s.cfgMu.Unlock()
	if err := os.RemoveAll(dir); err != nil {
		logx.Warnf("delete tagger %q: remove %q: %v", name, dir, err)
		fmt.Fprintf(w, `<div class="flash flash-err">Removed config entry but could not delete folder: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: tagger %q deleted (folder %s removed)", name, dir)
	w.Header().Set("HX-Refresh", "true")
	w.Write([]byte(`<div class="flash flash-ok">Tagger ` + html.EscapeString(name) + ` deleted.</div>`))
}

func (s *Server) settingsPasswordPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	currentPass := r.FormValue("current_password")
	newPass := r.FormValue("new_password")
	if newPass == "" {
		w.Write([]byte(`<div class="flash flash-err">New password required.</div>`))
		return
	}
	// If a password is already set, require the current one for verification.
	if s.cfg.Auth.EnablePassword && s.cfg.Auth.PasswordHash != "" {
		if err := bcrypt.CompareHashAndPassword([]byte(s.cfg.Auth.PasswordHash), []byte(currentPass)); err != nil {
			w.Write([]byte(`<div class="flash flash-err">Current password is incorrect.</div>`))
			return
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">Error hashing password.</div>`))
		return
	}
	s.cfgMu.Lock()
	s.cfg.Auth.PasswordHash = string(hash)
	s.cfg.Auth.EnablePassword = true
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: password updated from %s", clientIP(r))
	w.Write([]byte(`<div class="flash flash-ok">Password updated.</div>`))
	s.renderAuthPasswordOOB(w, r)
}

func (s *Server) settingsTokenPost(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		logx.Errorf("generating API token: %v", err)
		w.Write([]byte(`<div class="flash flash-err">Failed to generate token.</div>`))
		return
	}
	token := fmt.Sprintf("%x", buf)
	s.cfgMu.Lock()
	s.cfg.Auth.APIToken = token
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: API token regenerated from %s", clientIP(r))
	w.Header().Set("Cache-Control", "no-store")
	s.renderTemplate(w, "partials/flash_token.html", map[string]any{"Token": token})
}

func (s *Server) pruneMissingImagesPost(w http.ResponseWriter, r *http.Request) {
	// Fetch all missing image IDs first so we can clean up tags and thumbnails.
	rows, err := s.db().Read.Query(`SELECT id FROM images WHERE is_missing = 1`)
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if scanErr := rows.Scan(&id); scanErr != nil {
			rows.Close()
			w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(scanErr.Error()) + `</div>`))
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	// Bail before running the deletes when the cursor itself errored
	// part-way through the iteration; otherwise we'd report N removed
	// against a silently-truncated id list.
	if iterErr := rows.Err(); iterErr != nil {
		w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(iterErr.Error()) + `</div>`))
		return
	}

	// Delete in chunked IN (...) batches so the whole job is a handful of
	// write transactions rather than one per missing row. The schema cascades
	// image_tags / image_paths / sd_metadata / comfyui_metadata on image
	// delete, so a single DELETE FROM images clears the dependent rows.
	const chunkSize = 500
	removed := 0
	affectedTags := map[int64]struct{}{}
	for start := 0; start < len(ids); start += chunkSize {
		end := start + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}

		tx, err := s.db().Write.Begin()
		if err != nil {
			w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		// Collect the tag IDs so we can scope the post-delete recalculation
		// instead of walking the whole tags table.
		tagRows, err := tx.Query(
			`SELECT DISTINCT tag_id FROM image_tags WHERE image_id IN (`+placeholders+`)`, args...,
		)
		if err != nil {
			tx.Rollback()
			w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		for tagRows.Next() {
			var tid int64
			if err := tagRows.Scan(&tid); err == nil {
				affectedTags[tid] = struct{}{}
			}
		}
		tagRows.Close()
		res, err := tx.Exec(`DELETE FROM images WHERE id IN (`+placeholders+`)`, args...)
		if err != nil {
			tx.Rollback()
			w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		if err := tx.Commit(); err != nil {
			w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed += int(n)
		}
		for _, id := range chunk {
			os.Remove(gallery.ThumbnailPath(s.thumbnailsPath(), id))
			os.Remove(gallery.HoverPath(s.thumbnailsPath(), id))
		}
	}
	// Reconcile usage counts only on the tags we actually touched.
	if len(affectedTags) > 0 {
		tagIDs := make([]int64, 0, len(affectedTags))
		for id := range affectedTags {
			tagIDs = append(tagIDs, id)
		}
		if err := s.tagSvc().RecalcIDs(tagIDs); err != nil {
			logx.Warnf("prune-missing recalc IDs: %v", err)
		}
	}
	if removed > 0 {
		s.Active().InvalidateCaches()
	}
	w.Write([]byte(fmt.Sprintf(`<div class="flash flash-ok">Removed %d missing image(s).</div>`, removed)))
}

func (s *Server) pruneOrphanedThumbnailsPost(w http.ResponseWriter, r *http.Request) {
	thumbDir := s.thumbnailsPath()
	entries, err := os.ReadDir(thumbDir)
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">Error reading thumbnails directory: ` + html.EscapeString(err.Error()) + `</div>`))
		return
	}

	// Load the full set of image ids once, then diff against on-disk files
	// instead of issuing one SELECT per thumbnail. A partial set here would
	// flag legit thumbnails as orphans and remove them from disk, so a
	// scan/cursor failure mid-iteration must abort cleanly.
	known := map[int64]struct{}{}
	idRows, err := s.db().Read.Query(`SELECT id FROM images`)
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	for idRows.Next() {
		var id int64
		if scanErr := idRows.Scan(&id); scanErr != nil {
			idRows.Close()
			w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(scanErr.Error()) + `</div>`))
			return
		}
		known[id] = struct{}{}
	}
	if iterErr := idRows.Err(); iterErr != nil {
		idRows.Close()
		w.Write([]byte(`<div class="flash flash-err">Error: ` + html.EscapeString(iterErr.Error()) + `</div>`))
		return
	}
	idRows.Close()

	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Parse image ID from "123.jpg" or "123_hover.webp"
		var idStr string
		if strings.HasSuffix(name, "_hover.webp") {
			idStr = strings.TrimSuffix(name, "_hover.webp")
		} else if strings.HasSuffix(name, ".jpg") {
			idStr = strings.TrimSuffix(name, ".jpg")
		} else {
			continue
		}
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		if _, ok := known[id]; ok {
			continue
		}
		if removeErr := os.Remove(filepath.Join(thumbDir, name)); removeErr == nil {
			removed++
		}
	}
	w.Write([]byte(fmt.Sprintf(`<div class="flash flash-ok">Removed %d orphaned thumbnail(s).</div>`, removed)))
}

func (s *Server) recalcTagsPost(w http.ResponseWriter, r *http.Request) {
	updated := s.tagSvc().RecalcCount()
	s.Active().InvalidateCaches()
	w.Write([]byte(fmt.Sprintf(
		`<div class="flash flash-ok">Recalculated %d tag count(s).</div>`,
		updated,
	)))
}

func (s *Server) mergeGeneralTagsPost(w http.ResponseWriter, r *http.Request) {
	merged, err := s.tagSvc().MergeGeneralIntoCategorized()
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	s.Active().InvalidateCaches()
	w.Write([]byte(fmt.Sprintf(
		`<div class="flash flash-ok">Merged %d general tag(s) into categorized counterparts.</div>`,
		merged,
	)))
}

func (s *Server) settingsRemovePasswordPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	// Require current password to disable authentication.
	currentPass := r.FormValue("current_password")
	if s.cfg.Auth.EnablePassword && s.cfg.Auth.PasswordHash != "" {
		if err := bcrypt.CompareHashAndPassword([]byte(s.cfg.Auth.PasswordHash), []byte(currentPass)); err != nil {
			w.Write([]byte(`<div class="flash flash-err">Current password is incorrect.</div>`))
			return
		}
	}
	s.cfgMu.Lock()
	s.cfg.Auth.EnablePassword = false
	s.cfg.Auth.PasswordHash = ""
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		fmt.Fprintf(w, `<div class="flash flash-err">Could not save: %s</div>`, html.EscapeString(err.Error()))
		return
	}
	logx.Infof("settings: password removed from %s", clientIP(r))
	// Invalidate all sessions so nobody is locked out of the now-open instance
	s.sessions.Clear()
	w.Write([]byte(`<div class="flash flash-ok">Password removed. Authentication is now disabled.</div>`))
	s.renderAuthPasswordOOB(w, r)
}

// renderAuthPasswordOOB writes an out-of-band swap for the password subsection
// so the "currently enabled/disabled" text and form fields reflect the latest
// auth state without requiring a page reload.
func (s *Server) renderAuthPasswordOOB(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "partials/auth_password_section.html", map[string]any{
		"AuthEnabled": s.cfg.Auth.EnablePassword,
		"CSRFToken":   s.csrfToken(sessionFromContext(r.Context())),
		"OOB":         true,
	})
}

func (s *Server) categoryCountHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	count, err := s.tagSvc().GetCategoryTagCount(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"count":%d}`, count)
}

func (s *Server) duplicatesListHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db().Read.Query(`
		SELECT i.id, i.canonical_path, ip.id as path_id, ip.path
		FROM images i
		JOIN image_paths ip ON ip.image_id = i.id AND ip.is_canonical = 0
		ORDER BY i.id, ip.id
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type aliasRow struct {
		ImageID       int64
		CanonicalPath string
		PathID        int64
		AliasPath     string
	}
	var aliases []aliasRow
	for rows.Next() {
		var a aliasRow
		if err := rows.Scan(&a.ImageID, &a.CanonicalPath, &a.PathID, &a.AliasPath); err != nil {
			logx.Warnf("duplicates list scan: %v", err)
			continue
		}
		aliases = append(aliases, a)
	}

	s.renderTemplate(w, "partials/duplicates_list.html", map[string]any{
		"Aliases": aliases,
	})
}

func (s *Server) removeDuplicatesPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}

	// Remove a specific subset when the form carries path_id values (one per
	// listed row), or every non-canonical row when the form carries
	// `all=true`. Refusing to fall through unless one of the two is set
	// keeps a stray POST with just a CSRF token from wiping the whole
	// library of alias files at once.
	selected := r.Form["path_id"]
	allFlag := r.FormValue("all") == "true"
	if len(selected) == 0 && !allFlag {
		w.Write([]byte(`<div class="flash flash-err">No duplicate paths selected.</div>`))
		return
	}

	var (
		rows *sql.Rows
		err  error
	)
	if allFlag {
		rows, err = s.db().Read.Query(`
			SELECT ip.id, ip.path
			FROM image_paths ip
			WHERE ip.is_canonical = 0
		`)
	} else {
		// Build an IN (?,?,...) query restricted to the supplied path_ids
		// that still aren't canonical - callers can't use this endpoint to
		// remove the canonical path for an image.
		placeholders := strings.Repeat("?,", len(selected))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, 0, len(selected))
		for _, s := range selected {
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				args = append(args, id)
			}
		}
		if len(args) == 0 {
			w.Write([]byte(`<div class="flash flash-err">No valid path_ids in request.</div>`))
			return
		}
		rows, err = s.db().Read.Query(
			`SELECT ip.id, ip.path FROM image_paths ip
			 WHERE ip.is_canonical = 0 AND ip.id IN (`+placeholders+`)`,
			args...,
		)
	}
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}

	type pathRow struct {
		ID   int64
		Path string
	}
	var paths []pathRow
	for rows.Next() {
		var p pathRow
		if err := rows.Scan(&p.ID, &p.Path); err != nil {
			logx.Warnf("remove duplicates scan: %v", err)
			continue
		}
		paths = append(paths, p)
	}
	rows.Close()

	removed := 0
	for _, p := range paths {
		if _, err := s.db().Write.Exec(`DELETE FROM image_paths WHERE id = ?`, p.ID); err != nil {
			logx.Warnf("remove duplicate %d: %v", p.ID, err)
			continue
		}
		if p.Path != "" {
			if err := os.Remove(p.Path); err != nil && !os.IsNotExist(err) {
				logx.Warnf("remove duplicate %q: %v", p.Path, err)
			}
		}
		removed++
	}
	w.Write([]byte(fmt.Sprintf(`<div class="flash flash-ok">Removed %d duplicate path(s).</div>`, removed)))
}

func (s *Server) rebuildThumbnailsPost(w http.ResponseWriter, r *http.Request) {
	if err := s.startRebuildThumbsJob(s.Active()); err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	w.Write([]byte(`<div class="flash flash-ok">Thumbnail rebuild started.</div>`))
}

// startRebuildThumbsJob queues a rebuild-thumbs job against the supplied
// gallery context, reading images and writing thumbnails from that gallery's
// own DB + thumbnails dir. Reused by the manual handler (active gallery) and
// the post-import hook (imported non-active gallery).
func (s *Server) startRebuildThumbsJob(cx *galleryCtx) error {
	if cx == nil || cx.DB == nil {
		return fmt.Errorf("no gallery context")
	}
	type imgRow struct {
		ID       int64
		Path     string
		FileType string
	}
	rows, err := cx.DB.Read.Query(
		`SELECT id, canonical_path, file_type FROM images WHERE is_missing = 0`)
	if err != nil {
		return err
	}
	var imgs []imgRow
	for rows.Next() {
		var img imgRow
		if err := rows.Scan(&img.ID, &img.Path, &img.FileType); err != nil {
			rows.Close()
			return err
		}
		imgs = append(imgs, img)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if err := s.jobs.Start("rebuild-thumbs"); err != nil {
		return fmt.Errorf("a job is already running")
	}
	thumbnailsPath := cx.ThumbnailsPath
	galleryName := cx.Name
	go func() {
		ctx := s.jobs.Context()
		processed := 0
		total := len(imgs)
		for _, img := range imgs {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("[%s] rebuild cancelled (%d/%d rebuilt)", galleryName, processed, total))
				return
			}
			s.jobs.Update(processed, total, fmt.Sprintf("[%s] rebuilding %d/%d", galleryName, processed, total))
			if err := gallery.Generate(img.Path, thumbnailsPath, img.ID, img.FileType); err != nil {
				logx.Warnf("rebuild thumbnail for %d: %v", img.ID, err)
			}
			processed++
		}
		s.jobs.Complete(fmt.Sprintf("[%s] rebuilt %d thumbnail(s).", galleryName, processed))
	}()
	return nil
}

func (s *Server) vacuumDBPost(w http.ResponseWriter, r *http.Request) {
	// VACUUM holds the writer for tens of seconds on a multi-GB DB. Take
	// a job slot so the status bar reflects what's running and the
	// scheduler / a concurrent user-triggered job is refused with the
	// usual "a job is already running" message instead of silently
	// queueing behind the writer.
	if err := s.jobs.Start("vacuum"); err != nil {
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	beforeSize := dbFileSize(s.dbPath())
	if _, err := s.db().Write.Exec(`VACUUM`); err != nil {
		s.jobs.Fail(err.Error())
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	// VACUUM in WAL mode writes the rebuilt pages into the -wal file, so the
	// user sees no drop in on-disk footprint until the WAL is consolidated.
	// Truncate the WAL explicitly so the reclaimed space is actually released.
	if _, err := s.db().Write.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		logx.Warnf("vacuum wal_checkpoint: %v", err)
	}
	afterSize := dbFileSize(s.dbPath())
	freed := beforeSize - afterSize
	if freed < 0 {
		freed = 0
	}
	s.jobs.Complete(fmt.Sprintf("Vacuumed (reclaimed %s).", humanBytesFmt(freed)))
	w.Write([]byte(fmt.Sprintf(
		`<div class="flash flash-ok">Database vacuumed. Reclaimed %s.</div>`, humanBytesFmt(freed),
	)))
}

// dbFileSize returns the total on-disk footprint of the SQLite database -
// the main file plus the WAL and shared-memory sidecars. A post-VACUUM
// "reclaimed N" figure that only counts the main file misleads the user
// whenever the WAL holds the bulk of the pages (common after mass deletes).
func dbFileSize(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	return total
}

// humanBytesFmt mirrors the humanBytes template helper from the router for
// use in handler responses. Kept tiny so the two formatters stay trivially
// consistent.
func humanBytesFmt(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func (s *Server) reExtractMetadataPost(w http.ResponseWriter, r *http.Request) {
	// Stream rows into a slice of lightweight structs so the DB cursor is closed
	// before the long-running goroutine starts. This avoids holding a read
	// connection open for the entire re-extraction job while keeping memory
	// usage proportional to the number of images (IDs + short paths only).
	type imgRow struct {
		ID       int64
		Path     string
		FileType string
		// Current persisted hashes; we use them to skip the rewrite when the
		// new extraction would produce the same generation_hash - most runs
		// on an unchanged library now turn into pure reads.
		sdHash    string
		comfyHash string
		source    string
	}

	rows, err := s.db().Read.Query(`
		SELECT i.id, i.canonical_path, i.file_type, i.source_type,
		       COALESCE(sm.generation_hash, ''),
		       COALESCE(cm.generation_hash, '')
		FROM images i
		LEFT JOIN sd_metadata sm ON sm.image_id = i.id
		LEFT JOIN comfyui_metadata cm ON cm.image_id = i.id
		WHERE i.is_missing = 0
	`)
	if err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	var imgs []imgRow
	for rows.Next() {
		var img imgRow
		if err := rows.Scan(&img.ID, &img.Path, &img.FileType, &img.source, &img.sdHash, &img.comfyHash); err != nil {
			logx.Warnf("re-extract scan: %v", err)
			continue
		}
		imgs = append(imgs, img)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}

	if err := s.jobs.Start("re-extract"); err != nil {
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}

	database := s.db()
	go func() {
		ctx := s.jobs.Context()
		processed := 0
		updated := 0
		total := len(imgs)
		for _, img := range imgs {
			if ctx.Err() != nil {
				s.jobs.Complete(fmt.Sprintf("re-extraction cancelled (%d/%d processed, %d updated)", processed, total, updated))
				return
			}
			s.jobs.Update(processed, total, fmt.Sprintf("Processing %d/%d…", processed, total))
			sdMeta, comfyMeta, _ := meta.Extract(img.Path, img.FileType)

			sourceType := models.SourceTypeNone
			if sdMeta != nil && comfyMeta != nil {
				sourceType = models.SourceTypeBoth
			} else if sdMeta != nil {
				sourceType = models.SourceTypeA1111
			} else if comfyMeta != nil {
				sourceType = models.SourceTypeComfyUI
			}

			newSDHash := ""
			if sdMeta != nil {
				newSDHash = sdMeta.GenerationHash
			}
			newComfyHash := ""
			if comfyMeta != nil {
				newComfyHash = comfyMeta.GenerationHash
			}
			// Skip the delete+insert churn when the new extraction lines up
			// with what the DB already holds. Any pipeline change that adds
			// or drops fields changes the generation hash, so this stays
			// responsive to real metadata schema updates.
			if newSDHash == img.sdHash && newComfyHash == img.comfyHash && sourceType == img.source {
				processed++
				continue
			}

			// Single transaction per image so a mid-flight failure can't leave
			// images.source_type updated against a half-deleted metadata table
			// or a deleted-but-not-reinserted row.
			if err := reExtractApply(ctx, database, img.ID, sourceType, sdMeta, comfyMeta); err != nil {
				logx.Warnf("re-extract image %d: %v", img.ID, err)
				processed++
				continue
			}
			processed++
			updated++
		}
		s.jobs.Complete(fmt.Sprintf("Re-extracted metadata for %d image(s) (%d updated).", processed, updated))
	}()

	w.Write([]byte(`<div class="flash flash-ok">Re-extraction started.</div>`))
}

// reExtractApply commits a re-extracted image's source_type, deletes the
// previous SD/ComfyUI rows, and reinserts whichever the parser produced.
// All four steps run in one transaction so a partial failure (writer
// contention, ctx cancellation mid-statement) never leaves the row with
// updated source_type but missing metadata.
func reExtractApply(ctx context.Context, database *db.DB, imageID int64, sourceType string, sdMeta *models.SDMetadata, comfyMeta *models.ComfyUIMetadata) error {
	tx, err := database.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE images SET source_type = ? WHERE id = ?`, sourceType, imageID); err != nil {
		return fmt.Errorf("update source_type: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sd_metadata WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("delete sd_metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM comfyui_metadata WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("delete comfyui_metadata: %w", err)
	}
	if sdMeta != nil {
		sdMeta.ImageID = imageID
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sd_metadata (image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sdMeta.ImageID, sdMeta.Prompt, sdMeta.NegativePrompt, sdMeta.Model,
			sdMeta.Seed, sdMeta.Sampler, sdMeta.Steps, sdMeta.CFGScale, sdMeta.RawParams, sdMeta.GenerationHash,
		); err != nil {
			return fmt.Errorf("insert sd_metadata: %w", err)
		}
	}
	if comfyMeta != nil {
		comfyMeta.ImageID = imageID
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO comfyui_metadata (image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			comfyMeta.ImageID, comfyMeta.Prompt, comfyMeta.ModelCheckpoint,
			comfyMeta.Seed, comfyMeta.Sampler, comfyMeta.Steps, comfyMeta.CFGScale, comfyMeta.RawWorkflow, comfyMeta.GenerationHash,
		); err != nil {
			return fmt.Errorf("insert comfyui_metadata: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Server) jobDismissPost(w http.ResponseWriter, r *http.Request) {
	s.jobs.Dismiss()
	w.WriteHeader(http.StatusNoContent)
}

// jobCancelPost aborts the running job by cancelling its context. Workers
// observing ctx.Done() wrap up and call Complete/Fail themselves.
func (s *Server) jobCancelPost(w http.ResponseWriter, r *http.Request) {
	s.jobs.Cancel()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createCategoryPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.FormValue("name")
	color := r.FormValue("color")
	if color == "" {
		color = "#888888"
	}
	if _, err := s.tagSvc().CreateCategory(name, color); err != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/categories")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/categories", http.StatusSeeOther)
}

func (s *Server) updateCategoryPatch(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	color := r.FormValue("color")
	if err := s.tagSvc().UpdateCategoryColor(id, color); err != nil {
		logx.Warnf("update category %d color: %v", id, err)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	w.Write([]byte(`<div class="flash flash-ok">Updated.</div>`))
}

func (s *Server) deleteCategoryDelete(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	action := r.FormValue("action") // "move" | "delete_all"
	if action == "" {
		action = "move"
	}
	var targetID int64
	if ts := r.FormValue("target_id"); ts != "" {
		targetID, _ = strconv.ParseInt(ts, 10, 64)
	}
	if err := s.tagSvc().DeleteCategoryMoveOrDelete(id, action, targetID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/tags")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

func (s *Server) deleteSavedSearch(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, err := s.db().Write.Exec(`DELETE FROM saved_searches WHERE id = ?`, id); err != nil {
		logx.Warnf("delete saved search %d: %v", id, err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	s.Active().InvalidateCaches()
	// 200 + empty body - HTMX outerHTML swap removes the element.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) createSavedSearch(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	query := strings.TrimSpace(r.FormValue("query"))
	if name == "" || query == "" {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">Name and query required.</div>`))
			return
		}
		http.Error(w, "name and query required", http.StatusBadRequest)
		return
	}
	// Plain INSERT so the UNIQUE(name) constraint surfaces as an error
	// instead of clobbering the existing entry. The user can delete the
	// previous saved search from the sidebar and resubmit; same idiom
	// the category and tag-name uniqueness checks use elsewhere.
	if _, err := s.db().Write.Exec(
		`INSERT INTO saved_searches (name, query) VALUES (?, ?)`, name, query,
	); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "UNIQUE") {
			msg = "A saved search named " + name + " already exists. Delete it first or pick another name."
		}
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(msg) + `</div>`))
			return
		}
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	s.Active().InvalidateCaches()
	if isHTMXRequest(r) {
		w.Write([]byte(`<div class="flash flash-ok">Saved.</div>`))
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) deleteTagHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().DeleteTag(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.Active().InvalidateCaches()
	w.WriteHeader(http.StatusNoContent)
}

// deleteTagsSearchPost deletes every tag matching the current /tags
// filter. Mirrors the gallery's /internal/delete-search: resolves the
// id set up front, kicks off a background "tag" job, and returns 202
// Accepted so the client surfaces progress via the job status bar.
func (s *Server) deleteTagsSearchPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	q := r.Form
	zeroParam := q.Get("show_zero")
	zeroOnly := zeroParam == "only"
	showZero := zeroOnly || zeroParam != "0"
	filter := s.buildTagFilter(
		q.Get("cat"), q.Get("q"), q.Get("sort"), q.Get("order"),
		q.Get("origin"), showZero, zeroOnly, 1, 0,
	)
	ids, err := s.tagSvc().ListTagIDs(filter)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.jobs.Start("tag"); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	go s.runDeleteTagsByIDs(ids)
	w.WriteHeader(http.StatusAccepted)
}

// runDeleteTagsByIDs deletes the supplied tag ids one by one, reporting
// progress through the job manager and honouring cancellation.
// DeleteTag handles cascade and usage-count cleanup per row.
func (s *Server) runDeleteTagsByIDs(ids []int64) {
	ctx := s.jobs.Context()
	total := len(ids)
	processed, deleted := 0, 0
	cancelled := false

	s.jobs.Update(0, total, fmt.Sprintf("deleting tags 0/%d…", total))
	for i, id := range ids {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		if err := s.tagSvc().DeleteTag(id); err != nil {
			logx.Warnf("delete tag %d: %v", id, err)
		} else {
			deleted++
		}
		processed = i + 1
		if processed%50 == 0 || processed == total {
			s.jobs.Update(processed, total, fmt.Sprintf("deleting tags %d/%d…", processed, total))
		}
	}

	s.Active().InvalidateCaches()
	if cancelled {
		s.jobs.Complete(fmt.Sprintf("delete tags cancelled (%d/%d processed)", processed, total))
		return
	}
	s.jobs.Complete(fmt.Sprintf("deleted %d tag(s)", deleted))
}

func (s *Server) renameTagPost(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	newName := strings.TrimSpace(r.FormValue("name"))
	if newName == "" {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">Name required.</div>`))
			return
		}
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().RenameTag(id, newName); err != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/tags")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

func (s *Server) renameCategoryPost(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	newName := strings.TrimSpace(r.FormValue("name"))
	if newName == "" {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">Name required.</div>`))
			return
		}
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().RenameCategory(id, newName); err != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/categories")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/categories", http.StatusSeeOther)
}


func (s *Server) helpHandler(w http.ResponseWriter, r *http.Request) {
	base := s.base(r, "help", "Help - Monbooru")
	data := map[string]any{
		"Title":         base.Title,
		"ActiveNav":     base.ActiveNav,
		"CSRFToken":     base.CSRFToken,
		"AuthEnabled":   base.AuthEnabled,
		"Degraded":      base.Degraded,
		"Version":       base.Version,
		"RepoURL":       base.RepoURL,
		"Variant":       base.Variant,
		"CustomCSS":     base.CustomCSS,
		"ActiveGallery": base.ActiveGallery,
		"Galleries":     base.Galleries,
		"VisibleCount":  base.VisibleCount,
		"TagCount":      base.TagCount,
		"SavedCount":    base.SavedCount,
		"RatingLevels":  base.RatingLevels,
		"ActiveRating":  base.ActiveRating,
	}
	s.renderTemplate(w, "help.html", data)
}

// notFoundHandler renders a styled 404 for any unmatched GET path. The
// mux's default behaviour is unstyled `404 page not found` text on a
// white page; routing through the standard layout keeps the user inside
// the app with a back link.
func (s *Server) notFoundHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	s.renderTemplate(w, "notfound.html", s.base(r, "", "Not found - Monbooru"))
}

func (s *Server) jobStatusHandler(w http.ResponseWriter, r *http.Request) {
	// Mark before Get so the first render of a completed state starts the
	// short post-view dismiss timer. Subsequent views don't re-arm it.
	s.jobs.MarkViewed()
	state := s.jobs.Get()
	s.renderTemplate(w, "partials/job_status.html", state)
}

func (s *Server) syncTrigger(w http.ResponseWriter, r *http.Request) {
	if cx := s.Active(); cx == nil || cx.Degraded {
		http.Error(w, "sync unavailable: gallery path is unreadable", http.StatusServiceUnavailable)
		return
	}
	if err := s.jobs.Start("sync"); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	// Snapshot the active gallery's state under the request's RLock so the
	// background goroutine is not racing a subsequent swap. The IsRunning
	// guard in SwitchGallery refuses swaps while the sync runs, so these
	// handles stay valid for the job's lifetime.
	cx := s.Active()
	database := s.db()
	galleryPath := s.galleryPath()
	thumbnailsPath := s.thumbnailsPath()
	maxFileSizeMB := s.cfg.Gallery.MaxFileSizeMB
	go func() {
		ctx := s.jobs.Context()
		result, err := gallery.Sync(ctx, database, galleryPath, thumbnailsPath, maxFileSizeMB, s.jobs.Update)
		cx.InvalidateCaches()
		if ctx.Err() != nil {
			s.jobs.Complete("sync cancelled")
			return
		}
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		s.jobs.Complete(fmt.Sprintf("%d added, %d missing, %d moved",
			result.Added, result.Removed, result.Moved))
	}()

	redirectTo := sameOriginReferer(r)
	if isHTMXRequest(r) {
		// Signal the client to reload the gallery when the job finishes.
		w.Header().Set("HX-Trigger", "syncStarted")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	http.Redirect(w, r, redirectTo, http.StatusSeeOther)
}


func (s *Server) batchDelete(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	idStrs := r.Form["ids"]

	var targets []search.DeleteTarget
	for _, idStr := range idStrs {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		t := search.DeleteTarget{ID: id}
		var isMissing int
		if err := s.db().Read.QueryRow(
			`SELECT canonical_path, folder_path, is_missing FROM images WHERE id = ?`, id,
		).Scan(&t.CanonicalPath, &t.FolderPath, &isMissing); err != nil {
			continue
		}
		t.IsMissing = isMissing == 1
		targets = append(targets, t)
	}

	s.startBulkDelete(w, targets)
}

func (s *Server) deleteSearchPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	queryStr := r.FormValue("q")

	expr, parseErr := search.Parse(queryStr)
	if parseErr != nil {
		logx.Warnf("delete-search parse: %v", parseErr)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">Could not parse search: ` +
			html.EscapeString(parseErr.Error()) + `</div>`))
		return
	}

	// Stream the matching targets off the cursor so very large result sets
	// don't allocate a second intermediate copy on top of whatever the
	// bulk-delete worker holds.
	var targets []search.DeleteTarget
	err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
		targets = append(targets, t)
		return nil
	})
	if err != nil {
		logx.Errorf("delete-search: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`<div class="flash flash-err">Search error.</div>`))
		return
	}

	s.startBulkDelete(w, targets)
}

// startBulkDelete kicks off a background delete job for the given targets and
// writes the response. The job reports progress via jobs.Manager; the client
// sees the running state in the top-right status bar.
func (s *Server) startBulkDelete(w http.ResponseWriter, targets []search.DeleteTarget) {
	if len(targets) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.jobs.Start("delete"); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	go s.runBulkDelete(targets)
	w.WriteHeader(http.StatusAccepted)
}

// runBulkDelete processes targets in chunks with one transaction per chunk.
// The images schema cascades image_tags / image_paths / sd_metadata /
// comfyui_metadata on image delete, so a single DELETE FROM images clears the
// dependent rows. Tag usage counts are reconciled at the end by a targeted
// recalc scoped to the tag IDs actually touched by the cascade (collected
// from image_tags before the DELETE), avoiding a full-table Recalc
// that would walk every tag in the library.
func (s *Server) runBulkDelete(targets []search.DeleteTarget) {
	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(targets)
	processed := 0
	folders := map[string]struct{}{}
	affectedTags := map[int64]struct{}{}
	cancelled := false

	s.jobs.Update(0, total, fmt.Sprintf("deleting 0/%d…", total))

	for start := 0; start < total; start += chunkSize {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		end := start + chunkSize
		if end > total {
			end = total
		}
		chunk := targets[start:end]

		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, t := range chunk {
			args[i] = t.ID
		}

		tx, err := s.db().Write.Begin()
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		rows, err := tx.Query(`SELECT DISTINCT tag_id FROM image_tags WHERE image_id IN (`+placeholders+`)`, args...)
		if err != nil {
			tx.Rollback()
			s.jobs.Fail(err.Error())
			return
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				tx.Rollback()
				s.jobs.Fail(err.Error())
				return
			}
			affectedTags[id] = struct{}{}
		}
		rows.Close()
		if _, err := tx.Exec(`DELETE FROM images WHERE id IN (`+placeholders+`)`, args...); err != nil {
			tx.Rollback()
			s.jobs.Fail(err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			s.jobs.Fail(err.Error())
			return
		}

		for _, t := range chunk {
			os.Remove(gallery.ThumbnailPath(s.thumbnailsPath(), t.ID))
			os.Remove(gallery.HoverPath(s.thumbnailsPath(), t.ID))
			if !t.IsMissing && t.CanonicalPath != "" {
				if err := os.Remove(t.CanonicalPath); err != nil && !os.IsNotExist(err) {
					logx.Warnf("bulk delete file %q: %v", t.CanonicalPath, err)
				}
			}
			if !t.IsMissing && t.FolderPath != "" {
				folders[t.FolderPath] = struct{}{}
			}
		}

		processed = end
		s.jobs.Update(processed, total, fmt.Sprintf("deleting %d/%d…", processed, total))
	}

	if len(affectedTags) > 0 {
		s.jobs.Update(processed, total, "reconciling tag counts…")
		ids := make([]int64, 0, len(affectedTags))
		for id := range affectedTags {
			ids = append(ids, id)
		}
		if err := s.tagSvc().RecalcIDs(ids); err != nil {
			logx.Warnf("bulk delete recalc IDs: %v", err)
		}
	}

	for fp := range folders {
		gallery.DeleteEmptyFolderIfEmpty(s.galleryPath(), fp)
	}

	if processed > 0 {
		s.Active().InvalidateCaches()
	}
	if cancelled {
		s.jobs.Complete(fmt.Sprintf("delete cancelled (%d/%d deleted)", processed, total))
		return
	}
	s.jobs.Complete(fmt.Sprintf("Deleted %d image(s).", processed))
}

// batchMove kicks off a background `move` job that relocates the selected
// image IDs into the requested folder. Collisions on filename auto-suffix via
// UniqueDestPath. The watcher suppresses its events while this job runs so
// the Rename pairs don't flap the images as missing in transit.
//
// scope=search materialises ids by streaming the search result through
// search.ExecuteForDeleteStream (same idiom as batchTag and deleteSearchPost);
// scope=selection (or empty) reads ids[] from the form.
func (s *Server) batchMove(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	targetFolder := strings.TrimSpace(r.FormValue("folder"))

	// Validate the folder once up-front so the user sees the error inline
	// rather than as a per-image log entry once the job starts.
	if _, err := gallery.ResolveSubdir(s.galleryPath(), targetFolder); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}

	scope := strings.TrimSpace(r.FormValue("scope"))
	var ids []int64
	if scope == "search" {
		expr, parseErr := search.Parse(r.FormValue("q"))
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`<div class="flash flash-err">Could not parse search: ` +
				html.EscapeString(parseErr.Error()) + `</div>`))
			return
		}
		err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
			ids = append(ids, t.ID)
			return nil
		})
		if err != nil {
			logx.Errorf("batch-move search: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`<div class="flash flash-err">Search error.</div>`))
			return
		}
	} else {
		for _, idStr := range r.Form["ids"] {
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
	}

	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if err := s.jobs.Start("move"); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	go s.runBatchMove(ids, targetFolder)
	w.WriteHeader(http.StatusAccepted)
}

// runBatchMove processes move targets one image at a time. Each MoveImage has
// its own small write txn + Rename; per-image failures are logged and counted
// but don't stop the run so a single unreadable file can't strand the rest.
// Empty source folders are cleaned up at the end, matching single-image move.
func (s *Server) runBatchMove(ids []int64, targetFolder string) {
	ctx := s.jobs.Context()
	total := len(ids)
	moved, failed := 0, 0
	cancelled := false
	// Track every observed source folder, not just successful ones. A
	// failed move can still be the last image in its source folder
	// (because earlier successful moves emptied it), and the post-loop
	// cleanup must consider those too. DeleteEmptyFolderIfEmpty is a
	// no-op on non-empty folders so over-eager calls are safe.
	observedSources := map[string]struct{}{}

	s.jobs.Update(0, total, fmt.Sprintf("moving 0/%d…", total))

	for i, id := range ids {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		res, err := gallery.MoveImage(s.db(), s.galleryPath(), id, targetFolder)
		if err != nil {
			logx.Warnf("batch move %d: %v", id, err)
			failed++
			// Pull the source folder from the row directly so we can
			// still try to clean it up. MoveImage rolls back on
			// failure but the row's folder_path is still known.
			var oldFolder string
			_ = s.db().Read.QueryRow(`SELECT folder_path FROM images WHERE id = ?`, id).Scan(&oldFolder)
			if oldFolder != "" {
				observedSources[oldFolder] = struct{}{}
			}
			continue
		}
		if res.OldFolderPath != res.NewFolderPath && res.OldFolderPath != "" {
			observedSources[res.OldFolderPath] = struct{}{}
		}
		moved++
		if (i+1)%25 == 0 || i == total-1 {
			s.jobs.Update(i+1, total, fmt.Sprintf("moving %d/%d…", i+1, total))
		}
	}

	for fp := range observedSources {
		gallery.DeleteEmptyFolderIfEmpty(s.galleryPath(), fp)
	}

	if moved > 0 {
		s.Active().InvalidateCaches()
	}
	if cancelled {
		s.jobs.Complete(fmt.Sprintf("move cancelled (%d/%d moved)", moved, total))
		return
	}
	summary := fmt.Sprintf("Moved %d image(s).", moved)
	if failed > 0 {
		summary = fmt.Sprintf("Moved %d image(s), %d failed.", moved, failed)
	}
	s.jobs.Complete(summary)
}

// batchTag kicks off a background `tag` job that adds (op=add) or removes
// (op=remove) a tag set across either every image in the current search
// (scope=search) or just the checked ids (scope=selection). The dialogs in
// gallery.html post the tags string verbatim (parsed server-side so
// category:name and quoted spans behave identically to the detail-page
// tag input). The op=remove path is the "specific tags by name" branch of
// #batch-strip-dialog; the bulk user/auto/all branches go through batchStrip.
func (s *Server) batchTag(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	op := strings.TrimSpace(r.FormValue("op"))
	if op != "add" && op != "remove" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">op must be add or remove</div>`))
		return
	}
	tagInput := strings.TrimSpace(r.FormValue("tags"))
	if tagInput == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">No tags provided.</div>`))
		return
	}
	catTags, parseErrMsg := s.parseTagInput(tagInput)
	if parseErrMsg != "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(parseErrMsg) + `</div>`))
		return
	}
	if len(catTags) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">No tags to apply.</div>`))
		return
	}

	scope := strings.TrimSpace(r.FormValue("scope"))
	var ids []int64
	switch scope {
	case "selection":
		for _, idStr := range r.Form["ids"] {
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
	case "search":
		expr, parseErr := search.Parse(r.FormValue("q"))
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`<div class="flash flash-err">Could not parse search: ` +
				html.EscapeString(parseErr.Error()) + `</div>`))
			return
		}
		// ExecuteForDeleteStream is just "iterate matching image ids"; reuse
		// it so the search → ids materialisation is identical to delete-all.
		err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
			ids = append(ids, t.ID)
			return nil
		})
		if err != nil {
			logx.Errorf("batch-tag search: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`<div class="flash flash-err">Search error.</div>`))
			return
		}
	default:
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">scope must be search or selection</div>`))
		return
	}

	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.jobs.Start("tag"); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	go s.runBatchTag(ids, op, catTags)
	w.WriteHeader(http.StatusAccepted)
}

// runBatchTag resolves each (catID, name) token to a tag id once up front
// (creating new tags on add, looking up only existing ones on remove) and
// applies the resolved set to every image in turn. Cancellable via the
// shared job context, identical to runBulkDelete's pattern.
func (s *Server) runBatchTag(ids []int64, op string, catTags []catTag) {
	type resolvedTag struct {
		id   int64
		name string
	}
	var resolved []resolvedTag
	if op == "add" {
		for _, ct := range catTags {
			t, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
			if err != nil {
				logx.Warnf("batch-tag get-or-create %q: %v", ct.name, err)
				continue
			}
			resolved = append(resolved, resolvedTag{t.ID, t.Name})
		}
	} else {
		for _, ct := range catTags {
			var id int64
			err := s.db().Read.QueryRow(
				`SELECT id FROM tags WHERE name = ? AND category_id = ?`, ct.name, ct.catID,
			).Scan(&id)
			if err != nil {
				continue // unknown tag; nothing to remove
			}
			resolved = append(resolved, resolvedTag{id, ct.name})
		}
	}
	if len(resolved) == 0 {
		s.jobs.Complete(fmt.Sprintf("nothing to %s (no matching tags)", op))
		return
	}

	label, summary := "tagging", "Tagged"
	if op == "remove" {
		label, summary = "untagging", "Untagged"
	}

	ctx := s.jobs.Context()
	total := len(ids)
	processed, applied := 0, 0
	cancelled := false
	affectedTags := map[int64]struct{}{}
	for _, t := range resolved {
		affectedTags[t.id] = struct{}{}
	}

	s.jobs.Update(0, total, fmt.Sprintf("%s 0/%d…", label, total))
	for i, id := range ids {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		for _, t := range resolved {
			if op == "add" {
				added, _, err := s.tagSvc().AddTagToImageReportingDup(id, t.id, false, nil, "")
				if err != nil {
					logx.Warnf("batch-tag add %d/%d: %v", id, t.id, err)
					continue
				}
				if added {
					applied++
				}
			} else {
				if err := s.tagSvc().RemoveTagFromImage(id, t.id); err != nil {
					logx.Warnf("batch-tag remove %d/%d: %v", id, t.id, err)
					continue
				}
				applied++
			}
		}
		processed = i + 1
		if processed%50 == 0 || processed == total {
			s.jobs.Update(processed, total, fmt.Sprintf("%s %d/%d…", label, processed, total))
		}
	}

	if len(affectedTags) > 0 {
		tagIDs := make([]int64, 0, len(affectedTags))
		for id := range affectedTags {
			tagIDs = append(tagIDs, id)
		}
		if err := s.tagSvc().RecalcIDs(tagIDs); err != nil {
			logx.Warnf("batch-tag recalc IDs: %v", err)
		}
	}
	s.Active().InvalidateCaches()

	if cancelled {
		s.jobs.Complete(fmt.Sprintf("%s cancelled (%d/%d processed)", label, processed, total))
		return
	}
	s.jobs.Complete(fmt.Sprintf("%s %d image(s) (%d row change(s)).", summary, processed, applied))
}

// batchStrip kicks off a background `tag` job that strips tags by category
// (mode=user|auto|all) across either every image in the current search
// (scope=search) or the checked ids (scope=selection). Mirrors batchTag's
// scope dispatch; the per-mode predicate decides which image_tags rows the
// chunked DELETE in runBatchStrip touches. When mode=auto and tagger_name is
// set, the strip is further scoped to that tagger's output rows.
func (s *Server) batchStrip(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	mode := strings.TrimSpace(r.FormValue("mode"))
	switch mode {
	case "user", "auto", "all":
	default:
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">mode must be user, auto, or all</div>`))
		return
	}
	taggerName := strings.TrimSpace(r.FormValue("tagger_name"))
	if taggerName != "" && mode != "auto" {
		// tagger_name only narrows mode=auto; user/all carry no tagger_name
		// concept. Reject silently to keep the predicate composition simple.
		taggerName = ""
	}

	scope := strings.TrimSpace(r.FormValue("scope"))
	var ids []int64
	switch scope {
	case "selection":
		for _, idStr := range r.Form["ids"] {
			if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
	case "search":
		expr, parseErr := search.Parse(r.FormValue("q"))
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`<div class="flash flash-err">Could not parse search: ` +
				html.EscapeString(parseErr.Error()) + `</div>`))
			return
		}
		err := search.ExecuteForDeleteStream(s.db(), expr, func(t search.DeleteTarget) error {
			ids = append(ids, t.ID)
			return nil
		})
		if err != nil {
			logx.Errorf("batch-strip search: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`<div class="flash flash-err">Search error.</div>`))
			return
		}
	default:
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">scope must be search or selection</div>`))
		return
	}

	if len(ids) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.jobs.Start("tag"); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}
	go s.runBatchStrip(ids, mode, taggerName)
	w.WriteHeader(http.StatusAccepted)
}

// runBatchStrip processes targets in chunks of 500 with one transaction per
// chunk. The per-chunk pattern collects the distinct touched tag_ids before
// the DELETE so the post-pass RecalcIDs is scoped to the tags that
// actually changed (mirrors runBulkDelete). modePredicate narrows the strip:
//
//	user → AND is_auto = 0
//	auto → AND is_auto = 1                  (+ AND tagger_name = ? when scoped)
//	all  → (no extra predicate)
func (s *Server) runBatchStrip(ids []int64, mode, taggerName string) {
	var modePredicate, label, summary string
	var extraArgs []any
	switch mode {
	case "user":
		modePredicate = ` AND is_auto = 0`
		label, summary = "removing user tags", "Removed user tags from"
	case "auto":
		modePredicate = ` AND is_auto = 1`
		if taggerName != "" {
			modePredicate += ` AND tagger_name = ?`
			extraArgs = append(extraArgs, taggerName)
			label = fmt.Sprintf("removing %s auto-tags", taggerName)
			summary = fmt.Sprintf("Removed %s auto-tags from", taggerName)
		} else {
			label, summary = "removing auto-tags", "Removed auto-tags from"
		}
	case "all":
		modePredicate = ``
		label, summary = "removing tags", "Removed all tags from"
	}

	ctx := s.jobs.Context()
	const chunkSize = 500
	total := len(ids)
	processed := 0
	affectedTags := map[int64]struct{}{}
	cancelled := false

	s.jobs.Update(0, total, fmt.Sprintf("%s 0/%d…", label, total))

	for start := 0; start < total; start += chunkSize {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		end := start + chunkSize
		if end > total {
			end = total
		}
		chunk := ids[start:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, 0, len(chunk)+len(extraArgs))
		for _, id := range chunk {
			args = append(args, id)
		}
		args = append(args, extraArgs...)

		tx, err := s.db().Write.Begin()
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		rows, err := tx.Query(
			`SELECT DISTINCT tag_id FROM image_tags WHERE image_id IN (`+placeholders+`)`+modePredicate, args...)
		if err != nil {
			tx.Rollback()
			s.jobs.Fail(err.Error())
			return
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				tx.Rollback()
				s.jobs.Fail(err.Error())
				return
			}
			affectedTags[id] = struct{}{}
		}
		rows.Close()
		if _, err := tx.Exec(
			`DELETE FROM image_tags WHERE image_id IN (`+placeholders+`)`+modePredicate, args...); err != nil {
			tx.Rollback()
			s.jobs.Fail(err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			s.jobs.Fail(err.Error())
			return
		}

		processed = end
		s.jobs.Update(processed, total, fmt.Sprintf("%s %d/%d…", label, processed, total))
	}

	if len(affectedTags) > 0 {
		s.jobs.Update(processed, total, "reconciling tag counts…")
		tagIDs := make([]int64, 0, len(affectedTags))
		for id := range affectedTags {
			tagIDs = append(tagIDs, id)
		}
		if err := s.tagSvc().RecalcIDs(tagIDs); err != nil {
			logx.Warnf("batch-strip recalc IDs: %v", err)
		}
	}
	s.Active().InvalidateCaches()

	if cancelled {
		s.jobs.Complete(fmt.Sprintf("%s cancelled (%d/%d processed)", label, processed, total))
		return
	}
	s.jobs.Complete(fmt.Sprintf("%s %d image(s).", summary, processed))
}

// moveImage relocates the one image at {id} into the requested folder. A `move`
// job is used even for single-image moves to reuse the watcher suppression
// pattern from batch moves; the job is brief and auto-dismisses like any other.
func (s *Server) moveImage(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	targetFolder := strings.TrimSpace(r.FormValue("folder"))

	if err := s.jobs.Start("move"); err != nil {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`<div class="flash flash-err">A job is already running.</div>`))
		return
	}

	res, moveErr := gallery.MoveImage(s.db(), s.galleryPath(), id, targetFolder)
	if moveErr != nil {
		s.jobs.Fail(moveErr.Error())
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(moveErr.Error()) + `</div>`))
		return
	}
	if res.OldFolderPath != res.NewFolderPath && res.OldFolderPath != "" {
		gallery.DeleteEmptyFolderIfEmpty(s.galleryPath(), res.OldFolderPath)
	}
	s.Active().InvalidateCaches()
	s.jobs.Complete("Moved image.")

	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/images/"+idStr)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/images/"+idStr, http.StatusSeeOther)
}

// foldersSuggest returns up to 10 existing folder paths whose name or leading
// segments match the typed prefix. Drives the autocomplete dropdown on the
// move dialogs. Root (empty folder_path) is excluded from suggestions because it
// maps to an empty input anyway.
func (s *Server) foldersSuggest(w http.ResponseWriter, r *http.Request) {
	prefix := strings.TrimSpace(r.URL.Query().Get("prefix"))
	rows, err := s.db().Read.Query(
		`SELECT DISTINCT folder_path FROM images
		 WHERE is_missing = 0 AND folder_path != '' AND folder_path LIKE ?
		 ORDER BY folder_path LIMIT 10`,
		prefix+"%",
	)
	if err != nil {
		logx.Warnf("folders suggest: %v", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer rows.Close()
	var folders []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			continue
		}
		folders = append(folders, fp)
	}
	if len(folders) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.renderTemplate(w, "partials/folder_suggest.html", map[string]any{
		"Folders": folders,
	})
}

func (s *Server) deleteFolderPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	folderPath := r.FormValue("folder")

	if folderPath == "" {
		http.Error(w, "invalid folder path", http.StatusBadRequest)
		return
	}

	// Reuse the gallery-root validator from the upload path: filepath.Rel
	// rejects sibling directories that share the gallery prefix (e.g.
	// `/data/gallery_backup`) without false-positiving on `foo..bar`.
	absPath, err := gallery.ResolveSubdir(s.galleryPath(), folderPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := os.Remove(absPath); err != nil {
		// Treat "already gone" as success so a stale UI can re-issue the
		// delete without an error toast. ENOTEMPTY (raised by Linux when
		// the directory still has children) maps to the same 409 the UI
		// already surfaces. Anything else is a real failure - permission
		// denied, busy, etc. - and must not silently masquerade as a
		// successful redirect.
		switch {
		case os.IsNotExist(err):
			// nothing to do - fall through to the success redirect
		case errors.Is(err, syscall.ENOTEMPTY):
			http.Error(w, "directory not empty", http.StatusConflict)
			return
		default:
			logx.Warnf("delete folder %q: %v", absPath, err)
			http.Error(w, "could not delete folder: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", "/")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) tagSuggest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// Accept the input's value however it arrives: q=, tag=, or canonical_id=
	prefix := q.Get("q")
	if prefix == "" {
		prefix = q.Get("tag")
	}
	if prefix == "" {
		prefix = q.Get("canonical_id")
	}

	// If the prefix contains "category:name" and the prefix is a real
	// category, filter by category. Otherwise suggest literal tags whose
	// full name matches the raw input (so tags like "nier:automata" still
	// surface while the user types).
	var catName, tagPrefix string
	if idx := strings.Index(prefix, ":"); idx > 0 && s.categoryExists(prefix[:idx]) {
		catName = prefix[:idx]
		tagPrefix = prefix[idx+1:]
	} else {
		tagPrefix = prefix
	}

	var suggestions []models.Tag
	if catName != "" {
		suggestions, _ = s.tagSvc().SuggestTagsInCategory(tagPrefix, catName, 10)
	} else {
		suggestions, _ = s.tagSvc().SuggestTags(tagPrefix, 10)
	}

	// Attribute each suggestion with its category so selecting a non-general
	// tag adds it in the right category on submit.
	if catName != "" {
		for i := range suggestions {
			suggestions[i].Name = catName + ":" + suggestions[i].Name
		}
	} else {
		for i := range suggestions {
			if suggestions[i].CategoryName != "" && suggestions[i].CategoryName != "general" {
				suggestions[i].Name = suggestions[i].CategoryName + ":" + suggestions[i].Name
			}
		}
	}

	s.renderTemplate(w, "partials/tag_suggest.html", map[string]any{
		"Suggestions": suggestions,
	})
}

// searchSuggestRow is the render shape of the search-bar autocomplete
// dropdown. Tag rows leave Category empty and the template falls through
// to the count column; `system:` cheat-sheet rows set Category to
// "system" and the template suppresses the count. Description, when
// non-empty, renders just left of the category column as a short
// English label of what the row does.
type searchSuggestRow struct {
	Name          string
	CategoryColor string
	Category      string
	Description   string
	UsageCount    int
}

func (s *Server) searchSuggest(w http.ResponseWriter, r *http.Request) {
	// Pin the swap target server-side. When an auto-refresh fires concurrently
	// with the debounced input request, htmx has been observed to resolve the
	// input's hx-target to the form-inherited #gallery-grid, which lands the
	// dropdown inside the grid with no way to dismiss it. HX-Retarget forces
	// the response back onto #search-suggest regardless of what the client
	// computed at request time.
	w.Header().Set("HX-Retarget", "#search-suggest")
	w.Header().Set("HX-Reswap", "innerHTML")

	input := r.URL.Query().Get("q")
	// Split the input: everything except the last word is the "context"
	// that must also match, and the last word is the prefix being typed.
	// The last word has its leading "-" stripped so the suggestion list works
	// while the user is still typing the negated tag.
	words := strings.Fields(input)
	prefix := ""
	var catFilter string // category name if user typed "catname:prefix"
	var contextTokens []string
	if len(words) > 0 {
		last := words[len(words)-1]
		contextTokens = words[:len(words)-1]
		last = strings.TrimPrefix(last, "-")
		// system: hijacks the suggest endpoint to surface the query
		// language itself - the keywords, operators, and closed-vocabulary
		// values - without the user leaving the search bar. "system" is
		// reserved at the category layer, so the categoryExists branch
		// below cannot reach this name.
		if rest, ok := strings.CutPrefix(last, "system:"); ok {
			s.renderSystemSuggest(w, rest)
			return
		}
		if colonIdx := strings.IndexByte(last, ':'); colonIdx >= 0 {
			key := strings.ToLower(last[:colonIdx])
			val := last[colonIdx+1:]
			// Filter keyword: surface the level-2 hint - operators for
			// date/width/height, closed-vocabulary values for
			// fav/source/rating/etc., live category names for cat: - so
			// the dropdown helps the user the same way `system:<key>:`
			// would. Avoids forcing the user to remember the cheat-sheet
			// trigger after they've already committed to the filter.
			if searchkw.IsKeyword(key) {
				rows := s.systemSuggestLevel2(key, strings.ToLower(val))
				if len(rows) == 0 {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				s.renderTemplate(w, "partials/search_suggest.html", map[string]any{
					"Suggestions": rows,
				})
				return
			}
			// Category-qualified only when the prefix actually names a
			// category; otherwise suggest literal tags that match the
			// whole "key:val" string (e.g. "nier:aut..." → "nier:automata").
			if colonIdx > 0 && s.categoryExists(key) {
				catFilter = key
				prefix = val
			} else {
				prefix = last
			}
		} else {
			prefix = last
		}
	}
	if prefix == "" && catFilter == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Parse the preceding tokens as a query. Empty context → expr is nil and
	// the combination filter degrades to a plain global-usage suggestion.
	contextExpr, _ := search.Parse(strings.Join(contextTokens, " "))

	suggestions, _ := search.SuggestTagsWithFilter(s.db(), contextExpr, prefix, catFilter, 10)

	// Prefix non-general tags (or category-qualified searches) so clicking a
	// suggestion appends the correct token to the search bar.
	for i := range suggestions {
		if catFilter != "" {
			suggestions[i].Name = catFilter + ":" + suggestions[i].Name
		} else if suggestions[i].CategoryName != "" && suggestions[i].CategoryName != "general" {
			suggestions[i].Name = suggestions[i].CategoryName + ":" + suggestions[i].Name
		}
	}

	// Drop suggestions whose formatted name is already present in the search
	// bar - otherwise typing a partial tag that overlaps an existing one would
	// re-suggest what the user already picked.
	if alreadyTyped := alreadyTypedTags(contextTokens); len(alreadyTyped) > 0 {
		out := suggestions[:0]
		for _, sug := range suggestions {
			if _, ok := alreadyTyped[sug.Name]; ok {
				continue
			}
			out = append(out, sug)
		}
		suggestions = out
	}

	rows := make([]searchSuggestRow, len(suggestions))
	for i, t := range suggestions {
		rows[i] = searchSuggestRow{
			Name:          t.Name,
			CategoryColor: t.CategoryColor,
			UsageCount:    t.UsageCount,
		}
	}
	s.renderTemplate(w, "partials/search_suggest.html", map[string]any{
		"Suggestions": rows,
	})
}

// renderSystemSuggest emits cheat-sheet rows for the search-bar's
// `system:` namespace. rest is what follows "system:" in the user's
// last word. Without an inner colon the level-1 list surfaces every
// real prefix (filter keywords plus existing tag categories); with an
// inner colon the per-keyword level-2 list takes over (static operators
// or values for filter keywords, live tags for category prefixes).
func (s *Server) renderSystemSuggest(w http.ResponseWriter, rest string) {
	var rows []searchSuggestRow
	if colonIdx := strings.IndexByte(rest, ':'); colonIdx >= 0 {
		key := strings.ToLower(rest[:colonIdx])
		valPrefix := strings.ToLower(rest[colonIdx+1:])
		rows = s.systemSuggestLevel2(key, valPrefix)
	} else {
		rows = s.systemSuggestLevel1(strings.ToLower(rest))
	}
	if len(rows) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.renderTemplate(w, "partials/search_suggest.html", map[string]any{
		"Suggestions": rows,
	})
}

// systemSuggestLevel1 lists every prefix the user can type to start a
// `key:value` search token: the search-filter keywords plus every
// existing tag category. A category whose name doubles as a filter
// keyword (rating: is both) is folded into the keyword row to avoid
// duplicate dropdown entries.
func (s *Server) systemSuggestLevel1(prefix string) []searchSuggestRow {
	var rows []searchSuggestRow
	for _, kw := range searchkw.Keywords {
		if !strings.HasPrefix(kw, prefix) {
			continue
		}
		rows = append(rows, searchSuggestRow{
			Name:        kw + ":",
			Category:    "system",
			Description: searchkw.Descriptions[kw],
		})
	}
	for _, name := range s.systemCategoryNames() {
		if searchkw.IsKeyword(name) {
			continue
		}
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rows = append(rows, searchSuggestRow{
			Name:        name + ":",
			Category:    "system",
			Description: "tag category",
		})
	}
	return rows
}

func (s *Server) systemSuggestLevel2(key, valPrefix string) []searchSuggestRow {
	if key == "cat" {
		var rows []searchSuggestRow
		for _, name := range s.systemCategoryNames() {
			if !strings.HasPrefix(name, valPrefix) {
				continue
			}
			rows = append(rows, searchSuggestRow{
				Name:     "cat:" + name,
				Category: "system",
			})
			if len(rows) >= 10 {
				break
			}
		}
		return rows
	}
	if expansions, ok := searchkw.Expansions[key]; ok {
		descs := searchkw.ExpansionDescriptions[key]
		var rows []searchSuggestRow
		for _, exp := range expansions {
			if !strings.HasPrefix(exp, valPrefix) {
				continue
			}
			rows = append(rows, searchSuggestRow{
				Name:        key + ":" + exp,
				Category:    "system",
				Description: descs[exp],
			})
		}
		return rows
	}
	// Filter keyword without a static expansion (folder, folderonly,
	// generated): no level-2 hint - the user types the value freeform.
	if searchkw.IsKeyword(key) {
		return nil
	}
	// Real category at level 2: list tags in that category, mirroring
	// the existing `<category>:<prefix>` autocomplete path. These rows
	// wear the category color and a usage count, not the dim "system"
	// label, since they're real data, not a static hint.
	if s.categoryExists(key) {
		suggestions, _ := search.SuggestTagsWithFilter(s.db(), nil, valPrefix, key, 10)
		rows := make([]searchSuggestRow, 0, len(suggestions))
		for _, t := range suggestions {
			rows = append(rows, searchSuggestRow{
				Name:          key + ":" + t.Name,
				CategoryColor: t.CategoryColor,
				UsageCount:    t.UsageCount,
			})
		}
		return rows
	}
	return nil
}

// systemCategoryNames pulls the live category list once per request.
// tag_categories is small (~9 builtin plus a handful of user rows) so
// it's cheaper to read all and filter in Go than to run a LIKE per
// keystroke and worry about escaping underscored names.
func (s *Server) systemCategoryNames() []string {
	d := s.db()
	if d == nil {
		return nil
	}
	dbrows, err := d.Read.Query(`SELECT name FROM tag_categories ORDER BY name`)
	if err != nil {
		return nil
	}
	defer dbrows.Close()
	var out []string
	for dbrows.Next() {
		var name string
		if err := dbrows.Scan(&name); err != nil {
			return out
		}
		out = append(out, name)
	}
	return out
}

// alreadyTypedTags normalizes the preceding search-bar tokens into the same
// shape as formatted suggestion names (plain "tag" or "category:tag") so the
// suggest filter can drop tags the user has already committed. Filter keywords
// (fav:true, folder:..., etc.) are skipped because they aren't tag names and
// would never match a suggestion anyway.
func alreadyTypedTags(contextTokens []string) map[string]struct{} {
	set := make(map[string]struct{}, len(contextTokens))
	for _, tok := range contextTokens {
		t := strings.TrimPrefix(tok, "-")
		if t == "" {
			continue
		}
		// Skip filter keywords; only tag tokens belong in the de-dup set.
		// Shares searchkw.IsKeyword with searchSuggest's value-only check.
		if colonIdx := strings.IndexByte(t, ':'); colonIdx > 0 {
			if searchkw.IsKeyword(strings.ToLower(t[:colonIdx])) {
				continue
			}
		}
		set[t] = struct{}{}
	}
	return set
}

func (s *Server) changeTagCategory(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	catIDStr := r.FormValue("category_id")
	catID, err := strconv.ParseInt(catIDStr, 10, 64)
	if err != nil {
		http.Error(w, "bad category_id", http.StatusBadRequest)
		return
	}
	// Route through the tag service for validation and consistency.
	if err := s.tagSvc().ChangeTagCategory(id, catID); err != nil {
		if isHTMXRequest(r) {
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(err.Error()) + `</div>`))
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if isHTMXRequest(r) {
		w.Write([]byte(`<div class="flash flash-ok">Category updated.</div>`))
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}


func (s *Server) getImageTagsHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	s.renderTagListWithSidebar(w, r, id, "", "", false)
}

// findAdjacentImages finds the prev/next image IDs in the given search context
// via cursor-style LIMIT 1 queries - O(log n) per side instead of loading the
// full matching ID list. seedStr carries the random-sort seed forward from the
// referring gallery so the same shuffle resolves to the same neighbours.
// ceiling AND-chains the cookie-ceiling NotExprs onto the parsed back_q so
// adjacency walks the same set the gallery shows.
func (s *Server) findAdjacentImages(currentID int64, queryStr, sortStr, orderStr, seedStr, ceiling string) (prevID, nextID *int64) {
	sq := adjacentSearchQuery(queryStr, sortStr, orderStr, seedStr, ceiling)
	prevID, nextID, err := search.ExecuteAdjacent(s.db(), sq, currentID)
	if err != nil {
		logx.Warnf("findAdjacentImages: %v", err)
	}
	return
}

// findImagePosition returns the 1-based rank of currentID in the referring
// search and the total matching rows. Used by the detail page's X/Y counter.
// Shares the same query-building path as findAdjacentImages so both numbers
// agree with the prev/next arrows they sit next to.
func (s *Server) findImagePosition(currentID int64, queryStr, sortStr, orderStr, seedStr, ceiling string) (pos, total int) {
	sq := adjacentSearchQuery(queryStr, sortStr, orderStr, seedStr, ceiling)
	pos, total, err := search.ExecutePosition(s.db(), sq, currentID)
	if err != nil {
		logx.Warnf("findImagePosition: %v", err)
	}
	return
}

func adjacentSearchQuery(queryStr, sortStr, orderStr, seedStr, ceiling string) search.Query {
	expr, _ := search.Parse(queryStr)
	expr = applyRatingCeiling(expr, ceiling)
	sq := search.Query{
		Expr:  expr,
		Sort:  sortStr,
		Order: orderStr,
	}
	if sortStr == "random" && seedStr != "" {
		if seed, err := strconv.ParseInt(seedStr, 10, 64); err == nil {
			sq.RandomSeed = seed
		}
	}
	return sq
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

func loadImage(ctx context.Context, database *db.DB, id int64) (*models.Image, error) {
	var img models.Image
	var isMissing, isFav int
	var width, height *int
	var autoTaggedAt *string
	var ingestedAt string

	err := database.Read.QueryRowContext(ctx,
		`SELECT id, sha256, canonical_path, folder_path, file_type,
		        width, height, file_size, is_missing, is_favorited,
		        auto_tagged_at, source_type, origin, ingested_at
		 FROM images WHERE id = ?`, id,
	).Scan(
		&img.ID, &img.SHA256, &img.CanonicalPath, &img.FolderPath, &img.FileType,
		&width, &height, &img.FileSize, &isMissing, &isFav,
		&autoTaggedAt, &img.SourceType, &img.Origin, &ingestedAt,
	)
	if err != nil {
		return nil, err
	}
	img.IsMissing = isMissing == 1
	img.IsFavorited = isFav == 1
	img.Width = width
	img.Height = height
	if autoTaggedAt != nil {
		t, _ := time.Parse(time.RFC3339, *autoTaggedAt)
		img.AutoTaggedAt = &t
	}
	img.IngestedAt, _ = time.Parse(time.RFC3339, ingestedAt)
	return &img, nil
}

func loadSDMeta(ctx context.Context, database *db.DB, id int64) *models.SDMetadata {
	var m models.SDMetadata
	var rawParams, genHash *string
	err := database.Read.QueryRowContext(ctx,
		`SELECT image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash
		 FROM sd_metadata WHERE image_id = ?`, id,
	).Scan(&m.ImageID, &m.Prompt, &m.NegativePrompt, &m.Model, &m.Seed, &m.Sampler, &m.Steps, &m.CFGScale, &rawParams, &genHash)
	if err != nil {
		return nil
	}
	if rawParams != nil {
		m.RawParams = *rawParams
	}
	if genHash != nil {
		m.GenerationHash = *genHash
	}
	if m.RawParams != "" {
		m.ParsedParams = meta.ParseAllSDParams(m.RawParams)
	}
	return &m
}

func loadComfyMeta(ctx context.Context, database *db.DB, id int64) *models.ComfyUIMetadata {
	var m models.ComfyUIMetadata
	var genHash *string
	err := database.Read.QueryRowContext(ctx,
		`SELECT image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash
		 FROM comfyui_metadata WHERE image_id = ?`, id,
	).Scan(&m.ImageID, &m.Prompt, &m.ModelCheckpoint, &m.Seed, &m.Sampler, &m.Steps, &m.CFGScale, &m.RawWorkflow, &genHash)
	if err != nil {
		return nil
	}
	if genHash != nil {
		m.GenerationHash = *genHash
	}
	return &m
}

func loadImagePaths(ctx context.Context, database *db.DB, id int64) []models.ImagePath {
	rows, err := database.Read.QueryContext(ctx,
		`SELECT id, image_id, path, is_canonical FROM image_paths WHERE image_id = ? ORDER BY is_canonical DESC, id`,
		id,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var paths []models.ImagePath
	for rows.Next() {
		var p models.ImagePath
		var isCanon int
		if err := rows.Scan(&p.ID, &p.ImageID, &p.Path, &isCanon); err != nil {
			logx.Warnf("load image paths scan: %v", err)
			continue
		}
		p.IsCanonical = isCanon == 1
		paths = append(paths, p)
	}
	return paths
}
