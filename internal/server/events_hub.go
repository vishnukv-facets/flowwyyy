package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"flow/internal/flowdb"
)

// eventHub is the in-process pub/sub fan-out for live events that the UI
// (or any external subscriber) wants to react to in real time. Subscribers
// connect via the /ws/events WebSocket; the hub pushes ndjson-shaped
// envelopes onto each subscriber's buffered channel and drops events for
// any subscriber that's too slow to drain.
//
// This intentionally lives in-process and uses Go channels rather than a
// real broker. flow already has exactly one server per host; there's no
// horizontal scale to plan for, and an external broker would just add an
// operational dependency without buying anything.
type eventHub struct {
	mu          sync.RWMutex
	subscribers map[*eventSubscriber]struct{}
}

type eventSubscriber struct {
	id        string
	filter    eventFilter
	send      chan eventEnvelope
	done      chan struct{}
	closeOnce sync.Once
}

// eventFilter narrows what each subscriber sees. Empty fields match
// everything. Both filters are AND'd: a subscriber that requests
// session="X" AND types=[hook_event] only sees hook events for that
// session.
type eventFilter struct {
	SessionID string
	TaskSlug  string
	Types     []string // event envelope Type values to include
}

// eventEnvelope is the wire shape pushed to subscribers. Type is the
// stable enum the UI switches on; SessionID/TaskSlug let clients filter
// efficiently without parsing Data. Data is whatever payload the
// publisher chose to attach (typically the hook response or a
// liveness/reconciler snapshot).
type eventEnvelope struct {
	Type        string            `json:"type"`
	Timestamp   string            `json:"timestamp"`
	SessionID   string            `json:"session_id,omitempty"`
	TaskSlug    string            `json:"task_slug,omitempty"`
	Seq         int64             `json:"seq,omitempty"`
	Data        json.RawMessage   `json:"data,omitempty"`
	HookEvent   *eventHookData    `json:"hook,omitempty"`
	Liveness    *eventLiveness    `json:"liveness,omitempty"`
	Runtime     *eventRuntime     `json:"runtime,omitempty"`
	HookHealth  *uiHookHealth     `json:"hook_health,omitempty"`
	MonitorSync *eventMonitorSync `json:"monitor_sync,omitempty"`
	InboxItem   *eventInboxItem   `json:"inbox_item,omitempty"`
}

type eventHookData struct {
	Provider     string `json:"provider,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Event        string `json:"event,omitempty"`
	SubagentID   string `json:"subagent_id,omitempty"`
	HookVersion  int    `json:"hook_version,omitempty"`
	HookOwned    bool   `json:"hook_owned,omitempty"`
	HookOutdated bool   `json:"hook_outdated,omitempty"`
}

type eventLiveness struct {
	Provider string `json:"provider"`
	Slug     string `json:"slug,omitempty"`
	Status   string `json:"status"` // "alive" | "dead"
	Reason   string `json:"reason,omitempty"`
}

type eventRuntime struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Kind     string `json:"kind,omitempty"`
}

// eventMonitorSync mirrors a row of monitor_sync_state on the wire. Pushed
// to /ws/events?types=monitor_sync subscribers when a poll starts (is_syncing
// flips true) or ends (status + last_sync_at update). The Inbox UI uses
// this to drive the per-source "syncing now…" / "synced 23s ago" badge
// without polling the API.
type eventMonitorSync struct {
	Source     string `json:"source"`
	IsSyncing  bool   `json:"is_syncing"`
	LastStatus string `json:"last_status"`
	LastSyncAt string `json:"last_sync_at,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

// eventInboxItem is published when a new external-source event (slack /
// github) lands as a fresh row in monitor_events. The UI uses it to (a)
// add the item to the inbox list in real time without re-fetching
// /api/ui-data, and (b) decide whether to fire a desktop notification
// when NeedsReview is true. Re-polls of the same item never publish —
// only the genuine-isNew transition fires this.
//
// NeedsReview captures whether the item should pull the user's attention
// (notification level=approval, ping outcome, secret/write/reply note). It's
// computed server-side so the client can fire the desktop notification before
// the next /api/ui-data refresh hydrates the durable inbox row.
type eventInboxItem struct {
	EventID     string `json:"event_id"`
	Source      string `json:"source"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Body        string `json:"body,omitempty"`
	URL         string `json:"url,omitempty"`
	Severity    string `json:"severity"`
	Level       string `json:"level,omitempty"`
	NeedsReview bool   `json:"needs_review"`
	Outcome     string `json:"outcome,omitempty"`
}

func newEventHub() *eventHub {
	return &eventHub{
		subscribers: map[*eventSubscriber]struct{}{},
	}
}

func (h *eventHub) subscribe(filter eventFilter) *eventSubscriber {
	sub := &eventSubscriber{
		id:     subscriberID(),
		filter: filter,
		// Buffered channel deliberately small: live events are bursty and
		// we'd rather drop than block the publisher. 64 covers a typical
		// PreToolUse → PostToolUse storm without grief.
		send: make(chan eventEnvelope, 64),
		done: make(chan struct{}),
	}
	h.mu.Lock()
	h.subscribers[sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

func (h *eventHub) unsubscribe(sub *eventSubscriber) {
	sub.closeOnce.Do(func() {
		close(sub.done)
		close(sub.send)
	})
	h.mu.Lock()
	delete(h.subscribers, sub)
	h.mu.Unlock()
}

func (h *eventHub) publish(env eventEnvelope) {
	if env.Timestamp == "" {
		env.Timestamp = flowdb.NowISO()
	}
	h.mu.RLock()
	subs := make([]*eventSubscriber, 0, len(h.subscribers))
	for s := range h.subscribers {
		subs = append(subs, s)
	}
	h.mu.RUnlock()
	for _, sub := range subs {
		if !sub.filter.matches(env) {
			continue
		}
		select {
		case <-sub.done:
		case sub.send <- env:
		default:
			// Subscriber is back-pressured. Drop rather than block so
			// that one slow client can't stall ingestAgentHook for
			// everyone else. The UI should reconnect and re-sync from
			// the DB if it notices a gap.
		}
	}
}

func (f eventFilter) matches(env eventEnvelope) bool {
	if f.SessionID != "" && env.SessionID != f.SessionID {
		return false
	}
	if f.TaskSlug != "" && env.TaskSlug != f.TaskSlug {
		return false
	}
	if len(f.Types) == 0 {
		return true
	}
	for _, t := range f.Types {
		if t == env.Type {
			return true
		}
	}
	return false
}

// publishHookEvent fans out a hook-ingest response to the event hub so
// subscribers see the same lifecycle the DB just recorded. It is safe to
// call before the hub exists (no-op). The payload parameter is reserved
// for future extension (per-event extras like tool_name) — accepted now
// so callers don't churn when extras land.
func (s *Server) publishHookEvent(resp agentHookIngestResponse, _ map[string]any) {
	if s.events == nil {
		return
	}
	s.events.publish(eventEnvelope{
		Type:      "agent_hook",
		SessionID: resp.SessionID,
		TaskSlug:  resp.Task,
		Seq:       resp.Seq,
		HookEvent: &eventHookData{
			Provider:     resp.Provider,
			Kind:         resp.Kind,
			Event:        resp.Event,
			SubagentID:   resp.SubagentID,
			HookVersion:  resp.HookVersion,
			HookOwned:    resp.HookOwned,
			HookOutdated: resp.HookOutdated,
		},
	})
}

// publishInboxItem fans out a freshly-arrived external-source monitor
// event so the Inbox UI can add the row without re-fetching /api/ui-data,
// AND so the client can fire a desktop notification when needs_review is
// true. Only "genuinely new" events fire this — re-polls of an existing
// (source, source_id) pair are silent. The classifier matches the
// frontend's monitorItemNeedsReview so backend / frontend agree on what
// pulls user attention.
func (s *Server) publishInboxItem(event flowdb.MonitorEvent, outcome string, note string) {
	if s == nil || s.events == nil {
		return
	}
	level := event.Severity
	if notification, err := flowdb.GetMonitorNotificationForEvent(s.cfg.DB, event.ID); err == nil {
		level = notification.Level
	}
	item := &eventInboxItem{
		EventID:     event.ID,
		Source:      event.Source,
		Kind:        event.Kind,
		Title:       event.Title,
		Severity:    event.Severity,
		Level:       level,
		Outcome:     outcome,
		NeedsReview: inboxNeedsReview(level, event.Severity, outcome, note),
	}
	if event.Body.Valid {
		item.Body = event.Body.String
	}
	if event.URL.Valid {
		item.URL = event.URL.String
	}
	s.events.publish(eventEnvelope{
		Type:      "inbox_item",
		InboxItem: item,
	})
}

// inboxNeedsReview decides whether an inbox event should pull the user's
// attention via a desktop notification. Mirrors the frontend's
// monitorItemNeedsReview helper so both sides agree.
//
//	level=approval → always needs review (approval-mode Slack/GitHub item)
//	severity=high   → always needs review (CI failed, security alert)
//	outcome=ping    → the routing layer explicitly flagged "user attention"
//	note mentions   → secret/write/reply/push/merge — words that imply a
//	                  side-effect the user should consent to
func inboxNeedsReview(level, severity, outcome, note string) bool {
	if strings.EqualFold(level, "approval") {
		return true
	}
	if strings.EqualFold(severity, "high") {
		return true
	}
	if strings.EqualFold(outcome, "ping") {
		return true
	}
	n := strings.ToLower(note)
	for _, kw := range []string{"approval", "secret", "write", "reply", "push", "merge"} {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
}

// publishMonitorSync fans out a monitor-sync transition (start/end) so
// the Inbox UI can update the per-source "syncing now…" / "synced X ago"
// badge in real time. Safe to call before the hub exists (no-op). state
// must be non-nil; pass the row returned from RecordMonitorSyncStart /
// RecordMonitorSyncEnd directly — those functions already round-trip
// through GetMonitorSyncState so the emitted shape matches what
// /api/monitor/sync-state would return.
func (s *Server) publishMonitorSync(state *flowdb.MonitorSyncState) {
	if s == nil || s.events == nil || state == nil {
		return
	}
	env := eventEnvelope{
		Type: "monitor_sync",
		MonitorSync: &eventMonitorSync{
			Source:     state.Source,
			IsSyncing:  state.IsSyncing,
			LastStatus: state.LastStatus,
		},
	}
	if state.LastSyncAt.Valid {
		env.MonitorSync.LastSyncAt = state.LastSyncAt.String
	}
	if state.LastError.Valid {
		env.MonitorSync.LastError = state.LastError.String
	}
	s.events.publish(env)
}

// publishLiveness fans out a liveness reconciler observation. Slug is
// empty when no task is bound to the session.
func (s *Server) publishLiveness(provider, sessionID, slug, status, reason string) {
	if s.events == nil {
		return
	}
	s.events.publish(eventEnvelope{
		Type:      "liveness",
		SessionID: sessionID,
		TaskSlug:  slug,
		Liveness: &eventLiveness{
			Provider: provider,
			Slug:     slug,
			Status:   status,
			Reason:   reason,
		},
	})
}

// handleEventWebSocket upgrades the connection, accepts an optional
// subscribe message that narrows the filter, then pumps envelopes until
// either side closes. Newline-delimited JSON over the WS frame stream
// keeps the wire format trivial to parse with browser EventSource-style
// clients.
func (s *Server) handleEventWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.events == nil {
		http.Error(w, "events hub unavailable", http.StatusServiceUnavailable)
		return
	}
	filter := eventFilterFromQuery(r)
	conn, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	sub := s.events.subscribe(filter)
	defer s.events.unsubscribe(sub)

	// Send an ack so the client knows the subscription took effect and
	// has the assigned subscriber id (useful in logs when debugging).
	_ = conn.WriteJSON(map[string]any{
		"type":           "subscribed",
		"subscriber_id":  sub.id,
		"server_version": s.cfg.Version,
		"hook_version":   CurrentHookVersion,
	})

	// Send-loop: drain envelopes; bail when the channel closes or the
	// peer goes away.
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	readCh := make(chan error, 1)
	go func() {
		// Drain any incoming messages so the connection's read buffer
		// stays unblocked; we don't currently accept subscription
		// updates over the wire, but ignoring reads would let TCP
		// back-pressure block the WS heartbeat.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				readCh <- err
				return
			}
		}
	}()

	for {
		select {
		case env, ok := <-sub.send:
			if !ok {
				return
			}
			if err := conn.WriteJSON(env); err != nil {
				return
			}
		case <-pingTicker.C:
			if err := conn.WriteJSON(map[string]any{"type": "ping", "ts": flowdb.NowISO()}); err != nil {
				return
			}
		case <-readCh:
			return
		case <-sub.done:
			return
		}
	}
}

func eventFilterFromQuery(r *http.Request) eventFilter {
	q := r.URL.Query()
	filter := eventFilter{
		SessionID: strings.TrimSpace(q.Get("session_id")),
		TaskSlug:  strings.TrimSpace(q.Get("task_slug")),
	}
	if types := strings.TrimSpace(q.Get("types")); types != "" {
		for _, t := range strings.Split(types, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				filter.Types = append(filter.Types, t)
			}
		}
	}
	return filter
}

var subscriberSeq int64

func subscriberID() string {
	subscriberMu.Lock()
	subscriberSeq++
	id := subscriberSeq
	subscriberMu.Unlock()
	return "sub-" + strconvFormatInt(id)
}

var subscriberMu sync.Mutex

// strconvFormatInt is a tiny shim to avoid a strconv import bloat in this
// file; the caller's allocation pattern matters less than keeping the
// hot ingest path lean.
func strconvFormatInt(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
