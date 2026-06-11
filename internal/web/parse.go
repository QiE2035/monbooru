package web

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/leqwin/monbooru/internal/tagger"
)

// pathInt64 parses a numeric path segment, writing 404 on failure.
func pathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return v, true
}

// pathTaggerName trims the named path segment and validates it through
// tagger.ValidateTaggerName, writing a 400 plain-text error on failure.
func pathTaggerName(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	v := strings.TrimSpace(r.PathValue(name))
	if err := tagger.ValidateTaggerName(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", false
	}
	return v, true
}

// formInt64 parses an integer form field, writing the error flash on
// failure. Status 200 (rather than 400) so HTMX picks the body up and
// swaps it into the dialog target the caller hands it; default config
// drops 4xx swaps and the operator would otherwise see no feedback.
func formInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `<div class="flash flash-err">Missing %s.</div>`, html.EscapeString(name))
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `<div class="flash flash-err">Invalid %s.</div>`, html.EscapeString(name))
		return 0, false
	}
	return v, true
}
