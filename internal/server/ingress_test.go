package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestFlowSharesToPrune(t *testing.T) {
	// Two flow shares + one non-flow share, across the env. Keep the live one,
	// prune the other flow share, never touch the non-flow share.
	overview := `{"environments":[{"shares":[
		{"shareToken":"flowLIVE","backendProxyEndpoint":"flow"},
		{"shareToken":"flowSTALE","backendProxyEndpoint":"flow"},
		{"shareToken":"someoneElse","backendProxyEndpoint":"http://localhost:9999"}
	]}]}`
	got := flowSharesToPrune(overview, "flowLIVE")
	if len(got) != 1 || got[0] != "flowSTALE" {
		t.Fatalf("toPrune = %#v, want [flowSTALE]", got)
	}
}

func TestFlowSharesToPrune_KeepEmptyPrunesAllFlowShares(t *testing.T) {
	overview := `{"environments":[{"shares":[
		{"shareToken":"a","backendProxyEndpoint":"flow"},
		{"shareToken":"b","backendProxyEndpoint":"flow"},
		{"shareToken":"keep-me","backendProxyEndpoint":"other"}
	]}]}`
	got := flowSharesToPrune(overview, "")
	if len(got) != 2 {
		t.Fatalf("toPrune = %#v, want [a b]", got)
	}
}

func TestFlowSharesToPrune_IgnoresGarbage(t *testing.T) {
	if got := flowSharesToPrune("not json", "x"); got != nil {
		t.Fatalf("garbage overview should prune nothing, got %#v", got)
	}
}

// clearIngressEnv zeros every env var the ingress subsystem reads so real
// shell exports can never pollute these tests.
func clearIngressEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"FLOW_INGRESS_PROVIDER", "FLOW_PUBLIC_BASE_URL",
		"FLOW_ZROK_SHARE_NAME", "FLOW_ZROK_AUTO_START",
		"FLOW_GH_WEBHOOK_SECRET",
	} {
		t.Setenv(k, "")
	}
}

// setZrokLive simulates an established zrok share by injecting the
// runtime-discovered public URL the manager would have read from the share's
// frontend endpoint. The real value only exists after a live CreateShare, so
// tests stub it here rather than reaching the network.
func setZrokLive(t *testing.T, srv *Server, baseURL string) {
	t.Helper()
	srv.zrok.mu.Lock()
	srv.zrok.baseURL = baseURL
	srv.zrok.mu.Unlock()
}

// ---------------------------------------------------------------------------
// publicBaseURL
// ---------------------------------------------------------------------------

func TestPublicBaseURL(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		root, db := testRootDB(t)
		srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
		clearIngressEnv(t)
		if got := srv.publicBaseURL(); got != "" {
			t.Errorf("none provider: want empty, got %q", got)
		}
	})

	t.Run("zrok before share is up", func(t *testing.T) {
		root, db := testRootDB(t)
		srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
		clearIngressEnv(t)
		t.Setenv("FLOW_INGRESS_PROVIDER", "zrok")
		// No live share → URL not yet known → empty (caller falls back).
		if got := srv.publicBaseURL(); got != "" {
			t.Errorf("zrok no share: want empty, got %q", got)
		}
	})

	t.Run("zrok runtime URL", func(t *testing.T) {
		root, db := testRootDB(t)
		srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
		clearIngressEnv(t)
		t.Setenv("FLOW_INGRESS_PROVIDER", "zrok")
		setZrokLive(t, srv, "https://flow-xyz.share.zrok.io")
		if got := srv.publicBaseURL(); got != "https://flow-xyz.share.zrok.io" {
			t.Errorf("zrok runtime URL = %q", got)
		}
	})

	t.Run("manual reads env with trimming", func(t *testing.T) {
		cases := []struct{ in, want string }{
			{"https://flow.example.com", "https://flow.example.com"},
			{"https://flow.example.com/", "https://flow.example.com"},
			{"  https://flow.example.com  ", "https://flow.example.com"},
			{"", ""},
		}
		for _, c := range cases {
			root, db := testRootDB(t)
			srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
			clearIngressEnv(t)
			t.Setenv("FLOW_INGRESS_PROVIDER", "manual")
			t.Setenv("FLOW_PUBLIC_BASE_URL", c.in)
			if got := srv.publicBaseURL(); got != c.want {
				t.Errorf("manual %q = %q, want %q", c.in, got, c.want)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// connectorCallbackURL
// ---------------------------------------------------------------------------

func TestConnectorCallbackURL(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearIngressEnv(t)

	// Without public ingress: returns "" (caller uses its fallback).
	if got := srv.connectorCallbackURL("/api/github/webhook"); got != "" {
		t.Errorf("no ingress: want empty, got %q", got)
	}

	// With a live zrok URL: builds the signed GitHub webhook URL.
	t.Setenv("FLOW_INGRESS_PROVIDER", "zrok")
	setZrokLive(t, srv, "https://mytoken.share.zrok.io")

	if got := srv.connectorCallbackURL("/api/github/webhook"); got != "https://mytoken.share.zrok.io/api/github/webhook" {
		t.Errorf("zrok callback = %q", got)
	}
	// Path without a leading slash is normalised.
	if got := srv.connectorCallbackURL("api/github/webhook"); got != "https://mytoken.share.zrok.io/api/github/webhook" {
		t.Errorf("no-slash path = %q", got)
	}
}

// ---------------------------------------------------------------------------
// GET /api/ingress/status
// ---------------------------------------------------------------------------

func TestHandleIngressStatus_None(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearIngressEnv(t)

	rec := httptest.NewRecorder()
	srv.handleIngressStatus(rec, httptest.NewRequest("GET", "/api/ingress/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var st IngressStatusView
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Provider != "none" || st.Running || st.BaseURL != "" {
		t.Errorf("none ingress: %+v", st)
	}
}

func TestHandleIngressStatus_ZrokLiveShare(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearIngressEnv(t)
	t.Setenv("FLOW_INGRESS_PROVIDER", "zrok")
	t.Setenv("FLOW_ZROK_SHARE_NAME", "flowshare")
	setZrokLive(t, srv, "https://flowshare.share.zrok.io")

	rec := httptest.NewRecorder()
	srv.handleIngressStatus(rec, httptest.NewRequest("GET", "/api/ingress/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var st IngressStatusView
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Provider != "zrok" {
		t.Errorf("provider = %q, want zrok", st.Provider)
	}
	if st.BaseURL != "https://flowshare.share.zrok.io" {
		t.Errorf("base_url = %q (should be the runtime-discovered URL)", st.BaseURL)
	}
	if st.ShareName != "flowshare" {
		t.Errorf("share_name = %q", st.ShareName)
	}
	if st.GithubWebhookURL != "https://flowshare.share.zrok.io/api/github/webhook" {
		t.Errorf("github_webhook_url = %q", st.GithubWebhookURL)
	}
	if !st.Running {
		t.Error("Running should be true when the runtime URL is known")
	}
}

func TestHandleIngressStatus_ZrokNotYetUp(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearIngressEnv(t)
	t.Setenv("FLOW_INGRESS_PROVIDER", "zrok")
	// No live share injected: URL not known yet.

	rec := httptest.NewRecorder()
	srv.handleIngressStatus(rec, httptest.NewRequest("GET", "/api/ingress/status", nil))
	var st IngressStatusView
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Provider != "zrok" || st.BaseURL != "" || st.Running {
		t.Errorf("zrok not up: %+v", st)
	}
}

func TestHandleIngressStatus_Manual(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearIngressEnv(t)
	t.Setenv("FLOW_INGRESS_PROVIDER", "manual")
	t.Setenv("FLOW_PUBLIC_BASE_URL", "https://flow.example.com")

	rec := httptest.NewRecorder()
	srv.handleIngressStatus(rec, httptest.NewRequest("GET", "/api/ingress/status", nil))
	var st IngressStatusView
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.BaseURL != "https://flow.example.com" || !st.Running {
		t.Errorf("manual ingress: %+v", st)
	}
	if st.GithubWebhookURL == "" {
		t.Error("derived GitHub webhook URL missing for manual ingress")
	}
}

func TestHandleIngressStatus_MethodNotAllowed(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearIngressEnv(t)

	rec := httptest.NewRecorder()
	srv.handleIngressStatus(rec, httptest.NewRequest("POST", "/api/ingress/status", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/ingress/status: status %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Slack: callback URL stays local even when public ingress is live
// ---------------------------------------------------------------------------

func TestSlackCallbackURL_AlwaysLocalhost(t *testing.T) {
	t.Run("default localhost", func(t *testing.T) {
		root, db := testRootDB(t)
		srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
		clearIngressEnv(t)
		clearSlackSetupEnv(t)
		got := srv.slackCallbackURL()
		if !strings.HasPrefix(got, "https://localhost:") {
			t.Errorf("no ingress: expected localhost URL, got %q", got)
		}
		if !strings.Contains(got, slackOAuthCallbackPath) {
			t.Errorf("callback path missing: %q", got)
		}
	})

	t.Run("zrok live URL ignored for Slack", func(t *testing.T) {
		root, db := testRootDB(t)
		srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
		clearIngressEnv(t)
		clearSlackSetupEnv(t)
		t.Setenv("FLOW_INGRESS_PROVIDER", "zrok")
		setZrokLive(t, srv, "https://zrtoken.share.zrok.io")
		got := srv.slackCallbackURL()
		if !strings.HasPrefix(got, "https://localhost:") {
			t.Errorf("zrok ingress should not change Slack callback URL, got %q", got)
		}
	})

	t.Run("manual URL ignored for Slack", func(t *testing.T) {
		root, db := testRootDB(t)
		srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
		clearIngressEnv(t)
		clearSlackSetupEnv(t)
		t.Setenv("FLOW_INGRESS_PROVIDER", "manual")
		t.Setenv("FLOW_PUBLIC_BASE_URL", "https://my.host.com")
		got := srv.slackCallbackURL()
		if !strings.HasPrefix(got, "https://localhost:") {
			t.Errorf("manual ingress should not change Slack callback URL, got %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Slack: manifest never registers standing public ingress URLs
// ---------------------------------------------------------------------------

func TestSlackManifestUsesLocalhost(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearIngressEnv(t)
	clearSlackSetupEnv(t)

	// Default: manifest carries the localhost redirect URL.
	m := slackAppManifest("flow", srv.slackCallbackURL())
	redirects := m["oauth_config"].(map[string]any)["redirect_urls"].([]string)
	if len(redirects) != 1 || !strings.HasPrefix(redirects[0], "https://localhost:") {
		t.Fatalf("default manifest redirect = %v", redirects)
	}

	// With a live zrok share: manifest still carries the localhost redirect URL.
	t.Setenv("FLOW_INGRESS_PROVIDER", "zrok")
	setZrokLive(t, srv, "https://maniftoken.share.zrok.io")
	m2 := slackAppManifest("flow", srv.slackCallbackURL())
	redirects2 := m2["oauth_config"].(map[string]any)["redirect_urls"].([]string)
	if len(redirects2) != 1 || !strings.HasPrefix(redirects2[0], "https://localhost:") {
		t.Fatalf("zrok manifest redirect = %v, want localhost", redirects2)
	}
}

// ---------------------------------------------------------------------------
// Slack: OAuth dance always uses the short-lived local TLS listener
// ---------------------------------------------------------------------------

func TestSlackOAuthDance_PublicIngressStillUsesLocalTLSListener(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearIngressEnv(t)
	clearSlackSetupEnv(t)

	// Activate zrok ingress with a live share URL.
	t.Setenv("FLOW_INGRESS_PROVIDER", "zrok")
	setZrokLive(t, srv, "https://dancetoken.share.zrok.io")

	dance, err := srv.startSlackOAuthDance("client-x", "secret-x", 0)
	if err != nil {
		t.Fatalf("startSlackOAuthDance: %v", err)
	}
	defer dance.shutdown()

	if dance.publicIngress {
		t.Fatal("Slack OAuth must not use standing public ingress")
	}
	if dance.srv == nil {
		t.Error("TLS srv must be non-nil for Slack OAuth")
	}
	if dance.addr != "" {
		// port 0 in tests should still bind a local listener.
	} else {
		t.Error("addr must be set for local Slack OAuth")
	}

	expectedRedirect := srv.slackCallbackURL()
	if !strings.Contains(dance.authorizeURL, "redirect_uri="+urlEncodeRaw(expectedRedirect)) {
		t.Errorf("authorize URL redirect_uri wrong: %s", dance.authorizeURL)
	}
}

func TestSlackOAuthDance_Localhost_HasTLSListener(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearIngressEnv(t)
	clearSlackSetupEnv(t)
	// No ingress configured: must use the TLS listener.

	dance, err := srv.startSlackOAuthDance("client-x", "secret-x", 0)
	if err != nil {
		t.Fatalf("startSlackOAuthDance: %v", err)
	}
	defer dance.shutdown()

	if dance.publicIngress {
		t.Fatal("publicIngress should be false with no ingress configured")
	}
	if dance.srv == nil {
		t.Error("TLS srv must be non-nil in localhost mode")
	}
	if dance.addr == "" {
		t.Error("addr must be set in localhost mode")
	}
}

// ---------------------------------------------------------------------------
// Slack setup status: callback_mode field
// ---------------------------------------------------------------------------

func TestSlackSetupStatus_CallbackMode(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		assertCallbackMode(t, "", false, "", "localhost")
	})
	t.Run("zrok not up", func(t *testing.T) {
		assertCallbackMode(t, "zrok", false, "", "localhost")
	})
	t.Run("zrok live", func(t *testing.T) {
		assertCallbackMode(t, "zrok", true, "", "localhost")
	})
	t.Run("manual no url", func(t *testing.T) {
		assertCallbackMode(t, "manual", false, "", "localhost")
	})
	t.Run("manual with url", func(t *testing.T) {
		assertCallbackMode(t, "manual", false, "https://cb.example.com", "localhost")
	})
}

func TestIngressMuxExposesOnlyGitHubWebhook(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearIngressEnv(t)

	rec := httptest.NewRecorder()
	srv.ingressMux().ServeHTTP(rec, httptest.NewRequest("GET", slackOAuthCallbackPath, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("public ingress must not expose Slack OAuth callback, status %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ingressMux().ServeHTTP(rec, httptest.NewRequest("GET", "/api/github/webhook", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GitHub webhook route missing from ingress mux, status %d", rec.Code)
	}
}

func TestGitHubWebhookRequiresSecretAndSignature(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearIngressEnv(t)

	body := []byte(`{"zen":"Keep it logically awesome."}`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/github/webhook", strings.NewReader(string(body)))
	srv.handleGitHubWebhook(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing secret status = %d, want 503", rec.Code)
	}

	t.Setenv("FLOW_GH_WEBHOOK_SECRET", "topsecret")
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/github/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", "sha256=bad")
	srv.handleGitHubWebhook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/github/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", githubTestSignature("topsecret", body))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-GitHub-Delivery", "delivery-1")
	srv.handleGitHubWebhook(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid signature status = %d, want 202 body %s", rec.Code, rec.Body.String())
	}
}

func githubTestSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func assertCallbackMode(t *testing.T, provider string, zrokLive bool, manualURL, want string) {
	t.Helper()
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearSlackSetupEnv(t)
	clearIngressEnv(t)
	t.Setenv("FLOW_INGRESS_PROVIDER", provider)
	t.Setenv("FLOW_PUBLIC_BASE_URL", manualURL)
	if zrokLive {
		setZrokLive(t, srv, "https://live.share.zrok.io")
	}

	rec := httptest.NewRecorder()
	srv.handleSlackSetupStatus(rec, httptest.NewRequest("GET", "/api/slack/setup/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var st struct {
		CallbackMode string `json:"callback_mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.CallbackMode != want {
		t.Errorf("callback_mode = %q, want %q", st.CallbackMode, want)
	}
}

// ---------------------------------------------------------------------------
// Slack: main-server OAuth callback fallback
// ---------------------------------------------------------------------------

func TestHandleSlackSetupOAuthCallbackMain_NoDance(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearSlackSetupEnv(t)
	clearIngressEnv(t)

	rec := httptest.NewRecorder()
	srv.handleSlackSetupOAuthCallbackMain(rec, httptest.NewRequest("GET", slackOAuthCallbackPath, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no active dance: expected 404, got %d", rec.Code)
	}
}

func TestHandleSlackSetupOAuthCallbackMain_ExchangesCode(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearSlackSetupEnv(t)
	clearIngressEnv(t)
	t.Setenv("FLOW_INGRESS_PROVIDER", "zrok")
	setZrokLive(t, srv, "https://maintoken.share.zrok.io")
	t.Setenv("FLOW_SLACK_CLIENT_ID", "cid")
	t.Setenv("FLOW_SLACK_CLIENT_SECRET", "csec")

	mockSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth.v2.access" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"ok":true,"access_token":"xoxb-main","authed_user":{"id":"UMAIN","access_token":"xoxp-main"},"team":{"name":"MainTeam"}}`)
	})

	dance, err := srv.startSlackOAuthDance("cid", "csec", 0)
	if err != nil {
		t.Fatalf("start dance: %v", err)
	}
	defer dance.shutdown()

	// Simulate Slack redirecting to the main server with the correct state+code.
	req := httptest.NewRequest("GET", slackOAuthCallbackPath+"?state="+dance.state+"&code=main-code", nil)
	rec := httptest.NewRecorder()

	srv.slackSetupMu.Lock()
	srv.slackOAuth = dance
	srv.slackSetupMu.Unlock()

	srv.handleSlackSetupOAuthCallbackMain(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	if os.Getenv("FLOW_SLACK_TOKEN") != "xoxb-main" {
		t.Errorf("bot token not applied: %q", os.Getenv("FLOW_SLACK_TOKEN"))
	}
	if os.Getenv("FLOW_SLACK_USER_TOKEN") != "xoxp-main" {
		t.Errorf("user token not applied: %q", os.Getenv("FLOW_SLACK_USER_TOKEN"))
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// urlEncodeRaw is a minimal percent-encoder matching url.QueryEscape behaviour
// for comparing the redirect_uri inside authorize URLs.
func urlEncodeRaw(s string) string {
	return strings.NewReplacer(
		":", "%3A", "/", "%2F", "?", "%3F", "=", "%3D", "&", "%26",
	).Replace(s)
}
