package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"flow/internal/monitor"
	"flow/internal/productdb"
)

type inboxNotifyRequest struct {
	TaskSlug      string `json:"task_slug"`
	Sender        string `json:"sender"`
	Message       string `json:"message"`
	Preview       string `json:"preview"`
	JSONLAppended bool   `json:"jsonl_appended"`
}

func (s *Server) handleInboxNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req inboxNotifyRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid inbox notify payload", http.StatusBadRequest)
		return
	}
	slug := strings.TrimSpace(req.TaskSlug)
	if slug == "" {
		http.Error(w, "task_slug is required", http.StatusBadRequest)
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		msg = strings.TrimSpace(req.Preview)
	}
	s.publishUIChange("inbox")
	if req.JSONLAppended {
		if err := s.wakeTaskForInboxJSONL(r.Context(), slug, req.Sender, msg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	if msg != "" {
		if err := s.wakeTaskForInboxNotify(slug, formatTellWakePrompt(slug, req.Sender, msg)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func formatTellWakePrompt(slug, sender, message string) string {
	sender = strings.TrimSpace(sender)
	if sender == "" {
		sender = "user"
	}
	message = strings.TrimSpace(message)
	var b strings.Builder
	fmt.Fprintf(&b, "Flow task %s has a new inbox.md message from %s.\n", slug, sender)
	b.WriteString("Read the latest task inbox entry and continue in this same session.\n")
	if message != "" {
		b.WriteString("\nDelivered message:\n")
		b.WriteString(message)
	}
	return strings.TrimSpace(b.String())
}

func (s *Server) wakeTaskForInboxJSONL(ctx context.Context, slug, sender, message string) error {
	if s == nil {
		return nil
	}
	cursorPath := monitor.MonitorCursorPath(slug)
	if cursorPath != "" {
		if _, err := os.Stat(cursorPath); err == nil {
			return monitor.NewInboxMonitor(slug, inboxWakeTarget{server: s}, monitor.InboxMonitorOptions{}).ScanOnce(ctx)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	entry, ok, err := latestActionableInboxEntry(slug)
	if err != nil {
		return err
	}
	if !ok {
		entry = monitor.InboxEntry{
			EnqueuedAt: time.Now().UTC().Format(time.RFC3339),
			Event:      monitor.FlowTellEvent(sender, message, time.Now().UTC()),
			Meta:       monitor.InboxEventMeta{Source: "flow", Actionable: true},
		}
	}
	if err := s.deliverInboxEvents(slug, []monitor.InboxEntry{entry}); err != nil {
		return err
	}
	return markInboxMonitorCursorAtEnd(slug)
}

func latestActionableInboxEntry(slug string) (monitor.InboxEntry, bool, error) {
	entries, err := monitor.ReadInboxEntries(slug)
	if err != nil {
		return monitor.InboxEntry{}, false, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		meta := entry.Meta
		if meta.Source == "" {
			meta = monitor.ClassifyInboxEvent(entry.Event)
			entry.Meta = meta
		}
		if meta.Actionable {
			return entry, true, nil
		}
	}
	return monitor.InboxEntry{}, false, nil
}

func markInboxMonitorCursorAtEnd(slug string) error {
	path := monitor.InboxPath(slug)
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return monitor.WriteInboxMonitorCursor(slug, info.Size())
}

func (s *Server) wakeTaskForInboxNotify(slug, prompt string) error {
	if s == nil || s.terminals == nil {
		return nil
	}
	if s.terminals.running(slug) {
		return s.terminals.wakeTask(slug, prompt)
	}
	if s.terminals.wakeSharedTask(slug, prompt) {
		return nil
	}
	if s.cfg.DB == nil {
		return nil
	}
	task, err := productdb.GetTask(s.cfg.DB, slug)
	if err != nil {
		return nil
	}
	if s.taskAgentProcessLive(task) {
		return nil
	}
	if task.Status != "backlog" && task.Status != "in-progress" {
		return nil
	}
	provider := task.SessionProvider
	if provider == "" {
		provider = "claude"
	}
	if err := s.ensureProviderAvailable(provider); err != nil {
		return nil
	}
	if !s.respawn.allow(slug) {
		return nil
	}
	return s.terminals.wakeTask(slug, prompt)
}
