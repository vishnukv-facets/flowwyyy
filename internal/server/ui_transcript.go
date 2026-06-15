package server

import (
	"database/sql"
	"flow/internal/agents"
	"flow/internal/flowdb"
	"fmt"
	"os"
	"strings"
	"time"
)

func (s *Server) fullUITranscriptForTask(tv TaskView) []uiTranscript {
	return s.uiTranscriptForTaskLimit(tv, 0)
}

func (s *Server) uiTranscriptForTaskLimit(tv TaskView, limit int) []uiTranscript {
	if tv.SessionID == nil {
		return nil
	}
	provider := "claude"
	if tv.SessionProvider != nil && strings.TrimSpace(*tv.SessionProvider) != "" {
		provider = *tv.SessionProvider
	}
	t := &flowdb.Task{
		Slug:            tv.Slug,
		WorkDir:         tv.WorkDir,
		WorktreePath:    nullStringFromPtr(tv.WorktreePath),
		SessionProvider: provider,
		SessionID:       sql.NullString{String: *tv.SessionID, Valid: true},
		SessionPath:     nullStringFromPtr(tv.SessionPath),
	}
	path, err := sessionJSONLPath(s.cfg.DB, t)
	if err != nil {
		return nil
	}
	entry, err := s.transcripts.get(path)
	if err != nil {
		return nil
	}
	entries := entry.entries
	var out []uiTranscript
	for _, e := range entries {
		switch e.Type {
		case "user", "assistant", "thinking":
			if e.Text != "" {
				out = append(out, uiTranscript{Type: e.Type, Text: e.Text, Time: e.Timestamp})
			}
		case "tool_use":
			out = append(out, uiTranscript{Type: "tool_use", Tool: e.ToolName, Input: e.ToolInputSummary, Time: e.Timestamp})
		case "tool_result":
			out = append(out, uiTranscript{Type: "tool_result", Tool: "result", Summary: firstLine(e.ToolResultText), Preview: e.ToolResultText, Time: e.Timestamp})
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func (s *Server) syntheticTranscript(tv TaskView) []uiTranscript {
	var out []uiTranscript
	brief := readMarkdownSummary(tv.BriefPath)
	if brief != "" {
		out = append(out, uiTranscript{Type: "user", Text: brief})
	}
	for _, update := range tv.Updates {
		if body := readMarkdownSummary(update.Path); body != "" {
			out = append(out, uiTranscript{Type: "assistant", Text: body})
		}
		if len(out) >= 8 {
			break
		}
	}
	if len(out) == 0 {
		out = append(out, uiTranscript{Type: "assistant", Text: tv.Name})
	}
	return out
}

func latestMarkdownSummary(files []FileRef) string {
	for _, f := range files {
		if s := readMarkdownSummary(f.Path); s != "" {
			return s
		}
	}
	return ""
}

func readMarkdownSummary(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		if line == "" || strings.HasPrefix(line, "<!--") {
			continue
		}
		return truncateText(line, 180)
	}
	return ""
}

func readKBEntries(path string) []uiKBEntry {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []uiKBEntry
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		m := kbEntryRe.FindStringSubmatch(line)
		if len(m) == 3 {
			out = append(out, uiKBEntry{D: m[1], T: m[2]})
		} else {
			out = append(out, uiKBEntry{D: "", T: strings.TrimPrefix(line, "- ")})
		}
	}
	return out
}

// estimateTokens is only used when the provider transcript has no usage event.
func estimateTokens(tv TaskView, transcript []uiTranscript, max int) int {
	total := len(tv.Name) + len(tv.Slug)
	for _, e := range transcript {
		total += len(e.Text) + len(e.Input) + len(e.Summary) + len(e.Preview)
	}
	total = total/4 + len(tv.Updates)*300
	if total < 1200 {
		total = 1200
	}
	if max <= 0 {
		max = 200000
	}
	if total > max {
		total = max
	}
	return total
}

func recentTools(transcript []uiTranscript) []uiRecentTool {
	var out []uiRecentTool
	for i := len(transcript) - 1; i >= 0 && len(out) < 5; i-- {
		e := transcript[i]
		if e.Type != "tool_use" {
			continue
		}
		out = append(out, uiRecentTool{Name: e.Tool, S: e.Input})
	}
	return out
}

func terminalSample(tv TaskView, provider string, transcript []uiTranscript, mode string) uiTerminal {
	session := "no session"
	if tv.SessionID != nil {
		session = *tv.SessionID
	}
	if strings.TrimSpace(mode) == "" {
		mode = "idle"
	}
	name := "session"
	marker := "✻"
	if provider == "codex" {
		name = "codex"
		marker = "◇"
	}
	feed := []uiTermLine{}
	if tv.WaitingOn != nil {
		feed = append(feed,
			uiTermLine{C: "approval", Text: "┌─ Flow waiting ───────────────────────────────────────────────┐"},
			uiTermLine{C: "approval", Text: "│  Waiting on: " + truncateText(*tv.WaitingOn, 45)},
			uiTermLine{C: "approval", Text: "└───────────────────────────────────────────────────────────────┘"},
		)
	}
	for _, e := range transcript {
		switch e.Type {
		case "user":
			feed = append(feed, uiTermLine{C: "user", Text: "> " + e.Text})
		case "assistant":
			feed = append(feed, uiTermLine{C: "assistant", Text: marker + " " + e.Text})
		case "thinking":
			feed = append(feed, uiTermLine{C: "thinking", Text: marker + " " + e.Text})
		case "tool_use":
			feed = append(feed, uiTermLine{C: "tool", Text: "  " + marker + " " + e.Tool + "(" + e.Input + ")"})
		case "tool_result":
			feed = append(feed, uiTermLine{C: "tool-out", Text: "    └─ " + firstNonEmpty(e.Summary, e.Preview)})
		}
		if len(feed) >= 18 {
			break
		}
	}
	if len(feed) == 0 {
		feed = append(feed, uiTermLine{C: "assistant", Text: marker + " " + tv.Name})
	}
	return uiTerminal{
		Banner: []uiTermLine{
			{C: "banner", Text: fmt.Sprintf("%s %s · %s", marker, name, tv.WorkDir)},
			{C: "banner", Text: "session: " + session},
			{C: "space", Text: ""},
		},
		Feed:    feed,
		Appends: []uiTermLine{},
		Footer: []uiTermLine{
			{C: "space", Text: ""},
			{C: "status", Text: fmt.Sprintf("%s flow · %s · %s", marker, tv.Status, tv.Slug)},
		},
		Mode:    mode,
		Message: terminalModeMessage(provider, mode),
	}
}

func terminalModeMessage(provider, mode string) string {
	if provider == "" {
		provider = agents.ProviderClaude
	}
	switch mode {
	case "browser":
		return provider + " is attached to the browser terminal"
	case "shared":
		return provider + " is attached to a shared terminal"
	case "native":
		return provider + " is attached to a native terminal"
	default:
		return provider + " transcript snapshot"
	}
}

func (s *Server) transcriptAheadOfBrowserTerminal(slug string, transcript []uiTranscript) bool {
	if s.terminals == nil || len(transcript) == 0 {
		return false
	}
	lastOutput, ok := s.terminals.lastOutputAt(slug)
	if !ok {
		return false
	}
	var latest time.Time
	for _, entry := range transcript {
		if strings.TrimSpace(entry.Time) == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, entry.Time)
		if err != nil {
			continue
		}
		if t.After(latest) {
			latest = t
		}
	}
	return !latest.IsZero() && latest.After(lastOutput.Add(2*time.Second))
}
