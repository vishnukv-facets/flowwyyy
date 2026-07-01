package server

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"flow/internal/agents"
	"flow/internal/flowdb"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type jsonlRecord struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

type jsonlMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// sessionJSONLPath resolves a task's session transcript file path.
//
// Hot-path fast lane: if tasks.session_path is populated and the file
// still exists, that path is returned without computing or walking
// anything. The UI tick hits this path many times per second per
// connected client — keeping it to a single os.Stat is the difference
// between idle and a saturated CPU when ~/.codex/sessions has grown to
// thousands of files.
//
// Fallbacks (when session_path is unset or stale):
//   - Codex: walks ~/.codex/sessions to find the file matching session_id.
//   - Claude: computes the deterministic ~/.claude/projects/<cwd>/<id>.jsonl.
//
// Self-heal: on a successful fallback resolution, the discovered path is
// written back to tasks.session_path so subsequent ticks take the fast
// lane. Persistence is best-effort — a failed UPDATE never breaks the
// caller. Pass db=nil to skip persistence (used by call sites that
// don't have a DB handle wired through).
func sessionJSONLPath(db *sql.DB, task *flowdb.Task) (string, error) {
	if !task.SessionID.Valid || task.SessionID.String == "" {
		return "", errors.New("task has no session")
	}
	if task.SessionPath.Valid && task.SessionPath.String != "" {
		if _, err := os.Stat(task.SessionPath.String); err == nil {
			return task.SessionPath.String, nil
		}
		// Stale path (file moved/archived/deleted) — fall through and
		// re-resolve. The new path will be persisted below, replacing
		// the stale value.
	}
	resolved, err := resolveSessionJSONLPath(task)
	if err != nil {
		return "", err
	}
	if db != nil && resolved != "" && resolved != task.SessionPath.String {
		// Self-heal: cache the resolved path so the next tick takes the
		// fast lane. Best-effort: a write failure is silently ignored;
		// the worst case is one extra walk on the next tick.
		_, _ = db.Exec(
			`UPDATE tasks SET session_path = ? WHERE slug = ?`,
			resolved, task.Slug,
		)
	}
	return resolved, nil
}

// backfillSessionPaths populates tasks.session_path for any Codex task
// whose session has been captured but predates the column existing.
// Without this, the first UI tick after upgrading flow would still pay
// the recursive walk cost; with it, the steady-state fast lane kicks in
// immediately on server start.
//
// Best-effort: a row-level failure logs to stderr and continues with the
// next task. The function returns nil even on partial failure so a
// pathological ~/.codex/sessions tree (e.g. unreadable) can't block
// server startup.
func (s *Server) backfillSessionPaths() {
	if s.cfg.DB == nil {
		return
	}
	rows, err := s.cfg.DB.Query(
		`SELECT slug, session_id FROM tasks
		 WHERE session_provider = 'codex'
		   AND session_id IS NOT NULL AND session_id != ''
		   AND (session_path IS NULL OR session_path = '')
		   AND deleted_at IS NULL`,
	)
	if err != nil {
		return
	}
	type pending struct{ slug, sid string }
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.slug, &p.sid); err != nil {
			continue
		}
		todo = append(todo, p)
	}
	rows.Close()
	for _, p := range todo {
		path, err := agents.FindCodexSessionPathByID(p.sid)
		if err != nil || path == "" {
			continue
		}
		_, _ = s.cfg.DB.Exec(
			`UPDATE tasks SET session_path = ? WHERE slug = ? AND session_id = ?`,
			path, p.slug, p.sid,
		)
	}
}

func resolveSessionJSONLPath(task *flowdb.Task) (string, error) {
	if task.SessionProvider == agents.ProviderCodex {
		path, err := agents.FindCodexSessionPathByID(task.SessionID.String)
		if err != nil {
			return "", fmt.Errorf("codex session file not found for %s: %w", task.SessionID.String, err)
		}
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home dir: %w", err)
	}
	// Claude derives its project dir from the cwd it was launched in. When the
	// task runs in a git worktree (flow do --worktree), that cwd is the worktree
	// path, NOT work_dir — so the jsonl lives under the worktree-encoded dir.
	// Try the worktree first, then fall back to work_dir. (Missing this is why a
	// worktree session's token count fell through to the 1.2k estimate floor:
	// the usage parse couldn't find the transcript.)
	candidates := make([]string, 0, 2)
	if wt := strings.TrimSpace(task.WorktreePath.String); task.WorktreePath.Valid && wt != "" {
		candidates = append(candidates, wt)
	}
	if wd := strings.TrimSpace(task.WorkDir); wd != "" {
		candidates = append(candidates, wd)
	}
	var lastPath string
	for _, cwd := range candidates {
		path := filepath.Join(home, ".claude", "projects", encodeCwdForClaude(cwd), task.SessionID.String+".jsonl")
		lastPath = path
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("session file not found: %s", lastPath)
}

func encodeCwdForClaude(cwd string) string {
	return strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(cwd)
}

func parseTranscriptFile(path string) ([]TranscriptEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var entries []TranscriptEntry
	var offset int64
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		lineOffset := offset
		offset += int64(len(line)) + 1
		if len(line) == 0 {
			continue
		}
		parsed := parseTranscriptLine(line, lineOffset)
		entries = append(entries, parsed...)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

type transcriptUsageStats struct {
	// TokensUsed is the CURRENT context-window occupancy — the most recent
	// turn's token total (it's overwritten, not summed). This is the "38k/258k"
	// fill bar on a session card, paired with TokensMax (the model's context
	// window).
	TokensUsed int
	TokensMax  int
	// TokensSession is the cumulative token count this session, defined to MATCH
	// Claude Code's own `/stats` "total tokens": fresh input + output + cache
	// CREATION, EXCLUDING cache READS. Accumulated per turn via processedTokens().
	// This is the header pill ("tokens used this session") and what the Mission
	// Control tokens panel sums, so the two correlate.
	//
	// Why this basis: cache reads (re-reading already-cached context every turn)
	// run to billions over a busy history and would inflate this ~25x past what
	// /stats shows — they're real billed cost (priced at 0.1x in CostByDay) but
	// not new tokens, so we exclude them here. Cache CREATION is new tokens
	// written to cache and IS counted by /stats, so we include it (excluding it
	// was the main reason this metric used to read ~7x low vs Claude Code).
	TokensSession int
	Model         string
	LastTimestamp string
	// TokensByDay attributes each turn's processedTokens() to the local calendar
	// day (YYYY-MM-DD) of its timestamp — the basis for the token trend. Claude
	// reports per-turn usage in message.usage. Codex reports running totals via
	// payload.Info, so the accumulator buckets the delta between successive
	// totals.
	TokensByDay map[string]int
	// CostByDay is the estimated USD cost of each day's BILLED tokens. Unlike
	// TokensByDay (cache-excluded "work"), cost counts the full bill — fresh
	// input + output PLUS cache reads and cache creation at their cache
	// multipliers (see billedCostSplitUSD) — because that is what's actually
	// charged and what makes the figure track Claude Code's own /cost. Each turn is
	// priced at its own model's rate (see pricing.go), so a day's figure blends
	// however many models/providers were active that day. Turns whose model has
	// no published rate contribute 0, so this is a floor, not an invoice.
	CostByDay map[string]float64
	// CacheReadTokens and CacheCreationTokens are all-time session totals. They
	// are kept beside TokensSession so the UI can show "N (+ cached)" without
	// changing TokensSession's cache-excluded semantics.
	CacheReadTokens     int
	CacheCreationTokens int
	CostFresh           float64
	CostCacheRead       float64
	CostCacheCreation   float64
	// LookupEvents are raw retrieval-like tool calls found while scanning the
	// transcript. They stay raw here because own-task bootstrap reads can only
	// be filtered once the caller knows which task slug owns this session path.
	LookupEvents []transcriptLookupEvent
	// claudeSeen dedups Claude usage by (message.id, requestId). Claude Code
	// appends intermediate AND final usage snapshots for the SAME request to the
	// jsonl (identical token counts), so summing every line double-counts work
	// and cost ~2-3x on a long session. We count each request once (first
	// snapshot wins; in practice all snapshots of a request are identical). nil
	// until the first dedup-eligible Claude turn.
	claudeSeen map[string]struct{}
	// lastCodexFreshTotal is internal accumulator state for deriving per-event
	// Codex deltas from cumulative total_token_usage records.
	lastCodexFreshTotal int
	// lastCodexFreshInput / lastCodexFreshOutput mirror lastCodexFreshTotal but
	// split, so the per-event Codex cost delta can apply input and output rates
	// separately (Codex output is priced well above input).
	lastCodexFreshInput  int
	lastCodexFreshOutput int
	// lastCodexCached tracks the cumulative cached-input portion so the
	// per-event delta can be priced at the cache-read rate (Codex's cached input
	// is billed at 0.1x, not free).
	lastCodexCached int
}

type transcriptUsageRecord struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	RequestID string          `json:"requestId"`
	Payload   json.RawMessage `json:"payload"`
	Message   struct {
		ID      string               `json:"id"`
		Model   string               `json:"model"`
		Usage   transcriptTokenUsage `json:"usage"`
		Content json.RawMessage      `json:"content"`
	} `json:"message"`
}

// usageDedupKey identifies a Claude request for snapshot dedup. Empty when the
// record lacks both ids (e.g. Codex events, synthetic test lines) — callers
// treat an empty key as "don't dedup", counting the line normally.
func (r transcriptUsageRecord) usageDedupKey() string {
	if r.Message.ID == "" || r.RequestID == "" {
		return ""
	}
	return r.Message.ID + "|" + r.RequestID
}

type transcriptTokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	CachedInputTokens        int `json:"cached_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	ReasoningOutputTokens    int `json:"reasoning_output_tokens"`
	TotalTokens              int `json:"total_tokens"`
	ModelContextWindow       int `json:"model_context_window"`
	// CacheCreation is Claude's per-TTL breakdown of CacheCreationInputTokens.
	// 5-minute writes bill at 1.25x input, 1-hour writes at 2x, so the split
	// matters for cost. Codex doesn't emit this (no cache-write charge).
	CacheCreation struct {
		Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (u transcriptTokenUsage) total() int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.InputTokens +
		u.CachedInputTokens +
		u.CacheCreationInputTokens +
		u.CacheReadInputTokens +
		u.OutputTokens +
		u.ReasoningOutputTokens
}

// processedTokens is the per-turn token count we report, defined to MATCH what
// Claude Code's own `/stats` calls "total tokens": fresh input + output + cache
// CREATION (cache writes), EXCLUDING cache READS.
//
//   - Cache reads ARE excluded: re-reading already-cached context every turn is
//     repetitive (a long session re-reads the whole window thousands of times —
//     7B+ tokens across a busy history). Counting them would inflate the figure
//     ~25x past what /stats shows; they're priced into cost (at 0.1x), not work.
//   - Cache creation IS included: those are genuinely new tokens written to the
//     cache, and /stats counts them. Excluding them (the old behaviour) made
//     this metric ~7x smaller than /stats — the single biggest reason flow's
//     token numbers read low against Claude Code.
//
// Codex has no cache-creation tokens, so for Codex this is just fresh in+out.
// Claude reports fresh input in InputTokens (cache reads are separate in
// CacheReadInputTokens); Codex bundles the cached portion into InputTokens,
// exposed as CachedInputTokens, so freshInput subtracts it.
func (u transcriptTokenUsage) processedTokens() int {
	return u.freshInput() + u.freshOutput() + u.CacheCreationInputTokens
}

func (u transcriptTokenUsage) cacheCreationTokens() int {
	return u.CacheCreationInputTokens
}

// freshInput is the genuinely-fresh input for a turn: InputTokens minus the
// cached portion. Claude reports cache reads separately (CachedInputTokens is
// ~0 for Claude), while Codex bundles the cached portion into InputTokens and
// exposes it as CachedInputTokens — subtracting it works for both.
func (u transcriptTokenUsage) freshInput() int {
	in := u.InputTokens - u.CachedInputTokens
	if in < 0 {
		in = 0
	}
	return in
}

// freshOutput is what the model generated this turn: visible output plus
// reasoning tokens. Both are billed at the model's output rate.
func (u transcriptTokenUsage) freshOutput() int {
	return u.OutputTokens + u.ReasoningOutputTokens
}

// cacheReadTokens is the cache-hit input for a turn, billed at the reduced
// cache-read rate. Claude reports it in CacheReadInputTokens; Codex bundles the
// cached portion into InputTokens and exposes it as CachedInputTokens. Only one
// of the two is ever populated for a given provider, so summing unifies both.
func (u transcriptTokenUsage) cacheReadTokens() int {
	return u.CacheReadInputTokens + u.CachedInputTokens
}

func parseTranscriptLine(line []byte, offset int64) []TranscriptEntry {
	if entries := parseCodexTranscriptLine(line, offset); len(entries) > 0 {
		return entries
	}
	var rec jsonlRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil
	}
	switch rec.Type {
	case "user":
		return stampTranscriptEntries(parseUserRecord(rec.Message, offset), rec.Timestamp)
	case "assistant":
		return stampTranscriptEntries(parseAssistantRecord(rec.Message, offset), rec.Timestamp)
	default:
		return nil
	}
}

type codexTranscriptRecord struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Payload   json.RawMessage `json:"payload"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Action    struct {
		Command []string `json:"command"`
	} `json:"action"`
}

type codexTranscriptBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func parseCodexTranscriptLine(line []byte, offset int64) []TranscriptEntry {
	var rec codexTranscriptRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil
	}
	switch rec.Type {
	case "response_item":
		var payload codexTranscriptRecord
		if err := json.Unmarshal(rec.Payload, &payload); err != nil {
			return nil
		}
		if payload.Timestamp == "" {
			payload.Timestamp = rec.Timestamp
		}
		return stampTranscriptEntries(codexPayloadEntries(payload, offset), payload.Timestamp)
	case "message", "function_call", "function_call_output", "local_shell_call":
		return stampTranscriptEntries(codexPayloadEntries(rec, offset), rec.Timestamp)
	default:
		return nil
	}
}

func stampTranscriptEntries(entries []TranscriptEntry, timestamp string) []TranscriptEntry {
	if timestamp == "" {
		return entries
	}
	for i := range entries {
		if entries[i].Timestamp == "" {
			entries[i].Timestamp = timestamp
		}
	}
	return entries
}

func codexPayloadEntries(rec codexTranscriptRecord, offset int64) []TranscriptEntry {
	switch rec.Type {
	case "message":
		return codexMessageEntries(rec.Role, rec.Content, offset)
	case "function_call":
		return []TranscriptEntry{{
			Type:             "tool_use",
			ToolName:         rec.Name,
			ToolInputSummary: truncate(rec.Arguments, 220),
			ToolInput:        truncate(rec.Arguments, toolInputCap),
			ToolUseID:        rec.CallID,
			ByteOffset:       offset,
		}}
	case "function_call_output":
		return []TranscriptEntry{{
			Type:           "tool_result",
			ToolResultText: truncate(rawJSONAsText(rec.Output), 4000),
			ToolUseID:      rec.CallID,
			ByteOffset:     offset,
		}}
	case "local_shell_call":
		return []TranscriptEntry{{
			Type:             "tool_use",
			ToolName:         "local_shell",
			ToolInputSummary: strings.Join(rec.Action.Command, " "),
			ToolInput:        truncate(string(rec.Payload), toolInputCap),
			ByteOffset:       offset,
		}}
	default:
		return nil
	}
}

func codexMessageEntries(role string, raw json.RawMessage, offset int64) []TranscriptEntry {
	entryType := "assistant"
	if role == "user" {
		entryType = "user"
	}
	var blocks []codexTranscriptBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var entries []TranscriptEntry
		for _, block := range blocks {
			if block.Text == "" {
				continue
			}
			switch block.Type {
			case "input_text", "output_text", "text":
				entries = append(entries, TranscriptEntry{Type: entryType, Text: block.Text, ByteOffset: offset})
			}
		}
		return entries
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil && text != "" {
		return []TranscriptEntry{{Type: entryType, Text: text, ByteOffset: offset}}
	}
	return nil
}

func rawJSONAsText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return string(raw)
}

type codexPendingUserInput struct {
	CallID    string
	Timestamp string
	Question  string
	RawJSON   string
	Seq       int
}

func codexPayloadRecord(line []byte) (codexTranscriptRecord, bool) {
	var rec codexTranscriptRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return codexTranscriptRecord{}, false
	}
	if rec.Type == "response_item" {
		var payload codexTranscriptRecord
		if err := json.Unmarshal(rec.Payload, &payload); err != nil {
			return codexTranscriptRecord{}, false
		}
		if payload.Timestamp == "" {
			payload.Timestamp = rec.Timestamp
		}
		return payload, true
	}
	return rec, true
}

func codexRequestUserInputTool(name string) bool {
	tool := normalizeAgentHookPart(name)
	return tool == "request_user_input" || strings.Contains(tool, "request_user_input")
}

func codexUserInputQuestion(arguments string) string {
	var args struct {
		Questions []struct {
			Header   string `json:"header"`
			Question string `json:"question"`
		} `json:"questions"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return ""
	}
	for i, question := range args.Questions {
		text := strings.TrimSpace(question.Question)
		if text == "" {
			continue
		}
		if remaining := len(args.Questions) - i - 1; remaining > 0 {
			return truncateText(fmt.Sprintf("%s (+%d more)", text, remaining), 220)
		}
		return truncateText(text, 220)
	}
	return ""
}

func parseUserRecord(raw json.RawMessage, offset int64) []TranscriptEntry {
	var msg jsonlMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	var plain string
	if err := json.Unmarshal(msg.Content, &plain); err == nil {
		return []TranscriptEntry{{Type: "user", Text: plain, ByteOffset: offset}}
	}
	var blocks []contentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil
	}
	var entries []TranscriptEntry
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				entries = append(entries, TranscriptEntry{Type: "user", Text: b.Text, ByteOffset: offset})
			}
		case "tool_result":
			entries = append(entries, TranscriptEntry{
				Type:           "tool_result",
				ToolResultText: toolResultText(b),
				IsError:        b.IsError,
				ToolUseID:      b.ToolUseID,
				ByteOffset:     offset,
			})
		}
	}
	return entries
}

func parseAssistantRecord(raw json.RawMessage, offset int64) []TranscriptEntry {
	var msg jsonlMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil
	}
	var entries []TranscriptEntry
	for _, b := range blocks {
		switch b.Type {
		case "thinking":
			if b.Thinking != "" {
				entries = append(entries, TranscriptEntry{Type: "thinking", Text: b.Thinking, ByteOffset: offset})
			}
		case "text":
			if b.Text != "" {
				entries = append(entries, TranscriptEntry{Type: "assistant", Text: b.Text, ByteOffset: offset})
			}
		case "tool_use":
			entries = append(entries, TranscriptEntry{
				Type:             "tool_use",
				ToolName:         b.Name,
				ToolInputSummary: formatToolInput(b.Name, b.Input),
				ToolInput:        truncate(string(b.Input), toolInputCap),
				ToolUseID:        b.ID,
				ByteOffset:       offset,
			})
		}
	}
	return entries
}

// toolInputCap bounds the raw tool input carried to the UI. Generous enough
// for typical Edit/MultiEdit/Write payloads (so diffs render in full) while
// preventing a pathological large Write from bloating the transcript response.
const toolInputCap = 24 * 1024

func toolResultText(b contentBlock) string {
	var text string
	if err := json.Unmarshal(b.Content, &text); err == nil {
		return text
	}
	var blocks []contentBlock
	if err := json.Unmarshal(b.Content, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func formatToolInput(name string, raw json.RawMessage) string {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return string(raw)
	}
	switch name {
	case "Bash":
		if cmd, ok := m["command"].(string); ok {
			return "$ " + cmd
		}
	case "Read", "Write", "Edit":
		if fp, ok := m["file_path"].(string); ok {
			return fp
		}
	case "Glob":
		if p, ok := m["pattern"].(string); ok {
			return p
		}
	case "Grep":
		if p, ok := m["pattern"].(string); ok {
			if path, ok := m["path"].(string); ok {
				return p + " in " + path
			}
			return p
		}
	}
	compact, err := json.Marshal(m)
	if err != nil {
		return string(raw)
	}
	return truncate(string(compact), 220)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
