package server

import (
	"database/sql"
	"flow/internal/flowdb"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (s *Server) uiAgent(tv TaskView, live map[string]bool) uiAgent {
	workDir := tv.WorkDir
	staleOverviewSession := false
	if tv.Slug == overviewTaskSlug && strings.TrimSpace(s.cfg.FlowRoot) != "" {
		if abs, err := filepath.Abs(s.cfg.FlowRoot); err == nil {
			staleOverviewSession = filepath.Clean(tv.WorkDir) != filepath.Clean(abs)
			workDir = abs
		} else {
			staleOverviewSession = filepath.Clean(tv.WorkDir) != filepath.Clean(s.cfg.FlowRoot)
			workDir = s.cfg.FlowRoot
		}
	}
	provider := "claude"
	if tv.SessionProvider != nil && *tv.SessionProvider != "" {
		provider = *tv.SessionProvider
	}
	permissionMode := tv.PermissionMode
	if permissionMode == "" {
		permissionMode = flowdb.DefaultPermissionMode
	}
	sessionID := ""
	if tv.SessionID != nil {
		sessionID = *tv.SessionID
	}
	bridgeRunning := s.terminals != nil && s.terminals.running(tv.Slug)
	sharedRunning := s.terminals != nil && s.terminals.sharedRunning(tv.Slug)
	sessionLive := tv.SessionID != nil && live[strings.ToLower(*tv.SessionID)]
	terminalMode := "idle"
	switch {
	case bridgeRunning:
		terminalMode = "browser"
	case sharedRunning:
		terminalMode = "shared"
	case sessionLive:
		terminalMode = "native"
	}
	fullTranscript := s.fullUITranscriptForTask(tv)
	transcript := fullTranscript
	if len(transcript) > 24 {
		transcript = transcript[len(transcript)-24:]
	}
	if len(transcript) == 0 {
		transcript = s.syntheticTranscript(tv)
		fullTranscript = transcript
	}
	if terminalMode == "browser" && s.transcriptAheadOfBrowserTerminal(tv.Slug, fullTranscript) {
		terminalMode = "native"
	}
	insights := s.sessionInsightsForTask(tv, provider, fullTranscript)
	hookRuntime := s.agentHookRuntimeState(tv, provider)
	var status string // assigned on every path of the switch below
	runtimeSource := "task"
	runtimeEvent := ""
	switch tv.Status {
	case "in-progress":
		switch {
		case tv.WaitingOn != nil:
			status = "waiting"
			runtimeSource = "flow"
		case bridgeRunning:
			status = "running"
			runtimeSource = "browser_terminal"
		case sharedRunning:
			status = "running"
			runtimeSource = "shared_terminal"
		case sessionLive:
			status = "running"
			runtimeSource = "process"
		case tv.StaleDays != nil:
			status = "stale"
			runtimeSource = "task"
		default:
			status = "idle"
			runtimeSource = "task"
		}
	case "done":
		status = "idle"
	default:
		status = tv.Status
	}
	if staleOverviewSession {
		sessionID = ""
		status = "idle"
		runtimeSource = "task"
	}
	transcriptWaiting := s.codexTranscriptWaitingFor(tv, provider)
	if !staleOverviewSession && tv.Status == "in-progress" {
		if hookRuntime != nil && hookRuntime.Status != "" {
			status = hookRuntime.Status
			runtimeSource = "hook"
			runtimeEvent = hookRuntime.EventKind
			// If the hook says "running" but neither the hook nor the
			// transcript has moved in a while, the hook layer is stale
			// (a Stop hook may have failed to fire on interrupt, or
			// hooks aren't reaching the server). Demote to "idle" so
			// the UI reflects what the user observes in the terminal.
			if status == "running" && runtimeStateStaleForRunning(hookRuntime.UpdatedAt, insights.ActivityAt) {
				status = "idle"
				runtimeEvent = hookRuntime.EventKind + ":stale"
			}
		}
		if transcriptWaiting != nil {
			status = "waiting"
			runtimeSource = "transcript"
			runtimeEvent = "request_user_input"
		}
	}
	// When the task was bootstrapped into a git worktree (see `flow do`),
	// the user is editing on the worktree's branch, not the parent repo's
	// checked-out branch. Prefer the worktree path for git diff and branch
	// queries so the UI reflects what the agent session sees. Fall back to
	// workDir if the recorded worktree no longer exists on disk (user
	// manually removed it).
	gitDir := workDir
	if tv.WorktreePath != nil && *tv.WorktreePath != "" {
		if info, err := os.Stat(*tv.WorktreePath); err == nil && info.IsDir() {
			gitDir = *tv.WorktreePath
		}
	}
	branches := []string{}
	branch := "~/.flow"
	if tv.Slug != overviewTaskSlug {
		branch = s.cachedGitBranch(gitDir)
		if branch == "" {
			branch = tv.Slug + "/main"
		} else {
			branches = s.cachedGitBranches(gitDir, branch)
		}
	}
	lastActivityAt := laterTimestamp(latestTaskActivity(tv), insights.ActivityAt)
	lastActivity := secondsSince(lastActivityAt)
	startedAt := tv.CreatedAt
	if tv.SessionStarted != nil {
		startedAt = *tv.SessionStarted
	}
	diff, files := s.cachedGitDiff(gitDir)
	summary := latestMarkdownSummary(tv.Updates)
	if summary == "" {
		summary = readMarkdownSummary(tv.BriefPath)
	}
	if summary == "" {
		summary = tv.TemporalSummary
	}
	if summary == "" {
		summary = "No recent update has been written for this task."
	}
	nextStep := "Open the task with flow do " + tv.Slug
	if tv.WaitingOn != nil {
		nextStep = "Waiting on " + *tv.WaitingOn
	}
	lastAction := insights.LastAction
	if lastAction == "" {
		lastAction = tv.TemporalSummary
	}
	if lastAction == "" {
		lastAction = "updated " + formatActivity(lastActivity)
	}
	tokensMax := insights.TokensMax
	if tokensMax <= 0 {
		tokensMax = contextWindowForProvider(provider)
	}
	tokensUsed := insights.TokensUsed
	if tokensUsed <= 0 {
		tokensUsed = estimateTokens(tv, fullTranscript, tokensMax)
	}
	if tokensUsed > tokensMax {
		tokensUsed = tokensMax
	}
	agent := uiAgent{
		Slug:            tv.Slug,
		Name:            tv.Name,
		Project:         tv.ProjectSlug,
		Kind:            tv.Kind,
		PlaybookSlug:    tv.PlaybookSlug,
		Parent:          tv.Parent,
		Parents:         tv.Parents,
		Children:        tv.Children,
		ForkedFromSlug:  tv.ForkedFromSlug,
		ForkedFrom:      tv.ForkedFrom,
		ForkReason:      tv.ForkReason,
		Forks:           tv.Forks,
		Branch:          branch,
		Branches:        branches,
		WorkDir:         workDir,
		Provider:        provider,
		PermissionMode:  permissionMode,
		Priority:        tv.Priority,
		Status:          status,
		TaskStatus:      tv.Status,
		RuntimeStatus:   status,
		RuntimeEvent:    runtimeEvent,
		RuntimeSource:   runtimeSource,
		Monitored:       s.inboxMonitors != nil && s.inboxMonitors.running(tv.Slug),
		HookHealth:      s.agentHookHealth(tv, provider, fullTranscript, hookRuntime),
		SessionID:       sessionID,
		StartedMin:      minutesSince(startedAt),
		LastActivitySec: lastActivity,
		LastAction:      lastAction,
		Diff:            diff,
		DiffFiles:       files,
		TokensUsed:      tokensUsed,
		TokensMax:       tokensMax,
		TokensSession:   insights.TokensSession,
		CostSession:     insights.CostSession,
		Activity:        toolCallActivitySeries(fullTranscript, time.Now()),
		Tags:            tv.Tags,
		Summary:         summary,
		NextStep:        nextStep,
		Transcript:      transcript,
		Brief:           readMarkdownSummary(tv.BriefPath),
		RecentTools:     recentTools(transcript),
		BriefPath:       tv.BriefPath,
		Updates:         tv.Updates,
		AuxFiles:        tv.AuxFiles,
		Terminal:        terminalSample(withTaskWorkDir(tv, workDir), provider, transcript, terminalMode),
	}
	if tv.WaitingOn != nil {
		agent.WaitingFor = &uiWaitingFor{Kind: "flow", Cmd: "flow update task " + tv.Slug + " --clear-waiting", Why: *tv.WaitingOn}
	} else if transcriptWaiting != nil {
		agent.WaitingFor = transcriptWaiting
	}
	// AutoRun fields (U1, U2). Populate from TaskView; display-only reconcile
	// via the 15s TTL cache so we never call Signal(0) on the hot SSE path.
	if tv.AutoRunStatus != nil {
		st := *tv.AutoRunStatus
		if st == "running" && tv.AutoRunPID != nil {
			pid := int(*tv.AutoRunPID)
			alive, ok := s.caches.autoAlive.get(pid)
			if !ok {
				alive = processAliveProbe(pid)
				s.caches.autoAlive.set(pid, alive)
			}
			if !alive {
				st = "dead"
			}
		}
		agent.AutoRunStatus = st
	}
	if tv.AutoRunStarted != nil {
		agent.AutoRunStarted = *tv.AutoRunStarted
	}
	if tv.AutoRunFinished != nil {
		agent.AutoRunFinished = *tv.AutoRunFinished
	}
	if tv.AutoRunLog != nil {
		agent.AutoRunLog = *tv.AutoRunLog
	}
	return agent
}

func (s *Server) codexTranscriptWaitingFor(tv TaskView, provider string) *uiWaitingFor {
	if provider != "codex" || tv.SessionID == nil || strings.TrimSpace(*tv.SessionID) == "" {
		return nil
	}
	task := &flowdb.Task{
		Slug:            tv.Slug,
		WorkDir:         tv.WorkDir,
		WorktreePath:    nullStringFromPtr(tv.WorktreePath),
		SessionProvider: provider,
		SessionID:       sql.NullString{String: *tv.SessionID, Valid: true},
		SessionPath:     nullStringFromPtr(tv.SessionPath),
	}
	path, err := sessionJSONLPath(s.cfg.DB, task)
	if err != nil {
		return nil
	}
	entry, err := s.transcripts.get(path)
	if err != nil {
		return nil
	}
	pending := entry.pending
	if pending == nil {
		return nil
	}
	return &uiWaitingFor{
		Kind: "question",
		Cmd:  "Open session " + tv.Slug,
		Why:  pending.Question,
	}
}

func withTaskWorkDir(tv TaskView, workDir string) TaskView {
	tv.WorkDir = workDir
	return tv
}

func (s *Server) sessionInsightsForTask(tv TaskView, provider string, transcript []uiTranscript) taskSessionInsights {
	insights := taskSessionInsights{TokensMax: contextWindowForProvider(provider)}
	if action := latestTranscriptAction(transcript); action != "" {
		insights.LastAction = action
	}
	if tv.SessionID == nil || strings.TrimSpace(*tv.SessionID) == "" {
		return insights
	}
	task := &flowdb.Task{
		Slug:            tv.Slug,
		WorkDir:         tv.WorkDir,
		WorktreePath:    nullStringFromPtr(tv.WorktreePath),
		SessionProvider: provider,
		SessionID:       sql.NullString{String: *tv.SessionID, Valid: true},
		SessionPath:     nullStringFromPtr(tv.SessionPath),
	}
	path, err := sessionJSONLPath(s.cfg.DB, task)
	if err != nil {
		return insights
	}
	entry, err := s.transcripts.get(path)
	if err != nil {
		return insights
	}
	insights.ActivityAt = entry.mtime.Format(time.RFC3339)
	stats := entry.usage
	insights.ActivityAt = laterTimestamp(insights.ActivityAt, stats.LastTimestamp)
	if stats.TokensUsed > 0 {
		insights.TokensUsed = stats.TokensUsed
	}
	if stats.TokensSession > 0 {
		insights.TokensSession = stats.TokensSession
	}
	// All-time estimated cost = sum over every day the session was active. This
	// is the full billed cost (cache included), so it does NOT track
	// TokensSession (cache-excluded work) one-to-one — cache reads cost money but
	// aren't counted as work.
	for _, c := range stats.CostByDay {
		insights.CostSession += c
	}
	if stats.TokensMax > 0 {
		insights.TokensMax = stats.TokensMax
	} else if stats.Model != "" {
		insights.TokensMax = contextWindowForModel(provider, stats.Model)
	}
	if insights.TokensUsed > insights.TokensMax {
		insights.TokensMax = insights.TokensUsed
	}
	for _, entry := range transcript {
		insights.ActivityAt = laterTimestamp(insights.ActivityAt, entry.Time)
	}
	return insights
}

func runtimeStateStaleForRunning(hookUpdatedAt, transcriptActivityAt string) bool {
	hookAge := ageSince(hookUpdatedAt)
	if hookAge < runtimeHookStaleAfter {
		return false
	}
	if transcriptActivityAt == "" {
		return true
	}
	return ageSince(transcriptActivityAt) >= runtimeTranscriptStaleAfter
}

func ageSince(ts string) time.Duration {
	if strings.TrimSpace(ts) == "" {
		return time.Duration(1<<62 - 1)
	}
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Duration(1<<62 - 1)
	}
	delta := time.Since(parsed)
	if delta < 0 {
		return 0
	}
	return delta
}

func contextWindowForProvider(provider string) int {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "claude-code", "claudecode":
		return 1000000
	case "codex":
		return 200000
	default:
		return 200000
	}
}

// contextWindowForModel returns the provider's context cap, refined by model
// when known. Claude Opus 4.6+ ships a 1M context window (some model ids also
// carry a "[1m]" suffix); Sonnet, Haiku, and older Opus are 200k. Codex's window
// comes from the JSONL (model_context_window), handled by the caller.
func contextWindowForModel(provider, model string) int {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return contextWindowForProvider(provider)
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "claude-code", "claudecode":
		if claudeHas1MContext(m) {
			return 1000000
		}
		return 200000
	}
	return contextWindowForProvider(provider)
}

// claudeHas1MContext reports whether a (lowercased) Claude model id has the 1M
// context window. True for an explicit "[1m]" tag, any Opus 5+, and Opus 4.6+
// (4-6 / 4-7 / 4-8 / 4-9 …). Earlier Opus 4 (4-0..4-5), Sonnet, and Haiku are
// 200k. Parsing the minor digit (rather than listing each release) keeps new
// Opus 4.x models correct without a code change — the gap that made every
// opus-4-8 session mis-read as 200k and clamp to a bogus "381k/381k" bar.
func claudeHas1MContext(m string) bool {
	if strings.Contains(m, "[1m]") {
		return true
	}
	idx := strings.Index(m, "opus-")
	if idx < 0 {
		return false
	}
	rest := m[idx+len("opus-"):] // e.g. "4-8", "4-8-20260101", "5-0"
	major, after, ok := leadingInt(rest)
	if !ok {
		return false
	}
	if major >= 5 {
		return true
	}
	if major == 4 && strings.HasPrefix(after, "-") {
		if minor, _, ok := leadingInt(after[1:]); ok && minor >= 6 {
			return true
		}
	}
	return false
}

// leadingInt reads the leading run of ASCII digits from s, returning the value,
// the remainder after them, and whether any digit was present.
func leadingInt(s string) (int, string, bool) {
	j := 0
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == 0 {
		return 0, s, false
	}
	n, err := strconv.Atoi(s[:j])
	if err != nil {
		return 0, s, false
	}
	return n, s[j:], true
}

func latestTranscriptAction(transcript []uiTranscript) string {
	for i := len(transcript) - 1; i >= 0; i-- {
		entry := transcript[i]
		switch entry.Type {
		case "assistant":
			text := firstLine(entry.Text)
			if text == "" {
				continue
			}
			return "assistant: " + truncateText(text, 140)
		case "tool_use":
			label := strings.TrimSpace(entry.Tool)
			if entry.Input != "" {
				label = strings.TrimSpace(label + " " + entry.Input)
			}
			if label != "" {
				return "ran " + truncateText(label, 140)
			}
		case "tool_result":
			if entry.Summary != "" {
				return "tool result: " + truncateText(entry.Summary, 140)
			}
		case "user":
			if text := firstLine(entry.Text); text != "" {
				return "user: " + truncateText(text, 140)
			}
		}
	}
	return ""
}

func (s *Server) uiBacklog(tv TaskView) uiBacklogTask {
	project := "(floating)"
	if tv.ProjectSlug != nil {
		project = *tv.ProjectSlug
	}
	provider := "claude"
	if tv.SessionProvider != nil && strings.TrimSpace(*tv.SessionProvider) != "" {
		provider = *tv.SessionProvider
	}
	out := uiBacklogTask{
		Slug:       tv.Slug,
		Name:       tv.Name,
		Project:    project,
		Parent:     tv.Parent,
		Parents:    tv.Parents,
		Children:   tv.Children,
		Provider:   provider,
		Priority:   tv.Priority,
		Tags:       tv.Tags,
		WaitingOn:  tv.WaitingOn,
		StartedMin: minutesSince(tv.CreatedAt),
	}
	if tv.DueInfo != nil {
		out.Due = *tv.DueInfo
	}
	return out
}
