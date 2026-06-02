package server

import (
	"context"
	"flow/internal/flowdb"
	"flow/internal/monitor"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type uiToolCapability struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Path      string `json:"path,omitempty"`
	// Status is a short user-facing badge label ("connected", "not
	// configured", "not authenticated", etc.) — more nuanced than the
	// boolean Available. Populated for integrations; empty for tools and
	// terminals where Available alone is enough.
	Status string `json:"status,omitempty"`
}

type uiCapabilities struct {
	Providers    []uiToolCapability `json:"providers"`
	Terminals    []uiToolCapability `json:"terminals"`
	Integrations []uiToolCapability `json:"integrations"`
}

func detectCapabilities() uiCapabilities {
	return uiCapabilities{
		Providers: []uiToolCapability{
			binaryCapability("claude", "Claude Code", "claude"),
			binaryCapability("codex", "Codex", "codex"),
		},
		Terminals: []uiToolCapability{
			macAppCapability("iterm", "iTerm", []string{
				"/Applications/iTerm.app",
				"/Applications/iTerm2.app",
				filepath.Join(homeDirOrEmpty(), "Applications", "iTerm.app"),
				filepath.Join(homeDirOrEmpty(), "Applications", "iTerm2.app"),
			}, "requires iTerm2.app and osascript"),
			macAppCapability("terminal", "Terminal.app", []string{
				"/System/Applications/Utilities/Terminal.app",
				"/Applications/Utilities/Terminal.app",
			}, "requires Terminal.app and osascript"),
			macAppCapability("warp", "Warp", []string{
				"/Applications/Warp.app",
				filepath.Join(homeDirOrEmpty(), "Applications", "Warp.app"),
			}, "requires Warp.app and osascript"),
			binaryCapability("kitty", "kitty", "kitty"),
			binaryCapability("alacritty", "Alacritty", "alacritty"),
			binaryCapability("ghostty", "Ghostty", "ghostty"),
			binaryCapability("wezterm", "WezTerm", "wezterm"),
			binaryCapability("tmux", "tmux", "tmux"),
			binaryCapability("vscode", "VS Code", "code"),
		},
	}
}

func binaryCapability(id, label, bin string) uiToolCapability {
	path, err := exec.LookPath(bin)
	if err != nil {
		return uiToolCapability{ID: id, Label: label, Available: false, Reason: bin + " not found on PATH"}
	}
	return uiToolCapability{ID: id, Label: label, Available: true, Path: path}
}

func macAppCapability(id, label string, appPaths []string, missingReason string) uiToolCapability {
	if runtime.GOOS != "darwin" {
		return uiToolCapability{ID: id, Label: label, Available: false, Reason: "macOS only"}
	}
	if _, err := exec.LookPath("osascript"); err != nil {
		return uiToolCapability{ID: id, Label: label, Available: false, Reason: "osascript not found on PATH"}
	}
	for _, path := range appPaths {
		if path == "" {
			continue
		}
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			return uiToolCapability{ID: id, Label: label, Available: true, Path: path}
		}
	}
	return uiToolCapability{ID: id, Label: label, Available: false, Reason: missingReason}
}

func homeDirOrEmpty() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func capabilityByID(items []uiToolCapability, id string) uiToolCapability {
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	return uiToolCapability{ID: id, Label: id, Available: false, Reason: "unsupported"}
}

func (s *Server) availableProvider(raw string) (string, error) {
	provider, err := flowdb.NormalizeSessionProvider(raw)
	if err != nil {
		return "", err
	}
	caps := detectCapabilities()
	capability := capabilityByID(caps.Providers, provider)
	if capability.Available {
		return provider, nil
	}
	if raw == "" {
		for _, alt := range caps.Providers {
			if alt.Available {
				return alt.ID, nil
			}
		}
	}
	if capability.Reason == "" {
		capability.Reason = "not available"
	}
	return "", fmt.Errorf("%s is unavailable: %s", capability.Label, capability.Reason)
}

func (s *Server) ensureProviderAvailable(provider string) error {
	capability := capabilityByID(detectCapabilities().Providers, provider)
	if capability.Available {
		return nil
	}
	if capability.Reason == "" {
		capability.Reason = "not available"
	}
	return fmt.Errorf("%s is unavailable: %s", capability.Label, capability.Reason)
}

func (s *Server) ensureTerminalAvailable(kind string) error {
	capability := capabilityByID(detectCapabilities().Terminals, kind)
	if capability.Available {
		return nil
	}
	if capability.Reason == "" {
		capability.Reason = "not available"
	}
	return fmt.Errorf("%s is unavailable: %s", capability.Label, capability.Reason)
}

// uiCapabilities returns the full capability bundle the UI consumes:
// static tool detection (providers + terminals) plus live integration
// statuses (GitHub via gh; Slack via the socketmode listener) that need
// server context. Centralised here so buildUIData stays declarative.
func (s *Server) uiCapabilities() uiCapabilities {
	caps := detectCapabilities()
	caps.Integrations = s.detectIntegrationCapabilities()
	return caps
}

// detectIntegrationCapabilities returns live status for the external
// services flow integrates with. Each entry tells Mission Control
// whether the integration is configured, whether it is currently
// working, and a short user-facing status string for chip rendering.
func (s *Server) detectIntegrationCapabilities() []uiToolCapability {
	return []uiToolCapability{
		detectGitHubIntegration(),
		s.detectSlackIntegration(),
	}
}

func (s *Server) detectSlackIntegration() uiToolCapability {
	c := uiToolCapability{ID: "slack", Label: "Slack", Available: false}
	if !monitor.SocketModeEnabled() {
		c.Status = "not configured"
		c.Reason = "set FLOW_SLACK_APP_TOKEN + SLACK_BOT_TOKEN (and FLOW_SLACK_SOCKET_MODE=1) to enable"
		return c
	}
	if s == nil || s.slackListener == nil {
		c.Status = "configured"
		c.Reason = "tokens detected but listener not initialised (no DB?)"
		return c
	}
	if s.slackListener.Suppressed() {
		c.Status = "inactive"
		c.Reason = "another flow process already owns the Slack Socket Mode connection for this app token; this instance is not listening (stop the other server to take over)"
		return c
	}
	if !s.slackListener.Running() {
		c.Status = "configured"
		c.Reason = "listener not started"
		return c
	}
	if !s.slackListener.Connected() {
		c.Status = "connecting"
		c.Reason = "tokens valid; awaiting Slack socket-mode handshake"
		return c
	}
	c.Available = true
	c.Status = "connected"
	return c
}

var (
	ghAuthMu     sync.Mutex
	ghAuthCached uiToolCapability
	ghAuthExpiry time.Time
)

// ghAuthCacheTTL controls how often `gh auth status` is re-run. The
// shell-out sits on the UI refresh hot path; 15s is short enough that
// the chip flips within one human attention span when the user logs in
// or out, and long enough that a watch session doesn't fork hundreds of
// gh processes per minute.
const ghAuthCacheTTL = 15 * time.Second

func detectGitHubIntegration() uiToolCapability {
	ghAuthMu.Lock()
	if time.Now().Before(ghAuthExpiry) {
		c := ghAuthCached
		ghAuthMu.Unlock()
		return c
	}
	ghAuthMu.Unlock()

	c := uiToolCapability{ID: "gh", Label: "GitHub", Available: false}
	path, err := exec.LookPath("gh")
	if err != nil {
		c.Status = "not installed"
		c.Reason = "gh CLI not found on PATH"
	} else {
		c.Path = path
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := exec.CommandContext(ctx, "gh", "auth", "status").Run(); err != nil {
			c.Status = "not authenticated"
			c.Reason = "run `gh auth login` to connect"
		} else {
			c.Available = true
			c.Status = "connected"
		}
	}

	ghAuthMu.Lock()
	ghAuthCached = c
	ghAuthExpiry = time.Now().Add(ghAuthCacheTTL)
	ghAuthMu.Unlock()
	return c
}
