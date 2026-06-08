package server

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// clearSlackSetupEnv contains every env var the wizard reads or writes so
// the operator's real shell config (this repo's own dev machine exports
// FLOW_SLACK_* for the live listener) can never leak into fixtures.
func clearSlackSetupEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"FLOW_SLACK_APP_ID", "FLOW_SLACK_CLIENT_ID", "FLOW_SLACK_CLIENT_SECRET",
		"FLOW_SLACK_APP_TOKEN", "SLACK_APP_TOKEN",
		"FLOW_SLACK_TOKEN", "SLACK_BOT_TOKEN", "SLACK_TOKEN",
		"FLOW_SLACK_USER_TOKEN", "SLACK_USER_TOKEN",
		"FLOW_SLACK_SELF_USER_IDS", "FLOW_SLACK_OAUTH_PORT",
		"FLOW_SLACK_API_BASE_URL",
	} {
		t.Setenv(key, "")
	}
}

func TestSlackAppManifest(t *testing.T) {
	m := slackAppManifest("", "https://localhost:8790/api/slack/oauth/callback")

	display := m["display_information"].(map[string]any)
	if display["name"] != "flow" {
		t.Fatalf("default app name = %v, want flow", display["name"])
	}

	oauth := m["oauth_config"].(map[string]any)
	redirects := oauth["redirect_urls"].([]string)
	if len(redirects) != 1 || redirects[0] != "https://localhost:8790/api/slack/oauth/callback" {
		t.Fatalf("redirect_urls = %v", redirects)
	}

	scopes := oauth["scopes"].(map[string]any)
	bot := scopes["bot"].([]string)
	user := scopes["user"].([]string)
	for _, want := range []string{"reactions:read", "channels:history", "files:read", "chat:write"} {
		if !containsString(bot, want) {
			t.Fatalf("bot scopes missing %q: %v", want, bot)
		}
	}
	// DM following is the whole point of the user token — regression-pin it.
	for _, want := range []string{"im:history", "mpim:history", "files:read"} {
		if !containsString(user, want) {
			t.Fatalf("user scopes missing %q: %v", want, user)
		}
	}

	settings := m["settings"].(map[string]any)
	if settings["socket_mode_enabled"] != true {
		t.Fatal("socket_mode_enabled must be true — flow has no public request URL")
	}
	events := settings["event_subscriptions"].(map[string]any)
	userEvents := events["user_events"].([]string)
	// message.im under user_events is the documented gotcha: bot-side
	// subscription alone never delivers the operator's DMs.
	if !containsString(userEvents, "message.im") || !containsString(userEvents, "message.mpim") {
		t.Fatalf("user_events must include message.im + message.mpim: %v", userEvents)
	}

	named := slackAppManifest("  custom-name  ", "https://localhost:1/cb")
	if named["display_information"].(map[string]any)["name"] != "custom-name" {
		t.Fatal("custom app name not honored")
	}
}

func TestMergeSelfUserIDs(t *testing.T) {
	cases := []struct{ existing, discovered, want string }{
		{"", "U1", "U1"},
		{"U1", "U1", "U1"},
		{"U1,U2", "U3", "U1,U2,U3"},
		{" U1 , U2 ", "U2", "U1,U2"},
		{"U1", "", "U1"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := mergeSelfUserIDs(c.existing, c.discovered); got != c.want {
			t.Errorf("mergeSelfUserIDs(%q, %q) = %q, want %q", c.existing, c.discovered, got, c.want)
		}
	}
}

func TestHandleSettingsExcludesHiddenKeys(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearSlackSetupEnv(t)

	rec := httptest.NewRecorder()
	srv.handleSettings(rec, httptest.NewRequest("GET", "/api/settings", nil))
	body := rec.Body.String()
	for _, hidden := range []string{"FLOW_SLACK_APP_ID", "FLOW_SLACK_CLIENT_ID", "FLOW_SLACK_CLIENT_SECRET"} {
		if strings.Contains(body, hidden) {
			t.Fatalf("hidden key %s leaked into /api/settings schema", hidden)
		}
	}
	// Sibling visible keys still present.
	if !strings.Contains(body, "FLOW_SLACK_APP_TOKEN") {
		t.Fatal("visible Slack keys missing from schema")
	}
}

// mockSlackAPI fakes the four Slack endpoints the wizard calls.
func mockSlackAPI(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	t.Setenv("FLOW_SLACK_API_BASE_URL", ts.URL)
	return ts
}

func TestSlackSetupCreateApp(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearSlackSetupEnv(t)

	var sawValidate, sawCreate bool
	mockSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer xoxe.xoxp-1-testtoken" {
			t.Errorf("config token not forwarded: %q", got)
		}
		var body struct {
			Manifest map[string]any `json:"manifest"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Manifest == nil {
			t.Errorf("manifest missing from %s body: %v", r.URL.Path, err)
		}
		switch r.URL.Path {
		case "/apps.manifest.validate":
			sawValidate = true
			fmt.Fprint(w, `{"ok":true}`)
		case "/apps.manifest.create":
			sawCreate = true
			fmt.Fprint(w, `{"ok":true,"app_id":"A123","credentials":{"client_id":"42.99","client_secret":"shhh"}}`)
		default:
			t.Errorf("unexpected Slack call %s", r.URL.Path)
			fmt.Fprint(w, `{"ok":false,"error":"unknown_method"}`)
		}
	})

	post := func(payload string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/slack/setup/create-app", strings.NewReader(payload))
		srv.handleSlackSetupCreateApp(rec, req)
		return rec
	}

	// Wrong token shape rejected before any network call.
	if rec := post(`{"config_token":"xoxp-not-a-config-token"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad token prefix: status %d", rec.Code)
	}

	rec := post(`{"config_token":"xoxe.xoxp-1-testtoken"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create-app failed: %d %s", rec.Code, rec.Body.String())
	}
	if !sawValidate || !sawCreate {
		t.Fatalf("expected validate+create calls; validate=%v create=%v", sawValidate, sawCreate)
	}
	if os.Getenv("FLOW_SLACK_APP_ID") != "A123" || os.Getenv("FLOW_SLACK_CLIENT_ID") != "42.99" || os.Getenv("FLOW_SLACK_CLIENT_SECRET") != "shhh" {
		t.Fatalf("credentials not applied to env: %q %q", os.Getenv("FLOW_SLACK_APP_ID"), os.Getenv("FLOW_SLACK_CLIENT_ID"))
	}
	cfg := loadConfigFile(srv.configPath())
	if cfg["FLOW_SLACK_APP_ID"] != "A123" || cfg["FLOW_SLACK_CLIENT_SECRET"] != "shhh" {
		t.Fatalf("credentials not persisted: %#v", cfg)
	}

	// Second call without force resumes the existing app instead of minting
	// a duplicate (no Slack traffic: the mock would t.Error on extra calls).
	sawValidate, sawCreate = false, false
	rec = post(`{"config_token":"xoxe.xoxp-1-testtoken"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"existing":true`) {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body.String())
	}
	if sawValidate || sawCreate {
		t.Fatal("resume path must not re-create the app")
	}
}

func TestSlackSetupCreateAppRelaysSlackErrors(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearSlackSetupEnv(t)

	mockSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":false,"error":"invalid_manifest","errors":[{"message":"invalid scope","pointer":"/oauth_config/scopes"}]}`)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/slack/setup/create-app", strings.NewReader(`{"config_token":"xoxe.xoxp-1-tok"}`))
	srv.handleSlackSetupCreateApp(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "invalid_manifest") || !strings.Contains(body, "invalid scope") {
		t.Fatalf("Slack error detail not relayed: %s", body)
	}
	if os.Getenv("FLOW_SLACK_APP_ID") != "" {
		t.Fatal("failed create must not persist credentials")
	}
}

func TestSlackSetupAppToken(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearSlackSetupEnv(t)

	valid := true
	mockSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apps.connections.open" {
			t.Errorf("unexpected call %s", r.URL.Path)
		}
		if valid {
			fmt.Fprint(w, `{"ok":true,"url":"wss://example.invalid"}`)
		} else {
			fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
		}
	})

	post := func(payload string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/slack/setup/app-token", strings.NewReader(payload))
		srv.handleSlackSetupAppToken(rec, req)
		return rec
	}

	if rec := post(`{"app_token":"xoxb-wrong-family"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-xapp token accepted: %d", rec.Code)
	}

	if rec := post(`{"app_token":"xapp-1-good"}`); rec.Code != http.StatusOK {
		t.Fatalf("valid token rejected: %d %s", rec.Code, rec.Body.String())
	}
	if os.Getenv("FLOW_SLACK_APP_TOKEN") != "xapp-1-good" {
		t.Fatal("app token not applied to env")
	}

	valid = false
	if rec := post(`{"app_token":"xapp-1-bad"}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("invalid_auth not surfaced: %d", rec.Code)
	}
	if os.Getenv("FLOW_SLACK_APP_TOKEN") != "xapp-1-good" {
		t.Fatal("failed validation must not overwrite the stored token")
	}
}

func TestSlackSetupOAuthStartRequiresAppCredentials(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearSlackSetupEnv(t)

	rec := httptest.NewRecorder()
	srv.handleSlackSetupOAuthStart(rec, httptest.NewRequest("POST", "/api/slack/setup/oauth/start", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("oauth/start without app credentials: %d", rec.Code)
	}
}

// tlsGet hits the dance's self-signed listener the way a redirected browser
// would (minus the interstitial).
func tlsGet(t *testing.T, rawURL string) (*http.Response, string) {
	t.Helper()
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	_, _ = fmt.Fprintf(&sb, "")
	buf := make([]byte, 64<<10)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, sb.String()
}

func TestSlackOAuthDanceRoundTrip(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearSlackSetupEnv(t)
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "UEXISTING")

	mockSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth.v2.access" {
			t.Errorf("unexpected call %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if r.PostForm.Get("client_id") != "client-1" || r.PostForm.Get("client_secret") != "secret-1" {
			t.Errorf("client credentials not forwarded: %v", r.PostForm)
		}
		if r.PostForm.Get("code") != "thecode" {
			t.Errorf("code not forwarded: %v", r.PostForm)
		}
		fmt.Fprint(w, `{"ok":true,"access_token":"xoxb-new","authed_user":{"id":"UNEWUSER","access_token":"xoxp-new"},"team":{"name":"Testers"}}`)
	})

	dance, err := srv.startSlackOAuthDance("client-1", "secret-1", 0)
	if err != nil {
		t.Fatalf("start dance: %v", err)
	}
	defer dance.shutdown()

	if !strings.Contains(dance.authorizeURL, "client_id=client-1") ||
		!strings.Contains(dance.authorizeURL, "state="+dance.state) ||
		!strings.Contains(dance.authorizeURL, url.QueryEscape("im:history")) {
		t.Fatalf("authorize URL malformed: %s", dance.authorizeURL)
	}

	base := "https://" + dance.addr + slackOAuthCallbackPath

	// Wrong state is rejected and poisons nothing (a fresh dance is required
	// after any failure — matching the strict single-nonce lifecycle).
	resp, _ := tlsGet(t, base+"?state=wrong&code=x")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("state mismatch: status %d", resp.StatusCode)
	}

	// New dance for the happy path (the failed one is now in error state).
	dance2, err := srv.startSlackOAuthDance("client-1", "secret-1", 0)
	if err != nil {
		t.Fatalf("restart dance: %v", err)
	}
	defer dance2.shutdown()
	base2 := "https://" + dance2.addr + slackOAuthCallbackPath

	resp, body := tlsGet(t, base2+"?state="+dance2.state+"&code=thecode")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback: status %d body %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Slack connected") || !strings.Contains(body, "Testers") {
		t.Fatalf("success page wrong: %s", body)
	}

	if os.Getenv("FLOW_SLACK_TOKEN") != "xoxb-new" {
		t.Fatalf("bot token not applied: %q", os.Getenv("FLOW_SLACK_TOKEN"))
	}
	if os.Getenv("FLOW_SLACK_USER_TOKEN") != "xoxp-new" {
		t.Fatalf("user token not applied: %q", os.Getenv("FLOW_SLACK_USER_TOKEN"))
	}
	if got := os.Getenv("FLOW_SLACK_SELF_USER_IDS"); got != "UEXISTING,UNEWUSER" {
		t.Fatalf("self user IDs merge = %q, want UEXISTING,UNEWUSER", got)
	}

	cfg := loadConfigFile(srv.configPath())
	if cfg["FLOW_SLACK_TOKEN"] != "xoxb-new" || cfg["FLOW_SLACK_USER_TOKEN"] != "xoxp-new" {
		t.Fatalf("tokens not persisted: %#v", cfg)
	}

	status, _, _, team := dance2.snapshot()
	if status != "done" || team != "Testers" {
		t.Fatalf("dance status = %s team = %s", status, team)
	}
}

func TestSlackSetupStatusShape(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	clearSlackSetupEnv(t)
	t.Setenv("FLOW_SLACK_APP_ID", "A777")
	t.Setenv("FLOW_SLACK_CLIENT_ID", "1.2")
	t.Setenv("FLOW_SLACK_APP_TOKEN", "xapp-1-x")

	rec := httptest.NewRecorder()
	srv.handleSlackSetupStatus(rec, httptest.NewRequest("GET", "/api/slack/setup/status", nil))
	var st slackSetupStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !st.AppCreated || st.AppID != "A777" || !st.AppTokenSet || st.BotTokenSet {
		t.Fatalf("status wrong: %+v", st)
	}
	if st.ManageURL != "https://api.slack.com/apps/A777" || st.AppTokenURL != "https://api.slack.com/apps/A777/general" {
		t.Fatalf("deep links wrong: %+v", st)
	}
	if st.RedirectURL != "https://localhost:8790/api/slack/oauth/callback" {
		t.Fatalf("redirect URL = %s", st.RedirectURL)
	}
}
