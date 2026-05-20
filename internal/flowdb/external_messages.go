package flowdb

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

type ExternalMessage struct {
	ID              string
	Source          string
	EventID         sql.NullString
	ConversationID  string
	ChannelID       sql.NullString
	ThreadTS        sql.NullString
	MessageTS       string
	Direction       string
	SenderID        sql.NullString
	SenderName      sql.NullString
	Text            string
	NormalizedText  sql.NullString
	Intent          sql.NullString
	ConfidenceBasis sql.NullString
	TaskSlug        sql.NullString
	RawJSON         sql.NullString
	CreatedAt       string
}

type ExternalMessageInput struct {
	Source          string
	EventID         string
	ConversationID  string
	ChannelID       string
	ThreadTS        string
	MessageTS       string
	Direction       string
	SenderID        string
	SenderName      string
	Text            string
	NormalizedText  string
	Intent          string
	ConfidenceBasis string
	TaskSlug        string
	RawJSON         string
}

type ExternalAction struct {
	ID           string
	Source       string
	EventID      string
	MessageID    sql.NullString
	ActionType   string
	Status       string
	TaskSlug     sql.NullString
	PayloadJSON  sql.NullString
	ResponseJSON sql.NullString
	Error        sql.NullString
	AutoApproved bool
	CreatedAt    string
}

type ExternalActionInput struct {
	Source       string
	EventID      string
	MessageID    string
	ActionType   string
	Status       string
	TaskSlug     string
	PayloadJSON  string
	ResponseJSON string
	Error        string
	AutoApproved bool
}

func ExternalMessageID(source, conversationID, messageTS, direction string) string {
	key := normalizeMonitorPart(source) + ":" + strings.TrimSpace(conversationID) + ":" + strings.TrimSpace(messageTS) + ":" + normalizeMonitorPart(direction)
	sum := sha1.Sum([]byte(key))
	return normalizeMonitorPart(source) + "-msg-" + hex.EncodeToString(sum[:])[:16]
}

func ExternalActionID(source, eventID, actionType string) string {
	key := normalizeMonitorPart(source) + ":" + strings.TrimSpace(eventID) + ":" + normalizeMonitorPart(actionType)
	sum := sha1.Sum([]byte(key))
	return normalizeMonitorPart(source) + "-act-" + hex.EncodeToString(sum[:])[:16]
}

func RecordExternalMessage(db *sql.DB, input ExternalMessageInput) (*ExternalMessage, bool, error) {
	source := normalizeMonitorPart(input.Source)
	conversationID := strings.TrimSpace(input.ConversationID)
	messageTS := strings.TrimSpace(input.MessageTS)
	direction := normalizeMonitorPart(input.Direction)
	text := strings.TrimSpace(input.Text)
	if source == "" || conversationID == "" || messageTS == "" || text == "" {
		return nil, false, fmt.Errorf("external message requires source, conversation_id, message_ts, and text")
	}
	switch direction {
	case "inbound", "outbound":
	default:
		return nil, false, fmt.Errorf("invalid external message direction %q", input.Direction)
	}
	id := ExternalMessageID(source, conversationID, messageTS, direction)
	now := NowISO()
	res, err := db.Exec(
		`INSERT INTO external_messages (
			id, source, event_id, conversation_id, channel_id, thread_ts, message_ts,
			direction, sender_id, sender_name, text, normalized_text, intent,
			confidence_basis, task_slug, raw_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source, conversation_id, message_ts, direction) DO NOTHING`,
		id, source, NullString(input.EventID), conversationID, NullString(input.ChannelID),
		NullString(input.ThreadTS), messageTS, direction, NullString(input.SenderID),
		NullString(input.SenderName), text, NullString(input.NormalizedText),
		NullString(normalizeMonitorPart(input.Intent)), NullString(input.ConfidenceBasis),
		NullString(input.TaskSlug), NullString(input.RawJSON), now,
	)
	if err != nil {
		return nil, false, fmt.Errorf("record external message: %w", err)
	}
	affected, _ := res.RowsAffected()
	msg, err := GetExternalMessage(db, id)
	if err != nil {
		return nil, false, err
	}
	return msg, affected > 0, nil
}

func GetExternalMessage(db *sql.DB, id string) (*ExternalMessage, error) {
	row := db.QueryRow(
		`SELECT id, source, event_id, conversation_id, channel_id, thread_ts, message_ts,
		        direction, sender_id, sender_name, text, normalized_text, intent,
		        confidence_basis, task_slug, raw_json, created_at
		   FROM external_messages
		  WHERE id = ?`,
		strings.TrimSpace(id),
	)
	return scanExternalMessage(row)
}

func ListExternalMessagesForEvent(db *sql.DB, eventID string) ([]ExternalMessage, error) {
	rows, err := db.Query(
		`SELECT id, source, event_id, conversation_id, channel_id, thread_ts, message_ts,
		        direction, sender_id, sender_name, text, normalized_text, intent,
		        confidence_basis, task_slug, raw_json, created_at
		   FROM external_messages
		  WHERE event_id = ?
		  ORDER BY created_at ASC, rowid ASC`,
		strings.TrimSpace(eventID),
	)
	if err != nil {
		return nil, fmt.Errorf("list external messages: %w", err)
	}
	defer rows.Close()
	out := []ExternalMessage{}
	for rows.Next() {
		msg, err := scanExternalMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *msg)
	}
	return out, rows.Err()
}

func RecordExternalAction(db *sql.DB, input ExternalActionInput) error {
	source := normalizeMonitorPart(input.Source)
	eventID := strings.TrimSpace(input.EventID)
	actionType := normalizeMonitorPart(input.ActionType)
	status := normalizeMonitorPart(input.Status)
	if source == "" || eventID == "" || actionType == "" {
		return fmt.Errorf("external action requires source, event_id, and action_type")
	}
	switch actionType {
	case "reaction_add", "status_reply", "clarifying_question", "final_answer", "draft_ack", "working_ack", "task_draft", "post_failed", "skipped":
	default:
		return fmt.Errorf("invalid external action type %q", input.ActionType)
	}
	switch status {
	case "pending", "sent", "failed", "skipped":
	default:
		return fmt.Errorf("invalid external action status %q", input.Status)
	}
	id := ExternalActionID(source, eventID, actionType)
	now := NowISO()
	_, err := db.Exec(
		`INSERT INTO external_message_actions (
			id, source, event_id, message_id, action_type, status, task_slug,
			payload_json, response_json, error, auto_approved, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source, event_id, action_type) DO UPDATE SET
			message_id = excluded.message_id,
			status = excluded.status,
			task_slug = excluded.task_slug,
			payload_json = excluded.payload_json,
			response_json = excluded.response_json,
			error = excluded.error,
			auto_approved = excluded.auto_approved`,
		id, source, eventID, NullString(input.MessageID), actionType, status,
		NullString(input.TaskSlug), NullString(input.PayloadJSON), NullString(input.ResponseJSON),
		NullString(input.Error), boolInt(input.AutoApproved), now,
	)
	if err != nil {
		return fmt.Errorf("record external action: %w", err)
	}
	return nil
}

func ExternalActionExists(db *sql.DB, eventID, actionType string) (bool, error) {
	var id string
	err := db.QueryRow(
		`SELECT id FROM external_message_actions WHERE event_id = ? AND action_type = ? AND status = 'sent' LIMIT 1`,
		strings.TrimSpace(eventID), normalizeMonitorPart(actionType),
	).Scan(&id)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, err
}

func ListExternalActionsForEvent(db *sql.DB, eventID string) ([]ExternalAction, error) {
	rows, err := db.Query(
		`SELECT id, source, event_id, message_id, action_type, status, task_slug,
		        payload_json, response_json, error, auto_approved, created_at
		   FROM external_message_actions
		  WHERE event_id = ?
		  ORDER BY created_at ASC, rowid ASC`,
		strings.TrimSpace(eventID),
	)
	if err != nil {
		return nil, fmt.Errorf("list external actions: %w", err)
	}
	defer rows.Close()
	out := []ExternalAction{}
	for rows.Next() {
		action, err := scanExternalAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *action)
	}
	return out, rows.Err()
}

func scanExternalMessage(row interface{ Scan(dest ...any) error }) (*ExternalMessage, error) {
	var msg ExternalMessage
	if err := row.Scan(
		&msg.ID, &msg.Source, &msg.EventID, &msg.ConversationID, &msg.ChannelID,
		&msg.ThreadTS, &msg.MessageTS, &msg.Direction, &msg.SenderID, &msg.SenderName,
		&msg.Text, &msg.NormalizedText, &msg.Intent, &msg.ConfidenceBasis, &msg.TaskSlug,
		&msg.RawJSON, &msg.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &msg, nil
}

func scanExternalAction(row interface{ Scan(dest ...any) error }) (*ExternalAction, error) {
	var action ExternalAction
	var autoApproved int
	if err := row.Scan(
		&action.ID, &action.Source, &action.EventID, &action.MessageID, &action.ActionType,
		&action.Status, &action.TaskSlug, &action.PayloadJSON, &action.ResponseJSON,
		&action.Error, &autoApproved, &action.CreatedAt,
	); err != nil {
		return nil, err
	}
	action.AutoApproved = autoApproved != 0
	return &action, nil
}
