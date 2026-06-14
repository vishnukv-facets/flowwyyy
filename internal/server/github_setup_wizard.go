package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"flow/internal/monitor"
)

// githubSetupCallbackPath is the redirect_url path GitHub sends the manifest
// conversion code to. It is served on the public ingress mux (the manifest
// requires a public redirect) as well as locally.
const githubSetupCallbackPath = "/api/github/setup/callback"

// githubManifestTTL bounds how long a started setup remains valid before the
// state nonce is rejected.
const githubManifestTTL = 30 * time.Minute

// githubManifestPending is the server-side state of one in-flight Connect
// GitHub attempt.
type githubManifestPending struct {
	state   string
	target  string
	org     string
	created time.Time
}

func randomGitHubState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pendingGitHubSetupState returns the state nonce of the in-flight setup, or ""
// when none is active. Exposed for tests.
func (s *Server) pendingGitHubSetupState() string {
	s.githubSetupMu.Lock()
	defer s.githubSetupMu.Unlock()
	if s.githubSetup == nil {
		return ""
	}
	return s.githubSetup.state
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// GitHubSetupStatus drives the resumable Connect-GitHub wizard card. Every
// field is read from the process env (hydrated from config.json + the keyring
// at boot), so polling it never touches the keychain.
type GitHubSetupStatus struct {
	IngressReady     bool   `json:"ingress_ready"`
	WebhookURL       string `json:"webhook_url,omitempty"`
	RedirectURL      string `json:"redirect_url,omitempty"`
	AppCreated       bool   `json:"app_created"`
	AppID            string `json:"app_id,omitempty"`
	AppSlug          string `json:"app_slug,omitempty"`
	HTMLURL          string `json:"html_url,omitempty"`
	PemSet           bool   `json:"pem_set"`
	WebhookSecretSet bool   `json:"webhook_secret_set"`
	InstallURL       string `json:"install_url,omitempty"`
	InstallationIDs  string `json:"installation_ids,omitempty"`
	Installed        bool   `json:"installed"`
	// SelfLoginsSet reports whether FLOW_GH_SELF_LOGINS is configured. While it's
	// empty the involvement gate fails closed (drops every webhook event as
	// out-of-scope), so the wizard hard-warns — see githubSetupSummary (P0-2).
	SelfLogins    string `json:"self_logins,omitempty"`
	SelfLoginsSet bool   `json:"self_logins_set"`
	Transport     string `json:"transport"`
	Summary       string `json:"summary"`
}

func (s *Server) githubSetupStatus() GitHubSetupStatus {
	appID := strings.TrimSpace(os.Getenv("FLOW_GH_APP_ID"))
	slug := strings.TrimSpace(os.Getenv("FLOW_GH_APP_SLUG"))
	installs := strings.TrimSpace(os.Getenv("FLOW_GH_INSTALLATION_IDS"))
	selfLogins := strings.TrimSpace(os.Getenv("FLOW_GH_SELF_LOGINS"))
	st := GitHubSetupStatus{
		IngressReady:     s.publicBaseURL() != "",
		WebhookURL:       s.connectorCallbackURL("/api/github/webhook"),
		RedirectURL:      s.connectorCallbackURL(githubSetupCallbackPath),
		AppCreated:       appID != "" && os.Getenv("FLOW_GH_APP_PEM") != "",
		AppID:            appID,
		AppSlug:          slug,
		HTMLURL:          strings.TrimSpace(os.Getenv("FLOW_GH_HTML_URL")),
		PemSet:           strings.TrimSpace(os.Getenv("FLOW_GH_APP_PEM")) != "",
		WebhookSecretSet: githubWebhookSecret() != "",
		InstallationIDs:  installs,
		Installed:        installs != "",
		SelfLogins:       selfLogins,
		SelfLoginsSet:    selfLogins != "",
		Transport:        string(monitor.GitHubTransport()),
	}
	if slug != "" {
		st.InstallURL = "https://github.com/apps/" + url.PathEscape(slug) + "/installations/new"
	}
	st.Summary = githubSetupSummary(st)
	return st
}

func githubSetupSummary(st GitHubSetupStatus) string {
	switch {
	case !st.IngressReady:
		return "Start public ingress first — the App's webhook needs a public URL"
	case !st.AppCreated:
		return "Create a GitHub App to connect"
	case !st.Installed:
		return "App created — install it on your account or org"
	case !st.SelfLoginsSet:
		// Installed but no operator identity: the gate fails closed, so Flow
		// drops every webhook event (including the operator's own) as
		// out-of-scope until they set their login. Hard-warn rather than report
		// "connected" (P0-2).
		return "⚠ Connected, but your GitHub login isn't set — add it under “Your GitHub logins” so Flow picks up items that involve you. Until then every event is dropped as out-of-scope."
	default:
		return "Connected — receiving GitHub webhooks"
	}
}

func (s *Server) handleGitHubSetupStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.githubSetupStatus())
}

// ---------------------------------------------------------------------------
// create-app
// ---------------------------------------------------------------------------

func (s *Server) handleGitHubSetupCreateApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name   string `json:"name"`
		Target string `json:"target"` // "user" | "org"
		Org    string `json:"org"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "invalid JSON body"}, http.StatusBadRequest)
		return
	}

	// The manifest's hook_attributes.url must be a public URL at App-creation
	// time, so a running public ingress is a hard prerequisite.
	webhookURL := s.connectorCallbackURL("/api/github/webhook")
	redirectURL := s.connectorCallbackURL(githubSetupCallbackPath)
	if webhookURL == "" || redirectURL == "" {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "public ingress is not running — enable it first so GitHub can reach the webhook + setup callback"}, http.StatusServiceUnavailable)
		return
	}

	state := randomGitHubState()
	createURL, err := githubManifestCreateURL(req.Target, req.Org, state)
	if err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": err.Error()}, http.StatusBadRequest)
		return
	}

	s.githubSetupMu.Lock()
	s.githubSetup = &githubManifestPending{state: state, target: req.Target, org: strings.TrimSpace(req.Org), created: time.Now()}
	s.githubSetupMu.Unlock()

	writeJSON(w, map[string]any{
		"ok":         true,
		"state":      state,
		"create_url": createURL,
		"manifest":   githubAppManifest(req.Name, webhookURL, redirectURL),
	})
}

// ---------------------------------------------------------------------------
// callback — manifest conversion
// ---------------------------------------------------------------------------

func (s *Server) handleGitHubSetupCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	state := strings.TrimSpace(r.URL.Query().Get("state"))

	// Post-install redirect (App setup_url): carries installation_id, not a
	// manifest code or our state nonce. This branch is served on the PUBLIC
	// ingress mux with no state nonce (the install URL we hand the operator
	// carries none for GitHub to echo), so an attacker could otherwise POST an
	// arbitrary installation_id here. Verify the id actually belongs to this App
	// (App-JWT authed, reflects GitHub's truth) before persisting; fail closed.
	if code == "" {
		if instID := strings.TrimSpace(r.URL.Query().Get("installation_id")); instID != "" {
			vctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			if !verifyGitHubInstallation(vctx, instID) {
				writeSetupResultHTML(w, callbackError, "Couldn't verify the installation",
					"The installation_id did not match this App's installations. If you just installed it, retry from Connect GitHub.")
				return
			}
			s.captureInstallationID(instID)
			// The App is now installed, so its installation account login is
			// available — seed FLOW_GH_SELF_LOGINS from it (P0-2) so the
			// involvement gate has a non-empty operator identity. Best-effort and
			// non-clobbering; githubSetupStatus hard-warns if it stays empty.
			actx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			s.autoPopulateGitHubSelfLogins(actx)
			cancel()
			s.publishUIChange("github-setup")
			writeSetupResultHTML(w, callbackOK, "GitHub App installed", "Flow is now connected and receiving webhooks.")
			return
		}
	}

	s.githubSetupMu.Lock()
	pending := s.githubSetup
	s.githubSetupMu.Unlock()

	if pending == nil || state == "" || state != pending.state {
		writeSetupResultHTML(w, callbackError, "Couldn't verify the setup request", "The state nonce did not match or the setup expired. Start Connect GitHub again.")
		return
	}
	if time.Since(pending.created) > githubManifestTTL {
		s.clearGitHubSetup()
		writeSetupResultHTML(w, callbackError, "Setup expired", "Too much time passed before GitHub returned. Start Connect GitHub again.")
		return
	}
	if code == "" {
		writeSetupResultHTML(w, callbackError, "No code returned", "GitHub didn't return a manifest code. Try Connect GitHub again.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	conv, err := newGitHubSetupAPI().convertManifest(ctx, code)
	if err != nil {
		writeSetupResultHTML(w, callbackError, "GitHub setup failed", html.EscapeString(err.Error()))
		return
	}
	if err := s.persistGitHubApp(conv); err != nil {
		writeSetupResultHTML(w, callbackError, "Couldn't save the GitHub App", html.EscapeString(err.Error()))
		return
	}
	s.clearGitHubSetup()
	s.publishUIChange("github-setup")

	installURL := "https://github.com/apps/" + url.PathEscape(conv.Slug) + "/installations/new"
	writeSetupResultHTML(w, callbackOK, "GitHub App created",
		fmt.Sprintf("Flow now owns the App <b>%s</b>. Next, install it on your personal account and any org you want Flow to watch: <a href=%q>Install the App</a>.",
			html.EscapeString(conv.Slug), installURL))
}

// handleGitHubSetupBackfill replays GitHub App webhook deliveries missed while
// Flow / the public ingress was down — the correct gap-recovery path (redelivery
// replay, not re-polling). Idempotent: already-processed deliveries are skipped.
func (s *Server) handleGitHubSetupBackfill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.DB == nil || s.githubListener == nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "GitHub ingress is not initialized"}, http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	n, err := monitor.BackfillGitHubDeliveries(ctx, s.cfg.DB, s.githubListener.Dispatch)
	if err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": err.Error()}, http.StatusBadGateway)
		return
	}
	if n > 0 {
		s.publishUIChange("github-setup")
	}
	writeJSON(w, map[string]any{"ok": true, "replayed": n})
}

func (s *Server) clearGitHubSetup() {
	s.githubSetupMu.Lock()
	s.githubSetup = nil
	s.githubSetupMu.Unlock()
}

// ---------------------------------------------------------------------------
// disconnect — forget the App on this machine
// ---------------------------------------------------------------------------

// githubAppConfigKeys are the non-secret config.json keys persistGitHubApp /
// captureInstallationID write. Disconnect removes exactly these so config.json
// returns to its pre-Connect state. FLOW_GH_TRANSPORT is included so the mode
// reverts to its legacy default (gh polling if enabled, else off) instead of
// staying "webhook" while pointing at an App that no longer exists.
var githubAppConfigKeys = []string{
	"FLOW_GH_APP_ID",
	"FLOW_GH_APP_SLUG",
	"FLOW_GH_CLIENT_ID",
	"FLOW_GH_HTML_URL",
	"FLOW_GH_INSTALLATION_IDS",
	"FLOW_GH_TRANSPORT",
}

// forgetGitHubApp is the inverse of persistGitHubApp: it deletes the three App
// secrets from the keyring (and their hydrated env vars) and strips the
// non-secret App metadata from config.json + env, then bounces the listener +
// ingress so the cleared transport/secret take effect without a restart.
//
// It deliberately does NOT touch the App on github.com — that App and any
// installation still exist there and must be removed by the operator. This only
// severs Flow's copy of the credentials. It also leaves the legacy `gh` CLI
// keyring identity untouched, so the polling fallback keeps working.
func (s *Server) forgetGitHubApp() error {
	// "" clears both the keyring entry and the hydrated env var.
	for _, acct := range []string{keyringAcctAppPEM, keyringAcctWebhookSecret, keyringAcctClientSecret} {
		if err := storeGitHubSecret(acct, ""); err != nil {
			return fmt.Errorf("clear %s: %w", acct, err)
		}
	}
	cfg := loadConfigFile(s.configPath())
	for _, k := range githubAppConfigKeys {
		delete(cfg, k)
		os.Unsetenv(k)
	}
	if err := saveConfigFile(s.configPath(), cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	if s.githubListener != nil {
		s.githubListener.Stop()
		_ = s.githubListener.Start()
	}
	s.restartIngress()
	return nil
}

// handleGitHubSetupInstallations lists the accounts the connected App is
// installed on (personal + orgs), so the wizard can show "installed on X and Y"
// and nudge the operator to install on both. Always 200 — on failure (no App,
// API error) it returns an empty list + error string rather than breaking the
// wizard. Not folded into the polled setup-status because it hits the GitHub API.
func (s *Server) handleGitHubSetupInstallations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	installs, _, err := monitor.ListGitHubAppInstallations(ctx)
	resp := map[string]any{"installations": installs}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, resp)
}

// handleGitHubSetupDisconnect forgets the connected GitHub App's credentials on
// this machine. The App itself is not deleted on github.com (see forgetGitHubApp).
func (s *Server) handleGitHubSetupDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := s.forgetGitHubApp(); err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": err.Error()}, http.StatusInternalServerError)
		return
	}
	s.publishUIChange("github-setup")
	writeJSON(w, map[string]any{"ok": true})
}

// verifyGitHubInstallation reports whether an installation id genuinely belongs
// to the connected App. It lists the App's installations (App-JWT authed, so it
// reflects GitHub's truth, not Flow's captured list) and checks membership.
// Fails CLOSED: any API error, no connected App, or a non-matching id returns
// false — so the unauthenticated public setup callback can't persist an
// attacker-supplied installation_id. Overridable in tests.
var verifyGitHubInstallation = func(ctx context.Context, id string) bool {
	want := strings.TrimSpace(id)
	if want == "" {
		return false
	}
	installs, ok, err := monitor.ListGitHubAppInstallations(ctx)
	if err != nil || !ok {
		return false
	}
	for _, in := range installs {
		if strconv.FormatInt(in.ID, 10) == want {
			return true
		}
	}
	return false
}

// captureInstallationID appends an installation id to FLOW_GH_INSTALLATION_IDS
// (a comma-separated, order-preserving, deduped list) and persists it to
// config + env. One App can be installed on several accounts/orgs, so the list
// grows; the SDK mints tokens per installation.
func (s *Server) captureInstallationID(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	merged := mergeInstallationIDs(os.Getenv("FLOW_GH_INSTALLATION_IDS"), id)
	os.Setenv("FLOW_GH_INSTALLATION_IDS", merged)
	cfg := loadConfigFile(s.configPath())
	cfg["FLOW_GH_INSTALLATION_IDS"] = merged
	_ = saveConfigFile(s.configPath(), cfg)
}

// listGitHubAppInstallations is a package var (not a direct call) so tests can
// inject installations without standing up a real App JWT + GitHub API.
var listGitHubAppInstallations = monitor.ListGitHubAppInstallations

// autoPopulateGitHubSelfLogins seeds FLOW_GH_SELF_LOGINS from the connected
// App's installation account login(s) when the operator hasn't set it. This is
// the usability half of the P0-2 fix: the involvement gate now fails CLOSED
// when self-logins is empty, which would also drop the operator's OWN items —
// so we auto-fill their login from the install so their PRs/issues/mentions are
// tracked while everyone else's are dropped. Best-effort: any API error leaves
// the field empty so githubSetupStatus can hard-warn. Never clobbers an
// operator-set value.
func (s *Server) autoPopulateGitHubSelfLogins(ctx context.Context) {
	if len(monitor.GitHubSelfLogins()) > 0 {
		return // respect the operator's existing config
	}
	installs, ok, err := listGitHubAppInstallations(ctx)
	if err != nil || !ok {
		return
	}
	logins := selfLoginsFromInstallations(installs)
	if len(logins) == 0 {
		return
	}
	s.setGitHubSelfLogins(strings.Join(logins, ","))
}

// selfLoginsFromInstallations picks the operator's personal login(s) from the
// App's installations: User-type accounts only (deduped, order-preserving). Org
// accounts are skipped — an org never authors a PR/issue, so it can't match the
// gate's participant check and would only mask the hard-warn while leaving the
// operator's own items undetected. Org-only installs keep self-logins empty so
// the operator is prompted to enter their login manually.
func selfLoginsFromInstallations(installs []monitor.GitHubInstallation) []string {
	var out []string
	seen := map[string]bool{}
	for _, in := range installs {
		if !strings.EqualFold(strings.TrimSpace(in.Type), "User") {
			continue
		}
		login := strings.TrimSpace(in.Account)
		if login == "" || seen[strings.ToLower(login)] {
			continue
		}
		seen[strings.ToLower(login)] = true
		out = append(out, login)
	}
	return out
}

// setGitHubSelfLogins persists FLOW_GH_SELF_LOGINS to config.json and the live
// env. The involvement gates re-read the env per event (the steering config is
// rebuilt per event), so no listener bounce is needed for it to take effect.
func (s *Server) setGitHubSelfLogins(val string) {
	os.Setenv("FLOW_GH_SELF_LOGINS", val)
	cfg := loadConfigFile(s.configPath())
	cfg["FLOW_GH_SELF_LOGINS"] = val
	_ = saveConfigFile(s.configPath(), cfg)
}

// mergeInstallationIDs unions a new id into an existing comma-separated list,
// preserving order and dropping duplicates.
func mergeInstallationIDs(existing, add string) string {
	var ids []string
	seen := map[string]bool{}
	for _, v := range strings.Split(existing+","+add, ",") {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		ids = append(ids, v)
	}
	return strings.Join(ids, ",")
}

// persistGitHubApp stores the converted App credentials: secrets (PEM, client
// secret, webhook secret) go to the OS keyring; non-secret metadata to
// config.json. It flips the transport to webhook (the poller stays off) and
// bounces the listener + ingress so the new signing secret takes effect live.
func (s *Server) persistGitHubApp(conv githubManifestConversion) error {
	if err := storeGitHubSecret(keyringAcctAppPEM, conv.PEM); err != nil {
		return fmt.Errorf("store App private key: %w", err)
	}
	if err := storeGitHubSecret(keyringAcctWebhookSecret, conv.WebhookSecret); err != nil {
		return fmt.Errorf("store webhook secret: %w", err)
	}
	if err := storeGitHubSecret(keyringAcctClientSecret, conv.ClientSecret); err != nil {
		return fmt.Errorf("store client secret: %w", err)
	}

	cfg := loadConfigFile(s.configPath())
	cfg["FLOW_GH_APP_ID"] = strconv.FormatInt(conv.AppID, 10)
	cfg["FLOW_GH_APP_SLUG"] = conv.Slug
	cfg["FLOW_GH_CLIENT_ID"] = conv.ClientID
	cfg["FLOW_GH_HTML_URL"] = conv.HTMLURL
	// Webhook-first: the App's webhook delivers events, so stop the poller.
	cfg["FLOW_GH_TRANSPORT"] = "webhook"
	for k, v := range cfg {
		if strings.HasPrefix(k, "FLOW_GH_APP_") || k == "FLOW_GH_CLIENT_ID" || k == "FLOW_GH_HTML_URL" || k == "FLOW_GH_TRANSPORT" {
			os.Setenv(k, v)
		}
	}
	if err := saveConfigFile(s.configPath(), cfg); err != nil {
		return fmt.Errorf("save App metadata: %w", err)
	}

	// Bounce the GitHub listener + ingress so the new transport + webhook
	// secret take effect without a restart.
	if s.githubListener != nil {
		s.githubListener.Stop()
		_ = s.githubListener.Start()
	}
	s.restartIngress()
	return nil
}

// writeSetupResultHTML renders a minimal standalone result page for the OAuth /
// manifest callbacks, which open in the operator's browser on the public
// ingress (which serves no UI). Thin wrapper over the shared, brand-matched
// callback renderer (see callback_page.go) — kind selects the success/error
// styling; body is trusted HTML (callers escape any dynamic text). Always 200,
// matching the prior behavior (the wizard reads outcome from setup status).
func writeSetupResultHTML(w http.ResponseWriter, kind callbackResultKind, title, body string) {
	writeCallbackResultHTML(w, http.StatusOK, kind, title, body)
}
