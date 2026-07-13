package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/logx"
	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/tags"
)

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
	Type         string
	// CreatedAfter is the raw query value ("24h" / "7d" / "30d" or an ISO
	// timestamp from a sweep-review link) so the sidebar chips highlight
	// on the spelling the URL carries.
	CreatedAfter string
	// Conflicts narrows to names living in more than one category;
	// ConflictsTotal is the badge count on the sidebar toggle.
	Conflicts      bool
	ConflictsTotal int
	OriginCounts   []tags.OriginCount
	// OriginKinds classifies each origin label on the page for chip
	// coloring: "user", "auto", "ptr", or "site".
	OriginKinds map[string]string
	ShowZero    bool
	ZeroOnly    bool
}

// originKinds buckets the given origin labels for the template's chip
// classes. Anything that is not the operator, the PTR, or a known
// auto-tagger attribution reads as a site / import label.
func (s *Server) originKinds(labels []string) map[string]string {
	kinds := make(map[string]string, len(labels))
	var unknown []string
	for _, l := range labels {
		switch l {
		case "":
		case "user":
			kinds[l] = "user"
		case "ptr":
			kinds[l] = "ptr"
		case "auto":
			kinds[l] = "auto"
		default:
			unknown = append(unknown, l)
		}
	}
	if len(unknown) > 0 {
		autoSet, err := s.tagSvc().AutoTaggerLabels(unknown)
		if err != nil {
			logx.Warnf("classify origin labels: %v", err)
		}
		for _, l := range unknown {
			if _, ok := autoSet[l]; ok {
				kinds[l] = "auto"
			} else {
				kinds[l] = "site"
			}
		}
	}
	return kinds
}

// createdAfterCutoff resolves the created_after query value: the quick
// range tokens the sidebar emits become a UTC cutoff, anything else
// (the ISO timestamp a sweep-review link carries) passes through.
func createdAfterCutoff(raw string) string {
	now := time.Now().UTC()
	switch raw {
	case "":
		return ""
	case "24h":
		return now.Add(-24 * time.Hour).Format(time.RFC3339)
	case "7d":
		return now.AddDate(0, 0, -7).Format(time.RFC3339)
	case "30d":
		return now.AddDate(0, 0, -30).Format(time.RFC3339)
	}
	return raw
}

func (s *Server) tagsHandler(w http.ResponseWriter, r *http.Request) {
	// The tags page reflects rapidly-changing state (category re-assignment,
	// merges). Opt out of browser caching so a reload after a mutation never
	// serves a stale render.
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	catIDStr := q.Get("cat")
	prefix := q.Get("q")
	// `?q=character:` (a category prefix with no tag-name suffix) is a
	// dead end against tags.name (no tag carries a colon by spec). Mirror
	// the autocomplete's branch and route to the category-only filter so
	// the user's intent surfaces instead of "No tags found".
	if catIDStr == "" && prefix != "" && strings.HasSuffix(prefix, ":") && strings.Count(prefix, ":") == 1 {
		catName := strings.TrimSuffix(prefix, ":")
		if catName != "" && s.categoryExists(catName) {
			var catID int64
			if err := s.db().Read.QueryRow(`SELECT id FROM tag_categories WHERE name = ?`, catName).Scan(&catID); err == nil {
				dst := r.URL
				vals := dst.Query()
				vals.Del("q")
				vals.Set("cat", strconv.FormatInt(catID, 10))
				dst.RawQuery = vals.Encode()
				http.Redirect(w, r, dst.String(), http.StatusSeeOther)
				return
			}
		}
	}
	sortStr := q.Get("sort")
	if sortStr == "" {
		sortStr = "usage"
	}
	orderStr := q.Get("order")
	if orderStr != "asc" && orderStr != "desc" {
		// Default to the natural reading direction per sort: most-used /
		// newest / most recently applied first, alphabetical A→Z for name.
		switch sortStr {
		case "usage", "created", "last_used":
			orderStr = "desc"
		default:
			orderStr = "asc"
		}
	}
	originStr := q.Get("origin")
	// Plain tags by default; alias rows surface via the explicit sidebar
	// filter (whose links always carry a type=, so "All" stays reachable).
	// The legacy origin=alias spelling opts out - it selects alias rows by
	// structure and would otherwise always come back empty.
	typeStr := q.Get("type")
	if !q.Has("type") && originStr != "alias" {
		typeStr = "tag"
	}
	createdAfterRaw := q.Get("created_after")
	conflictsOnly := q.Get("conflicts") == "1"
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

	filter := s.buildTagFilter(catIDStr, prefix, sortStr, orderStr, originStr, typeStr, createdAfterRaw, showZero, zeroOnly, page, 100)
	filter.ConflictsOnly = conflictsOnly

	tagList, total, err := s.tagSvc().ListTags(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cats, _ := s.tagSvc().ListCategories()
	totalPages := (total + 99) / 100

	// Clamp past-the-end pages to the last valid one and re-run, mirroring
	// the gallery handler. Without this the header reads `Tags <total>`
	// while the body says "No tags found" when a stale ?page=N URL
	// survives a tag prune.
	if total > 0 && page > totalPages {
		page = totalPages
		filter = s.buildTagFilter(catIDStr, prefix, sortStr, orderStr, originStr, typeStr, createdAfterRaw, showZero, zeroOnly, page, 100)
		filter.ConflictsOnly = conflictsOnly
		tagList, total, err = s.tagSvc().ListTags(filter)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

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

	conflictsTotal, err := s.tagSvc().ConflictsCount()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	originCounts, err := s.tagSvc().OriginCounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pageLabels := make([]string, 0, len(tagList))
	for _, t := range tagList {
		pageLabels = append(pageLabels, t.Origin)
	}
	for _, oc := range originCounts {
		pageLabels = append(pageLabels, oc.Label)
	}

	data := tagsPageData{
		baseData:       s.base(r, "tags", "Tags - "+s.booruName()),
		Tags:           tagList,
		Categories:     cats,
		Implications:   imps,
		Total:          total,
		Page:           page,
		TotalPages:     totalPages,
		CategoryID:     catIDStr,
		Prefix:         prefix,
		Sort:           sortStr,
		Order:          orderStr,
		Origin:         originStr,
		Type:           typeStr,
		CreatedAfter:   createdAfterRaw,
		Conflicts:      conflictsOnly,
		ConflictsTotal: conflictsTotal,
		OriginCounts:   originCounts,
		OriginKinds:    s.originKinds(pageLabels),
		ShowZero:       showZero,
		ZeroOnly:       zeroOnly,
	}
	s.renderTemplate(w, "tags.html", data)
}

func (s *Server) buildTagFilter(catIDStr, prefix, sortStr, orderStr, originStr, typeStr, createdAfterRaw string, showZero, zeroOnly bool, page, limit int) tags.TagFilter {
	f := tags.TagFilter{
		Prefix:       prefix,
		Sort:         sortStr,
		Order:        orderStr,
		PageIndex:    page - 1,
		Limit:        limit,
		Origin:       originStr,
		Type:         typeStr,
		CreatedAfter: createdAfterCutoff(createdAfterRaw),
		ShowZero:     showZero,
		ZeroOnly:     zeroOnly,
	}
	if catIDStr != "" {
		if id, err := strconv.ParseInt(catIDStr, 10, 64); err == nil {
			f.CategoryID = &id
		}
	}
	return f
}

// resolveCanonicalTagInput resolves a "name", "category:name", or id
// input to a tag id. With create set, a missing name is minted via
// GetOrCreateTag - the implications dialog's parseTagInput →
// GetOrCreateTag flow, so users can declare an alias or edge to a
// still-pending name; without it the input must name an existing tag.
// A numeric input always requires the id to exist (a typo'd id
// shouldn't silently mint a fresh tag).
func (s *Server) resolveCanonicalTagInput(input string, create bool) (int64, string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, "Tag name is required."
	}
	if id, err := strconv.ParseInt(input, 10, 64); err == nil {
		var exists int
		if err := s.db().Read.QueryRow(`SELECT COUNT(*) FROM tags WHERE id = ?`, id).Scan(&exists); err != nil || exists == 0 {
			return 0, "Tag not found: " + input
		}
		return id, ""
	}
	if idx := strings.Index(input, ":"); idx > 0 && s.categoryExists(input[:idx]) {
		catName := input[:idx]
		tagName := strings.TrimSpace(input[idx+1:])
		if tagName == "" {
			return 0, "Tag name is required after the category prefix."
		}
		var catID int64
		if err := s.db().Read.QueryRow(
			`SELECT id FROM tag_categories WHERE name = ?`, catName,
		).Scan(&catID); err != nil {
			return 0, "Category not found: " + catName
		}
		if !create {
			var id int64
			if err := s.db().Read.QueryRow(
				`SELECT id FROM tags WHERE name = ? AND category_id = ?`, tagName, catID,
			).Scan(&id); err != nil {
				return 0, "Tag not found: " + input
			}
			return id, ""
		}
		tag, err := s.tagSvc().GetOrCreateTag(tagName, catID)
		if err != nil {
			return 0, err.Error()
		}
		return tag.ID, ""
	}
	rows, err := s.db().Read.Query(`SELECT id FROM tags WHERE name = ?`, input)
	if err != nil {
		return 0, "Tag lookup failed: " + err.Error()
	}
	defer func() { _ = rows.Close() }()
	ids, err := db.ScanIDs(rows)
	if err != nil {
		return 0, "Tag lookup failed: " + err.Error()
	}
	switch len(ids) {
	case 1:
		return ids[0], ""
	case 0:
		if !create {
			return 0, "Tag not found: " + input
		}
		cx := s.Active()
		if cx == nil || cx.GeneralCategoryID == 0 {
			return 0, "Could not resolve the general category."
		}
		tag, err := s.tagSvc().GetOrCreateTag(input, cx.GeneralCategoryID)
		if err != nil {
			return 0, err.Error()
		}
		return tag.ID, ""
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

	catID, err := strconv.ParseInt(catIDStr, 10, 64)
	if err != nil {
		externalErr(w, r, "Invalid category.", http.StatusBadRequest)
		return
	}
	if _, err := s.tagSvc().GetOrCreateTag(name, catID); err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	s.Active().InvalidateCaches()
	hxDone(w, r, "Tag "+name+" created.", "/tags?q="+url.QueryEscape(name), "/tags")
}

func (s *Server) createAliasPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	catIDStr := r.FormValue("category_id")
	canonInput := strings.TrimSpace(r.FormValue("canonical_id"))

	catID, err := strconv.ParseInt(catIDStr, 10, 64)
	if err != nil {
		externalErr(w, r, "Invalid category.", http.StatusBadRequest)
		return
	}
	canonID, msg := s.resolveCanonicalTagInput(canonInput, true)
	if msg != "" {
		externalErr(w, r, msg, http.StatusBadRequest)
		return
	}

	if _, err := s.tagSvc().CreateAlias(name, catID, canonID); err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	s.Active().InvalidateCaches()

	hxDone(w, r, "Alias "+name+" created.", "/tags?type=alias&q="+url.QueryEscape(name), "/tags?type=alias")
}

func (s *Server) deleteTagHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if err := s.tagSvc().DeleteTag(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.Active().InvalidateCaches()
	w.WriteHeader(http.StatusNoContent)
}

// deleteTagsSearchPost deletes every tag in scope - the checkbox
// selection when ids are posted, else everything matching the posted
// /tags filter. Mirrors the gallery's /internal/delete-search: resolve
// the id set up front, kick off a background "tag" job, return 202
// Accepted so the client surfaces progress via the job status bar.
func (s *Server) deleteTagsSearchPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	ids, ok := s.startTagScopeJob(w, r)
	if !ok {
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

	s.jobs.Update(0, total, "deleting tags…")
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
			s.jobs.Update(processed, total, "deleting tags…")
		}
	}

	s.Active().InvalidateCaches()
	s.finishJob(nil, cancelled, fmt.Sprintf("delete tags cancelled (%d/%d processed)", processed, total), fmt.Sprintf("deleted %d tag(s)", deleted))
}

func (s *Server) renameTagPost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	newName := strings.TrimSpace(r.FormValue("name"))
	if newName == "" {
		externalErr(w, r, "Name required.", http.StatusBadRequest)
		return
	}
	var err error
	if r.FormValue("keep_alias") == "1" {
		err = s.tagSvc().RenameTagKeepAlias(id, newName)
	} else {
		err = s.tagSvc().RenameTag(id, newName)
	}
	if err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	// A tag rename moves it to a new literal-name match in the search
	// resolver, so a cached `?q=oldname` snapshot must drop too.
	s.Active().InvalidateCaches()
	// Refresh the current URL instead of redirecting to /tags so the
	// user's active filter - q, sort, origin, page - survives the
	// rename and the renamed row stays in scope.
	hxDone(w, r, "Renamed to "+newName+".", "", "/tags")
}
