package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type settingType string

const (
	settingString settingType = "string"
	settingSecret settingType = "secret"
	settingBool   settingType = "bool"
	settingInt    settingType = "int"
	settingEnum   settingType = "enum"
)

// settingSpec describes one user-configurable setting. The registry is the
// single source of truth: it drives both the /api/settings schema and the
// Settings UI form, so adding a setting is one entry here.
type settingSpec struct {
	Key     string
	Label   string
	Group   string
	Type    settingType
	Default string
	Options []string
	Help    string
	// Hidden keeps a setting out of the /api/settings schema (and thus the
	// Settings form) while still persisting it in config.json and exporting
	// it to the env at boot. Used for wizard-managed credentials the
	// operator shouldn't hand-edit (e.g. the Slack app's client secret).
	Hidden bool
}

// settingsRegistry intentionally excludes per-session / runtime / bootstrap env
// vars (FLOW_TASK, FLOW_ROOT, HOME, CLAUDE_CODE_SESSION_ID, …) — only durable,
// operator-tunable configuration belongs here.
var settingsRegistry = []settingSpec{
	// Slack
	{Key: "FLOW_SLACK_APP_TOKEN", Label: "App-level token", Group: "Slack", Type: settingSecret, Help: "xapp- token required for Socket Mode."},
	{Key: "FLOW_SLACK_TOKEN", Label: "Bot / read token", Group: "Slack", Type: settingSecret, Help: "xoxb-/xoxp- token for Slack Web API reads."},
	{Key: "FLOW_SLACK_USER_TOKEN", Label: "User token", Group: "Slack", Type: settingSecret, Help: "xoxp- token used to post as you."},
	{Key: "FLOW_SLACK_WRITE_TOKEN", Label: "Write token", Group: "Slack", Type: settingSecret, Help: "Optional separate token for posting on your behalf."},
	{Key: "FLOW_SLACK_SOCKET_MODE", Label: "Socket Mode", Group: "Slack", Type: settingBool, Default: "true", Help: "Connect the Slack Socket Mode listener when tokens are present."},
	{Key: "FLOW_SLACK_SELF_USER_IDS", Label: "Your Slack user IDs", Group: "Slack", Type: settingString, Help: "Comma-separated. Reactions from these IDs trigger sessions, and their messages are treated as operator coordination."},
	{Key: "FLOW_SLACK_TRIGGER_EMOJI", Label: "Trigger emoji", Group: "Slack", Type: settingString, Default: "claude", Help: "Reaction shortname(s) that spawn a session. Comma-separated for multi-provider, e.g. claude,codex."},
	{Key: "FLOW_SLACK_OPEN_TARGET", Label: "Open target", Group: "Slack", Type: settingEnum, Options: []string{"ui", "iterm"}, Default: "ui", Help: "Where new Slack-reply tasks open."},
	{Key: "FLOW_SLACK_AUTOOPEN", Label: "Auto-open on trigger", Group: "Slack", Type: settingBool, Default: "true", Help: "Open a session automatically when a Slack thread is triggered."},
	{Key: "FLOW_SLACK_WRITES_ENABLED", Label: "Allow posting to Slack", Group: "Slack", Type: settingBool, Default: "false", Help: "Off by default. Gate for posting messages back to Slack."},
	// Wizard-managed Slack app identity (Connect Slack flow). Hidden from the
	// Settings form: the setup wizard writes these after apps.manifest.create
	// and the OAuth callback reads them; hand-editing only breaks the pairing.
	{Key: "FLOW_SLACK_APP_ID", Label: "Slack app ID", Group: "Slack", Type: settingString, Hidden: true},
	{Key: "FLOW_SLACK_CLIENT_ID", Label: "Slack client ID", Group: "Slack", Type: settingString, Hidden: true},
	{Key: "FLOW_SLACK_CLIENT_SECRET", Label: "Slack client secret", Group: "Slack", Type: settingSecret, Hidden: true},
	// GitHub
	{Key: "FLOW_GH_ENABLED", Label: "GitHub polling", Group: "GitHub", Type: settingBool, Default: "false", Help: "Poll GitHub for assigned issues/PRs and route them to task inboxes."},
	{Key: "FLOW_GH_SELF_LOGINS", Label: "Your GitHub logins", Group: "GitHub", Type: settingString, Help: "Comma-separated. Used to detect self-authored items and assignments."},
	{Key: "FLOW_GH_REPOS", Label: "Repo allowlist", Group: "GitHub", Type: settingString, Help: "owner/repo,owner/repo2 — leave empty to watch all repos visible to gh."},
	{Key: "FLOW_GH_POLL_INTERVAL", Label: "Poll interval", Group: "GitHub", Type: settingString, Help: "Go duration, e.g. 60s or 2m."},
	{Key: "FLOW_GH_AUTOOPEN", Label: "Auto-open on event", Group: "GitHub", Type: settingBool, Default: "true", Help: "Open a session automatically when a new GitHub item is detected."},
	// General
	{Key: "FLOW_STALE_DAYS", Label: "Stale threshold (days)", Group: "General", Type: settingInt, Default: "3", Help: "In-progress sessions quiet longer than this are flagged stale."},
	{Key: "FLOW_MISSION_QUOTE", Label: "Mission Control quote", Group: "General", Type: settingBool, Default: "true", Help: "Show the rotating anime quote beside the greeting on Mission Control."},
}

// missionQuoteEnabled reports whether the Mission Control anime quote should be
// served. Default on; toggled off via the FLOW_MISSION_QUOTE setting (read at
// request time so the Settings toggle applies live).
func missionQuoteEnabled() bool {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("FLOW_MISSION_QUOTE"))) {
	case "0", "false", "no", "n", "off":
		return false
	default:
		return true
	}
}

func settingSpecFor(key string) (settingSpec, bool) {
	for _, sp := range settingsRegistry {
		if sp.Key == key {
			return sp, true
		}
	}
	return settingSpec{}, false
}

func (s *Server) configPath() string {
	root := strings.TrimSpace(s.cfg.FlowRoot)
	if root == "" {
		return ""
	}
	return filepath.Join(root, "config.json")
}

func loadConfigFile(path string) map[string]string {
	out := map[string]string{}
	if path == "" {
		return out
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &out)
	}
	return out
}

func saveConfigFile(path string, cfg map[string]string) error {
	if path == "" {
		return fmt.Errorf("cannot save settings: FLOW_ROOT is unset")
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil { // 0600: may hold secrets
		return err
	}
	return os.Rename(tmp, path)
}

// applyConfigToEnv exports persisted settings so the rest of the code (which
// reads os.Getenv at call time) honors UI-managed values. Config is
// authoritative for keys it contains; absent keys fall back to the inherited
// shell env. Called at boot before the listeners start.
func (s *Server) applyConfigToEnv() {
	for key, val := range loadConfigFile(s.configPath()) {
		if strings.TrimSpace(val) == "" {
			continue
		}
		if _, ok := settingSpecFor(key); ok {
			os.Setenv(key, val)
		}
	}
}

type uiSettingField struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Group   string   `json:"group"`
	Type    string   `json:"type"`
	Default string   `json:"default,omitempty"`
	Options []string `json:"options,omitempty"`
	Help    string   `json:"help,omitempty"`
	Value   string   `json:"value"`  // current value; ALWAYS "" for secrets
	Set     bool     `json:"set"`    // is an explicit (non-default) value present?
	Source  string   `json:"source"` // "config" | "env" | "default"
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	cfg := loadConfigFile(s.configPath())
	fields := make([]uiSettingField, 0, len(settingsRegistry))
	for _, sp := range settingsRegistry {
		if sp.Hidden {
			continue
		}
		raw := strings.TrimSpace(os.Getenv(sp.Key))
		source := "default"
		if v, ok := cfg[sp.Key]; ok && strings.TrimSpace(v) != "" {
			source = "config"
		} else if raw != "" {
			source = "env"
		}
		f := uiSettingField{
			Key: sp.Key, Label: sp.Label, Group: sp.Group, Type: string(sp.Type),
			Default: sp.Default, Options: sp.Options, Help: sp.Help, Source: source,
			Set: raw != "",
		}
		if sp.Type != settingSecret {
			if raw != "" {
				f.Value = raw
			} else {
				f.Value = sp.Default
			}
		}
		fields = append(fields, f)
	}
	writeJSON(w, map[string]any{"fields": fields})
}

// updateSettings persists + applies submitted settings. Empty secret values are
// treated as "leave unchanged" so the UI never has to re-send a secret it can't
// read back. Changes are os.Setenv'd immediately, and the Slack/GitHub
// listeners are restarted when their keys change so it applies live.
func (s *Server) updateSettings(req actionRequest) (actionResponse, int) {
	if len(req.Settings) == 0 {
		return actionResponse{OK: false, Message: "no settings provided"}, http.StatusBadRequest
	}
	cfg := loadConfigFile(s.configPath())
	var changed []string
	for key, val := range req.Settings {
		sp, ok := settingSpecFor(key)
		if !ok {
			return actionResponse{OK: false, Message: "unknown setting: " + key}, http.StatusBadRequest
		}
		val = strings.TrimSpace(val)
		if sp.Type == settingSecret && val == "" {
			continue // blank secret => keep the stored value
		}
		if err := validateSettingValue(sp, val); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		if cfg[key] == val {
			continue
		}
		cfg[key] = val
		os.Setenv(key, val)
		changed = append(changed, key)
	}
	if len(changed) == 0 {
		return actionResponse{OK: true, Message: "no changes"}, http.StatusOK
	}
	if err := saveConfigFile(s.configPath(), cfg); err != nil {
		return actionResponse{OK: false, Message: "save settings: " + err.Error()}, http.StatusInternalServerError
	}
	s.applySettingsRestart(changed)
	s.publishUIChange("settings")
	return actionResponse{OK: true, Message: "settings applied"}, http.StatusOK
}

func validateSettingValue(sp settingSpec, val string) error {
	switch sp.Type {
	case settingBool:
		switch strings.ToLower(val) {
		case "1", "true", "yes", "y", "on", "0", "false", "no", "n", "off":
		default:
			return fmt.Errorf("%s must be true or false", sp.Label)
		}
	case settingInt:
		if _, err := strconv.Atoi(val); err != nil {
			return fmt.Errorf("%s must be a whole number", sp.Label)
		}
	case settingEnum:
		for _, o := range sp.Options {
			if o == val {
				return nil
			}
		}
		return fmt.Errorf("%s must be one of: %s", sp.Label, strings.Join(sp.Options, ", "))
	}
	return nil
}

// applySettingsRestart bounces the listener whose configuration changed so new
// tokens / Socket-Mode / enabled flags take effect without a server restart.
// Stop()/Start() are safe and no-op when the new config disables the listener.
func (s *Server) applySettingsRestart(changed []string) {
	slackTouched, ghTouched := false, false
	for _, k := range changed {
		if strings.HasPrefix(k, "FLOW_SLACK_") || strings.HasPrefix(k, "SLACK_") {
			slackTouched = true
		}
		if strings.HasPrefix(k, "FLOW_GH_") {
			ghTouched = true
		}
	}
	if slackTouched && s.slackListener != nil {
		s.slackListener.Stop()
		_ = s.slackListener.Start()
	}
	if ghTouched && s.githubListener != nil {
		s.githubListener.Stop()
		_ = s.githubListener.Start()
	}
}
