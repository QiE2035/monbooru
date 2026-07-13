package web

import (
	"net/http"
	"strings"

	"github.com/leqwin/monbooru/internal/logx"
)

func (s *Server) deleteSavedSearch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
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
		externalErr(w, r, "Name and query required.", http.StatusBadRequest)
		return
	}
	// Capture sort + order + seed so a `random` save reopens at the same
	// shuffle and explicit non-default sorts survive the round trip. Empty
	// values mean "use the gallery handler's defaults" on reopen.
	sortStr := strings.TrimSpace(r.FormValue("sort"))
	orderStr := strings.TrimSpace(r.FormValue("order"))
	seedStr := strings.TrimSpace(r.FormValue("seed"))
	// Plain INSERT so the UNIQUE(name) constraint surfaces as an error
	// instead of clobbering the existing entry. The user can delete the
	// previous saved search from the sidebar and resubmit; same idiom
	// the category and tag-name uniqueness checks use elsewhere.
	if _, err := s.db().Write.Exec(
		`INSERT INTO saved_searches (name, query, sort, sort_order, seed) VALUES (?, ?, ?, ?, ?)`,
		name, query, sortStr, orderStr, seedStr,
	); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "UNIQUE") {
			msg = "A saved search named " + name + " already exists. Delete it first or pick another name."
		}
		externalErr(w, r, msg, http.StatusBadRequest)
		return
	}
	s.Active().InvalidateCaches()
	if isHTMXRequest(r) {
		writeInlineFlash(w, "ok", "Saved.")
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
