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

// slackManifestRev is bumped whenever the manifest changes scopes, events, or
// app_home settings in a way that requires the operator to reinstall (OAuth +
// re-approve). Persisted as FLOW_SLACK_MANIFEST_REV after create-app and
// successful OAuth; status compares the stored value to this constant and sets
// NeedsReinstall when they differ.
const slackManifestRev = "4"

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

// slackLoopbackCallbackURL is the self-signed loopback fallback used when no
// public ingress is available — it needs the short-lived TLS listener started
// by startSlackOAuthDance, and costs the operator a one-time browser cert
// warning.
func (s *Server) slackLoopbackCallbackURL() string {
	return fmt.Sprintf("https://localhost:%d%s", slackOAuthPort(), slackOAuthCallbackPath)
}

// slackRedirectURL is the OAuth redirect Slack sends the operator back to. When
// a public ingress (zrok) is up it uses the public HTTPS URL — a real cert (no
// browser warning), served on the ingress mux like the GitHub callback;
// otherwise it falls back to the loopback listener. Used for the authorize
// round-trip and reported in setup status; the manifest registers BOTH (see
// slackRedirectURLs) so the app works whichever transport is live.
func (s *Server) slackRedirectURL() string {
	if pub := s.connectorCallbackURL(slackOAuthCallbackPath); pub != "" {
		return pub
	}
	return s.slackLoopbackCallbackURL()
}

// slackRedirectURLs is the set the manifest registers — both the public and the
// loopback URLs when a public ingress exists (so a later restart that loses the
// public URL still authorizes via loopback), else just the loopback.
func (s *Server) slackRedirectURLs() []string {
	loop := s.slackLoopbackCallbackURL()
	if pub := s.connectorCallbackURL(slackOAuthCallbackPath); pub != "" && pub != loop {
		return []string{pub, loop}
	}
	return []string{loop}
}

func (s *Server) slackCallbackMode() string {
	if s.connectorCallbackURL(slackOAuthCallbackPath) != "" {
		return "public"
	}
	return "localhost"
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
		"files:read",       // safe text/PDF extraction for channel file shares
		"app_mentions:read",
		"im:read",
		"im:history", // bot's own DMs; user-scope im:history below covers the operator's DMs
		"im:write",   // resolve/open operator↔bot IM via conversations.open
		"mpim:read",
		"chat:write",      // only used when FLOW_SLACK_WRITES_ENABLED=1
		"reactions:write", // same gate
		"files:write",     // upload attachments via `flow slack send --file` (same gate)
	}
	slackManifestUserScopes = []string{
		"im:history",   // DM following + backfill — the bot can't see your DMs
		"mpim:history", // group DMs
		"channels:history",
		"groups:history",
		// channel/DM monitoring rides the operator's membership, not the bot's:
		// the user-token *:read scopes below let flow enumerate the operator's DMs
		// (im/mpim:read — without these the DM backfill fails with missing_scope)
		// and read channel/member metadata on the user token, so a watched channel
		// is covered even when the bot was never invited to it. Mirrors the
		// pre-wizard manifest that worked before bot-membership was assumed.
		"im:read",
		"mpim:read",
		"channels:read",
		"groups:read",
		"users:read",
		"files:read",  // safe text/PDF extraction for DM file shares
		"chat:write",  // post replies/messages AS the operator (FLOW_SLACK_SEND_AS=user / `--as user`); gated by FLOW_SLACK_WRITES_ENABLED
		"files:write", // upload attachments AS the operator (`flow slack send --as user --file`); same gate
	}
	slackManifestBotEvents = []string{
		"reaction_added",
		"message.channels",
		"message.groups",
		"message.im", // bot receives DMs sent directly to it
		"app_mention",
	}
	// Delivered only when subscribed under "events on behalf of users" —
	// bot-side subscription alone never sees DM traffic. message.channels/groups
	// are subscribed here too (not just as bot_events) so the operator's channel
	// messages reach flow via THEIR membership — covering watched channels the
	// bot isn't a member of (the live half of the same fix as the user *:read
	// scopes above). Duplicate deliveries for channels the bot is also in are
	// deduped downstream by (channel, ts).
	slackManifestUserEvents = []string{
		"message.im",
		"message.mpim",
		"message.channels",
		"message.groups",
	}
)

// slackAppManifest builds the JSON manifest document for apps.manifest.create.
// Built as a plain map (marshalled by the API client) rather than a text
// template so there is no escaping surface.
func slackAppManifest(appName string, redirectURLs []string) map[string]any {
	name := strings.TrimSpace(appName)
	if name == "" {
		name = "flow"
	}
	return map[string]any{
		"display_information": map[string]any{
			"name":             name,
			"description":      "Turns your Slack reactions and replies into Claude/Codex work.",
			"background_color": "#1b1b1f",
		},
		"features": map[string]any{
			"bot_user": map[string]any{
				"display_name":  name,
				"always_online": true,
			},
			"app_home": map[string]any{
				"home_tab_enabled":               false,
				"messages_tab_enabled":           true,
				"messages_tab_read_only_enabled": false,
			},
		},
		"oauth_config": map[string]any{
			"redirect_urls": redirectURLs,
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
	BotUserID string
	Team      string
}

func (a *slackSetupAPI) exchangeOAuth(ctx context.Context, clientID, clientSecret, code, redirectURI string) (slackOAuthResult, error) {
	var out struct {
		slackAPIEnvelope
		AccessToken string `json:"access_token"`
		BotUserID   string `json:"bot_user_id"`
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
		BotUserID: out.BotUserID,
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
	cfgDirty := false
	for key, val := range values {
		val = strings.TrimSpace(val)
		if val == "" {
			continue
		}
		// Secrets (bot token, user token, client secret) go to the OS keyring,
		// never config.json — matching the GitHub App secret handling.
		if account, ok := slackSecretAccountForEnv(key); ok {
			if os.Getenv(key) != val {
				if err := storeSlackSecret(account, val); err != nil {
					return err
				}
				changed = append(changed, key)
			}
			// Migrate any legacy plaintext secret out of config.json so it no
			// longer lives at rest in the config file after an upgrade.
			if _, present := cfg[key]; present {
				delete(cfg, key)
				cfgDirty = true
			}
			continue
		}
		if cfg[key] == val {
			continue
		}
		cfg[key] = val
		os.Setenv(key, val)
		changed = append(changed, key)
		cfgDirty = true
	}
	if len(changed) == 0 && !cfgDirty {
		return nil
	}
	if cfgDirty {
		if err := saveConfigFile(s.configPath(), cfg); err != nil {
			return err
		}
	}
	if len(changed) > 0 {
		s.applySettingsRestart(changed)
	}
	s.publishUIChange("settings")
	return nil
}

// mergeSelfUserIDs unions an OAuth-discovered Slack user ID into an existing
// comma-separated identity list, preserving order and de-duplicating. Used for
// both FLOW_SLACK_SELF_USER_IDS (the operator) and FLOW_SLACK_SELF_BOT_USER_IDS
// (flow's own bot): either may legitimately hold multiple workspace identities,
// so the ones already configured are never dropped.
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
	mu            sync.Mutex
	state         string
	authorizeURL  string
	redirectURI   string // the exact redirect_uri used at authorize; reused at callback
	srv           *http.Server
	addr          string // host:port actually bound (tests bind port 0)
	expires       time.Time
	status        string // "waiting" | "done" | "error"
	errMsg        string
	team          string
	publicIngress bool // true when the callback rides the public ingress mux (not loopback TLS)
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

	redirectURI := s.slackRedirectURL()
	// Public mode when a public ingress is up: the callback rides the standing
	// ingress mux (real TLS cert, no localhost warning), so we skip the loopback
	// TLS listener. Otherwise fall back to the self-signed loopback listener.
	usingPublicIngress := s.connectorCallbackURL(slackOAuthCallbackPath) != ""

	authorize := "https://slack.com/oauth/v2/authorize?" + url.Values{
		"client_id":    {clientID},
		"scope":        {strings.Join(slackManifestBotScopes, ",")},
		"user_scope":   {strings.Join(slackManifestUserScopes, ",")},
		"redirect_uri": {redirectURI},
		"state":        {state},
	}.Encode()

	dance := &slackOAuthDance{
		state:         state,
		authorizeURL:  authorize,
		redirectURI:   redirectURI,
		expires:       time.Now().Add(slackOAuthDanceTTL),
		status:        "waiting",
		publicIngress: usingPublicIngress,
	}

	if !usingPublicIngress {
		// Localhost mode: bind an ephemeral self-signed TLS listener.
		// Slack mandates HTTPS for the redirect URL; for a loopback hop the
		// one-time browser certificate warning is the accepted cost. This avoids
		// exposing any Slack OAuth callback on the standing public ingress.
		cert, err := selfSignedLocalhostCert()
		if err != nil {
			return nil, fmt.Errorf("generate callback certificate: %w", err)
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return nil, fmt.Errorf("bind callback port %d: %w (another flow instance mid-install?)", port, err)
		}
		dance.addr = ln.Addr().String()
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
	}
	// Standing public ingress is intentionally not used for Slack OAuth.

	// Self-destruct after the TTL.
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
		// Branded error page (callback_page.go), same design as success.
		writeCallbackResultHTML(w, status, callbackError, "Couldn't connect Slack", htmlEscape(public))
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
		"FLOW_SLACK_TOKEN":             tokens.BotToken,
		"FLOW_SLACK_USER_TOKEN":        tokens.UserToken,
		"FLOW_SLACK_SELF_USER_IDS":     mergeSelfUserIDs(os.Getenv("FLOW_SLACK_SELF_USER_IDS"), tokens.UserID),
		"FLOW_SLACK_SELF_BOT_USER_IDS": mergeSelfUserIDs(os.Getenv("FLOW_SLACK_SELF_BOT_USER_IDS"), tokens.BotUserID),
		"FLOW_SLACK_MANIFEST_REV":      slackManifestRev,
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

	title := "Slack connected"
	if team := strings.TrimSpace(tokens.Team); team != "" {
		title += " to " + team
	}
	// Branded result page (callback_page.go) — same design as the GitHub callback.
	writeCallbackResultHTML(w, http.StatusOK, callbackOK, title, "flow has the tokens it needs.")
	go dance.shutdown()
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
	// CallbackMode reports how the OAuth redirect is handled. Slack setup is
	// intentionally localhost-only and never uses standing public ingress.
	CallbackMode string `json:"callback_mode"`

	OAuthActive       bool   `json:"oauth_active"`
	OAuthStatus       string `json:"oauth_status,omitempty"`
	OAuthError        string `json:"oauth_error,omitempty"`
	OAuthAuthorizeURL string `json:"oauth_authorize_url,omitempty"`
	OAuthTeam         string `json:"oauth_team,omitempty"`

	ListenerRunning    bool `json:"listener_running"`
	ListenerConnected  bool `json:"listener_connected"`
	ListenerSuppressed bool `json:"listener_suppressed"`

	// NeedsReinstall is true when the operator has installed (token present) but
	// the persisted FLOW_SLACK_MANIFEST_REV lags the current slackManifestRev —
	// meaning new scopes/events from a manifest update require a fresh OAuth install.
	NeedsReinstall bool `json:"needs_reinstall"`
}

// computeSlackSetupStatus builds the current Slack setup status without
// writing to an http.ResponseWriter — extracted for testability.
func (s *Server) computeSlackSetupStatus() slackSetupStatus {
	appID := strings.TrimSpace(os.Getenv("FLOW_SLACK_APP_ID"))
	st := slackSetupStatus{
		AppCreated:   appID != "" && strings.TrimSpace(os.Getenv("FLOW_SLACK_CLIENT_ID")) != "",
		AppID:        appID,
		AppTokenSet:  strings.TrimSpace(os.Getenv("FLOW_SLACK_APP_TOKEN")) != "",
		BotTokenSet:  strings.TrimSpace(os.Getenv("FLOW_SLACK_TOKEN")) != "",
		UserTokenSet: strings.TrimSpace(os.Getenv("FLOW_SLACK_USER_TOKEN")) != "",
		SelfUserIDs:  strings.TrimSpace(os.Getenv("FLOW_SLACK_SELF_USER_IDS")),
		RedirectURL:  s.slackRedirectURL(),
		CallbackMode: s.slackCallbackMode(),
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
	installed := st.UserTokenSet || st.BotTokenSet
	st.NeedsReinstall = installed && strings.TrimSpace(os.Getenv("FLOW_SLACK_MANIFEST_REV")) != slackManifestRev
	return st
}

func (s *Server) handleSlackSetupStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.computeSlackSetupStatus())
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

	manifest := slackAppManifest(req.AppName, s.slackRedirectURLs())
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
		"FLOW_SLACK_MANIFEST_REV":  slackManifestRev,
	}); err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "save app credentials: " + err.Error()}, http.StatusInternalServerError)
		return
	}
	s.publishUIChange("slack-setup")
	writeJSON(w, map[string]any{
		"ok":              true,
		"app_id":          result.AppID,
		"icon_upload_url": "https://api.slack.com/apps/" + url.PathEscape(result.AppID) + "/general",
		"icon_asset_url":  "/flow-app-icon-512.png",
	})
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
	writeJSON(w, map[string]any{"ok": true, "authorize_url": authorizeURL, "redirect_url": s.slackRedirectURL()})
}

// slackAppConfigKeys are the app-specific Slack config keys a reset clears so
// the wizard returns to step 1 (create app). The operator's own Slack user IDs
// (FLOW_SLACK_SELF_USER_IDS) are intentionally preserved — that's identity, not
// app config, and the next install repopulates it anyway.
var slackAppConfigKeys = []string{
	"FLOW_SLACK_APP_ID", "FLOW_SLACK_CLIENT_ID", "FLOW_SLACK_CLIENT_SECRET",
	"FLOW_SLACK_TOKEN", "FLOW_SLACK_USER_TOKEN", "FLOW_SLACK_APP_TOKEN",
}

// handleSlackSetupReset clears the connected Slack app's credentials so the
// operator can recreate it from scratch — the only way to change the registered
// OAuth redirect URL (e.g. localhost → public ingress), since Slack pins the
// redirect at app-creation time and a plain Reinstall can't switch it.
func (s *Server) handleSlackSetupReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Cancel any in-flight install first.
	s.slackSetupMu.Lock()
	dance := s.slackOAuth
	s.slackOAuth = nil
	s.slackSetupMu.Unlock()
	if dance != nil {
		dance.shutdown()
	}

	cfg := loadConfigFile(s.configPath())
	var changed []string
	for _, k := range slackAppConfigKeys {
		if _, ok := cfg[k]; ok {
			delete(cfg, k)
			changed = append(changed, k)
		}
		// Secrets live in the keyring, not config.json — clear them there too.
		if account, ok := slackSecretAccountForEnv(k); ok {
			if err := storeSlackSecret(account, ""); err != nil {
				writeJSONStatus(w, map[string]any{"ok": false, "error": err.Error()}, http.StatusInternalServerError)
				return
			}
			changed = append(changed, k)
		}
		os.Unsetenv(k)
	}
	if err := saveConfigFile(s.configPath(), cfg); err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": err.Error()}, http.StatusInternalServerError)
		return
	}
	s.applySettingsRestart(changed)
	s.publishUIChange("slack-setup")
	writeJSON(w, map[string]any{"ok": true})
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

// handleSlackSetupOAuthCallback serves GET /api/slack/oauth/callback. It is
// registered on BOTH the main local mux and the public ingress mux: in public
// mode Slack redirects the operator's browser to the zrok URL (real cert, no
// warning) and the delivery lands here via the ingress; in loopback mode the
// ephemeral self-signed TLS listener serves the same handler. It routes to the
// in-flight dance (s.slackOAuth) and reuses that dance's exact redirect_uri for
// the token exchange (Slack requires it to match the authorize request).
// Returns 404 when no install is in progress.
func (s *Server) handleSlackSetupOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.slackSetupMu.Lock()
	dance := s.slackOAuth
	clientID := os.Getenv("FLOW_SLACK_CLIENT_ID")
	clientSecret := os.Getenv("FLOW_SLACK_CLIENT_SECRET")
	s.slackSetupMu.Unlock()
	if dance == nil {
		http.Error(w, "no Slack install in progress", http.StatusNotFound)
		return
	}
	s.handleSlackOAuthCallback(w, r, dance, clientID, clientSecret, dance.redirectURI)
}
