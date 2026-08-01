package api

import (
	"net/http"
	"slices"
)

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
	h.cfgMu.RLock()
	configured := slices.Clone(h.cfg.Galleries)
	h.cfgMu.RUnlock()
	out := make([]galleryListEntry, 0, len(configured))
	for _, gc := range configured {
		g, ok := h.resolver(gc.Name)
		if !ok {
			continue
		}
		entry := galleryListEntry{Name: gc.Name, Active: gc.Name == activeName}
		entry.Images = galleryCount(g.VisibleCount, g, `SELECT COUNT(*) FROM images WHERE is_missing = 0`)
		entry.Tags = galleryCount(g.TagCount, g, `SELECT COUNT(*) FROM tags WHERE is_alias = 0`)
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// galleryCount returns the cached count when the accessor is wired - the
// resolver populates it from the gallery's cached aggregates so this
// endpoint matches what the Settings page shows without a fresh scan -
// and falls back to a direct query otherwise (e.g. a hand-built test
// Gallery). Counts are best-effort: a failure reports 0.
func galleryCount(cached func() (int, error), g Gallery, fallback string) int {
	if cached != nil {
		if n, err := cached(); err == nil {
			return n
		}
	}
	var n int
	_ = g.DB.Read.QueryRow(fallback).Scan(&n)
	return n
}
