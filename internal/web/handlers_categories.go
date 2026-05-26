package web

import (
	"fmt"
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

	data := s.base(r, "categories", "Categories - "+s.booruName()).AsMap()
	data["Galleries"] = s.galleryList()
	data["Categories"] = cats
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
	id, ok := pathInt64(w, r, "id")
	if !ok {
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
			writeInlineFlash(w, "err", err.Error())
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isHTMXRequest(r) {
		setFlashHeader(w, "Category "+name+" created.", "ok", nil)
		w.Header().Set("HX-Redirect", "/categories")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/categories", http.StatusSeeOther)
}

func (s *Server) updateCategoryPatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	color := r.FormValue("color")
	if err := s.tagSvc().UpdateCategoryColor(id, color); err != nil {
		logx.Warnf("update category %d color: %v", id, err)
		writeInlineFlash(w, "err", err.Error())
		return
	}
	if isHTMXRequest(r) {
		setFlashHeader(w, "Category color updated.", "ok", nil)
		w.Header().Set("HX-Redirect", "/categories")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/categories", http.StatusSeeOther)
}

func (s *Server) deleteCategoryDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
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
	// Surface on /tags (the redirect target), not /categories - the
	// flash rides the shared monbooru:flash channel which lands in
	// whichever flash slot the destination page exposes.
	if isHTMXRequest(r) {
		setFlashHeader(w, "Category deleted.", "ok", nil)
		w.Header().Set("HX-Redirect", "/tags")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

func (s *Server) renameCategoryPost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	newName := strings.TrimSpace(r.FormValue("name"))
	if newName == "" {
		if isHTMXRequest(r) {
			writeInlineFlash(w, "err", "Name required.")
			return
		}
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().RenameCategory(id, newName); err != nil {
		if isHTMXRequest(r) {
			writeInlineFlash(w, "err", err.Error())
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isHTMXRequest(r) {
		setFlashHeader(w, "Category renamed to "+newName+".", "ok", nil)
		w.Header().Set("HX-Redirect", "/categories")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/categories", http.StatusSeeOther)
}
