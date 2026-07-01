package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/config"
)

func TestPairingHandshake(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	post := func(body string) (int, map[string]string) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/pair/request", strings.NewReader(body)))
		var out map[string]string
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}
	status := func(id string) (int, map[string]string) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/pair/status?id="+id, nil))
		var out map[string]string
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}

	body := `{"app":"monloader","url":"http://ml:8081","requested_scopes":["read","write"],"peer_token":"ml-secret"}`
	code, out := post(body)
	if code != http.StatusOK || out["request_id"] == "" {
		t.Fatalf("request: code=%d out=%v", code, out)
	}
	id := out["request_id"]

	if _, st := status(id); st["status"] != "pending" {
		t.Fatalf("status before approval = %v, want pending", st)
	}

	// Simulate the operator approving in Settings.
	if !srv.pairs.setState(id, pairApproved) {
		t.Fatal("setState approved failed")
	}

	code, st := status(id)
	if code != http.StatusOK || st["status"] != "approved" || len(st["token"]) != 32 {
		t.Fatalf("claim: code=%d st=%v", code, st)
	}
	if !srv.pairedWith("monloader") {
		t.Error("no paired token after claim")
	}
	if srv.cfg.Monloader.APIToken != "ml-secret" {
		t.Errorf("reverse token not stored: %+v", srv.cfg.Monloader)
	}
	// The reachable address is derived from the request source (192.0.2.1 from
	// httptest) and the advertised port, not the advertised host.
	if got := srv.monloaderAPIBase(); got != "http://192.0.2.1:8081" {
		t.Errorf("api base not derived from request source: %q", got)
	}
	tok := srv.cfg.FindTokenByHash(config.HashToken(st["token"]))
	if tok == nil || !tok.HasScope(config.ScopeWrite) || tok.HasScope(config.ScopeDelete) {
		t.Errorf("issued token scopes wrong: %+v", tok)
	}

	// Second poll does not re-issue.
	if _, st2 := status(id); st2["token"] != "" {
		t.Errorf("token re-delivered on second poll: %v", st2)
	}

	// Re-pair guard: a new request for monloader is rejected.
	if code, _ := post(body); code != http.StatusConflict {
		t.Errorf("re-pair guard code = %d, want 409", code)
	}
}

func TestPairStoreUnclaimAllowsRetry(t *testing.T) {
	ps := newPairStore()
	id, ok := ps.create("monloader", "http://ml:8081", "192.0.2.1", []string{"read"}, "ml-secret")
	if !ok {
		t.Fatal("create failed")
	}
	if !ps.setState(id, pairApproved) {
		t.Fatal("approve failed")
	}
	if _, won := ps.claim(id); !won {
		t.Fatal("first claim should win")
	}
	if _, won := ps.claim(id); won {
		t.Fatal("second claim should not win while claimed")
	}
	ps.unclaim(id)
	if _, won := ps.claim(id); !won {
		t.Fatal("claim after unclaim should win so a failed mint can retry")
	}
}

func TestPairingApproveAndRemove(t *testing.T) {
	// monloader answers /health so approve passes the reachability gate, but it
	// has no teardown endpoint, so removing still warns the operator.
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer peer.Close()

	srv := newTestServer(t)
	h := srv.Handler()

	rr := httptest.NewRecorder()
	preq := httptest.NewRequest("POST", "/api/v1/pair/request",
		strings.NewReader(`{"app":"monloader","url":"`+peer.URL+`","requested_scopes":["read","write"],"peer_token":"sek"}`))
	// The derived callback uses the request source as the host, so point it at
	// the loopback test peer for the reachability probe to hit.
	preq.RemoteAddr = "127.0.0.1:54321"
	h.ServeHTTP(rr, preq)
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	id := out["request_id"]

	// Approve through the operator handler (direct call bypasses CSRF middleware).
	areq := httptest.NewRequest("POST", "/settings/monloader/pair/"+id+"/approve", nil)
	areq.SetPathValue("id", id)
	srv.monloaderPairApprove(httptest.NewRecorder(), areq)

	// The initiator claims, minting the token.
	srr := httptest.NewRecorder()
	h.ServeHTTP(srr, httptest.NewRequest("GET", "/api/v1/pair/status?id="+id, nil))
	var st map[string]string
	_ = json.Unmarshal(srr.Body.Bytes(), &st)
	if len(st["token"]) != 32 || !srv.pairedWith("monloader") {
		t.Fatalf("approve+claim failed: %v", st)
	}

	// Remove tears down both directions and refreshes the token list inline.
	rmr := httptest.NewRecorder()
	srv.monloaderPairRemove(rmr, httptest.NewRequest("POST", "/settings/monloader/pair/remove", nil))
	if srv.pairedWith("monloader") || srv.cfg.Monloader.APIToken != "" {
		t.Errorf("remove did not tear down: paired=%v creds=%+v", srv.pairedWith("monloader"), srv.cfg.Monloader)
	}
	if !strings.Contains(rmr.Body.String(), `id="auth-tokens"`) || !strings.Contains(rmr.Body.String(), "hx-swap-oob") {
		t.Errorf("remove should OOB-refresh the token list, got %q", rmr.Body.String())
	}
	// The peer has no teardown endpoint, so the operator is warned to remove
	// the far end by hand.
	if !strings.Contains(rmr.Body.String(), "could not reach monloader") {
		t.Errorf("unreachable peer should warn the operator, got %q", rmr.Body.String())
	}
}

func TestMonloaderAPIURLEditableWhenPaired(t *testing.T) {
	readonly := func(body string) bool {
		i := strings.Index(body, `id="monloader_api_url"`)
		if i < 0 {
			t.Fatal("api url field not found")
		}
		tag := body[i:]
		if e := strings.IndexByte(tag, '>'); e >= 0 {
			tag = tag[:e]
		}
		return strings.Contains(tag, "readonly")
	}

	// The api url is an optional override, editable whether or not a pairing
	// exists; the auto-detected address lives on the paired token, not here.
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/settings", nil))
	if readonly(rr.Body.String()) {
		t.Error("api url should be editable when not paired")
	}

	tok, _ := config.GenerateToken("monloader (paired)", config.AllScopes)
	tok.Paired = "monloader"
	srv.cfg.Auth.Tokens = []config.Token{tok}
	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, httptest.NewRequest("GET", "/settings", nil))
	if readonly(rr2.Body.String()) {
		t.Error("api url override should stay editable when paired")
	}
}

func TestPairingFragmentRefreshesTokensOnTransition(t *testing.T) {
	srv := newTestServer(t)
	tok, _ := config.GenerateToken("monloader (paired)", config.AllScopes)
	tok.Paired = "monloader"
	srv.cfg.Auth.Tokens = []config.Token{tok}

	// Browser last saw unpaired; the poll reports the flip, so the token list refreshes.
	rr := httptest.NewRecorder()
	srv.monloaderPairingFragment(rr, httptest.NewRequest("GET", "/internal/monloader-pairing?paired=false", nil))
	if body := rr.Body.String(); !strings.Contains(body, `id="auth-tokens"`) || !strings.Contains(body, "hx-swap-oob") {
		t.Errorf("transition to paired should OOB-refresh the token list, got %q", body)
	}

	// No change: no out-of-band token refresh.
	rr2 := httptest.NewRecorder()
	srv.monloaderPairingFragment(rr2, httptest.NewRequest("GET", "/internal/monloader-pairing?paired=true", nil))
	if strings.Contains(rr2.Body.String(), "hx-swap-oob") {
		t.Errorf("no state change should not OOB the token list, got %q", rr2.Body.String())
	}
}

func TestPairingFragmentStopsPollingWhenPaired(t *testing.T) {
	srv := newTestServer(t)

	// Unpaired: the panel polls so it surfaces new requests and the claim flip.
	rr := httptest.NewRecorder()
	srv.monloaderPairingFragment(rr, httptest.NewRequest("GET", "/internal/monloader-pairing?paired=false", nil))
	if !strings.Contains(rr.Body.String(), "every 3s") {
		t.Errorf("unpaired panel should poll, got %q", rr.Body.String())
	}

	// Paired: no poll, so a confirm-gated Remove isn't detached mid-dialog by a
	// poll swap (which would drop the first click).
	tok, _ := config.GenerateToken("monloader (paired)", config.AllScopes)
	tok.Paired = "monloader"
	srv.cfg.Auth.Tokens = []config.Token{tok}
	rr2 := httptest.NewRecorder()
	srv.monloaderPairingFragment(rr2, httptest.NewRequest("GET", "/internal/monloader-pairing?paired=true", nil))
	if strings.Contains(rr2.Body.String(), "every 3s") {
		t.Errorf("paired panel must not poll, got %q", rr2.Body.String())
	}
}

func TestRemovePairingKeepsAPIURL(t *testing.T) {
	srv := newTestServer(t)
	tok, _ := config.GenerateToken("monloader (paired)", config.AllScopes)
	tok.Paired = "monloader"
	srv.cfg.Auth.Tokens = []config.Token{tok}
	srv.cfg.Monloader.APIURL = "http://10.3.2.10:18081"
	srv.cfg.Monloader.APIToken = "peer-secret"

	if err := srv.removePairing("monloader"); err != nil {
		t.Fatal(err)
	}
	if srv.pairedWith("monloader") || srv.cfg.Monloader.APIToken != "" {
		t.Error("remove should drop the pairing token and credential")
	}
	if srv.cfg.Monloader.APIURL != "http://10.3.2.10:18081" {
		t.Errorf("remove dropped the configured api_url: %q", srv.cfg.Monloader.APIURL)
	}
}

func TestPairingPreservesConfiguredAPIURL(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer peer.Close()

	srv := newTestServer(t)
	srv.cfg.Monloader.APIURL = peer.URL // operator-configured
	h := srv.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/pair/request",
		strings.NewReader(`{"app":"monloader","url":"http://localhost:8081","requested_scopes":["read","write"],"peer_token":"sek"}`)))
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	id := out["request_id"]

	areq := httptest.NewRequest("POST", "/settings/monloader/pair/"+id+"/approve", nil)
	areq.SetPathValue("id", id)
	srv.monloaderPairApprove(httptest.NewRecorder(), areq)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/pair/status?id="+id, nil))

	if !srv.pairedWith("monloader") {
		t.Fatal("pairing did not complete")
	}
	if srv.cfg.Monloader.APIURL != peer.URL {
		t.Errorf("pairing overwrote the configured api_url: %q", srv.cfg.Monloader.APIURL)
	}
}

func TestPairingApproveBlockedWhenUnreachable(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	rr := httptest.NewRecorder()
	preq := httptest.NewRequest("POST", "/api/v1/pair/request",
		strings.NewReader(`{"app":"monloader","url":"http://127.0.0.1:1","requested_scopes":["read","write"],"peer_token":"sek"}`))
	preq.RemoteAddr = "127.0.0.1:54321"
	h.ServeHTTP(rr, preq)
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	id := out["request_id"]

	arr := httptest.NewRecorder()
	areq := httptest.NewRequest("POST", "/settings/monloader/pair/"+id+"/approve", nil)
	areq.SetPathValue("id", id)
	srv.monloaderPairApprove(arr, areq)
	if !strings.Contains(arr.Body.String(), "unreachable") {
		t.Errorf("approve should warn when monloader is unreachable, got %q", arr.Body.String())
	}

	// The request stays pending, so a claim mints nothing.
	if st, _ := srv.pairs.get(id); st.State != pairPending {
		t.Errorf("request should stay pending, got %q", st.State)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/pair/status?id="+id, nil))
	if srv.pairedWith("monloader") {
		t.Error("unreachable monloader must not become paired")
	}
}

func TestPairingRemoveNotifiesPeer(t *testing.T) {
	var gotPath, gotAuth string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
	}))
	defer peer.Close()

	srv := newTestServer(t)
	tok, _ := config.GenerateToken("monloader (paired)", config.AllScopes)
	tok.Paired = "monloader"
	srv.cfg.Auth.Tokens = []config.Token{tok}
	srv.cfg.Monloader.APIURL = peer.URL
	srv.cfg.Monloader.APIToken = "peer-secret"

	rmr := httptest.NewRecorder()
	srv.monloaderPairRemove(rmr, httptest.NewRequest("POST", "/settings/monloader/pair/remove", nil))

	if gotPath != "/api/v1/pair/remove" || gotAuth != "Bearer peer-secret" {
		t.Errorf("peer not notified: path=%q auth=%q", gotPath, gotAuth)
	}
	if srv.pairedWith("monloader") {
		t.Error("local pairing not removed")
	}
	if strings.Contains(rmr.Body.String(), "could not reach") {
		t.Errorf("a reachable peer should not warn, got %q", rmr.Body.String())
	}
}

func TestMonloaderCallbackURL(t *testing.T) {
	cases := []struct{ advertised, source, want string }{
		{"http://localhost:8081", "10.0.0.5", "http://10.0.0.5:8081"},
		{"http://monloader:8081", "172.18.0.4", "http://172.18.0.4:8081"},
		{"https://localhost", "10.0.0.5", "https://10.0.0.5"},
		{"http://localhost:8081", "fe80::1", "http://[fe80::1]:8081"},
		{"http://localhost:8081", "", "http://localhost:8081"},
		{"", "10.0.0.5", ""},
	}
	for _, c := range cases {
		if got := monloaderCallbackURL(c.advertised, c.source); got != c.want {
			t.Errorf("monloaderCallbackURL(%q, %q) = %q, want %q", c.advertised, c.source, got, c.want)
		}
	}
}
