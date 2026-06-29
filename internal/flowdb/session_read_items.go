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

type SessionReadItem struct {
	ID               string
	Kind             string
	Status           string
	Text             string
	CreatedAt        string
	UpdatedAt        string
	ReadAt           string
	AnsweredAt       string
	Provider         string
	SessionID        string
	TaskSlug         string
	ChatSlug         string
	ProjectSlug      string
	WorkContextID    string
	DedupeKey        string
	DependenciesJSON string
	MetadataJSON     string
}

type SessionReadItemFilter struct {
	Status        string
	Kind          string
	TaskSlug      string
	ChatSlug      string
	WorkContextID string
	Limit         int
}

const sessionReadItemCols = `
	id, kind, status, text, created_at, updated_at, COALESCE(read_at, ''), COALESCE(answered_at, ''),
	COALESCE(provider, ''), COALESCE(session_id, ''), COALESCE(task_slug, ''), COALESCE(chat_slug, ''),
	COALESCE(project_slug, ''), COALESCE(work_context_id, ''), COALESCE(dedupe_key, ''),
	dependencies_json, metadata_json
`

func AppendSessionReadItem(db *sql.DB, item SessionReadItem) (SessionReadItem, bool, error) {
	if db == nil {
		return SessionReadItem{}, false, fmt.Errorf("flowdb: append session read item requires db")
	}
	item = normalizeSessionReadItem(item)
	if item.ID == "" {
		item.ID = randomSessionReadItemID()
	}
	if item.Kind != "ask" && item.Kind != "say" {
		return SessionReadItem{}, false, fmt.Errorf("flowdb: session read kind must be ask or say")
	}
	if item.Text == "" {
		return SessionReadItem{}, false, fmt.Errorf("flowdb: session read text is required")
	}
	if item.Status == "" {
		if item.Kind == "ask" {
			item.Status = "pending"
		} else {
			item.Status = "unread"
		}
	}
	if !validSessionReadStatus(item.Status) {
		return SessionReadItem{}, false, fmt.Errorf("flowdb: invalid session read status %q", item.Status)
	}
	if item.CreatedAt == "" {
		item.CreatedAt = NowISO()
	}
	if item.UpdatedAt == "" {
		item.UpdatedAt = item.CreatedAt
	}
	deps, err := compactJSONDefault(item.DependenciesJSON, "[]")
	if err != nil {
		return SessionReadItem{}, false, fmt.Errorf("flowdb: compact dependencies json: %w", err)
	}
	meta, err := compactJSONDefault(item.MetadataJSON, "{}")
	if err != nil {
		return SessionReadItem{}, false, fmt.Errorf("flowdb: compact read metadata json: %w", err)
	}
	item.DependenciesJSON = deps
	item.MetadataJSON = meta

	res, err := db.Exec(`
		INSERT OR IGNORE INTO session_read_items (
			id, kind, status, text, created_at, updated_at, read_at, answered_at,
			provider, session_id, task_slug, chat_slug, project_slug, work_context_id,
			dedupe_key, dependencies_json, metadata_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.Kind, item.Status, item.Text, item.CreatedAt, item.UpdatedAt,
		nullStringOrTrimmed(item.ReadAt), nullStringOrTrimmed(item.AnsweredAt),
		nullStringOrTrimmed(item.Provider), nullStringOrTrimmed(item.SessionID),
		nullStringOrTrimmed(item.TaskSlug), nullStringOrTrimmed(item.ChatSlug),
		nullStringOrTrimmed(item.ProjectSlug), nullStringOrTrimmed(item.WorkContextID),
		nullStringOrTrimmed(item.DedupeKey), item.DependenciesJSON, item.MetadataJSON,
	)
	if err != nil {
		return SessionReadItem{}, false, fmt.Errorf("flowdb: append session read item %q: %w", item.ID, err)
	}
	inserted, _ := res.RowsAffected()
	if inserted > 0 {
		got, err := GetSessionReadItem(db, item.ID)
		return got, true, err
	}
	if item.DedupeKey == "" {
		got, err := GetSessionReadItem(db, item.ID)
		return got, false, err
	}
	got, err := sessionReadItemByDedupeKey(db, item.DedupeKey)
	return got, false, err
}

func GetSessionReadItem(db *sql.DB, id string) (SessionReadItem, error) {
	row := db.QueryRow(`SELECT `+sessionReadItemCols+` FROM session_read_items WHERE id = ?`, strings.TrimSpace(id))
	return scanSessionReadItem(row)
}

func ListSessionReadItems(db *sql.DB, f SessionReadItemFilter) ([]SessionReadItem, error) {
	if db == nil {
		return nil, fmt.Errorf("flowdb: list session read items requires db")
	}
	clauses := []string{"1=1"}
	var args []any
	status := strings.TrimSpace(strings.ToLower(f.Status))
	if status == "open" {
		clauses = append(clauses, "status IN (?, ?)")
		args = append(args, "pending", "unread")
	}
	add := func(col, val string) {
		val = strings.TrimSpace(val)
		if val == "" || val == "all" {
			return
		}
		clauses = append(clauses, col+" = ?")
		args = append(args, val)
	}
	if status != "open" {
		add("status", status)
	}
	add("kind", strings.ToLower(f.Kind))
	add("task_slug", f.TaskSlug)
	add("chat_slug", f.ChatSlug)
	add("work_context_id", f.WorkContextID)
	q := `SELECT ` + sessionReadItemCols + ` FROM session_read_items WHERE ` + strings.Join(clauses, " AND ") + ` ORDER BY created_at DESC, id DESC`
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("flowdb: list session read items: %w", err)
	}
	defer rows.Close()
	var out []SessionReadItem
	for rows.Next() {
		item, err := scanSessionReadItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func MarkSessionReadItem(db *sql.DB, id, status string) error {
	status = strings.TrimSpace(strings.ToLower(status))
	if status == "done" {
		status = "answered"
	}
	if status != "read" && status != "answered" {
		return fmt.Errorf("flowdb: mark status must be read or answered")
	}
	now := NowISO()
	var res sql.Result
	var err error
	if status == "answered" {
		res, err = db.Exec(`UPDATE session_read_items SET status = ?, answered_at = COALESCE(answered_at, ?), read_at = COALESCE(read_at, ?), updated_at = ? WHERE id = ?`,
			status, now, now, now, strings.TrimSpace(id))
	} else {
		res, err = db.Exec(`UPDATE session_read_items SET status = ?, read_at = COALESCE(read_at, ?), updated_at = ? WHERE id = ?`,
			status, now, now, strings.TrimSpace(id))
	}
	if err != nil {
		return fmt.Errorf("flowdb: mark session read item %q: %w", id, err)
	}
	if changed, _ := res.RowsAffected(); changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func ChatBySessionID(db *sql.DB, provider, sessionID string) (*Chat, error) {
	provider = strings.TrimSpace(strings.ToLower(provider))
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, sql.ErrNoRows
	}
	query := "SELECT " + chatCols + " FROM chats WHERE session_id = ? AND deleted_at IS NULL"
	args := []any{sessionID}
	if provider != "" {
		query += " AND provider = ?"
		args = append(args, provider)
	}
	query += " ORDER BY last_activity_at DESC LIMIT 1"
	return scanChat(db.QueryRow(query, args...))
}

func sessionReadItemByDedupeKey(db *sql.DB, key string) (SessionReadItem, error) {
	row := db.QueryRow(`SELECT `+sessionReadItemCols+` FROM session_read_items WHERE dedupe_key = ?`, strings.TrimSpace(key))
	return scanSessionReadItem(row)
}

func scanSessionReadItem(row interface{ Scan(dest ...any) error }) (SessionReadItem, error) {
	var item SessionReadItem
	err := row.Scan(
		&item.ID, &item.Kind, &item.Status, &item.Text, &item.CreatedAt, &item.UpdatedAt,
		&item.ReadAt, &item.AnsweredAt, &item.Provider, &item.SessionID,
		&item.TaskSlug, &item.ChatSlug, &item.ProjectSlug, &item.WorkContextID,
		&item.DedupeKey, &item.DependenciesJSON, &item.MetadataJSON,
	)
	if err != nil {
		return SessionReadItem{}, err
	}
	return item, nil
}

func normalizeSessionReadItem(item SessionReadItem) SessionReadItem {
	item.ID = strings.TrimSpace(item.ID)
	item.Kind = strings.TrimSpace(strings.ToLower(item.Kind))
	item.Status = strings.TrimSpace(strings.ToLower(item.Status))
	item.Text = strings.TrimSpace(item.Text)
	item.Provider = strings.TrimSpace(strings.ToLower(item.Provider))
	item.SessionID = strings.TrimSpace(item.SessionID)
	item.TaskSlug = strings.TrimSpace(item.TaskSlug)
	item.ChatSlug = strings.TrimSpace(item.ChatSlug)
	item.ProjectSlug = strings.TrimSpace(item.ProjectSlug)
	item.WorkContextID = strings.TrimSpace(item.WorkContextID)
	item.DedupeKey = strings.TrimSpace(item.DedupeKey)
	item.DependenciesJSON = strings.TrimSpace(item.DependenciesJSON)
	item.MetadataJSON = strings.TrimSpace(item.MetadataJSON)
	return item
}

func validSessionReadStatus(status string) bool {
	switch status {
	case "pending", "unread", "read", "answered":
		return true
	default:
		return false
	}
}

func compactJSONDefault(raw, def string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = def
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(raw)); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func randomSessionReadItemID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "read-" + hex.EncodeToString(b[:])
}
