package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Slack "Connect" wizard backend.
//
// Today's manual Slack setup is a six-stage token scavenger hunt (manifest
// paste, app-level token, install, two token copies, member-ID lookup). The
// wizard collapses it to three interactions, each backed by an endpoint here:
//
//  1. create-app    — the operator pastes an App Configuration Token
//     (xoxe.xoxp-…, minted at api.slack.com/apps); flow calls
//     apps.manifest.create with the embedded manifest and keeps the returned
//     app_id + client credentials. The config token is used in-flight and
//     never persisted.
//  2. app-token     — Slack ships no API for app-level (xapp-) tokens, so this
//     stays a paste — but deep-linked to the exact page, and validated via
//     apps.connections.open before being stored.
//  3. oauth/start   — real OAuth v2: an ephemeral HTTPS listener with an
//     in-memory self-signed cert serves the redirect URL registered in the
//     manifest (Slack requires https; localhost as a host is accepted —
//     verified live). The callback exchanges the code via oauth.v2.access,
//     which returns the bot token, the user token, AND the operator's user
//     ID in one response — then persists everything and restarts the
//     Socket Mode listener live.
//
// All persistence goes through the config.json settings layer so values
// survive restarts via applyConfigToEnv, and applySettingsRestart bounces the
// listener exactly like a manual Settings save would.

// slackSetupDefaultOAuthPort is the loopback port the OAuth callback listens
// on. It must be stable: the manifest registers the redirect URL at app
// creation time, and the authorize round trip later has to match it exactly.
const slackSetupDefaultOAuthPort = 8790

// slackOAuthCallbackPath is the path component of the registered redirect URL.
const slackOAuthCallbackPath = "/api/slack/oauth/callback"

// slackOAuthDanceTTL bounds how long the ephemeral TLS listener waits for the
// operator to approve the install before shutting down on its own.
const slackOAuthDanceTTL = 15 * time.Minute

func slackOAuthPort() int {
	if raw := strings.TrimSpace(os.Getenv("FLOW_SLACK_OAUTH_PORT")); raw != "" {
		if p, err := strconv.Atoi(raw); err == nil && p > 0 && p < 65536 {
			return p
		}
	}
	return slackSetupDefaultOAuthPort
}

func slackOAuthRedirectURL() string {
	return fmt.Sprintf("https://localhost:%d%s", slackOAuthPort(), slackOAuthCallbackPath)
}

// ---------------------------------------------------------------------------
// Manifest
// ---------------------------------------------------------------------------

// Scope and event sets mirror the README's documented manifest. The wizard is
// the programmatic twin of that YAML — if one changes, change both.
var (
	slackManifestBotScopes = []string{
		"reactions:read",   // the core trigger — seeing :claude:/:codex:
		"channels:history", // public-channel thread replies
		"groups:history",   // private-channel thread replies
		"channels:read",    // channel name/member resolution for task titles
		"groups:read",      // same, private channels
		"users:read",       // author display names
		"app_mentions:read",
		"im:read",
		"mpim:read",
		"chat:write",      // only used when FLOW_SLACK_WRITES_ENABLED=1
		"reactions:write", // same gate
	}
	slackManifestUserScopes = []string{
		"im:history",   // DM following + backfill — the bot can't see your DMs
		"mpim:history", // group DMs
		"channels:history",
		"groups:history",
	}
	slackManifestBotEvents = []string{
		"reaction_added",
		"message.channels",
		"message.groups",
		"app_mention",
	}
	// Delivered only when subscribed under "events on behalf of users" —
	// bot-side subscription alone never sees DM traffic.
	slackManifestUserEvents = []string{
		"message.im",
		"message.mpim",
	}
)

// slackAppManifest builds the JSON manifest document for apps.manifest.create.
// Built as a plain map (marshalled by the API client) rather than a text
// template so there is no escaping surface.
func slackAppManifest(appName, redirectURL string) map[string]any {
	name := strings.TrimSpace(appName)
	if name == "" {
		name = "flow"
	}
	return map[string]any{
		"display_information": map[string]any{
			"name":        name,
			"description": "Turns your Slack reactions and replies into Claude/Codex work.",
		},
		"features": map[string]any{
			"bot_user": map[string]any{
				"display_name":  name,
				"always_online": true,
			},
		},
		"oauth_config": map[string]any{
			"redirect_urls": []string{redirectURL},
			"scopes": map[string]any{
				"bot":  slackManifestBotScopes,
				"user": slackManifestUserScopes,
			},
		},
		"settings": map[string]any{
			"event_subscriptions": map[string]any{
				"bot_events":  slackManifestBotEvents,
				"user_events": slackManifestUserEvents,
			},
			"socket_mode_enabled":    true,
			"org_deploy_enabled":     false,
			"token_rotation_enabled": false,
		},
	}
}

// ---------------------------------------------------------------------------
// Slack API client (manifest + OAuth surface only)
// ---------------------------------------------------------------------------

// slackSetupAPI is a minimal client for the handful of Slack endpoints the
// wizard touches. BaseURL honors FLOW_SLACK_API_BASE_URL so tests can point
// it at an httptest server — the same override convention SlackWriter uses.
type slackSetupAPI struct {
	BaseURL    string
	HTTPClient *http.Client
}

func newSlackSetupAPI() *slackSetupAPI {
	base := strings.TrimRight(os.Getenv("FLOW_SLACK_API_BASE_URL"), "/")
	if base == "" {
		base = "https://slack.com/api"
	}
	return &slackSetupAPI{BaseURL: base}
}

func (a *slackSetupAPI) client() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// callJSON posts a JSON body with a bearer token and decodes the response
// into out (which must include the ok/error envelope fields).
func (a *slackSetupAPI) callJSON(ctx context.Context, method, bearer string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+"/"+method, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	return a.do(req, method, out)
}

// callForm posts application/x-www-form-urlencoded (oauth.v2.access refuses
// JSON bodies) and decodes the response into out.
func (a *slackSetupAPI) callForm(ctx context.Context, method string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.do(req, method, out)
}

func (a *slackSetupAPI) do(req *http.Request, method string, out any) error {
	resp, err := a.client().Do(req)
	if err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("slack %s: read response: %w", method, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("slack %s: unexpected response (HTTP %d)", method, resp.StatusCode)
	}
	return nil
}

type slackAPIEnvelope struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	// Manifest endpoints attach detail rows under errors.
	Errors []struct {
		Message string `json:"message"`
		Pointer string `json:"pointer"`
	} `json:"errors"`
}

func (e slackAPIEnvelope) err(method string) error {
	if e.OK {
		return nil
	}
	msg := e.Error
	if msg == "" {
		msg = "unknown error"
	}
	var details []string
	for _, row := range e.Errors {
		if row.Message == "" {
			continue
		}
		if row.Pointer != "" {
			details = append(details, row.Message+" ("+row.Pointer+")")
		} else {
			details = append(details, row.Message)
		}
	}
	if len(details) > 0 {
		return fmt.Errorf("slack %s: %s: %s", method, msg, strings.Join(details, "; "))
	}
	return fmt.Errorf("slack %s: %s", method, msg)
}

type slackManifestCreateResult struct {
	AppID        string
	ClientID     string
	ClientSecret string
}

func (a *slackSetupAPI) validateManifest(ctx context.Context, configToken string, manifest map[string]any) error {
	var out slackAPIEnvelope
	body := map[string]any{"manifest": manifest}
	if err := a.callJSON(ctx, "apps.manifest.validate", configToken, body, &out); err != nil {
		return err
	}
	return out.err("apps.manifest.validate")
}

func (a *slackSetupAPI) createApp(ctx context.Context, configToken string, manifest map[string]any) (slackManifestCreateResult, error) {
	var out struct {
		slackAPIEnvelope
		AppID       string `json:"app_id"`
		Credentials struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		} `json:"credentials"`
	}
	body := map[string]any{"manifest": manifest}
	if err := a.callJSON(ctx, "apps.manifest.create", configToken, body, &out); err != nil {
		return slackManifestCreateResult{}, err
	}
	if err := out.err("apps.manifest.create"); err != nil {
		return slackManifestCreateResult{}, err
	}
	return slackManifestCreateResult{
		AppID:        out.AppID,
		ClientID:     out.Credentials.ClientID,
		ClientSecret: out.Credentials.ClientSecret,
	}, nil
}

// checkAppToken proves an xapp- token is usable for Socket Mode by asking
// Slack to open a connection slot. The returned WebSocket URL is discarded —
// only the listener dials it for real.
func (a *slackSetupAPI) checkAppToken(ctx context.Context, appToken string) error {
	var out slackAPIEnvelope
	if err := a.callJSON(ctx, "apps.connections.open", appToken, struct{}{}, &out); err != nil {
		return err
	}
	return out.err("apps.connections.open")
}

type slackOAuthResult struct {
	BotToken  string
	UserToken string
	UserID    string
	Team      string
}

func (a *slackSetupAPI) exchangeOAuth(ctx context.Context, clientID, clientSecret, code, redirectURI string) (slackOAuthResult, error) {
	var out struct {
		slackAPIEnvelope
		AccessToken string `json:"access_token"`
		AuthedUser  struct {
			ID          string `json:"id"`
			AccessToken string `json:"access_token"`
		} `json:"authed_user"`
		Team struct {
			Name string `json:"name"`
		} `json:"team"`
	}
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	if err := a.callForm(ctx, "oauth.v2.access", form, &out); err != nil {
		return slackOAuthResult{}, err
	}
	if err := out.err("oauth.v2.access"); err != nil {
		return slackOAuthResult{}, err
	}
	return slackOAuthResult{
		BotToken:  out.AccessToken,
		UserToken: out.AuthedUser.AccessToken,
		UserID:    out.AuthedUser.ID,
		Team:      out.Team.Name,
	}, nil
}

// ---------------------------------------------------------------------------
// Settings persistence
// ---------------------------------------------------------------------------

// persistSlackSettings writes wizard-obtained values through the same
// config.json + env + listener-restart path a manual Settings save takes, so
// wizard state and hand-entered state are indistinguishable downstream.
func (s *Server) persistSlackSettings(values map[string]string) error {
	cfg := loadConfigFile(s.configPath())
	var changed []string
	for key, val := range values {
		val = strings.TrimSpace(val)
		if val == "" || cfg[key] == val {
			continue
		}
		cfg[key] = val
		os.Setenv(key, val)
		changed = append(changed, key)
	}
	if len(changed) == 0 {
		return nil
	}
	if err := saveConfigFile(s.configPath(), cfg); err != nil {
		return err
	}
	s.applySettingsRestart(changed)
	s.publishUIChange("settings")
	return nil
}

// mergeSelfUserIDs unions the OAuth-discovered operator ID into any existing
// comma-separated FLOW_SLACK_SELF_USER_IDS value, preserving order. The
// operator may legitimately hold multiple workspace identities — never drop
// the ones already configured.
func mergeSelfUserIDs(existing, discovered string) string {
	discovered = strings.TrimSpace(discovered)
	if discovered == "" {
		return strings.TrimSpace(existing)
	}
	var ids []string
	seen := map[string]bool{}
	for _, id := range strings.Split(existing, ",") {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if !seen[discovered] {
		ids = append(ids, discovered)
	}
	return strings.Join(ids, ",")
}

// ---------------------------------------------------------------------------
// OAuth dance — ephemeral TLS callback listener
// ---------------------------------------------------------------------------

// slackOAuthDance is the in-flight state of one install attempt: a state
// nonce, the TLS listener serving the registered redirect URL, and the
// outcome. At most one dance runs at a time; starting a new one replaces any
// active dance.
type slackOAuthDance struct {
	mu           sync.Mutex
	state        string
	authorizeURL string
	srv          *http.Server
	addr         string // host:port actually bound (tests bind port 0)
	expires      time.Time
	status       string // "waiting" | "done" | "error"
	errMsg       string
	team         string
}

func (d *slackOAuthDance) snapshot() (status, errMsg, authorizeURL, team string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status, d.errMsg, d.authorizeURL, d.team
}

func (d *slackOAuthDance) shutdown() {
	d.mu.Lock()
	srv := d.srv
	d.srv = nil
	d.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// selfSignedLocalhostCert mints a throwaway TLS cert for the callback
// listener. Slack mandates an https redirect URL; for a loopback hop the
// browser's one-time "proceed anyway" interstitial is the accepted cost —
// the authorization code never leaves the machine.
func selfSignedLocalhostCert() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost", Organization: []string{"flow slack oauth"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

func randomState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// startSlackOAuthDance binds the TLS callback listener and returns the dance.
// port 0 is honored (tests); production passes slackOAuthPort() and the
// redirect URL must match what the manifest registered.
func (s *Server) startSlackOAuthDance(clientID, clientSecret string, port int) (*slackOAuthDance, error) {
	state, err := randomState()
	if err != nil {
		return nil, err
	}
	cert, err := selfSignedLocalhostCert()
	if err != nil {
		return nil, fmt.Errorf("generate callback certificate: %w", err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("bind callback port %d: %w (another flow instance mid-install?)", port, err)
	}

	redirectURI := slackOAuthRedirectURL()
	authorize := "https://slack.com/oauth/v2/authorize?" + url.Values{
		"client_id":    {clientID},
		"scope":        {strings.Join(slackManifestBotScopes, ",")},
		"user_scope":   {strings.Join(slackManifestUserScopes, ",")},
		"redirect_uri": {redirectURI},
		"state":        {state},
	}.Encode()

	dance := &slackOAuthDance{
		state:        state,
		authorizeURL: authorize,
		addr:         ln.Addr().String(),
		expires:      time.Now().Add(slackOAuthDanceTTL),
		status:       "waiting",
	}

	mux := http.NewServeMux()
	mux.HandleFunc(slackOAuthCallbackPath, func(w http.ResponseWriter, r *http.Request) {
		s.handleSlackOAuthCallback(w, r, dance, clientID, clientSecret, redirectURI)
	})
	srv := &http.Server{
		Handler:           mux,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}},
		ReadHeaderTimeout: 5 * time.Second,
	}
	dance.srv = srv

	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			dance.mu.Lock()
			if dance.status == "waiting" {
				dance.status = "error"
				dance.errMsg = "callback listener: " + err.Error()
			}
			dance.mu.Unlock()
		}
	}()
	// Self-destruct after the TTL so an abandoned install doesn't hold the
	// port (or keep a stale state nonce alive) forever.
	go func() {
		time.Sleep(time.Until(dance.expires))
		dance.mu.Lock()
		stillWaiting := dance.status == "waiting"
		if stillWaiting {
			dance.status = "error"
			dance.errMsg = "install timed out; start it again from Settings"
		}
		dance.mu.Unlock()
		if stillWaiting {
			dance.shutdown()
		}
	}()
	return dance, nil
}

func (s *Server) handleSlackOAuthCallback(w http.ResponseWriter, r *http.Request, dance *slackOAuthDance, clientID, clientSecret, redirectURI string) {
	fail := func(status int, public, internal string) {
		dance.mu.Lock()
		dance.status = "error"
		dance.errMsg = internal
		dance.mu.Unlock()
		http.Error(w, public, status)
		s.publishUIChange("slack-setup")
		go dance.shutdown()
	}

	q := r.URL.Query()
	dance.mu.Lock()
	expectedState, expired := dance.state, time.Now().After(dance.expires)
	dance.mu.Unlock()
	if expired {
		fail(http.StatusGone, "this install link expired — start again from flow Settings", "install timed out; start it again from Settings")
		return
	}
	if q.Get("state") != expectedState {
		fail(http.StatusBadRequest, "state mismatch — start again from flow Settings", "OAuth state mismatch (stale or foreign callback)")
		return
	}
	if errParam := q.Get("error"); errParam != "" {
		fail(http.StatusBadRequest, "Slack reported: "+errParam, "Slack authorize error: "+errParam)
		return
	}
	code := q.Get("code")
	if code == "" {
		fail(http.StatusBadRequest, "missing authorization code", "callback carried no code")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tokens, err := newSlackSetupAPI().exchangeOAuth(ctx, clientID, clientSecret, code, redirectURI)
	if err != nil {
		fail(http.StatusBadGateway, "token exchange failed: "+err.Error(), err.Error())
		return
	}

	values := map[string]string{
		"FLOW_SLACK_TOKEN":         tokens.BotToken,
		"FLOW_SLACK_USER_TOKEN":    tokens.UserToken,
		"FLOW_SLACK_SELF_USER_IDS": mergeSelfUserIDs(os.Getenv("FLOW_SLACK_SELF_USER_IDS"), tokens.UserID),
	}
	if err := s.persistSlackSettings(values); err != nil {
		fail(http.StatusInternalServerError, "saving tokens failed: "+err.Error(), err.Error())
		return
	}

	dance.mu.Lock()
	dance.status = "done"
	dance.team = tokens.Team
	dance.mu.Unlock()
	s.publishUIChange("slack-setup")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><title>flow — Slack connected</title></head>
<body style="font-family:ui-monospace,monospace;background:#0f1115;color:#e8e8ea;display:grid;place-items:center;height:100vh;margin:0">
<div style="text-align:center">
<div style="font-size:2rem;margin-bottom:.5rem">✓</div>
<h2 style="margin:.2rem 0">Slack connected%s</h2>
<p style="opacity:.7">flow has the tokens it needs — close this tab and head back to Mission&nbsp;Control.</p>
</div></body></html>`, htmlTeamSuffix(tokens.Team))
	go dance.shutdown()
}

func htmlTeamSuffix(team string) string {
	team = strings.TrimSpace(team)
	if team == "" {
		return ""
	}
	return " to " + htmlEscape(team)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// ---------------------------------------------------------------------------
// HTTP endpoints
// ---------------------------------------------------------------------------

type slackSetupStatus struct {
	AppCreated   bool   `json:"app_created"`
	AppID        string `json:"app_id,omitempty"`
	ManageURL    string `json:"manage_url,omitempty"`
	AppTokenURL  string `json:"app_token_url,omitempty"`
	AppTokenSet  bool   `json:"app_token_set"`
	BotTokenSet  bool   `json:"bot_token_set"`
	UserTokenSet bool   `json:"user_token_set"`
	SelfUserIDs  string `json:"self_user_ids,omitempty"`
	RedirectURL  string `json:"redirect_url"`

	OAuthActive       bool   `json:"oauth_active"`
	OAuthStatus       string `json:"oauth_status,omitempty"`
	OAuthError        string `json:"oauth_error,omitempty"`
	OAuthAuthorizeURL string `json:"oauth_authorize_url,omitempty"`
	OAuthTeam         string `json:"oauth_team,omitempty"`

	ListenerRunning    bool `json:"listener_running"`
	ListenerConnected  bool `json:"listener_connected"`
	ListenerSuppressed bool `json:"listener_suppressed"`
}

func (s *Server) handleSlackSetupStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	appID := strings.TrimSpace(os.Getenv("FLOW_SLACK_APP_ID"))
	st := slackSetupStatus{
		AppCreated:   appID != "" && strings.TrimSpace(os.Getenv("FLOW_SLACK_CLIENT_ID")) != "",
		AppID:        appID,
		AppTokenSet:  strings.TrimSpace(os.Getenv("FLOW_SLACK_APP_TOKEN")) != "",
		BotTokenSet:  strings.TrimSpace(os.Getenv("FLOW_SLACK_TOKEN")) != "",
		UserTokenSet: strings.TrimSpace(os.Getenv("FLOW_SLACK_USER_TOKEN")) != "",
		SelfUserIDs:  strings.TrimSpace(os.Getenv("FLOW_SLACK_SELF_USER_IDS")),
		RedirectURL:  slackOAuthRedirectURL(),
	}
	if appID != "" {
		st.ManageURL = "https://api.slack.com/apps/" + url.PathEscape(appID)
		st.AppTokenURL = st.ManageURL + "/general"
	}
	s.slackSetupMu.Lock()
	dance := s.slackOAuth
	s.slackSetupMu.Unlock()
	if dance != nil {
		status, errMsg, authorizeURL, team := dance.snapshot()
		st.OAuthActive = status == "waiting"
		st.OAuthStatus = status
		st.OAuthError = errMsg
		st.OAuthTeam = team
		if status == "waiting" {
			st.OAuthAuthorizeURL = authorizeURL
		}
	}
	if s.slackListener != nil {
		st.ListenerRunning = s.slackListener.Running()
		st.ListenerConnected = s.slackListener.Connected()
		st.ListenerSuppressed = s.slackListener.Suppressed()
	}
	writeJSON(w, st)
}

func (s *Server) handleSlackSetupCreateApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ConfigToken string `json:"config_token"`
		AppName     string `json:"app_name"`
		Force       bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "invalid JSON body"}, http.StatusBadRequest)
		return
	}

	// Idempotent resume: when an app already exists from a prior run, hand
	// back its identity instead of minting a duplicate. force=true opts into
	// a genuinely new app (e.g. after deleting the old one on api.slack.com).
	existingApp := strings.TrimSpace(os.Getenv("FLOW_SLACK_APP_ID"))
	if existingApp != "" && strings.TrimSpace(os.Getenv("FLOW_SLACK_CLIENT_ID")) != "" && !req.Force {
		writeJSON(w, map[string]any{"ok": true, "app_id": existingApp, "existing": true})
		return
	}

	token := strings.TrimSpace(req.ConfigToken)
	if !strings.HasPrefix(token, "xoxe.xoxp-") {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "that doesn't look like an app configuration token (expected xoxe.xoxp-…) — generate one at api.slack.com/apps under “Your App Configuration Tokens”"}, http.StatusBadRequest)
		return
	}

	manifest := slackAppManifest(req.AppName, slackOAuthRedirectURL())
	api := newSlackSetupAPI()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := api.validateManifest(ctx, token, manifest); err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": err.Error()}, http.StatusBadGateway)
		return
	}
	result, err := api.createApp(ctx, token, manifest)
	if err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": err.Error()}, http.StatusBadGateway)
		return
	}
	if result.AppID == "" || result.ClientID == "" || result.ClientSecret == "" {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "slack created the app but returned incomplete credentials — check it at api.slack.com/apps"}, http.StatusBadGateway)
		return
	}
	if err := s.persistSlackSettings(map[string]string{
		"FLOW_SLACK_APP_ID":        result.AppID,
		"FLOW_SLACK_CLIENT_ID":     result.ClientID,
		"FLOW_SLACK_CLIENT_SECRET": result.ClientSecret,
	}); err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "save app credentials: " + err.Error()}, http.StatusInternalServerError)
		return
	}
	s.publishUIChange("slack-setup")
	writeJSON(w, map[string]any{"ok": true, "app_id": result.AppID})
}

func (s *Server) handleSlackSetupAppToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		AppToken string `json:"app_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "invalid JSON body"}, http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(req.AppToken)
	if !strings.HasPrefix(token, "xapp-") {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "that doesn't look like an app-level token (expected xapp-…)"}, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := newSlackSetupAPI().checkAppToken(ctx, token); err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": err.Error()}, http.StatusBadGateway)
		return
	}
	if err := s.persistSlackSettings(map[string]string{"FLOW_SLACK_APP_TOKEN": token}); err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "save app token: " + err.Error()}, http.StatusInternalServerError)
		return
	}
	s.publishUIChange("slack-setup")
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSlackSetupOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	clientID := strings.TrimSpace(os.Getenv("FLOW_SLACK_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("FLOW_SLACK_CLIENT_SECRET"))
	if clientID == "" || clientSecret == "" {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "create the Slack app first (step 1) — flow has no client credentials yet"}, http.StatusConflict)
		return
	}

	s.slackSetupMu.Lock()
	if s.slackOAuth != nil {
		old := s.slackOAuth
		s.slackOAuth = nil
		s.slackSetupMu.Unlock()
		old.shutdown()
		s.slackSetupMu.Lock()
	}
	dance, err := s.startSlackOAuthDance(clientID, clientSecret, slackOAuthPort())
	if err == nil {
		s.slackOAuth = dance
	}
	s.slackSetupMu.Unlock()
	if err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": err.Error()}, http.StatusConflict)
		return
	}
	_, _, authorizeURL, _ := dance.snapshot()
	writeJSON(w, map[string]any{"ok": true, "authorize_url": authorizeURL, "redirect_url": slackOAuthRedirectURL()})
}

func (s *Server) handleSlackSetupOAuthCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.slackSetupMu.Lock()
	dance := s.slackOAuth
	s.slackOAuth = nil
	s.slackSetupMu.Unlock()
	if dance != nil {
		dance.shutdown()
	}
	writeJSON(w, map[string]any{"ok": true})
}
