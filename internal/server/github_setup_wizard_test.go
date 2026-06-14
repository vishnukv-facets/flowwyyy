package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

func githubSetupTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	resetGitHubSecretsForTest(t)
	root := t.TempDir()
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	// A ready public ingress (the manifest's hook_attributes.url needs it).
	t.Setenv("FLOW_INGRESS_PROVIDER", "manual")
	t.Setenv("FLOW_PUBLIC_BASE_URL", "https://flow.example.com")
	return srv, db
}

func TestGitHubAppManifest_HasRequiredFields(t *testing.T) {
	m := githubAppManifest("flow-dev", "https://flow.example.com/api/github/webhook", "https://flow.example.com/api/github/setup/callback")

	hook, ok := m["hook_attributes"].(map[string]any)
	if !ok || hook["url"] != "https://flow.example.com/api/github/webhook" || hook["active"] != true {
		t.Errorf("hook_attributes wrong: %#v", m["hook_attributes"])
	}
	if m["redirect_url"] != "https://flow.example.com/api/github/setup/callback" {
		t.Errorf("redirect_url = %v", m["redirect_url"])
	}
	// Public ("Any account") so the App can be installed on the operator's
	// personal account AND any org they admin — a private App installs only on
	// its owner account, which would block the personal+org "both" case.
	if m["public"] != true {
		t.Errorf("public = %v, want true", m["public"])
	}
	perms, _ := m["default_permissions"].(map[string]any)
	if perms["issues"] != "write" || perms["pull_requests"] != "write" || perms["metadata"] != "read" {
		t.Errorf("default_permissions wrong: %#v", perms)
	}
	events, _ := m["default_events"].([]string)
	want := []string{"issues", "issue_comment", "pull_request", "pull_request_review", "pull_request_review_comment"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Errorf("default_events = %v, want %v", events, want)
	}
}

func TestGitHubManifestCreateURL_PersonalAndOrg(t *testing.T) {
	personal, err := githubManifestCreateURL("user", "", "nonce123")
	if err != nil {
		t.Fatalf("personal: %v", err)
	}
	if personal != "https://github.com/settings/apps/new?state=nonce123" {
		t.Errorf("personal URL = %q", personal)
	}

	org, err := githubManifestCreateURL("org", "acme", "nonce123")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	if org != "https://github.com/organizations/acme/settings/apps/new?state=nonce123" {
		t.Errorf("org URL = %q", org)
	}

	if _, err := githubManifestCreateURL("org", "", "nonce123"); err == nil {
		t.Error("org target without an org name should error")
	}
}

func TestConvertManifest_ParsesCredentials(t *testing.T) {
	useMockHTTPTransport(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app-manifests/the-code/conversions" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"id": 424242, "slug": "flow-dev", "client_id": "Iv1.abc",
			"client_secret": "csecret", "webhook_secret": "whsecret",
			"pem": "-----BEGIN RSA PRIVATE KEY-----\nMII...\n-----END RSA PRIVATE KEY-----\n",
			"html_url": "https://github.com/apps/flow-dev"
		}`))
	})
	t.Setenv("FLOW_GH_API_BASE_URL", "https://github.test")

	conv, err := newGitHubSetupAPI().convertManifest(t.Context(), "the-code")
	if err != nil {
		t.Fatalf("convertManifest: %v", err)
	}
	if conv.AppID != 424242 || conv.Slug != "flow-dev" || conv.ClientID != "Iv1.abc" {
		t.Errorf("metadata wrong: %#v", conv)
	}
	if conv.ClientSecret != "csecret" || conv.WebhookSecret != "whsecret" || !strings.Contains(conv.PEM, "BEGIN RSA PRIVATE KEY") {
		t.Errorf("secrets wrong: %#v", conv)
	}
}

func TestConvertManifest_ErrorOnNon2xx(t *testing.T) {
	useMockHTTPTransport(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Code is invalid or expired"}`))
	})
	t.Setenv("FLOW_GH_API_BASE_URL", "https://github.test")

	if _, err := newGitHubSetupAPI().convertManifest(t.Context(), "bad"); err == nil {
		t.Error("expected error on 422")
	}
}

func TestHandleGitHubSetupCreateApp_GatedOnIngress(t *testing.T) {
	srv, _ := githubSetupTestServer(t)
	// Remove the ready ingress.
	t.Setenv("FLOW_INGRESS_PROVIDER", "none")
	t.Setenv("FLOW_PUBLIC_BASE_URL", "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/github/setup/create-app", strings.NewReader(`{"target":"user"}`))
	srv.handleGitHubSetupCreateApp(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when ingress not ready", rec.Code)
	}
}

func TestHandleGitHubSetupCreateApp_ReturnsManifestAndState(t *testing.T) {
	srv, _ := githubSetupTestServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/github/setup/create-app", strings.NewReader(`{"target":"user","name":"flow-dev"}`))
	srv.handleGitHubSetupCreateApp(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK        bool           `json:"ok"`
		CreateURL string         `json:"create_url"`
		State     string         `json:"state"`
		Manifest  map[string]any `json:"manifest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.State == "" {
		t.Fatalf("bad response: %#v", resp)
	}
	if !strings.Contains(resp.CreateURL, "github.com/settings/apps/new?state="+resp.State) {
		t.Errorf("create_url = %q", resp.CreateURL)
	}
	hook, _ := resp.Manifest["hook_attributes"].(map[string]any)
	if hook["url"] != "https://flow.example.com/api/github/webhook" {
		t.Errorf("manifest hook url = %v", hook["url"])
	}
	// The pending state is retained server-side for callback validation.
	if srv.pendingGitHubSetupState() != resp.State {
		t.Errorf("pending state not stored")
	}
}

func TestHandleGitHubSetupCallback_ConvertsAndPersists(t *testing.T) {
	srv, _ := githubSetupTestServer(t)
	useMockHTTPTransport(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"id": 99, "slug": "flow-dev", "client_id": "Iv1.cid",
			"client_secret": "cs", "webhook_secret": "wh", "pem": "PEMDATA",
			"html_url": "https://github.com/apps/flow-dev"
		}`))
	})
	t.Setenv("FLOW_GH_API_BASE_URL", "https://github.test")

	// Begin the flow to obtain a valid state nonce.
	createRec := httptest.NewRecorder()
	srv.handleGitHubSetupCreateApp(createRec, httptest.NewRequest("POST", "/api/github/setup/create-app", strings.NewReader(`{"target":"user"}`)))
	var cr struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(createRec.Body.Bytes(), &cr)

	rec := httptest.NewRecorder()
	srv.handleGitHubSetupCallback(rec, httptest.NewRequest("GET", "/api/github/setup/callback?code=the-code&state="+cr.State, nil))
	if rec.Code != 200 {
		t.Fatalf("callback status = %d: %s", rec.Code, rec.Body.String())
	}

	// Secrets persisted to the keyring.
	if v, _ := getGitHubSecret(keyringAcctAppPEM); v != "PEMDATA" {
		t.Errorf("PEM not in keyring: %q", v)
	}
	if v, _ := getGitHubSecret(keyringAcctWebhookSecret); v != "wh" {
		t.Errorf("webhook secret not in keyring: %q", v)
	}
	if v, _ := getGitHubSecret(keyringAcctClientSecret); v != "cs" {
		t.Errorf("client secret not in keyring: %q", v)
	}
	// Metadata persisted to config + env; secrets live (env hydrated).
	cfg := loadConfigFile(srv.configPath())
	if cfg["FLOW_GH_APP_ID"] != "99" || cfg["FLOW_GH_APP_SLUG"] != "flow-dev" || cfg["FLOW_GH_CLIENT_ID"] != "Iv1.cid" {
		t.Errorf("metadata not persisted to config: %#v", cfg)
	}
	if githubWebhookSecret() != "wh" {
		t.Errorf("webhook secret not live in env")
	}
	// Transport flipped to webhook so the scheduled poller stays off.
	if cfg["FLOW_GH_TRANSPORT"] != "webhook" {
		t.Errorf("transport = %q, want webhook", cfg["FLOW_GH_TRANSPORT"])
	}
}

func TestHandleGitHubSetupCallback_RejectsBadState(t *testing.T) {
	srv, _ := githubSetupTestServer(t)
	createRec := httptest.NewRecorder()
	srv.handleGitHubSetupCreateApp(createRec, httptest.NewRequest("POST", "/api/github/setup/create-app", strings.NewReader(`{"target":"user"}`)))

	rec := httptest.NewRecorder()
	srv.handleGitHubSetupCallback(rec, httptest.NewRequest("GET", "/api/github/setup/callback?code=x&state=WRONG", nil))

	// No persistence on a state mismatch.
	if v, _ := getGitHubSecret(keyringAcctAppPEM); v != "" {
		t.Errorf("PEM persisted despite bad state: %q", v)
	}
	if rec.Code == 200 {
		// A bad-state callback should not report success.
		if !strings.Contains(strings.ToLower(rec.Body.String()), "state") && !strings.Contains(strings.ToLower(rec.Body.String()), "expired") {
			t.Errorf("expected a state-mismatch error page, got: %s", rec.Body.String())
		}
	}
}

func TestGitHubAppManifest_IncludesSetupURLForInstallRedirect(t *testing.T) {
	m := githubAppManifest("flow", "https://flow.example.com/api/github/webhook", "https://flow.example.com/api/github/setup/callback")
	// setup_url is where GitHub redirects after install, carrying installation_id.
	if m["setup_url"] != "https://flow.example.com/api/github/setup/callback" {
		t.Errorf("setup_url = %v, want the callback URL", m["setup_url"])
	}
}

// stubGitHubInstallationVerifier replaces the App-API installation verifier with
// an allowlist so the unauthenticated install callback can be tested without a
// real connected App. Restored on cleanup.
func stubGitHubInstallationVerifier(t *testing.T, allowed ...string) {
	t.Helper()
	prev := verifyGitHubInstallation
	allow := map[string]bool{}
	for _, id := range allowed {
		allow[id] = true
	}
	verifyGitHubInstallation = func(_ context.Context, id string) bool { return allow[id] }
	t.Cleanup(func() { verifyGitHubInstallation = prev })
}

func TestHandleGitHubSetupCallback_CapturesInstallationID(t *testing.T) {
	t.Setenv("FLOW_GH_INSTALLATION_IDS", "") // isolate from captureInstallationID's process-global os.Setenv
	srv, _ := githubSetupTestServer(t)
	t.Setenv("FLOW_GH_APP_SLUG", "flow-dev") // App already exists from a prior step
	stubGitHubInstallationVerifier(t, "555", "556")

	// Post-install redirect: installation_id + setup_action, no code, no state.
	rec := httptest.NewRecorder()
	srv.handleGitHubSetupCallback(rec, httptest.NewRequest("GET", "/api/github/setup/callback?installation_id=555&setup_action=install", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if cfg := loadConfigFile(srv.configPath()); cfg["FLOW_GH_INSTALLATION_IDS"] != "555" {
		t.Errorf("installation id = %q, want 555", cfg["FLOW_GH_INSTALLATION_IDS"])
	}

	// A second install appends; a duplicate is deduped.
	srv.handleGitHubSetupCallback(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/github/setup/callback?installation_id=556&setup_action=install", nil))
	srv.handleGitHubSetupCallback(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/github/setup/callback?installation_id=555&setup_action=install", nil))
	if cfg := loadConfigFile(srv.configPath()); cfg["FLOW_GH_INSTALLATION_IDS"] != "555,556" {
		t.Errorf("installation ids = %q, want 555,556", cfg["FLOW_GH_INSTALLATION_IDS"])
	}
}

// TestHandleGitHubSetupCallback_RejectsUnverifiedInstallationID is the P2-3
// security property: an installation_id that does not belong to this App (the
// verifier returns false — the fail-closed default for an attacker-supplied id
// on the public callback) must NOT be persisted.
func TestHandleGitHubSetupCallback_RejectsUnverifiedInstallationID(t *testing.T) {
	t.Setenv("FLOW_GH_INSTALLATION_IDS", "") // isolate from captureInstallationID's process-global os.Setenv
	srv, _ := githubSetupTestServer(t)
	t.Setenv("FLOW_GH_APP_SLUG", "flow-dev")
	stubGitHubInstallationVerifier(t /* allow nothing */)

	rec := httptest.NewRecorder()
	srv.handleGitHubSetupCallback(rec, httptest.NewRequest("GET", "/api/github/setup/callback?installation_id=99999&setup_action=install", nil))
	if cfg := loadConfigFile(srv.configPath()); cfg["FLOW_GH_INSTALLATION_IDS"] != "" {
		t.Errorf("unverified installation id was persisted: %q", cfg["FLOW_GH_INSTALLATION_IDS"])
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "verify") {
		t.Errorf("expected a verification-error page, got: %s", rec.Body.String())
	}
}

func TestHandleGitHubSetupBackfill_NoAppNoOp(t *testing.T) {
	srv, _ := githubSetupTestServer(t) // no App credentials configured

	rec := httptest.NewRecorder()
	srv.handleGitHubSetupBackfill(rec, httptest.NewRequest("POST", "/api/github/setup/backfill", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK       bool `json:"ok"`
		Replayed int  `json:"replayed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.Replayed != 0 {
		t.Errorf("want ok=true replayed=0 with no App; got %+v", resp)
	}
}

func TestHandleGitHubSetupStatus_Shape(t *testing.T) {
	srv, _ := githubSetupTestServer(t)
	t.Setenv("FLOW_GH_APP_ID", "77")
	t.Setenv("FLOW_GH_APP_SLUG", "flow-dev")
	t.Setenv("FLOW_GH_APP_PEM", "PEM")

	rec := httptest.NewRecorder()
	srv.handleGitHubSetupStatus(rec, httptest.NewRequest("GET", "/api/github/setup/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var st GitHubSetupStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !st.IngressReady || st.WebhookURL != "https://flow.example.com/api/github/webhook" {
		t.Errorf("ingress fields wrong: %#v", st)
	}
	if !st.AppCreated || st.AppID != "77" || st.AppSlug != "flow-dev" || !st.PemSet {
		t.Errorf("app fields wrong: %#v", st)
	}
	if st.InstallURL != "https://github.com/apps/flow-dev/installations/new" {
		t.Errorf("install_url = %q", st.InstallURL)
	}
}

func TestHandleGitHubSetupDisconnect_ForgetsCredentials(t *testing.T) {
	srv, _ := githubSetupTestServer(t)

	// Simulate a fully connected App: secrets in the keyring, metadata in
	// config + env, transport flipped to webhook, one installation captured.
	if err := srv.persistGitHubApp(githubManifestConversion{
		AppID: 99, Slug: "flow-dev", ClientID: "Iv1.cid",
		ClientSecret: "cs", WebhookSecret: "wh", PEM: "PEMDATA",
		HTMLURL: "https://github.com/settings/apps/flow-dev",
	}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	srv.captureInstallationID("12345")
	if pre := srv.githubSetupStatus(); !pre.AppCreated || !pre.Installed {
		t.Fatalf("precondition: app should be connected, got %+v", pre)
	}

	rec := httptest.NewRecorder()
	srv.handleGitHubSetupDisconnect(rec, httptest.NewRequest("POST", "/api/github/setup/disconnect", nil))
	if rec.Code != 200 {
		t.Fatalf("disconnect status = %d: %s", rec.Code, rec.Body.String())
	}

	// All three keyring secrets (and their hydrated env vars) are cleared.
	for _, acct := range []string{keyringAcctAppPEM, keyringAcctWebhookSecret, keyringAcctClientSecret} {
		if v, _ := getGitHubSecret(acct); v != "" {
			t.Errorf("secret %q not cleared from keyring: %q", acct, v)
		}
	}
	if os.Getenv("FLOW_GH_APP_PEM") != "" || githubWebhookSecret() != "" {
		t.Errorf("secret env vars still set after disconnect")
	}

	// Non-secret metadata is stripped from both config.json and the env.
	cfg := loadConfigFile(srv.configPath())
	for _, k := range githubAppConfigKeys {
		if cfg[k] != "" {
			t.Errorf("config key %q not removed: %q", k, cfg[k])
		}
		if os.Getenv(k) != "" {
			t.Errorf("env %q not unset: %q", k, os.Getenv(k))
		}
	}

	// Status reverts to not-connected; transport falls back off webhook.
	st := srv.githubSetupStatus()
	if st.AppCreated || st.Installed || st.AppID != "" || st.AppSlug != "" {
		t.Errorf("status still connected after disconnect: %+v", st)
	}
	if st.Transport == "webhook" {
		t.Errorf("transport still webhook after disconnect: %q", st.Transport)
	}
}

func TestHandleGitHubSetupDisconnect_RejectsGET(t *testing.T) {
	srv, _ := githubSetupTestServer(t)
	rec := httptest.NewRecorder()
	srv.handleGitHubSetupDisconnect(rec, httptest.NewRequest("GET", "/api/github/setup/disconnect", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET should be 405, got %d", rec.Code)
	}
}
