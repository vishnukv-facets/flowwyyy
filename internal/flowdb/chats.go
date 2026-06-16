package flowdb

import (
	"database/sql"
	"fmt"
	"strings"
)

// Chat mirrors the chats table. Chats are durable records of adhoc
// agent "command-center" sessions. They are NOT tasks and do not appear
// on the task board.
type Chat struct {
	Slug           string
	Title          string
	Provider       string // claude|codex
	Origin         string // ui|slack
	SessionID      sql.NullString
	CreatedAt      string
	LastActivityAt string
	ArchivedAt     sql.NullString
	DeletedAt      sql.NullString
}

// ChatFilter narrows ListChats.
type ChatFilter struct {
	IncludeArchived bool
}

// ---------- column list ----------

const chatCols = "slug, title, provider, origin, session_id, created_at, last_activity_at, archived_at, deleted_at"

// ---------- scan ----------

func scanChat(row interface{ Scan(dest ...any) error }) (*Chat, error) {
	var c Chat
	err := row.Scan(
		&c.Slug, &c.Title, &c.Provider, &c.Origin,
		&c.SessionID,
		&c.CreatedAt, &c.LastActivityAt,
		&c.ArchivedAt, &c.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ---------- CRUD ----------

// InsertChat writes a new chat row. Slug must be unique.
func InsertChat(db *sql.DB, c Chat) error {
	_, err := db.Exec(
		`INSERT INTO chats (`+chatCols+`) VALUES (?,?,?,?,?,?,?,?,?)`,
		c.Slug, c.Title, c.Provider, c.Origin,
		c.SessionID,
		c.CreatedAt, c.LastActivityAt,
		c.ArchivedAt, c.DeletedAt,
	)
	if err != nil {
		return fmt.Errorf("flowdb: insert chat %q: %w", c.Slug, err)
	}
	return nil
}

// GetChat returns the ACTIVE chat with the given slug, or sql.ErrNoRows when
// missing OR soft-deleted. Deleted chats are treated as absent so callers (e.g.
// the Slack command channel keyed on a deterministic per-channel slug) open a
// FRESH chat instead of resurrecting a deleted session. UpsertChat then reclaims
// the leftover tombstone row.
func GetChat(db *sql.DB, slug string) (*Chat, error) {
	row := db.QueryRow("SELECT "+chatCols+" FROM chats WHERE slug = ? AND deleted_at IS NULL", slug)
	c, err := scanChat(row)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// UpsertChat inserts a new chat, or — when a row already exists for the slug
// (e.g. a soft-deleted tombstone left by DeleteChat for a deterministic
// per-channel slug) — REPLACES it with the new chat: fresh title/provider/
// origin/session/timestamps, with archived_at and deleted_at cleared. This is
// how a deleted Slack chat starts cleanly on the next message rather than
// failing the primary-key conflict a plain InsertChat would hit. Slugs that are
// always unique (UI overview-<uuid>) simply insert.
func UpsertChat(db *sql.DB, c Chat) error {
	_, err := db.Exec(
		`INSERT INTO chats (`+chatCols+`) VALUES (?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(slug) DO UPDATE SET
		   title = excluded.title,
		   provider = excluded.provider,
		   origin = excluded.origin,
		   session_id = excluded.session_id,
		   created_at = excluded.created_at,
		   last_activity_at = excluded.last_activity_at,
		   archived_at = NULL,
		   deleted_at = NULL`,
		c.Slug, c.Title, c.Provider, c.Origin,
		c.SessionID,
		c.CreatedAt, c.LastActivityAt,
		c.ArchivedAt, c.DeletedAt,
	)
	if err != nil {
		return fmt.Errorf("flowdb: upsert chat %q: %w", c.Slug, err)
	}
	return nil
}

// SetChatSession persists a captured session id onto an existing chat row
// (and bumps last_activity_at). Codex assigns a brand-new session id on every
// launch/resume — only known after the process starts — so the chat's id is
// filled in (and overwritten on each resume) by the capture goroutine rather
// than at creation, the way Claude's pre-generated id is. Deleted chats are
// never touched. A no-op when the slug isn't a live chat row.
func SetChatSession(db *sql.DB, slug, sessionID, now string) error {
	_, err := db.Exec(
		`UPDATE chats SET session_id = ?, last_activity_at = ?
		 WHERE slug = ? AND deleted_at IS NULL`,
		sessionID, now, slug,
	)
	if err != nil {
		return fmt.Errorf("flowdb: set chat session %q: %w", slug, err)
	}
	return nil
}

// SetChatProvider flips the provider on an existing chat row (and bumps
// last_activity_at). Used by the steerer provider fork / manual switch
// (claude↔codex): once switched, chats.provider is authoritative for resume
// until changed again. Deleted chats are never touched.
func SetChatProvider(db *sql.DB, slug, provider, now string) error {
	_, err := db.Exec(
		`UPDATE chats SET provider = ?, last_activity_at = ?
		 WHERE slug = ? AND deleted_at IS NULL`,
		provider, now, slug,
	)
	if err != nil {
		return fmt.Errorf("flowdb: set chat provider %q: %w", slug, err)
	}
	return nil
}

// SetChatTitle renames an existing chat row (and bumps last_activity_at). Used by
// the steerer chat rename + auto-naming convention. Deleted chats are never
// touched.
func SetChatTitle(db *sql.DB, slug, title, now string) error {
	_, err := db.Exec(
		`UPDATE chats SET title = ?, last_activity_at = ?
		 WHERE slug = ? AND deleted_at IS NULL`,
		title, now, slug,
	)
	if err != nil {
		return fmt.Errorf("flowdb: set chat title %q: %w", slug, err)
	}
	return nil
}

// ListChats returns non-deleted chats ordered by last_activity_at DESC.
// Archived chats are hidden unless ChatFilter.IncludeArchived is true.
// Deleted chats (deleted_at IS NOT NULL) are always excluded.
func ListChats(db *sql.DB, filter ChatFilter) ([]*Chat, error) {
	var where []string
	where = append(where, "deleted_at IS NULL")
	if !filter.IncludeArchived {
		where = append(where, "archived_at IS NULL")
	}
	q := "SELECT " + chatCols + " FROM chats"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY last_activity_at DESC"
	rows, err := db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("flowdb: list chats: %w", err)
	}
	defer rows.Close()
	var out []*Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, fmt.Errorf("flowdb: scan chat: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TouchChat bumps last_activity_at for the given slug.
func TouchChat(db *sql.DB, slug, lastActivityAt string) error {
	_, err := db.Exec(
		`UPDATE chats SET last_activity_at = ? WHERE slug = ?`,
		lastActivityAt, slug,
	)
	if err != nil {
		return fmt.Errorf("flowdb: touch chat %q: %w", slug, err)
	}
	return nil
}

// ArchiveChat sets archived_at on the given slug.
func ArchiveChat(db *sql.DB, slug, at string) error {
	_, err := db.Exec(
		`UPDATE chats SET archived_at = ? WHERE slug = ?`,
		at, slug,
	)
	if err != nil {
		return fmt.Errorf("flowdb: archive chat %q: %w", slug, err)
	}
	return nil
}

// UnarchiveChat clears archived_at, making the chat visible again.
func UnarchiveChat(db *sql.DB, slug string) error {
	_, err := db.Exec(
		`UPDATE chats SET archived_at = NULL WHERE slug = ?`,
		slug,
	)
	if err != nil {
		return fmt.Errorf("flowdb: unarchive chat %q: %w", slug, err)
	}
	return nil
}

// DeleteChat soft-deletes the given slug by setting deleted_at.
func DeleteChat(db *sql.DB, slug, at string) error {
	_, err := db.Exec(
		`UPDATE chats SET deleted_at = ? WHERE slug = ?`,
		at, slug,
	)
	if err != nil {
		return fmt.Errorf("flowdb: delete chat %q: %w", slug, err)
	}
	return nil
}
