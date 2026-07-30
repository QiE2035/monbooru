package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
)

// remoteTaggerPairingFragment renders the remote tagger pairing panel.
// Used by the Settings page and the htmx poll loop.
func (s *Server) remoteTaggerPairingFragment(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "partials/remote_tagger_pairing.html", s.remoteTaggerPairViewData("", r))
}

// remoteTaggerPairPost initiates outbound pairing to device A.
func (s *Server) remoteTaggerPairPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	serverURL := strings.TrimRight(strings.TrimSpace(r.FormValue("server_url")), "/")
	if serverURL == "" {
		s.renderTemplate(w, "partials/remote_tagger_pairing.html", s.remoteTaggerPairViewData("Server URL is required", r))
		return
	}

	s.cfgMu.RLock()
	baseURL := s.cfg.Server.BaseURL
	s.cfgMu.RUnlock()

	body, _ := json.Marshal(map[string]any{
		"app":              "remote_tagger",
		"url":              baseURL,
		"requested_scopes": []string{"tag"},
		"peer_token":       "",
	})
	resp, err := http.Post(serverURL+"/api/v1/pair/request", "application/json", bytes.NewReader(body))
	if err != nil {
		s.renderTemplate(w, "partials/remote_tagger_pairing.html",
			s.remoteTaggerPairViewData("Cannot reach server: "+err.Error(), r))
		return
	}
	defer resp.Body.Close()

	var result struct {
		RequestID string `json:"request_id"`
		Status    string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.RequestID == "" {
		s.renderTemplate(w, "partials/remote_tagger_pairing.html",
			s.remoteTaggerPairViewData("Unexpected response from server", r))
		return
	}

	s.remoteTaggerPairMu.Lock()
	s.remoteTaggerRequestID = result.RequestID
	s.remoteTaggerServerURL = serverURL
	s.remoteTaggerPairMu.Unlock()

	s.renderTemplate(w, "partials/remote_tagger_pairing.html", map[string]any{
		"Paired":    false,
		"Pairing":   true,
		"PeerURL":   serverURL,
		"PairError": "",
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	})
}

// remoteTaggerPairStatus is the htmx poll target. Once approved on A,
// it claims the token and saves to config.
func (s *Server) remoteTaggerPairStatus(w http.ResponseWriter, r *http.Request) {
	s.remoteTaggerPairMu.Lock()
	requestID := s.remoteTaggerRequestID
	serverURL := s.remoteTaggerServerURL
	s.remoteTaggerPairMu.Unlock()

	if requestID == "" || serverURL == "" {
		s.remoteTaggerPairingFragment(w, r)
		return
	}

	resp, err := http.Get(serverURL + "/api/v1/pair/status?id=" + requestID)
	if err != nil {
		s.renderTemplate(w, "partials/remote_tagger_pairing.html",
			s.remoteTaggerPairViewData("", r))
		return
	}
	defer resp.Body.Close()

	var result struct {
		Status string `json:"status"`
		Token  string `json:"token,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		s.renderTemplate(w, "partials/remote_tagger_pairing.html",
			s.remoteTaggerPairViewData("", r))
		return
	}

	if result.Status != "approved" || result.Token == "" {
		s.renderTemplate(w, "partials/remote_tagger_pairing.html", map[string]any{
			"Paired":    false,
			"Pairing":   true,
			"PeerURL":   serverURL,
			"PairError": "",
			"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
		})
		return
	}

	if err := s.withConfig(func(c *config.Config) error {
		c.Tagger.RemoteClient.URL = serverURL
		c.Tagger.RemoteClient.Token = result.Token
		return nil
	}); err != nil {
		s.renderTemplate(w, "partials/remote_tagger_pairing.html",
			s.remoteTaggerPairViewData("Failed to save pairing: "+err.Error(), r))
		return
	}

	s.remoteTaggerPairMu.Lock()
	s.remoteTaggerRequestID = ""
	s.remoteTaggerServerURL = ""
	s.remoteTaggerPairMu.Unlock()

	logx.Infof("remote-tagger: paired with %s", serverURL)
	s.renderTemplate(w, "partials/remote_tagger_pairing.html", map[string]any{
		"Paired":    true,
		"Pairing":   false,
		"PeerURL":   serverURL,
		"PairError": "",
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	})
}

// remoteTaggerUnpairPost tears down the pairing on both sides.
func (s *Server) remoteTaggerUnpairPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}

	s.cfgMu.RLock()
	peerURL := s.cfg.Tagger.RemoteClient.URL
	token := s.cfg.Tagger.RemoteClient.Token
	s.cfgMu.RUnlock()

	if peerURL != "" && token != "" {
		req, _ := http.NewRequest("POST", peerURL+"/api/v1/pair/remove", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		http.DefaultClient.Do(req)
	}

	if err := s.removePairing("remote_tagger"); err != nil {
		logx.Errorf("remote-tagger: remove failed: %v", err)
	}

	s.renderTemplate(w, "partials/remote_tagger_pairing.html", map[string]any{
		"Paired":    false,
		"Pairing":   false,
		"PeerURL":   "",
		"PairError": "",
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	})
}

func (s *Server) remoteTaggerAdminViewData(r *http.Request) map[string]any {
	return map[string]any{
		"Pending":   s.pairs.listPendingByApp("remote_tagger"),
		"Paired":    s.pairedWith("remote_tagger"),
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	}
}

func (s *Server) remoteTaggerPendingFragment(w http.ResponseWriter, r *http.Request) {
	data := s.remoteTaggerAdminViewData(r)
	s.renderTemplate(w, "partials/remote_tagger_admin.html", data)
}

func (s *Server) remoteTaggerPendingApprove(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	id := r.PathValue("id")
	if _, ok := s.pairs.get(id); !ok {
		s.renderTemplate(w, "partials/remote_tagger_admin.html", s.remoteTaggerAdminViewData(r))
		return
	}
	s.pairs.setState(id, pairApproved)
	logx.Infof("pairing: approved remote-tagger request %s from %s", id, clientIP(r))
	s.renderTemplate(w, "partials/remote_tagger_admin.html", s.remoteTaggerAdminViewData(r))
	writeFlashOOB(w, "flash-tagger", "", "")
}

func (s *Server) remoteTaggerPendingDeny(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.pairs.setState(r.PathValue("id"), pairDenied)
	s.renderTemplate(w, "partials/remote_tagger_admin.html", s.remoteTaggerAdminViewData(r))
}

func (s *Server) remoteTaggerAdminUnpairPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.cfgMu.RLock()
	peerURL := ""
	secret := ""
	for _, t := range s.cfg.Auth.Tokens {
		if t.Paired == "remote_tagger" {
			secret = t.ID
			peerURL = t.PeerURL
			break
		}
	}
	s.cfgMu.RUnlock()
	if peerURL != "" && secret != "" {
		req, _ := http.NewRequest("POST", peerURL+"/api/v1/pair/remove", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		http.DefaultClient.Do(req)
	}
	if err := s.removePairing("remote_tagger"); err != nil {
		logx.Errorf("remote-tagger: admin remove failed: %v", err)
	}
	s.renderTemplate(w, "partials/remote_tagger_admin.html", s.remoteTaggerAdminViewData(r))
}

func (s *Server) remoteTaggerPairViewData(errMsg string, r *http.Request) map[string]any {
	s.cfgMu.RLock()
	paired := s.cfg.Tagger.RemoteClient.URL != "" && s.cfg.Tagger.RemoteClient.Token != ""
	peerURL := s.cfg.Tagger.RemoteClient.URL
	s.cfgMu.RUnlock()

	s.remoteTaggerPairMu.Lock()
	pairing := s.remoteTaggerRequestID != ""
	s.remoteTaggerPairMu.Unlock()

	return map[string]any{
		"Paired":    paired,
		"Pairing":   pairing,
		"PeerURL":   peerURL,
		"PairError": errMsg,
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	}
}
