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
	defer func() { _ = rows.Close() }()
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
	_, _ = fmt.Fprintf(w, `{"count":%d}`, count)
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
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	hxDone(w, r, localize("flash.category_created", map[string]any{"name": name}), "/categories", "/categories")
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
	hxDone(w, r, localize("flash.category_color_updated"), "/categories", "/categories")
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
	hxDone(w, r, localize("flash.category_deleted"), "/tags", "/tags")
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
		externalErr(w, r, "Name required.", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().RenameCategory(id, newName); err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	hxDone(w, r, localize("flash.category_renamed", map[string]any{"name": newName}), "/categories", "/categories")
}
