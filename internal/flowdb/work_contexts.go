package flowdb

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

// WorkContext is the durable "problem being worked" object. It is separate
// from transport anchors such as Slack threads or GitHub comments.
type WorkContext struct {
	ID        string
	Slug      sql.NullString
	Title     string
	Summary   string
	Status    string
	CreatedAt string
	UpdatedAt string
}

type WorkContextSourceAnchor struct {
	ID            string
	WorkContextID string
	Source        string
	AnchorType    string
	ExternalID    string
	URL           string
	Label         string
	MetadataJSON  string
	CreatedAt     string
}

type WorkContextEdge struct {
	FromContextID string
	ToContextID   string
	Kind          string
	Note          string
	CreatedAt     string
}

func CreateWorkContext(db *sql.DB, c WorkContext) (WorkContext, error) {
	if db == nil {
		return WorkContext{}, fmt.Errorf("flowdb: create work context requires db")
	}
	c.ID = strings.TrimSpace(c.ID)
	if c.ID == "" {
		c.ID = randomWorkContextID()
	}
	slug := strings.TrimSpace(c.Slug.String)
	c.Title = strings.TrimSpace(c.Title)
	c.Summary = strings.TrimSpace(c.Summary)
	c.Status = normalizeWorkContextStatus(c.Status)
	if c.Title == "" {
		return WorkContext{}, fmt.Errorf("flowdb: work context title is required")
	}
	if c.CreatedAt == "" {
		c.CreatedAt = NowISO()
	}
	if c.UpdatedAt == "" {
		c.UpdatedAt = c.CreatedAt
	}
	_, err := db.Exec(
		`INSERT INTO work_contexts (id, slug, title, summary, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.ID, NullIfEmpty(slug), c.Title, c.Summary, c.Status, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return WorkContext{}, fmt.Errorf("flowdb: create work context %q: %w", c.ID, err)
	}
	c.Slug = sql.NullString{String: slug, Valid: slug != ""}
	return c, nil
}

func GetWorkContext(db *sql.DB, id string) (WorkContext, error) {
	id = strings.TrimSpace(id)
	return scanWorkContext(db.QueryRow(
		`SELECT id, slug, title, summary, status, created_at, updated_at
		 FROM work_contexts WHERE id = ?`,
		id,
	))
}

func WorkContextBySlug(db *sql.DB, slug string) (WorkContext, error) {
	slug = strings.TrimSpace(slug)
	return scanWorkContext(db.QueryRow(
		`SELECT id, slug, title, summary, status, created_at, updated_at
		 FROM work_contexts WHERE slug = ?`,
		slug,
	))
}

func scanWorkContext(row interface{ Scan(dest ...any) error }) (WorkContext, error) {
	var c WorkContext
	if err := row.Scan(&c.ID, &c.Slug, &c.Title, &c.Summary, &c.Status, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return WorkContext{}, err
	}
	return c, nil
}

func CreateWorkContextSourceAnchor(db *sql.DB, a WorkContextSourceAnchor) (WorkContextSourceAnchor, error) {
	if db == nil {
		return WorkContextSourceAnchor{}, fmt.Errorf("flowdb: create work context source anchor requires db")
	}
	a.ID = strings.TrimSpace(a.ID)
	if a.ID == "" {
		a.ID = randomWorkContextAnchorID()
	}
	a.WorkContextID = strings.TrimSpace(a.WorkContextID)
	a.ExternalID = strings.TrimSpace(a.ExternalID)
	a.URL = strings.TrimSpace(a.URL)
	a.Label = strings.TrimSpace(a.Label)
	a.MetadataJSON = strings.TrimSpace(a.MetadataJSON)
	source, anchorType, err := normalizeWorkContextAnchor(a.Source, a.AnchorType)
	if err != nil {
		return WorkContextSourceAnchor{}, err
	}
	a.Source, a.AnchorType = source, anchorType
	if a.WorkContextID == "" || a.ExternalID == "" {
		return WorkContextSourceAnchor{}, fmt.Errorf("flowdb: work context source anchor requires work_context_id and external_id")
	}
	if a.CreatedAt == "" {
		a.CreatedAt = NowISO()
	}
	_, err = db.Exec(
		`INSERT INTO work_context_source_anchors (
			id, work_context_id, source, anchor_type, external_id, url, label, metadata_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.WorkContextID, a.Source, a.AnchorType, a.ExternalID,
		NullIfEmpty(a.URL), NullIfEmpty(a.Label), NullIfEmpty(a.MetadataJSON), a.CreatedAt,
	)
	if err != nil {
		return WorkContextSourceAnchor{}, fmt.Errorf("flowdb: create work context source anchor: %w", err)
	}
	return a, nil
}

func ListWorkContextSourceAnchors(db *sql.DB, contextID string) ([]WorkContextSourceAnchor, error) {
	rows, err := db.Query(`
		SELECT id, work_context_id, source, anchor_type, external_id,
		       COALESCE(url, ''), COALESCE(label, ''), COALESCE(metadata_json, ''), created_at
		FROM work_context_source_anchors
		WHERE work_context_id = ?
		ORDER BY created_at ASC, id ASC
	`, strings.TrimSpace(contextID))
	if err != nil {
		return nil, fmt.Errorf("flowdb: list work context anchors: %w", err)
	}
	defer rows.Close()
	var out []WorkContextSourceAnchor
	for rows.Next() {
		var a WorkContextSourceAnchor
		if err := rows.Scan(&a.ID, &a.WorkContextID, &a.Source, &a.AnchorType, &a.ExternalID, &a.URL, &a.Label, &a.MetadataJSON, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func CreateWorkContextEdge(db *sql.DB, e WorkContextEdge) error {
	if db == nil {
		return fmt.Errorf("flowdb: create work context edge requires db")
	}
	e.FromContextID = strings.TrimSpace(e.FromContextID)
	e.ToContextID = strings.TrimSpace(e.ToContextID)
	e.Note = strings.TrimSpace(e.Note)
	kind, err := normalizeWorkContextEdgeKind(e.Kind)
	if err != nil {
		return err
	}
	e.Kind = kind
	if e.FromContextID == "" || e.ToContextID == "" {
		return fmt.Errorf("flowdb: work context edge requires both context ids")
	}
	if e.FromContextID == e.ToContextID {
		return fmt.Errorf("flowdb: work context edge cannot point to itself")
	}
	if e.CreatedAt == "" {
		e.CreatedAt = NowISO()
	}
	_, err = db.Exec(
		`INSERT INTO work_context_edges (from_context_id, to_context_id, kind, note, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		e.FromContextID, e.ToContextID, e.Kind, NullIfEmpty(e.Note), e.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("flowdb: create work context edge: %w", err)
	}
	return nil
}

func ListWorkContextEdges(db *sql.DB, fromContextID string) ([]WorkContextEdge, error) {
	rows, err := db.Query(`
		SELECT from_context_id, to_context_id, kind, COALESCE(note, ''), created_at
		FROM work_context_edges
		WHERE from_context_id = ?
		ORDER BY created_at ASC, to_context_id ASC, kind ASC
	`, strings.TrimSpace(fromContextID))
	if err != nil {
		return nil, fmt.Errorf("flowdb: list work context edges: %w", err)
	}
	defer rows.Close()
	var out []WorkContextEdge
	for rows.Next() {
		var e WorkContextEdge
		if err := rows.Scan(&e.FromContextID, &e.ToContextID, &e.Kind, &e.Note, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func SetTaskWorkContext(db *sql.DB, slug, contextID string) error {
	_, err := db.Exec(
		`UPDATE tasks SET work_context_id = ?, updated_at = ? WHERE slug = ?`,
		nullStringOrTrimmed(contextID), NowISO(), strings.TrimSpace(slug),
	)
	if err != nil {
		return fmt.Errorf("flowdb: set task work context %q: %w", slug, err)
	}
	return nil
}

func SetChatWorkContext(db *sql.DB, slug, contextID string) error {
	_, err := db.Exec(
		`UPDATE chats SET work_context_id = ? WHERE slug = ? AND deleted_at IS NULL`,
		nullStringOrTrimmed(contextID), strings.TrimSpace(slug),
	)
	if err != nil {
		return fmt.Errorf("flowdb: set chat work context %q: %w", slug, err)
	}
	return nil
}

func SetAgentRuntimeWorkContext(db *sql.DB, provider, sessionID, contextID string) error {
	provider, err := NormalizeSessionProvider(provider)
	if err != nil {
		return err
	}
	_, err = db.Exec(
		`UPDATE agent_runtime_states SET work_context_id = ? WHERE provider = ? AND session_id = ?`,
		nullStringOrTrimmed(contextID), provider, strings.TrimSpace(sessionID),
	)
	if err != nil {
		return fmt.Errorf("flowdb: set agent runtime work context %q/%q: %w", provider, sessionID, err)
	}
	return nil
}

func normalizeWorkContextStatus(status string) string {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case "", "active":
		return "active"
	case "archived":
		return "archived"
	default:
		return strings.TrimSpace(strings.ToLower(status))
	}
}

func normalizeWorkContextAnchor(source, anchorType string) (string, string, error) {
	source = strings.TrimSpace(strings.ToLower(source))
	anchorType = strings.TrimSpace(strings.ToLower(anchorType))
	wantSource := map[string]string{
		"slack_channel_thread": "slack",
		"slack_dm":             "slack",
		"slack_group_dm":       "slack",
		"github_pr":            "github",
		"github_issue":         "github",
		"github_comment":       "github",
		"flow_chat":            "flow",
		"flow_task":            "flow",
		"flow_session":         "flow",
	}[anchorType]
	if wantSource == "" {
		return "", "", fmt.Errorf("flowdb: unsupported work context anchor type %q", anchorType)
	}
	if source == "" {
		source = wantSource
	}
	if source != wantSource {
		return "", "", fmt.Errorf("flowdb: anchor type %q belongs to source %q, got %q", anchorType, wantSource, source)
	}
	return source, anchorType, nil
}

func normalizeWorkContextEdgeKind(kind string) (string, error) {
	kind = strings.ReplaceAll(strings.TrimSpace(strings.ToLower(kind)), "_", "-")
	switch kind {
	case "related", "follow-up", "fork", "duplicate", "blocked-by":
		return kind, nil
	default:
		return "", fmt.Errorf("flowdb: unsupported work context edge kind %q", kind)
	}
}

func randomWorkContextID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "wc-" + hex.EncodeToString(b[:])
}

func randomWorkContextAnchorID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "wca-" + hex.EncodeToString(b[:])
}
