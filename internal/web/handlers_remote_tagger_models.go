package web

import (
	"context"
	"fmt"
	"html"
	"net/http"

	"github.com/monbooru/monbooru/internal/tagger"
)

// remoteTaggerModelsFragment renders the <option> list for the
// auto-tag dialogs' remote-server branch: a default "All models on
// server" entry followed by every model the paired A-side currently
// enables. The list always mirrors the server's live settings, so a
// model disabled there disappears on the next load. Servers that do
// not advertise a list (older protocol, or the server unreachable)
// degrade to the all-models entry with a hint.
func (s *Server) remoteTaggerModelsFragment(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), tagger.RemoteTaggerListTimeout)
	defer cancel()

	models, err := tagger.RemoteTaggerList(ctx, s.cfg)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		fmt.Fprintf(w, `<option value="">All models on server</option>`)
		fmt.Fprintf(w, `<option disabled>Cannot reach server: %s</option>`, html.EscapeString(err.Error()))
		return
	}
	if len(models) == 0 {
		fmt.Fprintf(w, `<option value="">All models on server</option>`)
		fmt.Fprintf(w, `<option disabled>Server advertises no model list</option>`)
		return
	}
	fmt.Fprintf(w, `<option value="">All models on server</option>`)
	for _, m := range models {
		label := m.Name
		if m.Description != "" {
			label += " — " + m.Description
		}
		fmt.Fprintf(w, `<option value="%s">%s</option>`, html.EscapeString(m.Name), html.EscapeString(label))
	}
}
