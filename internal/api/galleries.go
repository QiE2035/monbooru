package api

import "net/http"

// galleryListEntry is one row of GET /api/v1/galleries: a configured
// gallery the caller can target with ?gallery=<name>, plus the same
// visible-image and non-alias-tag counts the Settings page shows.
type galleryListEntry struct {
	Name   string `json:"name"`
	Images int    `json:"images"`
	Tags   int    `json:"tags"`
	Active bool   `json:"active"`
}

// listGalleries handles GET /api/v1/galleries. The set of galleries and
// the active one are derived from the configured list and the resolver
// already wired into the handler - resolver("") returns the active
// gallery - so no extra plumbing is needed. Counts are best-effort: a
// gallery whose count query fails still appears, with zero.
func (h *Handler) listGalleries(w http.ResponseWriter, r *http.Request) {
	activeName := ""
	if active, ok := h.resolver(""); ok {
		activeName = active.Name
	}
	out := make([]galleryListEntry, 0, len(h.cfg.Galleries))
	for _, gc := range h.cfg.Galleries {
		g, ok := h.resolver(gc.Name)
		if !ok {
			continue
		}
		entry := galleryListEntry{Name: gc.Name, Active: gc.Name == activeName}
		_ = g.DB.Read.QueryRow(`SELECT COUNT(*) FROM images WHERE is_missing = 0`).Scan(&entry.Images)
		_ = g.DB.Read.QueryRow(`SELECT COUNT(*) FROM tags WHERE is_alias = 0`).Scan(&entry.Tags)
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}
