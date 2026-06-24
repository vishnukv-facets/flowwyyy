package inbox

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// InboxEventMeta stores derived routing metadata for a normalized source
// event. Older inbox.jsonl rows may omit it; readers treat an empty meta as
// legacy data and can reclassify from Event when needed.
//
// CalibratedConfidence and TrustedSource are the auto-permit stamp written ONLY
// by the attention forward path (AppendInboxEventStamped). They let the
// unattended wake gate decide whether to deliver untrusted bodies to a
// no-human-approval session. Both are omitempty: a legacy/connector row that was
// never stamped reads back as confidence 0 / untrusted, so the gate fails CLOSED
// (withholds) by default — the stamp can only ever open the gate, never close it.
type InboxEventMeta struct {
	Source               string  `json:"source,omitempty"`
	Actionable           bool    `json:"actionable,omitempty"`
	CalibratedConfidence float64 `json:"calibrated_confidence,omitempty"`
	TrustedSource        bool    `json:"trusted_source,omitempty"`
}

// InboxEntry is the on-disk form of one source event appended to a task's
// inbox.jsonl. The schema is deliberately small and stable: a live agent
// session reads these entries to understand what happened in a monitored
// source after the task was created.
//
// EnqueuedAt is RFC3339 wall-clock at append time, distinct from the
// Slack event's TS (which uses Slack's own seconds.microseconds format
// and is global only within a channel).
type InboxEntry struct {
	EnqueuedAt string         `json:"enqueued_at"`
	Event      InboundEvent   `json:"event"`
	Meta       InboxEventMeta `json:"meta,omitempty"`
}

// ClassifyInboxEvent derives the source and actionability used by the
// same-session monitor. Source listeners still append all useful lifecycle
// events; only actionable entries wake a live task terminal.
func ClassifyInboxEvent(ev InboundEvent) InboxEventMeta {
	source := "unknown"
	if ev.ChannelType == "github" || strings.HasPrefix(ev.Kind, "pr_") || strings.HasPrefix(ev.Kind, "issue_") {
		source = "github"
	} else if ev.ChannelType == "slack" || ev.Kind == "message" || ev.Kind == "app_mention" || ev.Kind == "reaction_added" {
		source = "slack"
	} else if ev.ChannelType == "flow" || strings.HasPrefix(ev.Kind, "flow_") {
		source = "flow"
	} else if ev.ChannelType != "" {
		source = ev.ChannelType
	}

	actionable := false
	switch source {
	case "github":
		// Every GitHub PR/issue lifecycle event wakes the live session so the
		// agent can act on it: reply to a comment, re-review on a new head,
		// proceed on approval, or wrap up on merge/close. Previously only
		// comments and head-updates were actionable, so merges, approvals, and
		// closes were recorded to the inbox but silently never woke the session
		// (the agent never learned the PR had merged/closed).
		actionable = true
	case "slack":
		switch ev.Kind {
		case "message", "app_mention", "attention_forward":
			actionable = true
		}
	case "flow":
		switch ev.Kind {
		case "flow_tell":
			actionable = true
		}
	}

	return InboxEventMeta{Source: source, Actionable: actionable}
}

func FlowTellEvent(sender, message string, at time.Time) InboundEvent {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	ts := at.UTC().Format(time.RFC3339Nano)
	return InboundEvent{
		Kind:        "flow_tell",
		Channel:     "flow-tell",
		ChannelType: "flow",
		TS:          ts,
		ThreadTS:    ts,
		UserID:      strings.TrimSpace(sender),
		Text:        strings.TrimSpace(message),
		EventKey:    "flow-tell:" + ts,
	}
}

func AppendFlowTellEvent(slug, sender, message string) error {
	return AppendInboxEvent(slug, FlowTellEvent(sender, message, time.Now().UTC()))
}

// TaskDir returns the absolute path to a task's directory under
// $FLOW_ROOT (or ~/.flow as the default). Returns "" when neither
// $FLOW_ROOT nor $HOME is resolvable, which short-circuits any
// subsequent file ops gracefully.
func TaskDir(slug string) string {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return ""
	}
	if root := strings.TrimSpace(os.Getenv("FLOW_ROOT")); root != "" {
		return filepath.Join(root, "tasks", slug)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".flow", "tasks", slug)
}

// InboxPath returns the inbox.jsonl path for the given task slug.
func InboxPath(slug string) string {
	dir := TaskDir(slug)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "inbox.jsonl")
}

// CursorPath returns the inbox.cursor path for the given task slug. The
// cursor file is a single line containing the latest Slack ts processed
// for the thread — used by the listener's bootstrap catch-up to know
// where to resume from after a restart.
func CursorPath(slug string) string {
	dir := TaskDir(slug)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "inbox.cursor")
}

// MonitorCursorPath returns the inbox.monitor.cursor path for the given task
// slug. Unlike inbox.cursor, this stores a byte offset for the same-session
// inbox monitor and must not be used by Slack catch-up code.
func MonitorCursorPath(slug string) string {
	dir := TaskDir(slug)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "inbox.monitor.cursor")
}

// AppendInboxEvent appends one event line to the task's inbox.jsonl,
// creating the file if needed. Caller is expected to have created the
// task directory (flow add task creates it). If the directory is missing
// this returns the underlying error rather than swallowing — silent
// drops would lose Slack events the user expects to see.
//
// The append is one syscall via O_APPEND so concurrent appends from
// multiple Slack events don't interleave their bytes (POSIX guarantees
// atomic writes under PIPE_BUF size, and a single JSON line of a Slack
// event is well under 4KB).
func AppendInboxEvent(slug string, ev InboundEvent) error {
	return appendInboxEntry(slug, ev, InboxEventMeta{})
}

// AppendInboxEventStamped appends like AppendInboxEvent but overlays the
// auto-permit stamp (calibrated routing confidence + trusted-source verdict)
// onto the classified meta. Used ONLY by the attention forward path so the
// unattended wake gate can decide whether to deliver this untrusted body to a
// no-human-approval session. Source/Actionable are still derived from the event.
func AppendInboxEventStamped(slug string, ev InboundEvent, calibratedConfidence float64, trustedSource bool) error {
	return appendInboxEntry(slug, ev, InboxEventMeta{CalibratedConfidence: calibratedConfidence, TrustedSource: trustedSource})
}

// appendInboxEntry is the shared core: classify the event, overlay any caller
// stamp, dedup, and append one JSONL row. stamp carries only the auto-permit
// fields (Source/Actionable always come from classification).
func appendInboxEntry(slug string, ev InboundEvent, stamp InboxEventMeta) error {
	path := InboxPath(slug)
	if path == "" {
		return errors.New("monitor: cannot resolve inbox path (no FLOW_ROOT or HOME)")
	}
	meta := ClassifyInboxEvent(ev)
	meta.CalibratedConfidence = stamp.CalibratedConfidence
	meta.TrustedSource = stamp.TrustedSource
	// Dedup Slack events by (channel, ts). The same Slack event can be
	// delivered twice over one socket when it's visible to both the bot and
	// the authorizing user (user-scoped event subscriptions overlap the
	// bot's), and reconnects/backfill can replay it. (channel, ts) uniquely
	// identifies a Slack message or reaction, so a second copy is a no-op.
	// GitHub events are exempt: Channel=repo, TS=updated_at, so two distinct
	// events on a repo can share a timestamp-second without being duplicates.
	if meta.Source == "slack" && ev.Channel != "" && ev.TS != "" {
		if inboxContainsSlackEvent(slug, ev.Channel, ev.TS) {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("monitor: mkdir task dir: %w", err)
	}
	entry := InboxEntry{
		EnqueuedAt: time.Now().UTC().Format(time.RFC3339),
		Event:      ev,
		Meta:       meta,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("monitor: marshal inbox entry: %w", err)
	}
	line = append(line, '\n')
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("monitor: open inbox.jsonl: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("monitor: append inbox.jsonl: %w", err)
	}
	return nil
}

// inboxContainsSlackEvent reports whether a Slack event with the given
// (channel, ts) is already recorded in the task's inbox. Used by
// AppendInboxEvent to make duplicate socket deliveries idempotent. A read
// error (other than a missing file, which yields an empty slice) is treated
// as "not present" so a transient read failure never blocks a real append.
func inboxContainsSlackEvent(slug, channel, ts string) bool {
	entries, err := ReadInboxEntries(slug)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Meta.Source == "slack" && e.Event.Channel == channel && e.Event.TS == ts {
			return true
		}
	}
	return false
}

// ReadInboxEntries returns all entries currently in the task's
// inbox.jsonl, in append order. Missing file → empty slice + nil error.
// Malformed lines are skipped with no error; the spawned session's
// bootstrap doesn't need to choke on a single garbled line.
func ReadInboxEntries(slug string) ([]InboxEntry, error) {
	path := InboxPath(slug)
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []InboxEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry InboxEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		out = append(out, entry)
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// ReadInboxMonitorCursor returns the byte offset processed by the
// same-session monitor, or 0 when no cursor file exists.
func ReadInboxMonitorCursor(slug string) (int64, error) {
	path := MonitorCursorPath(slug)
	if path == "" {
		return 0, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return 0, nil
	}
	offset, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("monitor: parse inbox monitor cursor: %w", err)
	}
	if offset < 0 {
		return 0, fmt.Errorf("monitor: negative inbox monitor cursor %d", offset)
	}
	return offset, nil
}

// WriteInboxMonitorCursor atomically stores the byte offset processed by the
// same-session monitor.
func WriteInboxMonitorCursor(slug string, offset int64) error {
	if offset < 0 {
		return fmt.Errorf("monitor: negative inbox monitor cursor %d", offset)
	}
	path := MonitorCursorPath(slug)
	if path == "" {
		return errors.New("monitor: cannot resolve inbox monitor cursor path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("monitor: mkdir task dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(offset, 10)+"\n"), 0o644); err != nil {
		return fmt.Errorf("monitor: write inbox monitor cursor.tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("monitor: rename inbox monitor cursor: %w", err)
	}
	return nil
}

// SeedInboxMonitorCursorToEnd initializes a task's monitor cursor to the
// current end of its inbox.jsonl, but ONLY when no cursor exists yet. This
// makes a freshly-started monitor watch for events that arrive after it starts
// rather than replaying the entire historical backlog — without it, restoring
// monitors on boot would wake or respawn the agent for every old event. A task
// whose cursor already exists resumes from its saved position untouched.
func SeedInboxMonitorCursorToEnd(slug string) error {
	cursorPath := MonitorCursorPath(slug)
	if cursorPath == "" {
		return nil
	}
	if _, err := os.Stat(cursorPath); err == nil {
		return nil // cursor exists — resume from where we left off
	} else if !os.IsNotExist(err) {
		return err
	}
	size := int64(0)
	if info, err := os.Stat(InboxPath(slug)); err == nil {
		size = info.Size()
	} else if !os.IsNotExist(err) {
		return err
	}
	return WriteInboxMonitorCursor(slug, size)
}

// ReadInboxCursor returns the latest Slack ts processed for the task's
// thread, or "" when no cursor file exists. Used by the listener's
// catch-up sweep on startup to know where to resume.
func ReadInboxCursor(slug string) (string, error) {
	path := CursorPath(slug)
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// WriteInboxCursor atomically replaces the cursor file with ts. Uses
// write-to-temp + rename so a crash mid-write leaves the prior cursor
// intact rather than corrupting it.
func WriteInboxCursor(slug, ts string) error {
	path := CursorPath(slug)
	if path == "" {
		return errors.New("monitor: cannot resolve cursor path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("monitor: mkdir task dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.TrimSpace(ts)+"\n"), 0o644); err != nil {
		return fmt.Errorf("monitor: write cursor.tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("monitor: rename cursor: %w", err)
	}
	return nil
}
