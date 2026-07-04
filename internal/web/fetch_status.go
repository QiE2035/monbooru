package web

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// fetchStatusEntry is the last known outcome of a source metadata fetch for one
// image. State is "pending" while monloader works, then "ok" once the enrich
// lands.
type fetchStatusEntry struct {
	State string
	Msg   string
	At    time.Time
}

const (
	// fetchStatusTTL bounds how long a recorded outcome lingers, so a batch
	// fetch that no detail page polls can't grow the map without bound.
	fetchStatusTTL = 10 * time.Minute
	// fetchPollMax caps the pill's self-poll (~2s cadence) so a fetch that
	// never completes stops nagging instead of polling forever.
	fetchPollMax     = 30
	fetchPollDelayMs = 2000
)

func fetchStatusKey(gallery string, id int64) string {
	return gallery + "\x00" + strconv.FormatInt(id, 10)
}

// recordFetchStatus stores the latest fetch outcome for (gallery, id), pruning
// entries past fetchStatusTTL first.
func (s *Server) recordFetchStatus(gallery string, id int64, state, msg string) {
	s.fetchStatusMu.Lock()
	defer s.fetchStatusMu.Unlock()
	if s.fetchStatus == nil {
		s.fetchStatus = map[string]fetchStatusEntry{}
	}
	now := time.Now()
	for k, e := range s.fetchStatus {
		if now.Sub(e.At) > fetchStatusTTL {
			delete(s.fetchStatus, k)
		}
	}
	s.fetchStatus[fetchStatusKey(gallery, id)] = fetchStatusEntry{State: state, Msg: msg, At: now}
}

func (s *Server) loadFetchStatus(gallery string, id int64) (fetchStatusEntry, bool) {
	s.fetchStatusMu.Lock()
	defer s.fetchStatusMu.Unlock()
	e, ok := s.fetchStatus[fetchStatusKey(gallery, id)]
	return e, ok
}

func (s *Server) clearFetchStatus(gallery string, id int64) {
	s.fetchStatusMu.Lock()
	defer s.fetchStatusMu.Unlock()
	delete(s.fetchStatus, fetchStatusKey(gallery, id))
}

// writeFetchPending renders the "fetching..." pill into the target slot. Each
// render re-arms a delayed poll of fetchStatusHandler; n is the attempt count
// that fetchPollMax bounds.
func writeFetchPending(w http.ResponseWriter, id, n int64) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w,
		`<span class="fetch-status-msg monloader-accent" hx-get="/internal/images/%d/fetch-status?n=%d" hx-trigger="load delay:%dms" hx-swap="outerHTML">Fetching tags from source...</span>`,
		id, n+1, fetchPollDelayMs)
}

// fetchStatusHandler is the detail page's poll for a source fetch's outcome.
// While pending it re-emits the polling pill; on success it triggers a page
// reload so the freshly-applied tags render.
func (s *Server) fetchStatusHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	n, _ := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
	e, ok := s.loadFetchStatus(s.activeName, id)
	if !ok {
		// Nothing in flight (or already consumed): stop polling, clear the slot.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}
	switch e.State {
	case "pending":
		if n >= fetchPollMax {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			writeInlineFlash(w, "warn", "Still fetching from monloader; reload to check for new tags.")
			return
		}
		writeFetchPending(w, id, n)
	case "ok":
		// The refresh reloads the page so the applied tags render; the flash
		// rides the stash-and-show bridge to survive the reload.
		s.clearFetchStatus(s.activeName, id)
		msg := e.Msg
		if msg == "" {
			msg = "Fetched tags from the source."
		}
		setFlashHeader(w, msg, "ok", nil)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
	default:
		// Any other state is terminal: a hash mismatch or apply error from
		// enrich, or a code monloader reported for a fetch that failed before
		// it could enrich. Surface it inline and stop polling.
		s.clearFetchStatus(s.activeName, id)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		writeInlineFlash(w, "err", fetchFailureMessage(e.State, e.Msg))
	}
}

// fetchFailureMessage renders a terminal fetch state into operator-facing text.
// monloader reports a fetch that failed before it could enrich with one of its
// queue's stable error codes; map the actionable ones to a plain sentence and
// fall back to the recorded message otherwise (the enrich path already supplies
// a readable one for a hash mismatch or an apply error).
func fetchFailureMessage(state, msg string) string {
	switch state {
	case "unsupported_url":
		return "monloader can't fetch this source URL."
	case "network_unreachable":
		return "monloader couldn't reach the source."
	case "auth_required":
		return "The source needs a login monloader doesn't have."
	case "blocked":
		return "The source blocked monloader's fetch."
	case "rate_limited":
		return "The source is rate-limiting; try again later."
	case "download_failed":
		return "The source fetch failed on monloader."
	case "monbooru_unreachable", "monbooru_rejected":
		return "monloader fetched the source but couldn't apply the tags."
	case "mapping_failed":
		return "monloader couldn't read the source's metadata."
	}
	if msg != "" {
		return msg
	}
	return "The source fetch failed."
}
