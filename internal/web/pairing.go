package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
)

const (
	pairPendingTTL = 5 * time.Minute
	pairMaxPending = 16
)

type pairState string

const (
	pairPending  pairState = "pending"
	pairApproved pairState = "approved"
	pairDenied   pairState = "denied"
)

// pairReq is one in-flight pairing handshake, held in memory until the peer
// claims its issued token or the request ages out. Nothing is issued until the
// claim, so an approval the peer never collects mints no token.
type pairReq struct {
	ID        string
	App       string
	URL       string
	Source    string
	Scopes    []string
	PeerToken string
	State     pairState
	Claimed   bool
	CreatedAt time.Time
}

type pairStore struct {
	mu sync.Mutex
	m  map[string]*pairReq
}

func newPairStore() *pairStore { return &pairStore{m: map[string]*pairReq{}} }

func pairID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (ps *pairStore) sweepLocked() {
	cutoff := time.Now().Add(-pairPendingTTL)
	for id, r := range ps.m {
		if r.CreatedAt.Before(cutoff) {
			delete(ps.m, id)
		}
	}
}

// create records a pending request, capping the number outstanding.
func (ps *pairStore) create(app, url, source string, scopes []string, peerToken string) (string, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.sweepLocked()
	pending := 0
	for _, r := range ps.m {
		if r.State == pairPending {
			pending++
		}
	}
	if pending >= pairMaxPending {
		return "", false
	}
	id := pairID()
	ps.m[id] = &pairReq{ID: id, App: app, URL: url, Source: source, Scopes: scopes, PeerToken: peerToken, State: pairPending, CreatedAt: time.Now()}
	return id, true
}

func (ps *pairStore) listPending() []pairReq {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.sweepLocked()
	var out []pairReq
	for _, r := range ps.m {
		if r.State == pairPending {
			out = append(out, *r)
		}
	}
	return out
}

func (ps *pairStore) get(id string) (pairReq, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if r, ok := ps.m[id]; ok {
		return *r, true
	}
	return pairReq{}, false
}

// setState moves a pending request to approved or denied (operator action).
func (ps *pairStore) setState(id string, st pairState) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	r, ok := ps.m[id]
	if !ok || r.State != pairPending {
		return false
	}
	r.State = st
	return true
}

// claim transitions an approved request to claimed exactly once, returning the
// request and true only for the first caller.
func (ps *pairStore) claim(id string) (pairReq, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	r, ok := ps.m[id]
	if !ok || r.State != pairApproved || r.Claimed {
		return pairReq{}, false
	}
	r.Claimed = true
	return *r, true
}

// unclaim reverts a claim so a peer can retry after a failed token mint.
func (ps *pairStore) unclaim(id string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if r, ok := ps.m[id]; ok {
		r.Claimed = false
	}
}

func (ps *pairStore) remove(id string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.m, id)
}

func writePairJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// pairedWith reports whether a token issued to the given peer already exists.
func (s *Server) pairedWith(app string) bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	for _, t := range s.cfg.Auth.Tokens {
		if t.Paired == app {
			return true
		}
	}
	return false
}

// pairRequest receives a pairing offer from a companion app. It issues nothing;
// an operator approves it in Settings, after which the peer claims the token via
// pairStatus.
func (s *Server) pairRequest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		App             string   `json:"app"`
		URL             string   `json:"url"`
		RequestedScopes []string `json:"requested_scopes"`
		PeerToken       string   `json:"peer_token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body.App == "" {
		writePairJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request", "error": "app and a JSON body are required"})
		return
	}
	if s.pairedWith(body.App) {
		writePairJSON(w, http.StatusConflict, map[string]string{"code": "already_paired", "error": "already paired with " + body.App + "; remove the existing pairing first"})
		return
	}
	id, ok := s.pairs.create(body.App, body.URL, clientIP(r), body.RequestedScopes, body.PeerToken)
	if !ok {
		writePairJSON(w, http.StatusTooManyRequests, map[string]string{"code": "too_many_requests", "error": "too many pending pairing requests"})
		return
	}
	logx.Infof("pairing: request from %s (%s)", body.App, body.URL)
	writePairJSON(w, http.StatusOK, map[string]string{"request_id": id, "status": "pending"})
}

// pairStatus reports a request's state. On the first poll after approval it
// mints the peer's token, stores the reverse credentials, and returns the
// secret once.
func (s *Server) pairStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	req, ok := s.pairs.get(id)
	if !ok {
		writePairJSON(w, http.StatusNotFound, map[string]string{"code": "not_found", "error": "unknown pairing request"})
		return
	}
	if req.State != pairApproved {
		writePairJSON(w, http.StatusOK, map[string]string{"status": string(req.State)})
		return
	}
	claimed, won := s.pairs.claim(id)
	if !won {
		writePairJSON(w, http.StatusOK, map[string]string{"status": "approved"})
		return
	}
	secret, err := s.mintPairedToken(claimed)
	if err != nil {
		s.pairs.unclaim(id)
		writePairJSON(w, http.StatusInternalServerError, map[string]string{"code": "mint_failed", "error": err.Error()})
		return
	}
	s.pairs.remove(id)
	writePairJSON(w, http.StatusOK, map[string]string{"status": "approved", "token": secret})
}

// pairTeardown lets a paired peer drop the pairing on this side too, so one
// "remove pairing" tears down both ends. It removes only locally and never
// calls back, which would loop.
func (s *Server) pairTeardown(w http.ResponseWriter, r *http.Request) {
	secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.cfgMu.RLock()
	tok := s.cfg.FindTokenByHash(config.HashToken(secret))
	var paired string
	if tok != nil {
		paired = tok.Paired
	}
	s.cfgMu.RUnlock()
	if secret == "" || paired == "" {
		writePairJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized", "error": "pairing token required"})
		return
	}
	if err := s.removePairing(paired); err != nil {
		writePairJSON(w, http.StatusInternalServerError, map[string]string{"code": "remove_failed", "error": err.Error()})
		return
	}
	logx.Infof("pairing: %s removed the pairing remotely", paired)
	writePairJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// monloaderCallbackURL rewrites the address monloader advertised so its host is
// the source the pairing request came from, keeping the advertised scheme and
// port. monloader sends its base_url, which carries the right port but a host
// (usually localhost) that means nothing from monbooru's side; the source is
// where monbooru can actually reach it. Falls back to the advertised value when
// it can't be parsed or the source is unknown.
func monloaderCallbackURL(advertised, source string) string {
	source = strings.TrimSpace(source)
	u, err := url.Parse(strings.TrimSpace(advertised))
	if err != nil || u.Host == "" || source == "" {
		return advertised
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(source, port)
	} else {
		u.Host = source
	}
	return u.String()
}

// mintPairedToken issues the monbooru token the peer will carry, stores the
// reverse credentials the peer offered, and returns the new secret once. The
// address to call the peer back at is the source the request came from, unless
// an operator override is configured.
func (s *Server) mintPairedToken(req pairReq) (string, error) {
	scopes := filterScopes(req.Scopes)
	if len(scopes) == 0 {
		scopes = []string{config.ScopeRead, config.ScopeWrite}
	}
	tok, secret := config.GenerateToken(req.App+" (paired)", scopes)
	tok.Paired = req.App
	tok.PeerURL = monloaderCallbackURL(req.URL, req.Source)
	if err := s.withConfig(func(c *config.Config) error {
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		c.Monloader.APIToken = req.PeerToken
		return nil
	}); err != nil {
		return "", err
	}
	logx.Infof("pairing: issued token to %s", req.App)
	return secret, nil
}

func (s *Server) pairViewData(r *http.Request) map[string]any {
	return map[string]any{
		"Pending":   s.pairs.listPending(),
		"Paired":    s.pairedWith("monloader"),
		"PeerURL":   s.monloaderAPIBase(),
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	}
}

func (s *Server) monloaderPairingFragment(w http.ResponseWriter, r *http.Request) {
	data := s.pairViewData(r)
	paired, _ := data["Paired"].(bool)
	s.renderTemplate(w, "partials/monloader_pairing.html", data)
	// The poll carries the browser's last-rendered paired state; when it flips
	// (the peer claimed, or the pairing was removed) the token list needs a refresh too.
	if was := r.URL.Query().Get("paired"); was != "" && (was == "true") != paired {
		s.renderAuthTokensOOB(w, r)
	}
}

func (s *Server) monloaderPairApprove(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	id := r.PathValue("id")
	req, ok := s.pairs.get(id)
	if !ok {
		s.renderTemplate(w, "partials/monloader_pairing.html", s.pairViewData(r))
		return
	}
	// Probe the url monbooru will actually call (the configured override, else
	// the address the request came from) and refuse the pairing if unreachable.
	s.cfgMu.RLock()
	base := strings.TrimSpace(s.cfg.Monloader.APIURL)
	s.cfgMu.RUnlock()
	if base == "" {
		base = monloaderCallbackURL(req.URL, req.Source)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if !s.monloaderReachable(ctx, base) {
		msg := "no monloader api url to reach; pairing not completed."
		if base != "" {
			msg = "monloader is unreachable at " + base + "; pairing not completed. Check the api url and that monloader is running."
		}
		s.renderTemplate(w, "partials/monloader_pairing.html", s.pairViewData(r))
		writeFlashOOB(w, "flash-monloader", "warn", msg)
		return
	}
	s.pairs.setState(id, pairApproved)
	logx.Infof("pairing: approved request %s from %s", id, clientIP(r))
	s.renderTemplate(w, "partials/monloader_pairing.html", s.pairViewData(r))
	writeFlashOOB(w, "flash-monloader", "", "")
}

func (s *Server) monloaderPairDeny(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.pairs.setState(r.PathValue("id"), pairDenied)
	s.renderTemplate(w, "partials/monloader_pairing.html", s.pairViewData(r))
}

func (s *Server) monloaderPairRemove(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	notifyErr := s.teardownMonloaderPairing(r)
	s.renderTemplate(w, "partials/monloader_pairing.html", s.pairViewData(r))
	s.renderAuthTokensOOB(w, r)
	if notifyErr != nil {
		writeFlashOOB(w, "flash-monloader", "warn", "Removed here, but could not reach monloader - remove the pairing in monloader too.")
	}
}

// monloaderLightDisconnect pauses the monloader link from the footer light's
// kill switch: it suspends every call to monloader without dropping the
// pairing, so the operator can cut the link and later resume it from the same
// light with no re-pair.
func (s *Server) monloaderLightDisconnect(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if s.pairedWith("monloader") {
		if err := s.setMonloaderPaused(true); err != nil {
			logx.Errorf("pairing: pause failed: %v", err)
		}
		logx.Infof("pairing: monloader link paused from %s", clientIP(r))
	}
	s.renderMonloaderLight(w, r, "paused", "")
}

// monloaderLightReconnect lifts the pause, resuming connectivity with the
// credentials that stayed on disk. The light renders "checking" and its load
// poll probes monloader within a second.
func (s *Server) monloaderLightReconnect(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if err := s.setMonloaderPaused(false); err != nil {
		logx.Errorf("pairing: resume failed: %v", err)
	}
	logx.Infof("pairing: monloader link resumed from %s", clientIP(r))
	s.renderMonloaderLight(w, r, "", "")
}

// setMonloaderPaused persists the footer light's pause flag.
func (s *Server) setMonloaderPaused(paused bool) error {
	return s.withConfig(func(c *config.Config) error {
		c.Monloader.Paused = paused
		return nil
	})
}

// renderMonloaderLight writes the footer light in the given connection state.
func (s *Server) renderMonloaderLight(w http.ResponseWriter, r *http.Request, conn, version string) {
	s.renderTemplate(w, "partials/monloader_light.html", map[string]any{
		"MonloaderConn":    conn,
		"MonloaderVersion": version,
		"MonloaderURL":     s.monloaderWebBase(),
		"CSRFToken":        s.csrfToken(sessionFromContext(r.Context())),
	})
}

// teardownMonloaderPairing drops this side of the monloader pairing and
// notifies the peer, returning the notify error (nil on success). Shared by
// the settings unpair and the footer light's kill switch.
func (s *Server) teardownMonloaderPairing(r *http.Request) error {
	peerURL := s.monloaderAPIBase()
	s.cfgMu.RLock()
	peerToken := s.cfg.Monloader.APIToken
	s.cfgMu.RUnlock()
	if err := s.removePairing("monloader"); err != nil {
		logx.Errorf("pairing: remove failed: %v", err)
	}
	notifyErr := s.notifyMonloaderTeardown(peerURL, peerToken)
	if notifyErr != nil {
		logx.Errorf("pairing: could not notify monloader of teardown: %v", notifyErr)
	}
	logx.Infof("pairing: removed monloader pairing from %s", clientIP(r))
	return notifyErr
}

// removePairing tears down this side of a pairing: it drops the pairing token
// and the credential monbooru uses to authenticate to monloader, but keeps the
// configured api_url so an operator's URL survives an unpair/re-pair cycle.
func (s *Server) removePairing(app string) error {
	return s.withConfig(func(c *config.Config) error {
		c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t config.Token) bool { return t.Paired == app })
		c.Monloader.APIToken = ""
		return nil
	})
}
