package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// CurrentHookVersion is bumped whenever the agent-event hook wire format
// or contract changes in a way that older installed copies should be
// nudged to upgrade. The installer in internal/agenthooks stamps this
// value via `--hook-version` into every registered hook command line;
// when the server ingests a hook payload tagged with a lower version it
// surfaces an upgrade hint at the next SessionStart.
//
// Bumped to 4 when Codex repo-local hooks became Flow-owned-only so
// ordinary Codex terminals in the same workdir do not forward hook events.
const CurrentHookVersion = 4

type agentHookIngestResponse struct {
	OK           bool   `json:"ok"`
	Provider     string `json:"provider,omitempty"`
	Event        string `json:"event,omitempty"`
	Kind         string `json:"kind,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	Task         string `json:"task,omitempty"`
	Seq          int64  `json:"seq,omitempty"`
	HookOwned    bool   `json:"hook_owned,omitempty"`
	HookVersion  int    `json:"hook_version,omitempty"`
	SubagentID   string `json:"subagent_id,omitempty"`
	HookOutdated bool   `json:"hook_outdated,omitempty"`
}

func (s *Server) handleAgentHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var payload map[string]any
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&payload); err != nil {
		writeError(w, fmt.Errorf("decode hook payload: %w", err), http.StatusBadRequest)
		return
	}
	raw, _ := json.Marshal(payload)
	resp, err := s.ingestAgentHook(r, payload, string(raw))
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, resp)
}

// ingestAgentHook records the bare per-session runtime state for the
// agent hook payload and publishes a live event so subscribers see the
// transition. The cross-source notification surface (Slack DMs, monitor
// events, inbox notifications) was removed; only the runtime state used
// by Mission Control "is this agent live" indicators remains.
func (s *Server) ingestAgentHook(r *http.Request, payload map[string]any, raw string) (agentHookIngestResponse, error) {
	provider := agentHookProvider(r, payload)
	eventName := agentHookString(payload, "hook_event_name", "hookEventName")
	if eventName == "" {
		return agentHookIngestResponse{}, fmt.Errorf("hook_event_name is required")
	}
	sessionID := agentHookString(payload, "session_id", "sessionID")
	if sessionID == "" {
		sessionID = agentHookString(payload, "thread_id", "threadID")
	}
	kind := agentHookKind(eventName, payload)
	if kind == "" {
		kind = normalizeAgentHookPart(eventName)
	}
	seq := agentHookInt64(payload, "flow_seq", "flowSeq")
	hookOwned := agentHookBool(payload, "flow_hook_owned", "flowHookOwned")
	hookVersion := agentHookInt(payload, "flow_hook_version", "flowHookVersion")
	subagentID := agentHookString(payload, "agent_id", "agentID", "subagent_id", "subagentID")

	resp := agentHookIngestResponse{
		OK:           true,
		Provider:     provider,
		Event:        eventName,
		Kind:         kind,
		SessionID:    sessionID,
		Seq:          seq,
		HookOwned:    hookOwned,
		HookVersion:  hookVersion,
		SubagentID:   subagentID,
		HookOutdated: hookVersion > 0 && hookVersion < CurrentHookVersion,
	}
	if sessionID != "" {
		if task, err := flowdb.TaskBySessionID(s.cfg.DB, sessionID); err == nil {
			resp.Task = task.Slug
		}
	}
	// When the agent sends a DM (Claude or Codex), register that DM thread on
	// the task so the recipient's replies stream back — deterministic, at the
	// source, no dependence on the agent self-tagging or a fresh brief.
	if tag, ok := maybeRegisterDMThread(s.cfg.DB, eventName, resp.Task, payload); ok {
		fmt.Fprintf(os.Stderr, "monitor: registered DM thread %s -> %s (tool-use hook)\n", tag, resp.Task)
	}
	body := agentHookBody(payload)
	// Subagent stop/start events don't represent the parent agent's state.
	// Recording them on agent_runtime_states would flicker the parent
	// between idle and running, so we skip the upsert.
	if runtimeStatus := agentHookRuntimeStatus(kind, provider); runtimeStatus != "" && sessionID != "" && !isSubagentEvent(kind, subagentID) {
		if err := flowdb.UpsertAgentRuntimeState(s.cfg.DB, flowdb.AgentRuntimeStateInput{
			Provider:  provider,
			SessionID: sessionID,
			TaskSlug:  resp.Task,
			Status:    runtimeStatus,
			EventKind: kind,
			Message:   body,
			Seq:       seq,
			RawJSON:   raw,
		}); err != nil {
			return agentHookIngestResponse{}, err
		}
	}

	s.publishHookEvent(resp, payload)
	return resp, nil
}

// isSubagentEvent returns true for events emitted by a Claude subagent (a
// nested agent_id is set) — these must not flip the parent's session
// state. SubagentStart/SubagentStop kinds are always subagent-scoped
// regardless of payload shape.
func isSubagentEvent(kind, subagentID string) bool {
	switch kind {
	case "subagent_start", "subagent_stop":
		return true
	}
	return strings.TrimSpace(subagentID) != ""
}

func (s *Server) agentHookRuntimeState(tv TaskView, provider string) *flowdb.AgentRuntimeState {
	if tv.SessionID == nil || strings.TrimSpace(*tv.SessionID) == "" {
		return nil
	}
	state, err := flowdb.AgentRuntimeStateBySessionID(s.cfg.DB, provider, *tv.SessionID)
	if err != nil {
		return nil
	}
	if runtimeStateBeforeSessionStart(state.UpdatedAt, tv) {
		return nil
	}
	return state
}

func (s *Server) agentHookHealth(tv TaskView, provider string, transcript []uiTranscript, state *flowdb.AgentRuntimeState) *uiHookHealth {
	if provider != "codex" || tv.SessionID == nil || strings.TrimSpace(*tv.SessionID) == "" {
		return nil
	}
	hookStatus := codexLocalHookStatus(tv.WorkDir)
	if !hookStatus.installed {
		message := "Flow Codex hooks are not installed in this workdir yet, so live notifications may be stale."
		if hookStatus.stale {
			message = "Flow Codex hooks still point at an old command in this workdir, so live notifications may be stale."
		}
		return &uiHookHealth{
			Status:  "missing",
			Message: message,
			Action:  "Reopen or resume this session from Flow to install repo-local hooks.",
		}
	}
	if hookStatus.trusted {
		return nil
	}
	return &uiHookHealth{
		Status:  "needs_approval",
		Message: "Codex is blocking Flow hooks until they are reviewed, so questions and stop/start notifications may be incomplete.",
		Action:  "Open this Codex session and run /hooks, then approve the Flow hooks.",
	}
}

type codexHookStatus struct {
	installed bool
	stale     bool
	trusted   bool
}

func codexLocalHookStatus(workDir string) codexHookStatus {
	hooksPath := filepath.Join(workDir, ".codex", "hooks.json")
	body, err := os.ReadFile(hooksPath)
	if err != nil {
		return codexHookStatus{}
	}
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return codexHookStatus{}
	}
	status := codexHookStatus{}
	entries := codexManagedHookEntries(hooksPath, raw)
	for _, entry := range entries {
		if !strings.Contains(entry.command, "hook agent-event") || hookCommandProvider(entry.command) != "codex" {
			continue
		}
		if hookCommandUsesBareFlow(entry.command) {
			status.installed = true
			continue
		}
		status.stale = true
	}
	if status.installed {
		status.trusted = codexHookStatesTrusted(codexConfigPath(), entries)
	}
	return status
}

type codexHookEntry struct {
	sourcePath string
	event      string
	groupIndex int
	hookIndex  int
	command    string
}

func codexManagedHookEntries(hooksPath string, raw any) []codexHookEntry {
	cfg, _ := raw.(map[string]any)
	hooks, _ := cfg["hooks"].(map[string]any)
	if len(hooks) == 0 {
		return nil
	}
	sourcePath := hooksPath
	if abs, err := filepath.Abs(hooksPath); err == nil {
		sourcePath = abs
	}
	out := []codexHookEntry{}
	for eventName, eventGroups := range hooks {
		event := codexStateEventName(eventName)
		groups, ok := eventGroups.([]any)
		if !ok || event == "" {
			continue
		}
		for groupIndex, groupRaw := range groups {
			group, _ := groupRaw.(map[string]any)
			hookList, _ := group["hooks"].([]any)
			for hookIndex, hookRaw := range hookList {
				hook, _ := hookRaw.(map[string]any)
				command, _ := hook["command"].(string)
				if strings.Contains(command, "hook agent-event") && hookCommandProvider(command) == "codex" {
					out = append(out, codexHookEntry{
						sourcePath: sourcePath,
						event:      event,
						groupIndex: groupIndex,
						hookIndex:  hookIndex,
						command:    command,
					})
				}
			}
		}
	}
	return out
}

func hookCommandProvider(command string) string {
	fields := strings.Fields(command)
	for i, field := range fields {
		field = strings.Trim(field, `"'`)
		if field == "--provider" && i+1 < len(fields) {
			return strings.Trim(fields[i+1], `"'`)
		}
		if strings.HasPrefix(field, "--provider=") {
			return strings.Trim(strings.TrimPrefix(field, "--provider="), `"'`)
		}
	}
	return ""
}

func hookCommandUsesBareFlow(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	if strings.Trim(fields[0], `"'`) == "flow" {
		return true
	}
	return strings.Contains(command, "FLOW_HOOK_OWNED") && strings.Contains(command, "exec flow hook agent-event")
}

func codexHookStatesTrusted(configPath string, entries []codexHookEntry) bool {
	flowEntries := []codexHookEntry{}
	for _, entry := range entries {
		if hookCommandUsesBareFlow(entry.command) {
			flowEntries = append(flowEntries, entry)
		}
	}
	if len(flowEntries) == 0 {
		return false
	}
	state, err := readCodexTrustedHookState(configPath)
	if err != nil {
		return false
	}
	for _, entry := range flowEntries {
		if !state[entry.stateKey()] {
			return false
		}
	}
	return true
}

func readCodexTrustedHookState(configPath string) (map[string]bool, error) {
	body, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	state := map[string]bool{}
	current := ""
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[hooks.state.\"") && strings.HasSuffix(trimmed, "\"]") {
			current = strings.TrimSuffix(strings.TrimPrefix(trimmed, "[hooks.state.\""), "\"]")
			state[current] = false
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			current = ""
			continue
		}
		if current != "" && strings.HasPrefix(trimmed, "trusted_hash") && strings.Contains(trimmed, "\"sha256:") {
			state[current] = true
		}
	}
	return state, nil
}

func (entry codexHookEntry) stateKey() string {
	return fmt.Sprintf("%s:%s:%d:%d", entry.sourcePath, entry.event, entry.groupIndex, entry.hookIndex)
}

func codexConfigPath() string {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return filepath.Join(home, "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".codex", "config.toml")
}

func codexStateEventName(event string) string {
	event = strings.TrimSpace(event)
	if event == "" {
		return ""
	}
	out := strings.Builder{}
	var prev rune
	for i, r := range event {
		if r == '-' || r == ' ' {
			r = '_'
		}
		if unicode.IsUpper(r) {
			if i > 0 && prev != '_' {
				out.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		out.WriteRune(r)
		prev = r
	}
	return out.String()
}

func runtimeStateBeforeSessionStart(updatedAt string, tv TaskView) bool {
	baseline := ""
	if tv.SessionStarted != nil {
		baseline = laterTimestamp(baseline, *tv.SessionStarted)
	}
	if tv.SessionLastResumed != nil {
		baseline = laterTimestamp(baseline, *tv.SessionLastResumed)
	}
	if baseline == "" || updatedAt == "" {
		return false
	}
	updated, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return updatedAt < baseline
	}
	base, err := time.Parse(time.RFC3339, baseline)
	if err != nil {
		return updatedAt < baseline
	}
	return updated.Before(base)
}

func agentHookProvider(r *http.Request, payload map[string]any) string {
	provider := ""
	if r != nil {
		provider = strings.TrimSpace(r.URL.Query().Get("provider"))
	}
	if provider == "" {
		provider = agentHookString(payload, "provider", "session_provider", "sessionProvider")
	}
	if provider == "" {
		path := agentHookString(payload, "transcript_path", "transcriptPath")
		switch {
		case strings.Contains(path, ".codex"):
			provider = "codex"
		case strings.Contains(path, ".claude"):
			provider = "claude"
		}
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "codex":
		return "codex"
	default:
		return "claude"
	}
}

// slackDMSendFromHook inspects a PostToolUse hook payload and, when it's the
// agent posting to a DIRECT-MESSAGE channel via a Slack send tool, returns the
// DM channel and the thread root to monitor. Provider-agnostic: it matches by
// the action ("send" / "post_message") plus a DM channel id, so it catches both
// Claude's mcp__claude_ai_Slack__slack_send_message and Codex's slack
// send-message tool without hard-coding names. Drafts, reads, non-DM channels,
// and sends with no resolvable thread root are rejected (ok=false).
//
// Scoping to DMs (D-prefix) is deliberate: channel/thread posts are either the
// task's origin thread (already monitored) or out of scope — auto-registering
// every channel the agent posts in would over-monitor.
func slackDMSendFromHook(eventName string, payload map[string]any) (channel, threadTS string, ok bool) {
	if normalizeAgentHookPart(eventName) != "post_tool_use" {
		return "", "", false
	}
	tool := strings.ToLower(agentHookString(payload, "tool_name", "toolName"))
	if tool == "" || strings.Contains(tool, "draft") {
		return "", "", false
	}
	if !strings.Contains(tool, "send") && !strings.Contains(tool, "post_message") && !strings.Contains(tool, "postmessage") {
		return "", "", false
	}
	input := hookSubMap(payload, "tool_input", "toolInput")
	channel = strings.TrimSpace(hookMapString(input, "channel", "channel_id", "channelID"))
	if !isSlackDMChannelID(channel) {
		return "", "", false
	}
	threadTS = hookMapString(input, "thread_ts", "threadTs", "thread_timestamp")
	if threadTS == "" {
		// Fresh top-level DM: the posted message ts (in the tool response) is the
		// thread root the recipient will reply under.
		resp := hookSubMap(payload, "tool_response", "toolResponse", "tool_result", "toolResult")
		threadTS = hookMapString(resp, "ts", "message_ts", "timestamp")
		if threadTS == "" {
			threadTS = hookMapString(hookSubMap(resp, "message"), "ts", "timestamp")
		}
	}
	threadTS = strings.TrimSpace(threadTS)
	if threadTS == "" {
		return "", "", false
	}
	return strings.TrimSpace(channel), threadTS, true
}

// maybeRegisterDMThread registers a DM thread the agent just sent into so the
// recipient's replies route back to taskSlug. It reuses the thread model —
// slack-thread:<dm-channel>:<root> — so the existing thread routing and backfill
// handle it with no special-casing. Returns the tag and true when it registered,
// ("", false) otherwise (not a DM send, no task, or DB error). Idempotent.
func maybeRegisterDMThread(db *sql.DB, eventName, taskSlug string, payload map[string]any) (string, bool) {
	if db == nil || strings.TrimSpace(taskSlug) == "" {
		return "", false
	}
	ch, root, ok := slackDMSendFromHook(eventName, payload)
	if !ok {
		return "", false
	}
	tag := flowdb.NormalizeTag(monitor.SlackThreadTagPrefix + monitor.ThreadKey(ch, root))
	if err := flowdb.AddTaskTag(db, taskSlug, tag); err != nil {
		return "", false
	}
	return tag, true
}

// isSlackDMChannelID reports whether id is a direct-message channel (D-prefix).
// Group DMs (mpim) and private channels share the G prefix and are excluded to
// avoid mis-registering private-channel posts as DMs.
func isSlackDMChannelID(id string) bool {
	id = strings.TrimSpace(id)
	return len(id) > 1 && (id[0] == 'D' || id[0] == 'd')
}

// hookSubMap extracts a nested map[string]any from payload under any of keys.
func hookSubMap(payload map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if v, ok := payload[k]; ok {
			if m, ok := v.(map[string]any); ok {
				return m
			}
		}
	}
	return nil
}

// hookMapString reads the first present, non-empty string-valued key from m.
func hookMapString(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func agentHookKind(eventName string, payload map[string]any) string {
	event := normalizeAgentHookPart(eventName)
	if event == "notification" {
		if typ := normalizeAgentHookPart(agentHookString(payload, "notification_type", "notificationType")); typ != "" {
			return typ
		}
	}
	if event == "pre_tool_use" {
		tool := normalizeAgentHookPart(agentHookString(payload, "tool_name", "toolName"))
		switch {
		case tool == "ask_user_question", tool == "exit_plan_mode", tool == "request_user_input", strings.Contains(tool, "request_user_input"):
			return "elicitation"
		}
	}
	if event == "teammate_idle" {
		return "idle_prompt"
	}
	return event
}

func agentHookRuntimeStatus(kind, provider string) string {
	// Codex has no Notification/Elicitation/TeammateIdle hook (those are
	// Claude-only), so its only "I've yielded to the user" signal is Stop. For
	// Codex a Stop means "turn finished, awaiting your input" → waiting, which
	// fires the notification bell/toast. Claude's Stop stays a quiet turn
	// boundary because Claude emits dedicated idle_prompt/Notification events
	// when it actually needs you.
	if provider == "codex" && kind == "stop" {
		return "waiting"
	}
	switch kind {
	case "permission_request", "permission_prompt", "elicitation", "elicitation_dialog", "idle_prompt":
		return "waiting"
	case "stop", "task_completed":
		return "idle"
	case "session_start":
		return "idle"
	case "session_end":
		return "released"
	case "stop_failure":
		return "dead"
	case "subagent_start", "subagent_stop", "task_created", "user_prompt_submit", "pre_tool_use", "post_tool_use", "post_tool_use_failure", "post_tool_batch", "elicitation_result", "permission_denied":
		return "running"
	default:
		return ""
	}
}

func agentHookBody(payload map[string]any) string {
	for _, key := range []string{"message", "title", "reason", "last_assistant_message", "prompt"} {
		if value := agentHookString(payload, key); value != "" {
			return truncateText(value, 600)
		}
	}
	tool := agentHookString(payload, "tool_name", "toolName")
	if tool != "" {
		if b, ok := payload["tool_input"]; ok {
			if raw, err := json.Marshal(b); err == nil {
				return truncateText(tool+" "+string(raw), 600)
			}
		}
		return tool
	}
	return ""
}

func agentHookInt64(payload map[string]any, keys ...string) int64 {
	for _, key := range keys {
		v, ok := payload[key]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int:
			return int64(n)
		case int64:
			return n
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return i
			}
		case string:
			if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
				return i
			}
		}
	}
	return 0
}

func agentHookInt(payload map[string]any, keys ...string) int {
	return int(agentHookInt64(payload, keys...))
}

func agentHookBool(payload map[string]any, keys ...string) bool {
	for _, key := range keys {
		v, ok := payload[key]
		if !ok {
			continue
		}
		switch b := v.(type) {
		case bool:
			return b
		case string:
			s := strings.ToLower(strings.TrimSpace(b))
			return s == "true" || s == "1" || s == "yes"
		case float64:
			return b != 0
		}
	}
	return false
}

func agentHookString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key]; ok {
			switch v := value.(type) {
			case string:
				if strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			case fmt.Stringer:
				if strings.TrimSpace(v.String()) != "" {
					return strings.TrimSpace(v.String())
				}
			}
		}
	}
	return ""
}

func normalizeAgentHookPart(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	var prevLower bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			if prevLower {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			prevLower = false
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			prevLower = true
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			prevLower = false
		default:
			if b.Len() > 0 {
				b.WriteByte('_')
			}
			prevLower = false
		}
	}
	out := b.String()
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	return strings.Trim(out, "_")
}
