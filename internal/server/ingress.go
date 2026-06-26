package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/openziti/zrok/environment"
	"github.com/openziti/zrok/environment/env_core"
	zroksdk "github.com/openziti/zrok/sdk/golang/sdk"
)

// ingressProvider identifies the active public-ingress backend.
type ingressProvider string

const (
	ingressProviderNone   ingressProvider = "none"
	ingressProviderZrok   ingressProvider = "zrok"
	ingressProviderManual ingressProvider = "manual"
)

// activeIngressProvider reads FLOW_INGRESS_PROVIDER from the environment.
func activeIngressProvider() ingressProvider {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("FLOW_INGRESS_PROVIDER"))) {
	case "zrok":
		return ingressProviderZrok
	case "manual":
		return ingressProviderManual
	default:
		return ingressProviderNone
	}
}

// publicBaseURL returns the active public base URL, or "" when none is ready.
//
//   - zrok:   the URL is assigned by zrok at runtime and read back from the
//     share's frontend endpoint — it is NOT derivable from config. Returns ""
//     until the share is established (callers then fall back to localhost).
//   - manual: the operator supplies a fixed public URL via FLOW_PUBLIC_BASE_URL
//     (e.g. they run their own reverse proxy / tunnel / domain).
//   - none:   no public ingress.
func (s *Server) publicBaseURL() string {
	switch activeIngressProvider() {
	case ingressProviderZrok:
		if s.zrok != nil {
			return s.zrok.currentBaseURL()
		}
		return ""
	case ingressProviderManual:
		return strings.TrimRight(strings.TrimSpace(os.Getenv("FLOW_PUBLIC_BASE_URL")), "/")
	default:
		return ""
	}
}

// connectorCallbackURL builds a full public callback URL for the given path.
// Returns "" when no public base URL is ready, signalling that the caller
// should fall back to its provider-specific localhost behaviour.
func (s *Server) connectorCallbackURL(path string) string {
	base := s.publicBaseURL()
	if base == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

// ---------------------------------------------------------------------------
// Status view
// ---------------------------------------------------------------------------

// IngressStatusView is the JSON payload returned by GET /api/ingress/status.
type IngressStatusView struct {
	Provider     string `json:"provider"`
	BaseURL      string `json:"base_url,omitempty"`
	Running      bool   `json:"running"`
	EnvEnabled   bool   `json:"env_enabled,omitempty"`
	ShareName    string `json:"share_name,omitempty"`
	ShareRunning bool   `json:"share_running,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	// WebhookSecretSet reports whether a GitHub webhook signing secret is
	// configured (its value is never returned here — see revealWebhookSecret).
	WebhookSecretSet bool `json:"webhook_secret_set"`
	// Derived callback URLs for connectors.
	GithubWebhookURL string `json:"github_webhook_url,omitempty"`
}

func (s *Server) handleIngressStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	prov := activeIngressProvider()
	st := IngressStatusView{Provider: string(prov)}
	st.BaseURL = s.publicBaseURL()
	st.Running = st.BaseURL != ""
	st.WebhookSecretSet = githubWebhookSecret() != ""
	switch prov {
	case ingressProviderZrok:
		st.ShareName = strings.TrimSpace(os.Getenv("FLOW_ZROK_SHARE_NAME"))
		if enabled, _ := zrokEnvEnabled(); enabled {
			st.EnvEnabled = true
		}
		if s.zrok != nil {
			st.ShareRunning = s.zrok.running()
			if e := s.zrok.lastError(); e != nil {
				st.LastError = e.Error()
			}
		}
	case ingressProviderManual:
		// nothing extra; BaseURL already set
	}
	if st.BaseURL != "" {
		st.GithubWebhookURL = st.BaseURL + "/api/github/webhook"
	}
	writeJSON(w, st)
}

// ---------------------------------------------------------------------------
// zrok environment helpers
// ---------------------------------------------------------------------------

// zrokEnvEnabled loads the zrok root and reports whether the environment is
// enabled. Returns (false, err) when the config is absent or unreadable.
// This reads local config/identity files only — no network.
func zrokEnvEnabled() (bool, error) {
	root, err := environment.LoadRoot()
	if err != nil {
		return false, fmt.Errorf("zrok load root: %w", err)
	}
	return root.IsEnabled(), nil
}

// zrokAutoStart reports whether FLOW_ZROK_AUTO_START is set to a truthy value.
func zrokAutoStart() bool {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("FLOW_ZROK_AUTO_START"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// remoteAccessEnabled reports whether the operator turned on the remote-access
// (phone PWA) surface. Persisted in config.json as FLOW_REMOTE_ACCESS and
// repopulated into the env on boot, mirroring zrokAutoStart/githubWebhookSecret.
func remoteAccessEnabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("FLOW_REMOTE_ACCESS")))
	return v == "1" || v == "true" || v == "on"
}

// zrokRetryDelays is the backoff schedule retryTransientZrok waits between
// attempts after a transient zrok.io failure: len(delays)+1 total attempts.
// A package var so tests can shrink it to zero. The cumulative wait (~7s)
// runs in the background ingress goroutine, so it only delays bring-up — it
// never blocks the request path.
var zrokRetryDelays = []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}

// isTransientZrokErr reports whether err looks like a transient zrok.io
// control-plane failure worth retrying — a gateway 5xx (502/503/504) or 429
// from the hosted API (the mandatory clientVersionCheck handshake inside
// root.Client() surfaces its status here), or a transport-level
// timeout/DNS/connection error. Permanent failures (auth, a real version
// rejection, name-taken, environment-not-enabled) return false so callers fail
// fast instead of burning the backoff budget on something a retry can't fix.
func isTransientZrokErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, sig := range []string{
		"(status 502)", "(status 503)", "(status 504)", "(status 429)",
		"connection refused", "connection reset", "no such host",
		"network is unreachable", "deadline exceeded", "i/o timeout",
		"timeout exceeded", "tls handshake timeout",
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// zrokReleaseSettle is how long to wait after releasing an orphaned reserved
// share before re-creating it, so the zrok controller frees the unique-name.
const zrokReleaseSettle = 1500 * time.Millisecond

// isShareConflictErr reports whether a zrok CreateShare error is a reserved
// unique-name conflict — the name is already held by another (typically
// orphaned) share. Happens when a prior tunnel dropped uncleanly (e.g. a
// WiFi→hotspot switch) and zrok never released the reservation, so the next
// reserve of the same pinned name returns HTTP 409 shareConflict.
func isShareConflictErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, sig := range []string{"shareconflict", "(status 409)", "[409]", "already exists"} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// retryTransientZrok runs fn, retrying on transient zrok.io errors per
// zrokRetryDelays with a bounded backoff. A non-transient error or a success
// returns immediately; once the delays are exhausted the last error is
// returned. This lets a flaky hosted control plane (e.g. a 504 on
// clientVersionCheck) self-heal instead of leaving the connector hard-failed.
func retryTransientZrok(fn func() error) error {
	err := fn()
	for _, d := range zrokRetryDelays {
		if !isTransientZrokErr(err) {
			return err
		}
		time.Sleep(d)
		err = fn()
	}
	return err
}

// ---------------------------------------------------------------------------
// zrokManager — SDK share + listener lifecycle
// ---------------------------------------------------------------------------

// zrokManager owns an optional zrok SDK share and listener. When
// FLOW_ZROK_AUTO_START is enabled it creates (or re-discovers) a public share,
// reads its runtime-assigned public URL, and serves handler over the ziti
// overlay. All methods are safe to call concurrently.
type zrokManager struct {
	mu       sync.Mutex
	root     env_core.Root
	share    *zroksdk.Share
	listener net.Listener // edge.Listener implements net.Listener
	baseURL  string       // runtime-discovered public URL (frontend endpoint)
	reserved bool         // true when the share is reserved (don't delete on stop)
	runErr   error
	handler  http.Handler
}

// start establishes a zrok share + listener and serves handler in the
// background. It is a no-op if already running, auto-start is disabled, or the
// active provider is not zrok. The public URL is discovered from zrok at
// runtime; callers read it via currentBaseURL once the share is up.
func (m *zrokManager) start() {
	if activeIngressProvider() != ingressProviderZrok || !zrokAutoStart() {
		return
	}
	if githubWebhookSecret() == "" && !remoteAccessEnabled() {
		m.setErr(errors.New("GitHub webhook secret required before public ingress can start"))
		return
	}

	m.mu.Lock()
	if m.listener != nil {
		m.mu.Unlock()
		return
	}
	handler := m.handler
	m.mu.Unlock()

	// A configured share name pins a stable (reserved) public URL across
	// restarts — required because Slack and GitHub register the callback URL.
	// Empty name → an ephemeral share whose URL changes each boot.
	name := strings.TrimSpace(os.Getenv("FLOW_ZROK_SHARE_NAME"))

	go func() {
		root, err := environment.LoadRoot()
		if err != nil {
			m.setErr(fmt.Errorf("zrok load root: %w", err))
			return
		}
		if !root.IsEnabled() {
			m.setErr(errors.New("zrok environment not enabled — run `zrok enable <token>` first"))
			return
		}

		// Reclaim orphaned flow shares up front, keyed on the configured
		// reserved name. The post-success prune below only runs once the
		// listener is up, so a leftover share (e.g. from a manual rename or an
		// aborted start) would otherwise linger whenever bring-up fails — which
		// is exactly when zrok.io is flaky. Keyed on a non-empty name it's safe:
		// it keeps the configured share and deletes the rest. Skipped for an
		// ephemeral share (no stable keep token until CreateShare returns).
		if name != "" {
			if n, perr := pruneStaleZrokShares(root, name); perr != nil {
				fmt.Fprintf(os.Stderr, "zrok prune (pre-start): %v\n", perr)
			} else if n > 0 {
				fmt.Fprintf(os.Stderr, "zrok: pruned %d stale flow share(s)\n", n)
			}
		}

		shr, baseURL, reserved, err := ensureZrokShare(root, name)
		if err != nil {
			m.setErr(err)
			return
		}

		listener, err := zroksdk.NewListener(shr.Token, root)
		if err != nil {
			// An ephemeral share we just created is now orphaned; clean it up.
			if !reserved {
				_ = zroksdk.DeleteShare(root, shr)
			}
			m.setErr(fmt.Errorf("zrok new listener: %w", err))
			return
		}

		m.mu.Lock()
		m.root = root
		m.share = shr
		m.listener = listener
		m.baseURL = baseURL
		m.reserved = reserved
		m.runErr = nil
		m.mu.Unlock()

		// Backstop prune keyed on the share we actually brought up — covers the
		// ephemeral case (no name, so the pre-start prune above was skipped) and
		// re-runs cheaply for reserved shares (shr.Token == name, nothing left).
		if n, perr := pruneStaleZrokShares(root, shr.Token); perr != nil {
			fmt.Fprintf(os.Stderr, "zrok prune: %v\n", perr)
		} else if n > 0 {
			fmt.Fprintf(os.Stderr, "zrok: pruned %d stale flow share(s)\n", n)
		}

		if err := http.Serve(listener, handler); err != nil && !errors.Is(err, net.ErrClosed) {
			m.setErr(fmt.Errorf("zrok serve: %w", err))
		}

		m.mu.Lock()
		m.listener = nil
		m.baseURL = ""
		m.mu.Unlock()
	}()
}

// releaseReservedShare best-effort deletes a previously-reserved share by its
// unique name so a rotated-away URL stops resolving and doesn't linger against
// the zrok account's reserved-share quota. Network-bound; all errors are
// swallowed because rotation has already switched the active name and the new
// share is what matters from here on.
func (m *zrokManager) releaseReservedShare(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	root, err := environment.LoadRoot()
	if err != nil || !root.IsEnabled() {
		return
	}
	if shr, _, ok := lookupReservedShare(root, name); ok {
		_ = zroksdk.DeleteShare(root, shr)
	}
}

// stop closes the listener (ending http.Serve) and deletes the share if it was
// ephemeral. Reserved shares are intentionally left in place so their URL
// survives the next start.
func (m *zrokManager) stop() {
	m.mu.Lock()
	listener := m.listener
	root := m.root
	shr := m.share
	reserved := m.reserved
	m.listener = nil
	m.baseURL = ""
	m.share = nil
	m.mu.Unlock()

	if listener != nil {
		listener.Close()
	}
	if shr != nil && root != nil && !reserved {
		_ = zroksdk.DeleteShare(root, shr)
	}
}

// running reports whether the listener is currently active.
func (m *zrokManager) running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listener != nil
}

// currentBaseURL returns the runtime-discovered public URL, or "" when no
// share is established.
func (m *zrokManager) currentBaseURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.baseURL
}

// lastError returns the most recent start/serve failure, or nil.
func (m *zrokManager) lastError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runErr
}

func (m *zrokManager) setErr(err error) {
	m.mu.Lock()
	m.runErr = err
	m.mu.Unlock()
}

// flowZrokTarget is the proxy-backend target flow stamps on every share it
// creates. It doubles as the marker pruneStaleZrokShares matches on, so we only
// ever delete shares flow itself made.
const flowZrokTarget = "flow"

// flowSharesToPrune returns the share tokens in a zrok account Overview that
// flow created (backend target == flowZrokTarget) but are not keepToken — the
// leaked shares from earlier runs to delete. Pure (no network) so it's unit
// tested; pruneStaleZrokShares wires it to Overview + DeleteShare. Pass an empty
// keepToken to select every flow share.
func flowSharesToPrune(overviewJSON, keepToken string) []string {
	var ov struct {
		Environments []struct {
			Shares []struct {
				ShareToken           string `json:"shareToken"`
				BackendProxyEndpoint string `json:"backendProxyEndpoint"`
			} `json:"shares"`
		} `json:"environments"`
	}
	if err := json.Unmarshal([]byte(overviewJSON), &ov); err != nil {
		return nil
	}
	var out []string
	for _, env := range ov.Environments {
		for _, sh := range env.Shares {
			if sh.ShareToken == "" || sh.ShareToken == keepToken {
				continue
			}
			if sh.BackendProxyEndpoint == flowZrokTarget {
				out = append(out, sh.ShareToken)
			}
		}
	}
	return out
}

// pruneStaleZrokShares deletes flow-created public shares that aren't the one
// currently in use (keepToken), reclaiming the zrok account's share quota. flow
// brings up a fresh share each (re)start; an ungraceful exit leaks the old one,
// so without pruning they pile up (the stale "flow" shares on the dashboard).
// Best-effort: a delete error on one share doesn't stop the rest. Returns how
// many were pruned.
//
// Caveat: if two real flow servers share one zrok account, each will prune the
// other's share. That's already a broken setup (one zrok env per server); the
// throwaway/smoke servers don't enable zrok auto-start, so they never prune.
func pruneStaleZrokShares(root env_core.Root, keepToken string) (int, error) {
	var raw string
	if err := retryTransientZrok(func() error {
		var e error
		raw, e = zroksdk.Overview(root)
		return e
	}); err != nil {
		return 0, fmt.Errorf("zrok overview: %w", err)
	}
	pruned := 0
	for _, tok := range flowSharesToPrune(raw, keepToken) {
		if err := zroksdk.DeleteShare(root, &zroksdk.Share{Token: tok}); err == nil {
			pruned++
		}
	}
	return pruned, nil
}

// ensureZrokShare returns a usable public share and its runtime public URL.
// When name is set it first looks for an existing reserved share of that name
// (so restarts reuse the same stable URL); otherwise it creates a new share
// (reserved when named, ephemeral when not). The returned bool reports whether
// the share is reserved.
func ensureZrokShare(root env_core.Root, name string) (*zroksdk.Share, string, bool, error) {
	if name != "" {
		if shr, url, ok := lookupReservedShare(root, name); ok {
			return shr, url, true, nil
		}
	}
	req := &zroksdk.ShareRequest{
		ShareMode:   zroksdk.PublicShareMode,
		BackendMode: zroksdk.ProxyBackendMode,
		Frontends:   []string{"public"},
		Target:      flowZrokTarget,
		Reserved:    name != "",
		UniqueName:  name,
	}
	createShare := func() (*zroksdk.Share, error) {
		var s *zroksdk.Share
		err := retryTransientZrok(func() error {
			var e error
			s, e = zroksdk.CreateShare(root, req)
			return e
		})
		return s, err
	}
	shr, err := createShare()
	if err != nil && name != "" && isShareConflictErr(err) {
		// The pinned unique-name is held by an orphaned reserved share — e.g. the
		// previous tunnel dropped uncleanly on a network change (WiFi→hotspot) and
		// zrok never released the reservation. Release the stale reservation by its
		// token (== the unique-name for reserved shares) and retry once. Re-reserving
		// the same name restores the SAME stable public URL, so ingress self-heals
		// instead of staying wedged on 409 shareConflict until a manual rotate.
		_ = zroksdk.DeleteShare(root, &zroksdk.Share{Token: name})
		time.Sleep(zrokReleaseSettle)
		shr, err = createShare()
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("zrok create share: %w", err)
	}
	if len(shr.FrontendEndpoints) == 0 {
		if name == "" {
			_ = zroksdk.DeleteShare(root, shr)
		}
		return nil, "", false, errors.New("zrok share has no frontend endpoint")
	}
	return shr, shr.FrontendEndpoints[0], name != "", nil
}

// lookupReservedShare scans the zrok account overview for a reserved share
// whose token matches name and returns it with its frontend endpoint URL.
func lookupReservedShare(root env_core.Root, name string) (*zroksdk.Share, string, bool) {
	var raw string
	if err := retryTransientZrok(func() error {
		var e error
		raw, e = zroksdk.Overview(root)
		return e
	}); err != nil {
		return nil, "", false
	}
	var ov struct {
		Environments []struct {
			Shares []struct {
				ShareToken       string `json:"shareToken"`
				FrontendEndpoint string `json:"frontendEndpoint"`
				Reserved         bool   `json:"reserved"`
			} `json:"shares"`
		} `json:"environments"`
	}
	if err := json.Unmarshal([]byte(raw), &ov); err != nil {
		return nil, "", false
	}
	for _, env := range ov.Environments {
		for _, sh := range env.Shares {
			if sh.ShareToken == name && sh.FrontendEndpoint != "" {
				return &zroksdk.Share{
					Token:             sh.ShareToken,
					FrontendEndpoints: []string{sh.FrontendEndpoint},
				}, sh.FrontendEndpoint, true
			}
		}
	}
	return nil, "", false
}

// ingressMux returns the restricted handler served over the public zrok URL.
// Only connector callback endpoints are exposed — never the Mission Control UI
// or its data-plane API, which have no authentication and must stay local.
func (s *Server) ingressMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/github/webhook", s.handleGitHubWebhook)
	mux.HandleFunc("/api/clickup/webhook", s.handleClickUpWebhook)
	// The Connect-GitHub App-manifest flow registers a public redirect_url; the
	// operator's browser lands here after GitHub creates the App. State-nonce
	// validated; no UI or data-plane is exposed.
	mux.HandleFunc(githubSetupCallbackPath, s.handleGitHubSetupCallback)
	// Slack OAuth callback in public mode: the operator's browser lands here on
	// the zrok URL (real cert — no localhost warning) after approving the install.
	// State-nonce validated against the in-flight dance; routes to the same
	// handler as the loopback path. No UI or data-plane is exposed.
	mux.HandleFunc(slackOAuthCallbackPath, s.handleSlackSetupOAuthCallback)
	mux.HandleFunc(clickUpOAuthCallbackPath, s.handleClickUpSetupOAuthCallback)
	return mux
}
