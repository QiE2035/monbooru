package web

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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
	// DeletableTotal excludes built-in rating rows from the bulk-delete
	// dialog count so the wording doesn't overstate the blast radius -
	// rating rows survive the bulk delete (they get usage-stripped, the
	// catalog row stays).
	DeletableTotal int
	// HasFilter reports whether any of q / cat / origin / show_zero
	// narrowed the listing. Drives the dialog wording so a no-filter
	// "Delete tags in current search" reads honestly as "Delete all N
	// tags in this gallery".
	HasFilter  bool
	Page       int
	TotalPages int
	CategoryID string
	Prefix     string
	Sort       string
	Order      string
	Origin     string
	ShowZero   bool
	ZeroOnly   bool
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

	// Clamp past-the-end pages to the last valid one and re-run, mirroring
	// the gallery handler. Without this the header reads `Tags <total>`
	// while the body says "No tags found" when a stale ?page=N URL
	// survives a tag prune.
	if total > 0 && page > totalPages {
		page = totalPages
		filter = s.buildTagFilter(catIDStr, prefix, sortStr, orderStr, originStr, showZero, zeroOnly, page, 100)
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

	// Compute the count that bulk-delete would actually remove from the
	// catalog: total minus rating rows (which DeleteTag treats as a
	// usage-strip, leaving the catalog rows in place).
	deletable := total
	if ratingID := s.tagSvc().RatingCategoryID(); ratingID != 0 {
		ratingFilter := filter
		ratingFilter.CategoryID = &ratingID
		ratingFilter.PageIndex = 0
		ratingFilter.Limit = 0
		_, ratingTotal, ratingErr := s.tagSvc().ListTags(ratingFilter)
		if ratingErr == nil {
			deletable = total - ratingTotal
			if deletable < 0 {
				deletable = 0
			}
		}
	}
	hasFilter := prefix != "" || catIDStr != "" || originStr != "" || zeroParam == "0" || zeroOnly

	data := tagsPageData{
		baseData:       s.base(r, "tags", "Tags - "+s.booruName()),
		Tags:           tagList,
		Categories:     cats,
		Implications:   imps,
		Total:          total,
		DeletableTotal: deletable,
		HasFilter:      hasFilter,
		Page:           page,
		TotalPages:     totalPages,
		CategoryID:     catIDStr,
		Prefix:         prefix,
		Sort:           sortStr,
		Order:          orderStr,
		Origin:         originStr,
		ShowZero:       showZero,
		ZeroOnly:       zeroOnly,
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

	mergeErr := func(msg string) {
		if isHTMXRequest(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<div class="flash flash-err">` + html.EscapeString(msg) + `</div>`))
			return
		}
		http.Error(w, msg, http.StatusBadRequest)
	}
	canonID, msg := s.resolveOrCreateCanonicalTag(canonInput)
	if msg != "" {
		mergeErr(msg)
		return
	}

	// Capture the source name for the post-merge redirect to
	// /tags?origin=alias&q=<source>.
	srcTag, _ := s.tagSvc().GetTag(aliasID)
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
		canon, _ := s.tagSvc().GetTag(canonID)
		canonName := canonInput
		if canon != nil && canon.Name != "" {
			canonName = canon.Name
		}
		setFlashHeader(w, "Aliased to "+canonName+".", "ok", nil)
		// Land on the alias-only filtered listing so the freshly-created
		// alias row is the only thing on screen, mirroring the create-
		// alias dialog's post-submit redirect. Falls back to /tags if
		// the source lookup couldn't recover a name.
		dest := "/tags?origin=alias"
		if srcTag != nil && srcTag.Name != "" {
			dest = "/tags?origin=alias&q=" + url.QueryEscape(srcTag.Name)
		}
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

// resolveOrCreateCanonicalTag is the alias-side variant of
// resolveCanonicalTag: when the canonical name doesn't yet name a
// tag, the missing row is created via GetOrCreateTag instead of
// surfacing a "Tag not found" error. Mirrors the implications dialog's
// parseTagInput → GetOrCreateTag flow so users can declare an alias
// (Create alias / Alias→ / Repoint→) to a still-pending name. A
// numeric input still requires the id to exist (a typo'd id shouldn't
// silently mint a fresh tag).
func (s *Server) resolveOrCreateCanonicalTag(input string) (int64, string) {
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
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			logx.Warnf("resolveOrCreateCanonicalTag scan: %v", err)
			continue
		}
		ids = append(ids, id)
	}
	switch len(ids) {
	case 1:
		return ids[0], ""
	case 0:
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
		setFlashHeader(w, "Tag "+name+" created.", "ok", nil)
		w.Header().Set("HX-Redirect", "/tags?q="+url.QueryEscape(name))
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
	canonID, msg := s.resolveOrCreateCanonicalTag(canonInput)
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
		setFlashHeader(w, "Alias "+name+" created.", "ok", nil)
		w.Header().Set("HX-Redirect", "/tags?origin=alias&q="+url.QueryEscape(name))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/tags?origin=alias", http.StatusSeeOther)
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
	if err := s.jobs.Start(models.JobTypeTag); err != nil {
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
	// A tag rename moves it to a new literal-name match in the search
	// resolver, so a cached `?q=oldname` snapshot must drop too.
	s.Active().InvalidateCaches()
	if isHTMXRequest(r) {
		// Refresh the current URL instead of redirecting to /tags so the
		// user's active filter - q, sort, origin, page - survives the
		// rename and the renamed row stays in scope.
		setFlashHeader(w, "Renamed to "+newName+".", "ok", nil)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}
