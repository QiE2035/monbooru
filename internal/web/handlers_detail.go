package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/leqwin/monbooru/internal/logx"
	meta "github.com/leqwin/monbooru/internal/metadata"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/search"
	"github.com/leqwin/monbooru/internal/tagger"
)

type detailData struct {
	baseData
	Image          models.Image
	Filename       string // basename of the canonical path, shown on the detail page topbar
	ImageTags      []models.ImageTag
	SDMeta         *models.SDMetadata
	ComfyMeta      *models.ComfyUIMetadata
	ComfyNodes     []models.ComfyNode
	GenericMeta    []models.SDParam
	MangaMeta      *models.MangaMetadata // populated for cbz rows when ComicInfo.xml was parsed
	IsManga        bool                  // shorthand for FileType == "cbz" so the template doesn't string-compare
	ImagePaths     []models.ImagePath
	ThumbnailURL   string
	PrevID         *int64
	NextID         *int64
	RefURL         string // predecessor detail URL when the user arrived via a Similar-images click; drives the "← Previous image" back link and Escape
	Ref            string // raw ref=<sourceID> value when valid; forwarded on the delete button so the post-delete redirect returns to the source instead of an arbitrary neighbour
	BackQuery      string
	BackSort       string
	BackOrder      string
	BackPage       string
	BackSeed       string
	// BackQS is the URL-safe `?back_*=...` fragment carrying every back_*
	// the detail handler saw. Forwarded verbatim on the manga Read /
	// Pages anchors so click-through preserves the gallery context;
	// rendered as template.URL so html/template doesn't URL-encode the
	// `&` separators when the fragment is interpolated after `?page=N`.
	BackQS         template.URL
	BackKVQS       template.URL
	EnabledTaggers []tagger.TaggerStatus // enabled+available taggers offered in the auto-tag control
	ImageTaggers   []string              // distinct tagger names currently on this image's auto-tags
	HasUserTags    bool                  // true when at least one manual (non-auto) tag is on this image
	Aliases        []models.Tag          // alias rows pointing at any non-implied tag on this image, flattened for display
	// PhashDistance is the configured Find-pairs Hamming distance used by
	// the phash row's [search near-duplicates] link. Pulled live from
	// Config.Relations.DefaultDistance so a settings tweak is honoured
	// without a restart.
	PhashDistance int
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

	// Resolve back_page so Escape and "← Back" land on the page that
	// actually contains the current image, even after prev/next walked
	// past the page the user arrived from. Warm path: slice-scan the
	// cached match list. Cold path: spawn a COUNT-rank query in the
	// detail-handler's parallel block so it doesn't extend latency
	// past the slowest other read; cache miss with a back_q context
	// is the only time this fires.
	var rankPage string
	rankReady := make(chan struct{})
	rankFired := false
	if wantAdjacent && s.cfg.UI.PageSize > 0 {
		var seed int64
		if backSort == "random" && backSeed != "" {
			seed, _ = strconv.ParseInt(backSeed, 10, 64)
		}
		cacheKey := search.BuildAdjacencyCacheKey(s.activeName, backQ, backSort, backOrder, seed, ceiling)
		if ids, ok := search.AdjacencyCacheGet(cacheKey); ok {
			for i, mid := range ids {
				if mid == id {
					backPage = strconv.Itoa(i/s.cfg.UI.PageSize + 1)
					break
				}
			}
		} else {
			rankFired = true
			go func() {
				defer close(rankReady)
				sq := adjacentSearchQuery(backQ, backSort, backOrder, backSeed, ceiling)
				ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
				defer cancel()
				rank, err := search.RankInQuery(ctx, s.db(), sq, id)
				if err != nil || rank < 0 {
					return
				}
				rankPage = strconv.Itoa(rank/s.cfg.UI.PageSize + 1)
			}()
		}
	}
	if !rankFired {
		close(rankReady)
	}

	// The remaining reads are independent of each other and target different
	// tables (or the filesystem for ExtractGeneric). Run them in parallel
	// across the read pool. Related images are fetched lazily via
	// /images/{id}/related so the page paints before that join finishes -
	// on libraries with millions of image_tags rows it was the single
	// largest contributor to detail-page latency. comfyNodes parsing stays
	// sequential - it's pure CPU on the comfyMeta payload and only matters
	// once that read returns.
	var (
		imageTags   []models.ImageTag
		sdMeta      *models.SDMetadata
		comfyMeta   *models.ComfyUIMetadata
		genericMeta []models.SDParam
		mangaMeta   *models.MangaMetadata
		imagePaths  []models.ImagePath
		prevID      *int64
		nextID      *int64
	)
	isManga := img.FileType == models.FileTypeCBZ
	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); imageTags, _ = s.tagSvc().GetImageTags(id) }()
	go func() { defer wg.Done(); sdMeta = loadSDMeta(ctx, s.db(), id) }()
	go func() { defer wg.Done(); comfyMeta = loadComfyMeta(ctx, s.db(), id) }()
	go func() { defer wg.Done(); imagePaths = loadImagePaths(ctx, s.db(), id) }()
	go func() {
		defer wg.Done()
		// Skip the generic-EXIF/text-chunk extraction for manga - the
		// archive's bytes don't carry per-image metadata in the way SD
		// images do, and the work would just walk the cbz's central
		// directory for nothing. ComicInfo.xml is loaded separately.
		if !isManga {
			genericMeta = meta.ExtractGeneric(img.CanonicalPath, img.FileType)
		}
	}()
	if isManga {
		wg.Add(1)
		go func() { defer wg.Done(); mangaMeta = loadMangaMeta(ctx, s.db(), id) }()
	}
	if wantAdjacent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prevID, nextID = s.findAdjacentImages(id, backQ, backSort, backOrder, backSeed, ceiling)
		}()
	}
	wg.Wait()
	<-rankReady
	if rankPage != "" {
		backPage = rankPage
	}

	var comfyNodes []models.ComfyNode
	if comfyMeta != nil && comfyMeta.RawWorkflow != "" {
		comfyNodes = meta.ParseComfyWorkflowNodes(comfyMeta.RawWorkflow)
	}

	enabledTaggers := tagger.EnabledTaggersForGallery(s.cfg, s.activeName)
	imageTaggers := distinctAutoTaggerNames(imageTags)
	hasUserTags := false
	for _, t := range imageTags {
		if !t.IsAuto {
			hasUserTags = true
			break
		}
	}

	baseName := filepath.Base(img.CanonicalPath)
	// Prefix the immediate parent folder so a tab strip with several
	// generic basenames (file.png, vol2.cbz, ...) stays distinguishable.
	titleName := baseName
	if parent := filepath.Base(filepath.Dir(img.CanonicalPath)); parent != "" && parent != "." && parent != "/" {
		titleName = parent + "/" + baseName
	}
	data := detailData{
		baseData:       s.base(r, "gallery", fmt.Sprintf("%s - Monbooru", titleName)),
		Image:          *img,
		Filename:       baseName,
		ImageTags:      imageTags,
		SDMeta:         sdMeta,
		ComfyMeta:      comfyMeta,
		ComfyNodes:     comfyNodes,
		GenericMeta:    genericMeta,
		MangaMeta:      mangaMeta,
		IsManga:        isManga,
		ImagePaths:     imagePaths,
		ThumbnailURL:   fmt.Sprintf("/thumbnails/%s/%d.jpg", s.activeName, id),
		PrevID:         prevID,
		NextID:         nextID,
		RefURL:         refURL,
		Ref:            refStrValid,
		BackQuery:      backQ,
		BackSort:       backSort,
		BackOrder:      backOrder,
		BackPage:       backPage,
		BackSeed:       backSeed,
		BackQS:         buildDetailBackQS(backQ, backSort, backOrder, backPage, backSeed, "?"),
		BackKVQS:       buildDetailBackQS(backQ, backSort, backOrder, backPage, backSeed, "&"),
		EnabledTaggers: enabledTaggers,
		ImageTaggers:   imageTaggers,
		HasUserTags:    hasUserTags,
		Aliases:        s.aliasesForImageTags(imageTags),
		PhashDistance:  s.relationsPhashDistance(),
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
	related, _ := s.tagSvc().RelatedImages(id, 6, ratingCeilingFromRequest(r))
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

// buildDetailBackQS returns a URL-encoded `back_*` fragment with the
// supplied separator (`?` for stand-alone hrefs, `&` for hrefs that
// already opened a query string). Returns empty when no back_* is set.
// The result is template.URL so the html/template engine doesn't
// double-escape `&` and `=` when interpolated into a URL attribute.
func buildDetailBackQS(q, sort, order, page, seed, sep string) template.URL {
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
		return ""
	}
	return template.URL(sep + v.Encode())
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

// findAdjacentImages finds the prev/next image IDs in the given search context
// via cursor-style LIMIT 1 queries - O(log n) per side instead of loading the
// full matching ID list. seedStr carries the random-sort seed forward from the
// referring gallery so the same shuffle resolves to the same neighbours.
// ceiling AND-chains the cookie-ceiling NotExprs onto the parsed back_q so
// adjacency walks the same set the gallery shows.
func (s *Server) findAdjacentImages(currentID int64, queryStr, sortStr, orderStr, seedStr, ceiling string) (prevID, nextID *int64) {
	sq := adjacentSearchQuery(queryStr, sortStr, orderStr, seedStr, ceiling)
	sq.CacheKey = search.BuildAdjacencyCacheKey(s.activeName, queryStr, sortStr, orderStr, sq.RandomSeed, ceiling)
	prevID, nextID, err := search.ExecuteAdjacent(s.db(), sq, currentID)
	if err != nil {
		logx.Warnf("findAdjacentImages: %v", err)
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
