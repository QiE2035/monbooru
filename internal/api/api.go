// Package api implements the /api/v1/ REST handlers for monbooru.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/jobs"
	"github.com/leqwin/monbooru/internal/relations"
	"github.com/leqwin/monbooru/internal/tags"
)

// Gallery is what API handlers need to act on a single gallery.
// InvalidateCaches is called after every image add/delete; may be nil.
type Gallery struct {
	Name             string
	GalleryPath      string
	ThumbnailsPath   string
	DB               *db.DB
	TagSvc           *tags.Service
	RelationsSvc     *relations.Service
	InvalidateCaches func()
	// VisibleCount / TagCount return the cached non-missing-image and
	// non-alias-tag counts (the same values the Settings page shows). May
	// be nil; listGalleries falls back to a direct query then.
	VisibleCount func() (int, error)
	TagCount     func() (int, error)
}

// invalidate runs the gallery's cache-invalidation hook when one is
// wired (it is nil in the test harness).
func (g Gallery) invalidate() {
	if g.InvalidateCaches != nil {
		g.InvalidateCaches()
	}
}

// relationsOnDelete returns the OnImageDelete callback for the given
// service, or nil when the gallery has no relations service wired (the
// test harness). gallery.DeleteImage treats a nil callback as a no-op.
func relationsOnDelete(svc *relations.Service) func(int64) error {
	if svc == nil {
		return nil
	}
	return svc.OnImageDelete
}

// ResolverFunc resolves a gallery by name. Empty name = active gallery.
type ResolverFunc func(name string) (Gallery, bool)

// Handler is the root handler for all /api/v1/ routes.
type Handler struct {
	cfg      *config.Config
	jobs     *jobs.Manager
	resolver ResolverFunc
	version  string
}

// New creates a new API handler. version is surfaced on the /api/v1/ root so
// clients (e.g. monloader) can read the server version without scraping HTML.
func New(cfg *config.Config, jobManager *jobs.Manager, resolver ResolverFunc, version string) *Handler {
	return &Handler{cfg: cfg, jobs: jobManager, resolver: resolver, version: version}
}

// resolveGallery picks the target gallery from ?gallery=... (preferred)
// or the X-Monbooru-Gallery header; empty falls back to the active one.
func (h *Handler) resolveGallery(w http.ResponseWriter, r *http.Request) (Gallery, bool) {
	name := strings.TrimSpace(r.URL.Query().Get("gallery"))
	if name == "" {
		name = strings.TrimSpace(r.Header.Get("X-Monbooru-Gallery"))
	}
	g, ok := h.resolver(name)
	if !ok {
		if name == "" {
			apiError(w, http.StatusServiceUnavailable, "api_disabled", "no active gallery")
		} else {
			apiError(w, http.StatusBadRequest, "invalid_gallery", "unknown gallery: "+name)
		}
		return Gallery{}, false
	}
	return g, true
}

// Mount registers every API route on mux under /api/v1/.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/images", h.auth(h.createImage))
	mux.HandleFunc("GET /api/v1/images/search", h.auth(h.searchImages))
	mux.HandleFunc("GET /api/v1/images/{id}", h.auth(h.getImage))
	mux.HandleFunc("PATCH /api/v1/images/{id}", h.auth(h.patchImage))
	mux.HandleFunc("DELETE /api/v1/images/{id}", h.auth(h.deleteImage))
	mux.HandleFunc("GET /api/v1/images/{id}/tags", h.auth(h.listImageTags))
	mux.HandleFunc("POST /api/v1/images/{id}/tags", h.auth(h.addImageTags))
	mux.HandleFunc("DELETE /api/v1/images/{id}/tags", h.auth(h.removeImageTags))

	mux.HandleFunc("GET /api/v1/images/{id}/file", h.auth(h.serveImageFile))
	mux.HandleFunc("GET /api/v1/images/{id}/thumbnail", h.auth(h.serveThumbnail))
	mux.HandleFunc("GET /api/v1/images/{id}/page/{n}", h.auth(h.serveMangaPage))
	mux.HandleFunc("GET /api/v1/images/{id}/page/{n}/thumb", h.auth(h.serveMangaPageThumb))

	mux.HandleFunc("GET /api/v1/tags", h.auth(h.listTags))
	mux.HandleFunc("POST /api/v1/tags", h.auth(h.createTag))
	mux.HandleFunc("POST /api/v1/tags/aliases", h.auth(h.createAlias))
	mux.HandleFunc("POST /api/v1/tags/merge", h.auth(h.mergeTags))
	mux.HandleFunc("PATCH /api/v1/tags/{id}", h.auth(h.patchTag))
	mux.HandleFunc("DELETE /api/v1/tags/{id}", h.auth(h.deleteTag))
	mux.HandleFunc("GET /api/v1/tags/{id}/implications", h.auth(h.listImplications))
	mux.HandleFunc("POST /api/v1/tags/{id}/implications", h.auth(h.addImplication))
	mux.HandleFunc("DELETE /api/v1/tags/{id}/implications/{impliedID}", h.auth(h.removeImplication))

	mux.HandleFunc("GET /api/v1/categories", h.auth(h.listCategories))
	mux.HandleFunc("POST /api/v1/categories", h.auth(h.createCategory))
	mux.HandleFunc("PATCH /api/v1/categories/{id}", h.auth(h.patchCategory))
	mux.HandleFunc("DELETE /api/v1/categories/{id}", h.auth(h.deleteCategory))

	mux.HandleFunc("GET /api/v1/galleries", h.auth(h.listGalleries))

	mux.HandleFunc("GET /api/v1/images/{id}/relations", h.auth(h.relationsForImage))
	mux.HandleFunc("POST /api/v1/relations", h.auth(h.addRelation))
	mux.HandleFunc("DELETE /api/v1/relations", h.auth(h.removeRelation))

	mux.HandleFunc("GET /api/v1/openapi.json", h.openAPIJSON)
	mux.HandleFunc("GET /api/v1/docs", h.openAPIDocs)

	mux.HandleFunc("GET /api/v1/", h.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/" {
			apiError(w, http.StatusNotFound, "not_found", "endpoint not found: "+r.URL.Path)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"api":     "monbooru",
			"version": h.version,
			"docs":    "/api/v1/docs",
			"openapi": "/api/v1/openapi.json",
		})
	}))
}

// auth wraps a handler with bearer-token authentication and the
// configured-base-URL CORS check.
func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		// Browsers always send Origin without a trailing slash; an
		// operator's base_url written as "http://host/" would otherwise
		// reject every CORS request with no obvious diagnostic.
		baseURL := strings.TrimRight(h.cfg.Server.BaseURL, "/")
		if origin != "" && baseURL != "" {
			if origin != baseURL {
				apiError(w, http.StatusForbidden, "forbidden", "CORS: origin not allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", baseURL)
		}

		if h.cfg.Auth.APIToken == "" {
			apiError(w, http.StatusServiceUnavailable, "api_disabled",
				"API is disabled: generate an API token in Settings to enable it")
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			apiError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid authorization header")
			return
		}
		token := auth[len(prefix):]
		if subtle.ConstantTimeCompare([]byte(token), []byte(h.cfg.Auth.APIToken)) != 1 {
			apiError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}

		next(w, r)
	}
}

// apiPathInt64 parses a numeric path segment, writing an
// invalid_request apiError on failure. The bool reports whether the
// caller can keep going.
func apiPathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid "+name)
		return 0, false
	}
	return v, true
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writePage emits the paginated envelope documented by paginatedSchema:
// {page, limit, total, results}.
func writePage(w http.ResponseWriter, page, limit, total int, results any) {
	writeJSON(w, http.StatusOK, map[string]any{
		"page":    page,
		"limit":   limit,
		"total":   total,
		"results": results,
	})
}

// parsePage reads page + limit from the query string and clamps limit
// to maxLimit. `page_size` is accepted as a synonym for `limit` so a
// caller using the more common page_size convention isn't silently
// clamped to defaultLimit.
func parsePage(r *http.Request, defaultLimit, maxLimit int) (offset, limit int) {
	page := 1
	limit = defaultLimit
	q := r.URL.Query()
	if p := q.Get("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			page = n
		}
	}
	l := q.Get("limit")
	if l == "" {
		l = q.Get("page_size")
	}
	if l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = min(n, maxLimit)
		}
	}
	return (page - 1) * limit, limit
}
