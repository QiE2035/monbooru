package web

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/leqwin/monbooru/internal/logx"
)

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
		"RequestStart":  base.RequestStart,
		"Categories":    cats,
	}
	s.renderTemplate(w, "categories.html", data)
}

// categoryColors returns a name → color map for every row in
// tag_categories on the active gallery. Used by the threshold dialog
// so each category label renders in its own colour. Database errors
// yield an empty map so the dialog still renders without colour.
func (s *Server) categoryColors() map[string]string {
	cx := s.Active()
	if cx == nil {
		return nil
	}
	rows, err := cx.DB.Read.Query(`SELECT name, color FROM tag_categories`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, color string
		if err := rows.Scan(&name, &color); err != nil {
			return out
		}
		out[name] = color
	}
	return out
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
