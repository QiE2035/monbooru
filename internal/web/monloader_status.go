package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// monloaderClient is the only outbound HTTP client monbooru uses, and it is
// only ever pointed at the configured monloader (SPECIFICATIONS.md 14.5).
var monloaderClient = &http.Client{Timeout: 4 * time.Second}

// notifyMonloaderTeardown asks monloader to drop its side of the pairing and
// returns an error when it could not be reached, so the caller can tell the
// operator to remove the far end by hand.
func (s *Server) notifyMonloaderTeardown(baseURL, token string) error {
	base := strings.TrimRight(baseURL, "/")
	if base == "" || token == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/pair/remove", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := monloaderClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("monloader returned %s", resp.Status)
	}
	return nil
}

// EnqueueMetadataFetch asks monloader to re-read the post at url (metadata
// only, no download) and enrich monbooru image imageID in gallery. All the
// work - gallery-dl, mapping, the enrich call back into monbooru - runs on
// monloader; monbooru only enqueues, keeping its single-egress model intact.
func (s *Server) EnqueueMetadataFetch(ctx context.Context, imageID int64, gallery, url string) error {
	base := strings.TrimRight(s.monloaderAPIBase(), "/")
	s.cfgMu.RLock()
	token := s.cfg.Monloader.APIToken
	s.cfgMu.RUnlock()
	if base == "" || token == "" {
		return fmt.Errorf("monloader is not configured")
	}
	body, _ := json.Marshal(map[string]any{"image_id": imageID, "gallery": gallery, "url": url})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/metadata", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := monloaderClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("monloader returned %s", resp.Status)
	}
	return nil
}

// errPTRUnavailable marks a lookup monloader refused because its PTR backend
// is off - a stale capability read, not a connectivity failure.
var errPTRUnavailable = errors.New("the PTR lookup is unavailable on monloader")

// EnqueueHashLookup asks monloader to find tags for image imageID by file
// hash - backend "booru" walks the opted-in sites' md5 search, backend "ptr"
// queries monloader's local PTR index by sha256 - and enrich the image back
// through the same callbacks a source refetch uses.
func (s *Server) EnqueueHashLookup(ctx context.Context, imageID int64, gallery, backend, md5, sha256 string) error {
	base := strings.TrimRight(s.monloaderAPIBase(), "/")
	s.cfgMu.RLock()
	token := s.cfg.Monloader.APIToken
	s.cfgMu.RUnlock()
	if base == "" || token == "" {
		return fmt.Errorf("monloader is not configured")
	}
	body, _ := json.Marshal(map[string]any{
		"image_id": imageID, "gallery": gallery, "backend": backend, "md5": md5, "sha256": sha256,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/lookup", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := monloaderClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		return errPTRUnavailable
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("monloader returned %s", resp.Status)
	}
	return nil
}

// ptrTagInfo is one tag's answer from monloader's PTR graph query, in
// monbooru-form names (bare or category:name).
type ptrTagInfo struct {
	Known        bool     `json:"known"`
	Ideal        string   `json:"ideal"`
	Aliases      []string `json:"aliases"`
	Implications []string `json:"implications"`
}

// ptrTagLookup asks monloader's PTR index for the alias / implication graph
// of the given monbooru-form tag names (at most ptrLookupBatch per call).
func (s *Server) ptrTagLookup(ctx context.Context, names []string) (map[string]ptrTagInfo, error) {
	base := strings.TrimRight(s.monloaderAPIBase(), "/")
	s.cfgMu.RLock()
	token := s.cfg.Monloader.APIToken
	s.cfgMu.RUnlock()
	if base == "" || token == "" {
		return nil, fmt.Errorf("monloader is not configured")
	}
	body, _ := json.Marshal(map[string]any{"tags": names})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/ptr/tags", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := monloaderClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		return nil, errPTRUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("monloader returned %s", resp.Status)
	}
	var out struct {
		Results map[string]ptrTagInfo `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// monloaderAPIBase is the address monbooru calls monloader at: the operator's
// configured override when set, otherwise the address discovered during pairing
// (the source the request came from, stored on the paired token).
func (s *Server) monloaderAPIBase() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if u := strings.TrimSpace(s.cfg.Monloader.APIURL); u != "" {
		return u
	}
	for _, t := range s.cfg.Auth.Tokens {
		if t.Paired == "monloader" {
			return t.PeerURL
		}
	}
	return ""
}

// checkMonloader probes the configured monloader for the footer light: /health
// for up/down + version, then one authed read to surface a revoked token. The
// PTR capability rides the same probe: the lookup buttons only need a
// fresh-ish answer, and monloader 409s a lookup sent on a stale "enabled".
func (s *Server) checkMonloader(ctx context.Context) (status, version string, ptrEnabled, ptrSyncing bool) {
	base := strings.TrimRight(s.monloaderAPIBase(), "/")
	if base == "" {
		return "", "", false, false
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return "down", "", false, false
	}
	hresp, err := monloaderClient.Do(hreq)
	if err != nil {
		return "down", "", false, false
	}
	defer func() { _ = hresp.Body.Close() }()
	if hresp.StatusCode != http.StatusOK {
		return "down", "", false, false
	}
	var h struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(hresp.Body).Decode(&h)
	s.cfgMu.RLock()
	tok := s.cfg.Monloader.APIToken
	s.cfgMu.RUnlock()
	if tok != "" {
		qreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/queue?limit=1", nil)
		qreq.Header.Set("Authorization", "Bearer "+tok)
		if qresp, qerr := monloaderClient.Do(qreq); qerr == nil {
			defer func() { _ = qresp.Body.Close() }()
			if qresp.StatusCode == http.StatusUnauthorized || qresp.StatusCode == http.StatusForbidden {
				return "rejected", h.Version, false, false
			}
		}
		preq, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/ptr/status", nil)
		preq.Header.Set("Authorization", "Bearer "+tok)
		if presp, perr := monloaderClient.Do(preq); perr == nil {
			var p struct {
				Enabled bool   `json:"enabled"`
				State   string `json:"state"`
			}
			_ = json.NewDecoder(presp.Body).Decode(&p)
			_ = presp.Body.Close()
			ptrEnabled = presp.StatusCode == http.StatusOK && p.Enabled
			ptrSyncing = ptrEnabled && p.State == "syncing"
		}
	}
	return "ok", h.Version, ptrEnabled, ptrSyncing
}

// monloaderReachable reports whether monloader answers a health probe at base.
// Approving a pairing monbooru can't reach would leave a dead pairing (no light,
// no refetch), so the operator is blocked until the api url responds.
func (s *Server) monloaderReachable(ctx context.Context, base string) bool {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := monloaderClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// monloaderStatusTTL bounds how often the footer light re-probes monloader, so
// a burst of navigations (each firing the light's load poll) reuses one probe
// instead of fanning out into a probe per page. Kept under the 15s poll cadence
// so a page left open still refreshes on schedule.
const monloaderStatusTTL = 10 * time.Second

// monloaderStatusSeed returns the last cached probe result without probing, for
// seeding a page's initial light render so it shows its last known state rather
// than "checking". A cold cache yields "", which the partial renders as
// "checking monloader". The PTR flag seeds the lookup buttons the same way: a
// cold cache hides them until the light's first poll lands.
func (s *Server) monloaderStatusSeed() (status, version string, ptrEnabled, ptrSyncing bool) {
	s.monloaderStatusMu.Lock()
	defer s.monloaderStatusMu.Unlock()
	return s.monloaderConn, s.monloaderVersion, s.monloaderPTR, s.monloaderPTRSyncing
}

// monloaderStatusCached probes monloader at most once per monloaderStatusTTL and
// serves the cached result otherwise, so the light's per-navigation poll does
// not re-probe on every page load. The probe runs without the lock held so a
// slow monloader never serializes concurrent page renders.
func (s *Server) monloaderStatusCached(ctx context.Context) (status, version string) {
	s.monloaderStatusMu.Lock()
	if s.monloaderConn != "" && time.Since(s.monloaderCheckedAt) < monloaderStatusTTL {
		status, version = s.monloaderConn, s.monloaderVersion
		s.monloaderStatusMu.Unlock()
		return status, version
	}
	s.monloaderStatusMu.Unlock()

	status, version, ptrEnabled, ptrSyncing := s.checkMonloader(ctx)

	s.monloaderStatusMu.Lock()
	s.monloaderConn, s.monloaderVersion, s.monloaderPTR, s.monloaderPTRSyncing, s.monloaderCheckedAt = status, version, ptrEnabled, ptrSyncing, time.Now()
	s.monloaderStatusMu.Unlock()
	return status, version
}

func (s *Server) monloaderStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !s.pairedWith("monloader") {
		// Stop polling and clear the light once the pairing is gone.
		_, _ = w.Write([]byte(`<span id="monloader-light"></span>`))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	status, version := s.monloaderStatusCached(ctx)
	s.renderTemplate(w, "partials/monloader_light.html", map[string]any{
		"MonloaderConn":    status,
		"MonloaderVersion": version,
		"MonloaderURL":     s.monloaderWebBase(),
	})
}
