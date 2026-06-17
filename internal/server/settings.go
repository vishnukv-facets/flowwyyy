package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"flow/internal/monitor"
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
	// Category and Connector classify a setting as belonging to an external
	// connector so the Mission Control → Connectors page can group it by
	// category (messaging, git, network, …) and provider (slack, github,
	// ingress, …). Both empty means the setting is generic and stays on the
	// Settings page. Group is retained for backward-compatible flat grouping;
	// the Connectors UI prefers Category + Connector. Adding a new provider is
	// a metadata change here, not a UI patch.
	Category  string
	Connector string
	// Hidden keeps a setting out of the /api/settings schema (and thus the
	// Settings form) while still persisting it in config.json and exporting
	// it to the env at boot. Used for wizard-managed credentials the
	// operator shouldn't hand-edit (e.g. the Slack app's client secret).
	Hidden bool
}

// Connector taxonomy. Categories group connectors by purpose; connector ids are
// the concrete providers. Kept as constants so the registry tags and any
// future provider stay spelled consistently.
const (
	categoryMessaging = "messaging"
	categoryGit       = "git"
	categoryNetwork   = "network"

	connectorSlack   = "slack"
	connectorGitHub  = "github"
	connectorIngress = "ingress"
)

// settingsRegistry intentionally excludes per-session / runtime / bootstrap env
// vars (FLOW_TASK, FLOW_ROOT, HOME, CLAUDE_CODE_SESSION_ID, …) — only durable,
// operator-tunable configuration belongs here.
var settingsRegistry = []settingSpec{
	// Slack — messaging connector
	{Key: "FLOW_SLACK_APP_TOKEN", Label: "App-level token", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingSecret, Help: "xapp- token required for Socket Mode."},
	{Key: "FLOW_SLACK_TOKEN", Label: "Bot / read token", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingSecret, Help: "xoxb-/xoxp- token for Slack Web API reads."},
	{Key: "FLOW_SLACK_USER_TOKEN", Label: "User token", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingSecret, Help: "xoxp- token used to post as you."},
	{Key: "FLOW_SLACK_WRITE_TOKEN", Label: "Write token", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingSecret, Help: "Optional separate token for posting on your behalf."},
	{Key: "FLOW_SLACK_SOCKET_MODE", Label: "Socket Mode", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingBool, Default: "true", Help: "Connect the Slack Socket Mode listener when tokens are present."},
	{Key: "FLOW_SLACK_SELF_USER_IDS", Label: "Your Slack user IDs", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingString, Help: "Comma-separated. Reactions from these IDs trigger sessions, and their messages are treated as operator coordination."},
	{Key: "FLOW_SLACK_TRIGGER_EMOJI", Label: "Trigger emoji", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingString, Default: "claude", Help: "Reaction shortname(s) that spawn a session. Comma-separated for multi-provider, e.g. claude,codex."},
	{Key: "FLOW_SLACK_OPEN_TARGET", Label: "Open target", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingEnum, Options: []string{"ui", "iterm"}, Default: "ui", Help: "Where new Slack-reply tasks open."},
	{Key: "FLOW_SLACK_AUTOOPEN", Label: "Auto-open on trigger", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingBool, Default: "true", Help: "Open a session automatically when a Slack thread is triggered."},
	{Key: "FLOW_SLACK_WRITES_ENABLED", Label: "Allow posting to Slack", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingBool, Default: "false", Help: "Off by default. Gate for posting messages back to Slack — required for the bot to reply to your DMs."},
	{Key: "FLOW_SLACK_SEND_AS", Label: "Post replies as", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingEnum, Options: []string{"bot", "user"}, Default: "bot", Help: "Identity for messages flow posts into channels/threads: the flow bot (recommended — clearly flow), or you (your user token, needs the User token set above). Your DM with the flow bot always replies as the bot regardless of this."},
	{Key: "FLOW_SLACK_COMMAND_ENABLED", Label: "DM command channel", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingBool, Default: "false", Help: "Off by default. Lets you DM the flow bot to run work on this machine while you're away (remote control). Only your own Slack user IDs are accepted; everyone else gets a polite decline. Needs 'Allow posting to Slack' for replies, and your IDs set above."},
	{Key: "FLOW_SLACK_COMMAND_PROVIDER", Label: "DM chat agent", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingEnum, Options: []string{"claude", "codex"}, Default: "claude", Help: "Which agent powers a chat started from a Slack DM."},
	// Wizard-managed Slack app identity (Connect Slack flow). Hidden from the
	// Settings form: the setup wizard writes these after apps.manifest.create
	// and the OAuth callback reads them; hand-editing only breaks the pairing.
	{Key: "FLOW_SLACK_APP_ID", Label: "Slack app ID", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingString, Hidden: true},
	{Key: "FLOW_SLACK_CLIENT_ID", Label: "Slack client ID", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingString, Hidden: true},
	{Key: "FLOW_SLACK_CLIENT_SECRET", Label: "Slack client secret", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingSecret, Hidden: true},
	{Key: "FLOW_SLACK_MANIFEST_REV", Label: "Slack manifest revision", Group: "Slack", Category: categoryMessaging, Connector: connectorSlack, Type: settingString, Hidden: true},
	// GitHub — git connector
	{Key: "FLOW_GH_TRANSPORT", Label: "Event transport", Group: "GitHub", Category: categoryGit, Connector: connectorGitHub, Type: settingEnum, Options: []string{"auto", "webhook", "polling", "hybrid", "off"}, Default: "auto", Help: "How GitHub events reach Flow. webhook = signed webhook deliveries, live, no API polling (needs a webhook secret + public ingress); polling = legacy gh-api search polling; hybrid = webhook plus the legacy search-poller (to also catch mentions/involvement in repos without a webhook installed); off = no ingress; auto = derive from the legacy 'GitHub polling' toggle below."},
	{Key: "FLOW_GH_ENABLED", Label: "GitHub polling (legacy)", Group: "GitHub", Category: categoryGit, Connector: connectorGitHub, Type: settingBool, Default: "false", Help: "Legacy on/off for gh-api polling. Used only when transport is 'auto' or 'polling'/'hybrid'. Prefer setting the transport above."},
	{Key: "FLOW_GH_SELF_LOGINS", Label: "Your GitHub logins", Group: "GitHub", Category: categoryGit, Connector: connectorGitHub, Type: settingString, Help: "Comma-separated. Used to detect self-authored items and assignments."},
	{Key: "FLOW_GH_REPOS", Label: "Repo allowlist", Group: "GitHub", Category: categoryGit, Connector: connectorGitHub, Type: settingString, Help: "owner/repo,owner/repo2 — leave empty to watch all repos visible to gh."},
	{Key: "FLOW_GH_POLL_INTERVAL", Label: "Poll interval", Group: "GitHub", Category: categoryGit, Connector: connectorGitHub, Type: settingString, Help: "Go duration, e.g. 60s or 2m."},
	{Key: "FLOW_GH_AUTOOPEN", Label: "Auto-open on event", Group: "GitHub", Category: categoryGit, Connector: connectorGitHub, Type: settingBool, Default: "true", Help: "Open a session automatically when a new GitHub item is detected."},
	{Key: "FLOW_GH_WEBHOOK_SECRET", Label: "Webhook signing secret", Group: "GitHub", Category: categoryGit, Connector: connectorGitHub, Type: settingSecret, Help: "Required before public ingress starts. GitHub webhook deliveries must carry a matching X-Hub-Signature-256 HMAC."},
	// Wizard-managed GitHub App identity (Connect GitHub flow). Hidden from the
	// Settings form: the wizard writes these after the App-manifest conversion
	// and the install callback; hand-editing only breaks the pairing. The App's
	// private key (PEM), OAuth client secret, and webhook secret are kept out of
	// config.json entirely — they live in the OS keyring (see secret_store.go).
	{Key: "FLOW_GH_APP_ID", Label: "GitHub App ID", Group: "GitHub", Category: categoryGit, Connector: connectorGitHub, Type: settingString, Hidden: true},
	{Key: "FLOW_GH_APP_SLUG", Label: "GitHub App slug", Group: "GitHub", Category: categoryGit, Connector: connectorGitHub, Type: settingString, Hidden: true},
	{Key: "FLOW_GH_CLIENT_ID", Label: "GitHub client ID", Group: "GitHub", Category: categoryGit, Connector: connectorGitHub, Type: settingString, Hidden: true},
	{Key: "FLOW_GH_HTML_URL", Label: "GitHub App URL", Group: "GitHub", Category: categoryGit, Connector: connectorGitHub, Type: settingString, Hidden: true},
	{Key: "FLOW_GH_INSTALLATION_IDS", Label: "GitHub installation IDs", Group: "GitHub", Category: categoryGit, Connector: connectorGitHub, Type: settingString, Hidden: true},
	// Steering (attention router)
	{Key: "FLOW_STEERING_WATCH_CHANNELS", Label: "Watched channels", Group: "Steering", Type: settingString, Help: "Comma-separated Slack channel IDs the attention router watches (in addition to DMs + @mentions)."},
	{Key: "FLOW_STEERING_MUTED_CHANNELS", Label: "Muted channels", Group: "Steering", Type: settingString, Help: "Comma-separated Slack channel IDs to never surface."},
	{Key: "FLOW_STEERING_MUTED_KEYWORDS", Label: "Muted keywords", Group: "Steering", Type: settingString, Help: "Comma-separated keywords; messages containing them are dropped before triage."},
	// Per-action autonomy policy as JSON ({"make_task":{"enabled":true,"threshold":0.8},...}).
	// Exposed through /api/settings so the dedicated Settings → Steering autonomy
	// panel can reload saved values, but filtered out of the generic form.
	{Key: "FLOW_STEERING_AUTONOMY", Label: "Autonomy policy", Group: "Steering", Type: settingString, Help: "Per-action autonomy (JSON). Lets the steerer act without asking above a confidence threshold."},
	{Key: "FLOW_STEERING_AUTO_RESOLVE_WAITING", Label: "Auto-resolve waiting_on", Group: "Steering", Type: settingBool, Default: "true", Help: "When a reply arrives on a task you're waiting on, automatically clear its waiting_on note."},
	{Key: "FLOW_STEERING_SEND_MODEL", Label: "Reply send model (fallback)", Group: "Steering", Type: settingEnum, Options: []string{"claude-haiku-4-5", "claude-sonnet-4-6", "claude-opus-4-8"}, Default: "claude-sonnet-4-6", Help: "FALLBACK only. With the session model on, your approved reply is posted by the channel's own per-channel chat (it has the thread context + Slack MCP). This model is used only for the ephemeral send session when NO chat exists for the channel / the session model is off. Sonnet is reliable + cheap; Haiku often fumbles the Slack tool call; Opus is overkill. Applies to the next send (no restart)."},
	{Key: "FLOW_STEERING_CLASSIFIER_BUDGET_PER_HOUR", Label: "Classifier budget / hour", Group: "Steering", Type: settingInt, Default: "30", Help: "Maximum Stage 1/2 classifier subprocess turns per rolling hour. Lower this if Mission Control heats the laptop under Slack/GitHub noise."},
	{Key: "FLOW_STEERING_CLASSIFIER_FAILURE_COOLDOWN", Label: "Classifier failure cooldown", Group: "Steering", Type: settingString, Default: "30m", Help: "Go duration for pausing classifier subprocesses after quota/auth failures, e.g. 30m or 1h."},
	// Per-channel session model (Phase 4 — enable for Slack, validate coinswitch cases).
	// Default off; enable once Stage-0/1/2 are validated. Routing takes effect
	// immediately on the next Slack event; idle-sweep and compact workers require a
	// server restart to start (they are gated at Serve() time).
	{Key: "FLOW_STEERING_SESSIONS", Label: "Per-channel session model", Group: "Steering", Type: settingBool, Default: "false", Help: "Route Stage-0 Slack survivors to a persistent per-channel Claude session (conversation memory, coinswitch grouping) instead of a stateless claude -p call per event. Routing takes effect immediately; idle-sweep and compact workers start on the next server restart."},
	{Key: "FLOW_STEERER_DEFAULT_PROVIDER", Label: "Steerer default provider", Group: "Steering", Type: settingEnum, Options: []string{"claude", "codex"}, Default: "claude", Help: "Which agent a NEW per-channel steerer session launches with. Applies at chat creation; once a chat exists (or auto-forks/is switched manually) its own provider is authoritative until changed again."},
	// Ingress — public URL for GitHub webhook callbacks only. Slack OAuth uses
	// a short-lived localhost callback listener during install/reinstall.
	{Key: "FLOW_INGRESS_PROVIDER", Label: "GitHub ingress provider", Group: "Ingress", Category: categoryNetwork, Connector: connectorIngress, Type: settingEnum, Options: []string{"none", "zrok", "manual"}, Default: "none", Help: "Public URL provider for signed GitHub webhook callbacks only. Slack OAuth stays local and does not use standing ingress."},
	{Key: "FLOW_PUBLIC_BASE_URL", Label: "Public base URL (manual only)", Group: "Ingress", Category: categoryNetwork, Connector: connectorIngress, Type: settingString, Help: "Only for the 'manual' GitHub webhook provider: your own public HTTPS base URL, e.g. https://flow.example.com (own reverse proxy/tunnel). Ignored for zrok, which discovers its URL at runtime."},
	{Key: "FLOW_ZROK_SHARE_NAME", Label: "zrok reserved share name", Group: "Ingress", Category: categoryNetwork, Connector: connectorIngress, Type: settingString, Help: "Optional reserved share unique-name. Set it to pin a stable GitHub webhook URL across restarts. Leave empty for an ephemeral share whose URL changes each restart."},
	{Key: "FLOW_ZROK_AUTO_START", Label: "Auto-start zrok share", Group: "Ingress", Category: categoryNetwork, Connector: connectorIngress, Type: settingBool, Default: "false", Help: "Create the zrok public share for signed GitHub webhooks automatically when Flow starts. Requires zrok enablement and FLOW_GH_WEBHOOK_SECRET."},
	// General
	{Key: "FLOW_STALE_DAYS", Label: "Stale threshold (days)", Group: "General", Type: settingInt, Default: "3", Help: "In-progress sessions quiet longer than this are flagged stale."},
	{Key: "FLOW_MISSION_QUOTE", Label: "Mission Control quote", Group: "General", Type: settingBool, Default: "true", Help: "Show the rotating anime quote beside the greeting on Mission Control."},
	{Key: "FLOW_KB_DISTILL_ENABLED", Label: "Auto KB capture from sessions", Group: "General", Type: settingBool, Default: "true", Help: "Periodically capture durable knowledge from live tasks/chats into your KB while they run. Only fires when a session has gone idle (never interrupts a working agent), and only on new activity. Disable to capture KB only at task close-out."},
	{Key: "FLOW_KB_DISTILL_IDLE", Label: "Auto KB capture — idle wait", Group: "General", Type: settingString, Default: "8m", Help: "Go duration (e.g. 8m). A session is only swept for KB once its transcript has been quiet this long — so a working agent is never interrupted."},
	{Key: "FLOW_KB_DISTILL_COOLDOWN", Label: "Auto KB capture — cooldown", Group: "General", Type: settingString, Default: "30m", Help: "Go duration (e.g. 30m). Minimum time between KB captures for the same session."},
	{Key: "FLOW_KB_DISTILL_INTERVAL", Label: "Auto KB capture — check interval", Group: "General", Type: settingString, Default: "5m", Help: "Go duration (e.g. 5m). How often the distiller checks live sessions for eligible KB capture."},
	{Key: "FLOW_KB_DREAM_ENABLED", Label: "KB hygiene (dreaming)", Group: "General", Type: settingBool, Default: "true", Help: "Periodically review the KB for stale/superseded/incorrect entries and move them into a 'Pending removal' section in each file. You review and Keep/remove them; anything left flagged past the max age below is auto-removed."},
	{Key: "FLOW_KB_DREAM_MAX_AGE", Label: "KB hygiene — auto-remove after", Group: "General", Type: settingString, Default: "720h", Help: "Go duration (e.g. 720h = 30 days). Entries left in 'Pending removal' longer than this are permanently deleted."},
	{Key: "FLOW_KB_DREAM_INTERVAL", Label: "KB hygiene — run every", Group: "General", Type: settingString, Default: "24h", Help: "Go duration (e.g. 24h). How often the hygiene pass reviews the KB and prunes expired flagged entries."},
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

// seedConfigFromEnv persists operator configuration that was supplied via the
// environment (e.g. exported in a shell rc) but not yet written to config.json,
// so it survives a restart launched from a different shell or a launcher (cron,
// launchd) that lacks those exports. Without this, a setting like
// FLOW_GH_ENABLED that lives only in the launching shell silently reverts to
// its default for any server started without it — quietly disabling GitHub
// polling (and with it PR auto-linking, the inbox, etc.).
//
// Secrets are skipped on purpose: an operator who injects tokens via the
// environment (from a secret manager, say) should not have them written to
// disk behind their back. Keys already present in config.json are left alone —
// config stays authoritative; this only fills genuine gaps. Runs at boot,
// after applyConfigToEnv, so it captures the original inherited env for keys
// config did not already define.
func (s *Server) seedConfigFromEnv() {
	path := s.configPath()
	if path == "" {
		return
	}
	cfg := loadConfigFile(path)
	changed := false
	for _, sp := range settingsRegistry {
		if sp.Type == settingSecret {
			continue
		}
		if v, ok := cfg[sp.Key]; ok && strings.TrimSpace(v) != "" {
			continue // already persisted — config wins
		}
		raw := strings.TrimSpace(os.Getenv(sp.Key))
		if raw == "" {
			continue
		}
		cfg[sp.Key] = raw
		changed = true
	}
	if changed {
		_ = saveConfigFile(path, cfg)
	}
}

// migrateConfigSecretsToKeyring moves any keyring-routed secret that is still
// sitting in config.json (from an install created before the secret was
// keyring-backed) into the OS keyring, then strips the plaintext from
// config.json. One-shot and idempotent: once migrated the key is gone from
// config, so later boots are no-ops. Runs at boot after applyConfigToEnv/
// seedConfigFromEnv and before the keyring hydration, so the migrated value is
// what subsequent loads pick up. Without this, an existing install that merely
// upgrades would keep its plaintext Slack token at rest in config.json
// indefinitely (security audit P2-2).
func (s *Server) migrateConfigSecretsToKeyring() {
	path := s.configPath()
	if path == "" {
		return
	}
	cfg := loadConfigFile(path)
	if len(cfg) == 0 {
		return
	}
	changed := false
	for key, val := range cfg {
		if strings.TrimSpace(val) == "" {
			continue
		}
		service, account, routed := secretKeyringRouteForEnv(key)
		if !routed {
			continue
		}
		if err := storeKeyringSecret(service, account, key, val); err != nil {
			log.Printf("flow: could not migrate secret %s from config.json to keyring: %v", key, err)
			continue // leave it in config rather than lose the secret
		}
		delete(cfg, key)
		changed = true
		log.Printf("flow: migrated %s from config.json to the OS keyring", key)
	}
	if changed {
		if err := saveConfigFile(path, cfg); err != nil {
			log.Printf("flow: could not rewrite config.json after secret migration: %v", err)
		}
	}
}

type uiSettingField struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Group string `json:"group"`
	// Category and Connector are additive: present only for connector-owned
	// settings (omitted for generic ones), so existing consumers that group by
	// Group keep working while the Connectors UI groups by category/connector.
	Category  string   `json:"category,omitempty"`
	Connector string   `json:"connector,omitempty"`
	Type      string   `json:"type"`
	Default   string   `json:"default,omitempty"`
	Options   []string `json:"options,omitempty"`
	Help      string   `json:"help,omitempty"`
	Value     string   `json:"value"`  // current value; ALWAYS "" for secrets
	Set       bool     `json:"set"`    // is an explicit (non-default) value present?
	Source    string   `json:"source"` // "config" | "env" | "default"
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
			Key: sp.Key, Label: sp.Label, Group: sp.Group,
			Category: sp.Category, Connector: sp.Connector, Type: string(sp.Type),
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
	cfgDirty := false
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
		// Keyring-backed secrets (Slack/GitHub tokens, client/webhook secrets)
		// must never be persisted to config.json in plaintext — route them to the
		// OS keyring (which also exports them to the env) and strip any prior
		// plaintext copy from config (security audit P2-2). Without this, editing
		// such a secret in the Settings UI would defeat the keyring migration.
		if service, account, routed := secretKeyringRouteForEnv(key); routed {
			if err := storeKeyringSecret(service, account, key, val); err != nil {
				return actionResponse{OK: false, Message: "store secret: " + err.Error()}, http.StatusInternalServerError
			}
			if _, had := cfg[key]; had {
				delete(cfg, key)
				cfgDirty = true
			}
			changed = append(changed, key)
			continue
		}
		if cfg[key] == val {
			if os.Getenv(key) != val {
				os.Setenv(key, val)
				changed = append(changed, key)
			}
			continue
		}
		cfg[key] = val
		os.Setenv(key, val)
		cfgDirty = true
		changed = append(changed, key)
	}
	if len(changed) == 0 {
		return actionResponse{OK: true, Message: "no changes"}, http.StatusOK
	}
	if cfgDirty {
		if err := saveConfigFile(s.configPath(), cfg); err != nil {
			return actionResponse{OK: false, Message: "save settings: " + err.Error()}, http.StatusInternalServerError
		}
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
	slackTouched, ghTouched, ingressTouched := false, false, false
	for _, k := range changed {
		if strings.HasPrefix(k, "FLOW_SLACK_") || strings.HasPrefix(k, "SLACK_") {
			slackTouched = true
		}
		if strings.HasPrefix(k, "FLOW_GH_") {
			ghTouched = true
		}
		if strings.HasPrefix(k, "FLOW_ZROK_") || strings.HasPrefix(k, "FLOW_INGRESS_") || k == "FLOW_PUBLIC_BASE_URL" || k == "FLOW_GH_WEBHOOK_SECRET" {
			ingressTouched = true
		}
	}
	if slackTouched {
		// Invalidate the operator↔bot IM channel cache. It depends on
		// SlackBotToken()+SelfUserIDs(), both of which live in FLOW_SLACK_* /
		// SLACK_* env vars. If the token or self-user IDs changed, the cached
		// channel IDs are stale and must be re-resolved on the next use.
		monitor.ResetCommandChannelCache()
		if s.slackListener != nil {
			s.slackListener.Stop()
			_ = s.slackListener.Start()
		}
	}
	if ghTouched && s.githubListener != nil {
		s.githubListener.Stop()
		_ = s.githubListener.Start()
	}
	if ingressTouched {
		// Mint + persist the webhook secret / reserved share name when this
		// change turns zrok ingress on, so the share can start and its URL
		// stays stable across restarts. No-op once both are set.
		s.ensureZrokIngressCredentials()
		s.restartIngress()
	}
}
