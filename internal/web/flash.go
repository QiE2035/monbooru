package web

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"

	"github.com/leqwin/monbooru/internal/i18n"
	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
)

// localize returns the translated message for the given key. For messages
// with template data, pass a map as the second argument.
func localize(key string, data ...map[string]any) string {
	if len(data) > 0 {
		return i18n.Localizer().MustLocalize(&goi18n.LocalizeConfig{MessageID: key, TemplateData: data[0]})
	}
	return i18n.Localizer().MustLocalize(&goi18n.LocalizeConfig{MessageID: key})
}

// flashErr surfaces a validation / failure message in the right format
// for the caller's request mode. HTMX callers receive the inline flash
// fragment with the requested status so the swap target paints it;
// non-HTMX callers fall back to plain http.Error which sets a content
// type the browser renders as text.
func flashErr(w http.ResponseWriter, r *http.Request, msg string) {
	if isHTMXRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `<div class="flash flash-err">%s</div>`, html.EscapeString(msg))
		return
	}
	http.Error(w, msg, http.StatusBadRequest)
}

// setDialogSavedTrigger fires the JS-side dialog-close event ("tagger-saved"
// / "token-saved") naming the dialog, so one body listener per event serves
// every config dialog of that family.
func setDialogSavedTrigger(w http.ResponseWriter, event, dialogID string) {
	payload, _ := json.Marshal(map[string]any{event: map[string]any{"dialog": dialogID}})
	w.Header().Set("HX-Trigger", string(payload))
}

// writeOOBSummaryFlash swaps a summary span and a flash slot out-of-band in
// one fragment, the shared epilogue of the settings config dialogs.
func writeOOBSummaryFlash(w http.ResponseWriter, spanID, summary, flashID, msg string) {
	_, _ = fmt.Fprintf(w,
		`<span id="%s" hx-swap-oob="true">%s</span><div id="%s" hx-swap-oob="true"><div class="flash flash-ok">%s</div></div>`,
		html.EscapeString(spanID), html.EscapeString(summary), html.EscapeString(flashID), html.EscapeString(msg))
}
