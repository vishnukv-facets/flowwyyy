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
	if githubWebhookSecret() == "" {
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

		// Reclaim leaked shares from earlier (re)starts — keep only the one we
		// just brought up. Runs on every start, so reset/recreate cleans up too.
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
	raw, err := zroksdk.Overview(root)
	if err != nil {
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
	shr, err := zroksdk.CreateShare(root, req)
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
	raw, err := zroksdk.Overview(root)
	if err != nil {
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
	// The Connect-GitHub App-manifest flow registers a public redirect_url; the
	// operator's browser lands here after GitHub creates the App. State-nonce
	// validated; no UI or data-plane is exposed.
	mux.HandleFunc(githubSetupCallbackPath, s.handleGitHubSetupCallback)
	return mux
}
