package flowdb

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type WorkEventLogEntry struct {
	EventID        string
	EventType      string
	OccurredAt     string
	CreatedAt      string
	Provider       string
	SessionID      string
	TaskSlug       string
	ChatSlug       string
	ProjectSlug    string
	WorkContextID  string
	ActorKind      string
	ActorID        string
	SourceAnchorID string
	Source         string
	ExternalID     string
	ExternalURL    string
	MetadataJSON   string
}

type WorkEventLogFilter struct {
	EventType     string
	TaskSlug      string
	ChatSlug      string
	ProjectSlug   string
	WorkContextID string
	Limit         int
}

const workEventLogCols = `
	event_id, event_type, occurred_at, created_at, COALESCE(provider, ''), COALESCE(session_id, ''),
	COALESCE(task_slug, ''), COALESCE(chat_slug, ''), COALESCE(project_slug, ''), COALESCE(work_context_id, ''),
	COALESCE(actor_kind, ''), COALESCE(actor_id, ''), COALESCE(source_anchor_id, ''), COALESCE(source, ''),
	COALESCE(external_id, ''), COALESCE(external_url, ''), metadata_json
`

func AppendWorkEventLog(db *sql.DB, e WorkEventLogEntry) (WorkEventLogEntry, bool, error) {
	if db == nil {
		return WorkEventLogEntry{}, false, fmt.Errorf("flowdb: append work event log requires db")
	}
	e = normalizeWorkEventLogEntry(e)
	if e.EventID == "" {
		e.EventID = randomWorkEventID()
	}
	if e.EventType == "" {
		return WorkEventLogEntry{}, false, fmt.Errorf("flowdb: work event type is required")
	}
	if e.OccurredAt == "" {
		e.OccurredAt = NowISO()
	}
	if e.CreatedAt == "" {
		e.CreatedAt = NowISO()
	}
	metadata, err := compactWorkEventMetadata(e.MetadataJSON)
	if err != nil {
		return WorkEventLogEntry{}, false, err
	}
	e.MetadataJSON = metadata

	res, err := db.Exec(`
		INSERT OR IGNORE INTO work_event_log (
			event_id, event_type, occurred_at, created_at, provider, session_id,
			task_slug, chat_slug, project_slug, work_context_id, actor_kind, actor_id,
			source_anchor_id, source, external_id, external_url, metadata_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.EventID, e.EventType, e.OccurredAt, e.CreatedAt, NullIfEmpty(e.Provider), NullIfEmpty(e.SessionID),
		NullIfEmpty(e.TaskSlug), NullIfEmpty(e.ChatSlug), NullIfEmpty(e.ProjectSlug), NullIfEmpty(e.WorkContextID),
		NullIfEmpty(e.ActorKind), NullIfEmpty(e.ActorID), NullIfEmpty(e.SourceAnchorID), NullIfEmpty(e.Source),
		NullIfEmpty(e.ExternalID), NullIfEmpty(e.ExternalURL), e.MetadataJSON,
	)
	if err != nil {
		return WorkEventLogEntry{}, false, fmt.Errorf("flowdb: append work event log %q: %w", e.EventID, err)
	}
	inserted, _ := res.RowsAffected()
	got, err := GetWorkEventLog(db, e.EventID)
	if err != nil {
		return WorkEventLogEntry{}, false, err
	}
	return got, inserted > 0, nil
}

func GetWorkEventLog(db *sql.DB, eventID string) (WorkEventLogEntry, error) {
	row := db.QueryRow(`SELECT `+workEventLogCols+` FROM work_event_log WHERE event_id = ?`, strings.TrimSpace(eventID))
	return scanWorkEventLog(row)
}

func ListWorkEventLog(db *sql.DB, f WorkEventLogFilter) ([]WorkEventLogEntry, error) {
	if db == nil {
		return nil, fmt.Errorf("flowdb: list work event log requires db")
	}
	clauses := []string{"1=1"}
	var args []any
	add := func(col, val string) {
		val = strings.TrimSpace(val)
		if val == "" {
			return
		}
		clauses = append(clauses, col+" = ?")
		args = append(args, val)
	}
	add("event_type", f.EventType)
	add("task_slug", f.TaskSlug)
	add("chat_slug", f.ChatSlug)
	add("project_slug", f.ProjectSlug)
	add("work_context_id", f.WorkContextID)

	q := `SELECT ` + workEventLogCols + ` FROM work_event_log WHERE ` + strings.Join(clauses, " AND ") + ` ORDER BY occurred_at DESC, event_id DESC`
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("flowdb: list work event log: %w", err)
	}
	defer rows.Close()

	var out []WorkEventLogEntry
	for rows.Next() {
		e, err := scanWorkEventLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanWorkEventLog(row interface{ Scan(dest ...any) error }) (WorkEventLogEntry, error) {
	var e WorkEventLogEntry
	err := row.Scan(
		&e.EventID, &e.EventType, &e.OccurredAt, &e.CreatedAt, &e.Provider, &e.SessionID,
		&e.TaskSlug, &e.ChatSlug, &e.ProjectSlug, &e.WorkContextID, &e.ActorKind, &e.ActorID,
		&e.SourceAnchorID, &e.Source, &e.ExternalID, &e.ExternalURL, &e.MetadataJSON,
	)
	if err != nil {
		return WorkEventLogEntry{}, err
	}
	return e, nil
}

func normalizeWorkEventLogEntry(e WorkEventLogEntry) WorkEventLogEntry {
	e.EventID = strings.TrimSpace(e.EventID)
	e.EventType = strings.TrimSpace(strings.ToLower(e.EventType))
	e.Provider = strings.TrimSpace(strings.ToLower(e.Provider))
	e.SessionID = strings.TrimSpace(e.SessionID)
	e.TaskSlug = strings.TrimSpace(e.TaskSlug)
	e.ChatSlug = strings.TrimSpace(e.ChatSlug)
	e.ProjectSlug = strings.TrimSpace(e.ProjectSlug)
	e.WorkContextID = strings.TrimSpace(e.WorkContextID)
	e.ActorKind = strings.TrimSpace(strings.ToLower(e.ActorKind))
	e.ActorID = strings.TrimSpace(e.ActorID)
	e.SourceAnchorID = strings.TrimSpace(e.SourceAnchorID)
	e.Source = strings.TrimSpace(strings.ToLower(e.Source))
	e.ExternalID = strings.TrimSpace(e.ExternalID)
	e.ExternalURL = strings.TrimSpace(e.ExternalURL)
	e.OccurredAt = strings.TrimSpace(e.OccurredAt)
	e.CreatedAt = strings.TrimSpace(e.CreatedAt)
	e.MetadataJSON = strings.TrimSpace(e.MetadataJSON)
	return e
}

func compactWorkEventMetadata(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}", nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(raw)); err != nil {
		return "", fmt.Errorf("flowdb: compact work event metadata: %w", err)
	}
	return buf.String(), nil
}

func randomWorkEventID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "wev-" + hex.EncodeToString(b[:])
}
