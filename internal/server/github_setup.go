package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// githubAccount is one identity `gh` is logged in as on a host.
type githubAccount struct {
	Login  string `json:"login"`
	Active bool   `json:"active"`
	Source string `json:"source,omitempty"` // "keyring" | "GH_TOKEN" | "GITHUB_TOKEN" | …
}

// githubAuthView drives the GitHub connector card's identity panel:
// who flow is polling as, every account it could switch to, and whether the
// active identity is pinned by an environment token (which `gh auth switch`
// cannot override).
type githubAuthView struct {
	Installed     bool   `json:"installed"`
	Authenticated bool   `json:"authenticated"`
	Path          string `json:"path,omitempty"`
	Host          string `json:"host,omitempty"`
	ActiveLogin   string `json:"active_login,omitempty"`
	ActiveSource  string `json:"active_source,omitempty"`
	// EnvPinned reports that the active identity comes from a GH_TOKEN /
	// GITHUB_TOKEN env var. While set, it overrides the keyring account, so
	// switching has no effect until it's unset — the UI surfaces this instead
	// of offering a dead switch.
	EnvPinned bool            `json:"env_pinned"`
	Accounts  []githubAccount `json:"accounts"`
	Error     string          `json:"error,omitempty"`
}

// Indirections so tests can stub the `gh` shell-outs.
var (
	ghAuthStatusOutput = func(ctx context.Context) (string, error) {
		out, err := exec.CommandContext(ctx, "gh", "auth", "status").CombinedOutput()
		return string(out), err
	}
	ghAuthSwitch = func(ctx context.Context, host, login string) (string, error) {
		out, err := exec.CommandContext(ctx, "gh", "auth", "switch", "--hostname", host, "--user", login).CombinedOutput()
		return string(out), err
	}
	// ghOrgsOutput lists the orgs the active `gh` identity belongs to, one login
	// per line. --paginate covers users in more than one page of orgs.
	ghOrgsOutput = func(ctx context.Context) (string, error) {
		out, err := exec.CommandContext(ctx, "gh", "api", "--paginate", "user/orgs", "--jq", ".[].login").CombinedOutput()
		return string(out), err
	}
)

// Matches both the modern multi-account line
//
//	✓ Logged in to github.com account vishnukv (keyring)
//
// and the older single-account line
//
//	✓ Logged in to github.com as vishnukv (oauth_token)
var ghLoggedInRe = regexp.MustCompile(`Logged in to (\S+) (account|as) (\S+?)(?:\s+\(([^)]*)\))?$`)

// parseGitHubAuthStatus extracts the logged-in accounts from `gh auth status`
// combined output. A login may appear more than once (one entry per token
// source); the returned list is deduped by login, marking the active one. The
// active account is the entry followed by "Active account: true" (modern gh) or
// the sole "as <login>" entry (older gh).
func parseGitHubAuthStatus(output string) (accounts []githubAccount, activeLogin, activeSource, host string) {
	type entry struct {
		login, source string
		active        bool
	}
	var entries []entry
	curIdx := -1
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if m := ghLoggedInRe.FindStringSubmatch(line); m != nil {
			if host == "" {
				host = m[1]
			}
			// "as <login>" is the legacy single-account form → already active.
			entries = append(entries, entry{login: m[3], source: m[4], active: m[2] == "as"})
			curIdx = len(entries) - 1
			continue
		}
		if curIdx >= 0 && strings.HasPrefix(line, "- Active account:") {
			entries[curIdx].active = strings.Contains(line, "true")
			curIdx = -1
		}
	}

	for _, e := range entries {
		if e.active && activeLogin == "" {
			activeLogin = e.login
			activeSource = e.source
		}
	}

	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.login] {
			continue
		}
		seen[e.login] = true
		src := e.source
		if e.login == activeLogin && activeSource != "" {
			src = activeSource
		}
		accounts = append(accounts, githubAccount{Login: e.login, Active: e.login == activeLogin, Source: src})
	}
	// Single account with no explicit "Active account" marker → it's active.
	if activeLogin == "" && len(accounts) == 1 {
		accounts[0].Active = true
		activeLogin = accounts[0].Login
		activeSource = accounts[0].Source
	}
	return
}

func isEnvTokenSource(source string) bool {
	s := strings.ToUpper(strings.TrimSpace(source))
	return s == "GH_TOKEN" || s == "GITHUB_TOKEN"
}

// detectGitHubAuth resolves the live `gh` identity for the GitHub connector
// card. Read-only: it never mutates gh state.
func detectGitHubAuth() githubAuthView {
	v := githubAuthView{}
	path, err := ghLookPath("gh")
	if err != nil {
		v.Error = "gh CLI not found on PATH"
		return v
	}
	v.Installed = true
	v.Path = path

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, statusErr := ghAuthStatusOutput(ctx)
	accounts, active, source, host := parseGitHubAuthStatus(out)
	v.Accounts = accounts
	v.ActiveLogin = active
	v.ActiveSource = source
	v.Host = host
	v.EnvPinned = isEnvTokenSource(source)
	v.Authenticated = active != ""
	if !v.Authenticated {
		v.Error = "not authenticated — run `gh auth login`"
		_ = statusErr // exit code already reflected by the empty account list
	}
	return v
}

func (s *Server) handleGitHubAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, detectGitHubAuth())
}

// handleGitHubAuthSwitch changes the active `gh` account, then refreshes flow's
// view of GitHub: it invalidates the cached capability chip and bounces the
// GitHub listener so polling continues as the newly-active identity.
func (s *Server) handleGitHubAuthSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Login string `json:"login"`
		Host  string `json:"host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "invalid JSON body"}, http.StatusBadRequest)
		return
	}
	login := strings.TrimSpace(req.Login)
	if login == "" {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "login is required"}, http.StatusBadRequest)
		return
	}

	cur := detectGitHubAuth()
	if !cur.Installed {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "gh CLI not found on PATH"}, http.StatusBadRequest)
		return
	}
	// Only switch to an account gh actually knows about.
	known := false
	for _, a := range cur.Accounts {
		if a.Login == login {
			known = true
			break
		}
	}
	if !known {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "no gh account named " + login}, http.StatusBadRequest)
		return
	}
	if cur.EnvPinned {
		writeJSONStatus(w, map[string]any{"ok": false, "error": "active account is pinned by the " + cur.ActiveSource + " environment variable — unset it to switch accounts here"}, http.StatusConflict)
		return
	}

	host := strings.TrimSpace(req.Host)
	if host == "" {
		host = cur.Host
	}
	if host == "" {
		host = "github.com"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if out, err := ghAuthSwitch(ctx, host, login); err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error()
		}
		writeJSONStatus(w, map[string]any{"ok": false, "error": "gh auth switch: " + msg}, http.StatusBadGateway)
		return
	}

	// The identity changed: drop the cached chip status and bounce the GitHub
	// listener so polling resumes as the new account.
	invalidateGitHubAuthCache()
	if s != nil && s.githubListener != nil {
		s.githubListener.Stop()
		_ = s.githubListener.Start()
	}
	s.publishUIChange("github-auth")

	writeJSON(w, map[string]any{"ok": true, "status": detectGitHubAuth()})
}

// listGitHubOrgs returns the orgs the active `gh` identity belongs to, deduped
// and in gh's order. Read-only. An error (gh missing, not authenticated, API
// failure) is returned to the caller, which surfaces it without blocking the
// wizard's manual-entry fallback.
func listGitHubOrgs(ctx context.Context) ([]string, error) {
	if _, err := ghLookPath("gh"); err != nil {
		return nil, errors.New("gh CLI not found on PATH")
	}
	out, err := ghOrgsOutput(ctx)
	if err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	var orgs []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		login := strings.TrimSpace(line)
		if login == "" || seen[login] {
			continue
		}
		seen[login] = true
		orgs = append(orgs, login)
	}
	return orgs, nil
}

// handleGitHubSetupOrgs powers the org dropdown in the Connect-GitHub wizard:
// the orgs the active gh account can target. It always returns 200 — on
// failure it reports an empty list plus an error string so the wizard falls
// back to a manual org-login input rather than dead-ending.
func (s *Server) handleGitHubSetupOrgs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	orgs, err := listGitHubOrgs(ctx)
	resp := map[string]any{
		"orgs":         orgs,
		"active_login": detectGitHubAuth().ActiveLogin,
	}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, resp)
}
