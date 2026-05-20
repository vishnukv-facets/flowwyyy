package monitor

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"flow/internal/flowdb"
)

// SlackOrigin is the addressing tuple flow needs to write back into the
// originating Slack conversation. It is derived at read time from the
// monitor_event_actions / monitor_events rows produced by Slack Socket Mode
// ingest — no dedicated column lives on the tasks row.
//
// Channel is always populated when ok=true. ThreadTS is empty when the
// originating message was top-level rather than a thread reply; callers
// posting back should fall back to TS as the thread parent in that case
// (chat.postMessage with thread_ts=TS turns the original message into a
// thread root). Permalink is best-effort and may be empty if chat.getPermalink
// failed at ingest time.
type SlackOrigin struct {
	Channel   string
	TS        string
	ThreadTS  string
	UserID    string
	Permalink string
}

// PostTarget returns the (channel, thread_ts) pair to pass to chat.postMessage
// so a reply lands in the same conversation. For top-level messages this
// promotes the original TS to the thread parent, keeping the conversation
// threaded rather than scattering replies across the channel.
func (o SlackOrigin) PostTarget() (channel, threadTS string) {
	channel = o.Channel
	threadTS = o.ThreadTS
	if threadTS == "" {
		threadTS = o.TS
	}
	return channel, threadTS
}

// SlackOriginFor resolves the Slack addressing tuple for a task whose origin
// was a Slack monitor event. Returns ok=false (with nil err) when:
//   - the task has no monitor_event_actions row, or
//   - the linked event's source is not "slack", or
//   - the row exists but the channel/ts pair cannot be recovered.
//
// A non-nil err is reserved for actual SQL or JSON failures, not for the
// expected "not a Slack-origin task" path — write triggers call this on every
// task and need the not-applicable case to be cheap and silent.
func SlackOriginFor(db *sql.DB, taskSlug string) (SlackOrigin, bool, error) {
	taskSlug = strings.TrimSpace(taskSlug)
	if db == nil || taskSlug == "" {
		return SlackOrigin{}, false, nil
	}
	row := db.QueryRow(
		`SELECT e.source, e.source_id, e.url, e.raw_json
		   FROM monitor_event_actions a
		   JOIN monitor_events e ON e.id = a.event_id
		  WHERE a.task_slug = ?
		  ORDER BY a.created_at DESC
		  LIMIT 1`,
		taskSlug,
	)
	var (
		source   string
		sourceID string
		url      sql.NullString
		rawJSON  sql.NullString
	)
	if err := row.Scan(&source, &sourceID, &url, &rawJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SlackOrigin{}, false, nil
		}
		return SlackOrigin{}, false, fmt.Errorf("slack origin for %q: %w", taskSlug, err)
	}
	if !strings.EqualFold(source, "slack") {
		return SlackOrigin{}, false, nil
	}
	origin := SlackOrigin{Permalink: url.String}
	origin.Channel, origin.TS = parseSlackSourceID(sourceID)
	if rawJSON.Valid && rawJSON.String != "" {
		applyRawJSONToOrigin(rawJSON.String, &origin)
	}
	if origin.Channel == "" || origin.TS == "" {
		return SlackOrigin{}, false, nil
	}
	return origin, true, nil
}

// parseSlackSourceID splits the "channel:ts" composite key that
// Slack Socket Mode ingest writes into monitor_events.source_id. Slack timestamps
// always contain a dot (seconds.microseconds), so a single rsplit on ":"
// is unambiguous even if a future channel id ever contained one.
func parseSlackSourceID(sourceID string) (channel, ts string) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return "", ""
	}
	idx := strings.LastIndex(sourceID, ":")
	if idx <= 0 || idx == len(sourceID)-1 {
		return "", ""
	}
	return sourceID[:idx], sourceID[idx+1:]
}

// applyRawJSONToOrigin fills in ThreadTS (and refines Channel/TS if the
// composite source_id was malformed) from the raw_json payload that
// Socket Mode ingest persists this schema:
//
//	{"event": {"channel": "...", "ts": "...", "thread_ts": "..."}}
//
// Older rows from the removed Slack polling path used this schema:
//
//	{"conversation": {"id": "...", "name": "..."},
//	 "message":      {"ts": "...", "thread_ts": "..."}}
//
// Best-effort: malformed JSON or missing fields leave the origin unchanged
// rather than erroring, so a partially-recoverable row can still post back
// via source_id-derived addressing.
func applyRawJSONToOrigin(raw string, origin *SlackOrigin) {
	var payload struct {
		Conversation struct {
			ID string `json:"id"`
		} `json:"conversation"`
		Message struct {
			TS       string `json:"ts"`
			ThreadTS string `json:"thread_ts"`
			UserID   string `json:"user"`
		} `json:"message"`
		Event struct {
			Channel  string `json:"channel"`
			TS       string `json:"ts"`
			ThreadTS string `json:"thread_ts"`
			UserID   string `json:"user"`
		} `json:"event"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return
	}
	if origin.Channel == "" && payload.Event.Channel != "" {
		origin.Channel = payload.Event.Channel
	}
	if origin.TS == "" && payload.Event.TS != "" {
		origin.TS = payload.Event.TS
	}
	if payload.Event.ThreadTS != "" {
		origin.ThreadTS = strings.TrimSpace(payload.Event.ThreadTS)
	}
	if origin.UserID == "" && payload.Event.UserID != "" {
		origin.UserID = strings.TrimSpace(payload.Event.UserID)
	}
	if origin.Channel == "" && payload.Conversation.ID != "" {
		origin.Channel = payload.Conversation.ID
	}
	if origin.TS == "" && payload.Message.TS != "" {
		origin.TS = payload.Message.TS
	}
	if payload.Message.ThreadTS != "" {
		origin.ThreadTS = strings.TrimSpace(payload.Message.ThreadTS)
	}
	if origin.UserID == "" && payload.Message.UserID != "" {
		origin.UserID = strings.TrimSpace(payload.Message.UserID)
	}
}

// SlackOriginFromEvent derives the addressing tuple directly from a
// monitor_events row, without joining through monitor_event_actions.
//
// Use this when you're about to RECORD an action (e.g. mid-routing in
// `createAgentTaskForMonitorEvent`) and need to react/reply in-thread
// before the action row exists. SlackOriginFor would return ok=false
// at that moment because the join target hasn't been written yet.
//
// Returns ok=false (with nil err) when event.Source != "slack" or when
// the source_id / raw_json don't yield a usable (channel, ts) pair.
// Mirrors SlackOriginFor's "expected not-applicable returns silently"
// shape so callers can use `if origin, ok, _ := ...; !ok { return }`.
func SlackOriginFromEvent(event flowdb.MonitorEvent) (SlackOrigin, bool) {
	if !strings.EqualFold(event.Source, "slack") {
		return SlackOrigin{}, false
	}
	origin := SlackOrigin{Permalink: event.URL.String}
	origin.Channel, origin.TS = parseSlackSourceID(event.SourceID)
	if event.RawJSON.Valid && event.RawJSON.String != "" {
		applyRawJSONToOrigin(event.RawJSON.String, &origin)
	}
	if origin.Channel == "" || origin.TS == "" {
		return SlackOrigin{}, false
	}
	return origin, true
}
