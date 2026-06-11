package api

import (
	"net/http"

	"github.com/leqwin/monbooru/internal/tags"
)

type tagResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Category   string `json:"category"`
	Color      string `json:"color"`
	UsageCount int    `json:"usage_count"`
	IsAlias    bool   `json:"is_alias"`
}

// listTags handles GET /api/v1/tags.
func (h *Handler) listTags(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	prefix := q.Get("q")
	catName := q.Get("category")
	sortStr := q.Get("sort")
	if sortStr == "" {
		sortStr = "usage"
	}

	offset, limit := parsePage(r, 100, 500)

	filter := tags.TagFilter{
		Prefix:    prefix,
		Sort:      sortStr,
		PageIndex: offset / limit,
		Limit:     limit,
		Origin:    q.Get("origin"),
		// Tri-state with the /tags page: empty / anything but "0" → Show
		// (default so freshly-declared tags surface without a flag flip);
		// "0" → Hide. The UI also exposes "only" but the API has no use
		// for that triage view, so any non-"0" string folds into Show.
		ShowZero: q.Get("show_zero") != "0",
	}

	if catName != "" {
		var catID int64
		if err := g.DB.Read.QueryRow(
			`SELECT id FROM tag_categories WHERE name = ?`, catName,
		).Scan(&catID); err == nil {
			filter.CategoryID = &catID
		}
	}

	tagList, total, err := g.TagSvc.ListTags(filter)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	results := make([]tagResponse, 0, len(tagList))
	for _, t := range tagList {
		results = append(results, toTagResponse(&t))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"page":    offset/limit + 1,
		"limit":   limit,
		"total":   total,
		"results": results,
	})
}
