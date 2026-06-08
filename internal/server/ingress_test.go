package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// clearIngressEnv zeros every env var the ingress subsystem reads so real
// shell exports can never pollute these tests.
func clearIngressEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"FLOW_INGRESS_PROVIDER", "FLOW_PUBLIC_BASE_URL",
		"FLOW_ZROK_SHARE_NAME", "FLOW_ZROK_AUTO_START",
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
	if got := srv.connectorCallbackURL("/api/slack/oauth/callback"); got != "" {
		t.Errorf("no ingress: want empty, got %q", got)
	}

	// With a live zrok URL: builds full URL.
	t.Setenv("FLOW_INGRESS_PROVIDER", "zrok")
	setZrokLive(t, srv, "https://mytoken.share.zrok.io")

	if got := srv.connectorCallbackURL("/api/slack/oauth/callback"); got != "https://mytoken.share.zrok.io/api/slack/oauth/callback" {
		t.Errorf("zrok callback = %q", got)
	}
	// Path without a leading slash is normalised.
	if got := srv.connectorCallbackURL("api/github/webhook"); got != "https://mytoken.share.zrok.io/api/github/webhook" {
		t.Errorf("no-slash path = %q", got)
	}
}

// ---------------------------------------------------------------------------
// callbackMode
// ---------------------------------------------------------------------------

func TestCallbackMode(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		root, db := testRootDB(t)
		srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
		clearIngressEnv(t)
		if got := srv.callbackMode(); got != "localhost" {
			t.Errorf("none = %q, want localhost", got)
		}
	})

	t.Run("zrok before share is up falls back to localhost", func(t *testing.T) {
		root, db := testRootDB(t)
		srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
		clearIngressEnv(t)
		t.Setenv("FLOW_INGRESS_PROVIDER", "zrok")
		if got := srv.callbackMode(); got != "localhost" {
			t.Errorf("zrok no share = %q, want localhost", got)
		}
	})

	t.Run("zrok with live share", func(t *testing.T) {
		root, db := testRootDB(t)
		srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
		clearIngressEnv(t)
		t.Setenv("FLOW_INGRESS_PROVIDER", "zrok")
		setZrokLive(t, srv, "https://x.share.zrok.io")
		if got := srv.callbackMode(); got != "zrok" {
			t.Errorf("zrok live = %q, want zrok", got)
		}
	})

	t.Run("manual with URL", func(t *testing.T) {
		root, db := testRootDB(t)
		srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
		clearIngressEnv(t)
		t.Setenv("FLOW_INGRESS_PROVIDER", "manual")
		t.Setenv("FLOW_PUBLIC_BASE_URL", "https://x.com")
		if got := srv.callbackMode(); got != "manual" {
			t.Errorf("manual = %q, want manual", got)
		}
	})
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
	if st.SlackCallbackURL != "https://flowshare.share.zrok.io"+slackOAuthCallbackPath {
		t.Errorf("slack_callback_url = %q", st.SlackCallbackURL)
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
	if st.SlackCallbackURL == "" || st.GithubWebhookURL == "" {
		t.Error("derived callback URLs missing for manual ingress")
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
// Slack: callback URL uses public ingress when a URL is live
// ---------------------------------------------------------------------------

func TestSlackCallbackURL_PrefersPublicIngress(t *testing.T) {
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

	t.Run("zrok live URL preferred", func(t *testing.T) {
		root, db := testRootDB(t)
		srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
		clearIngressEnv(t)
		clearSlackSetupEnv(t)
		t.Setenv("FLOW_INGRESS_PROVIDER", "zrok")
		setZrokLive(t, srv, "https://zrtoken.share.zrok.io")
		got := srv.slackCallbackURL()
		if got != "https://zrtoken.share.zrok.io"+slackOAuthCallbackPath {
			t.Errorf("zrok callback URL = %q", got)
		}
	})

	t.Run("manual URL preferred", func(t *testing.T) {
		root, db := testRootDB(t)
		srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
		clearIngressEnv(t)
		clearSlackSetupEnv(t)
		t.Setenv("FLOW_INGRESS_PROVIDER", "manual")
		t.Setenv("FLOW_PUBLIC_BASE_URL", "https://my.host.com")
		got := srv.slackCallbackURL()
		if got != "https://my.host.com"+slackOAuthCallbackPath {
			t.Errorf("manual callback URL = %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Slack: manifest uses public ingress URL when a URL is live
// ---------------------------------------------------------------------------

func TestSlackManifestUsesPublicIngress(t *testing.T) {
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

	// With a live zrok share: manifest carries the runtime zrok URL.
	t.Setenv("FLOW_INGRESS_PROVIDER", "zrok")
	setZrokLive(t, srv, "https://maniftoken.share.zrok.io")
	m2 := slackAppManifest("flow", srv.slackCallbackURL())
	redirects2 := m2["oauth_config"].(map[string]any)["redirect_urls"].([]string)
	want := "https://maniftoken.share.zrok.io" + slackOAuthCallbackPath
	if len(redirects2) != 1 || redirects2[0] != want {
		t.Fatalf("zrok manifest redirect = %v, want %q", redirects2, want)
	}
}

// ---------------------------------------------------------------------------
// Slack: OAuth dance skips TLS listener in public ingress mode
// ---------------------------------------------------------------------------

func TestSlackOAuthDance_PublicIngress_NoTLSListener(t *testing.T) {
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

	// Public ingress mode: no self-signed TLS listener.
	if !dance.publicIngress {
		t.Fatal("publicIngress flag not set when a public URL is live")
	}
	if dance.srv != nil {
		t.Error("TLS srv should be nil in public ingress mode")
	}
	if dance.addr != "" {
		t.Errorf("addr should be empty in public ingress mode, got %q", dance.addr)
	}

	// Authorize URL must use the zrok callback URL, not localhost.
	expectedRedirect := "https://dancetoken.share.zrok.io" + slackOAuthCallbackPath
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
		assertCallbackMode(t, "zrok", true, "", "zrok")
	})
	t.Run("manual no url", func(t *testing.T) {
		assertCallbackMode(t, "manual", false, "", "localhost")
	})
	t.Run("manual with url", func(t *testing.T) {
		assertCallbackMode(t, "manual", false, "https://cb.example.com", "manual")
	})
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
// Slack: main-server OAuth callback (public ingress mode)
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
