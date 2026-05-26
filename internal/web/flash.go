package web

import (
	"fmt"
	"html"
	"net/http"
)

// flashErr surfaces a validation / failure message in the right format
// for the caller's request mode. HTMX callers receive the inline flash
// fragment with the requested status so the swap target paints it;
// non-HTMX callers fall back to plain http.Error which sets a content
// type the browser renders as text.
func flashErr(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if isHTMXRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(code)
		fmt.Fprintf(w, `<div class="flash flash-err">%s</div>`, html.EscapeString(msg))
		return
	}
	http.Error(w, msg, code)
}
