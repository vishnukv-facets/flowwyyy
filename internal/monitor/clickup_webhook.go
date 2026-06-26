package monitor

import (
	"encoding/json"
	"fmt"
	"strings"
)

func NormalizeClickUpWebhook(payload []byte) ([]ClickUpEvent, error) {
	var p clickUpWebhookPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("parse clickup webhook: %w", err)
	}
	kind := ClickUpEventKind(strings.TrimSpace(p.Event))
	if !supportedClickUpEvent(kind) {
		return nil, nil
	}
	if strings.TrimSpace(p.TaskID) == "" || strings.TrimSpace(p.WebhookID) == "" {
		return nil, nil
	}
	raw := string(payload)
	if len(p.HistoryItems) == 0 {
		return []ClickUpEvent{{
			Kind:      kind,
			TaskID:    strings.TrimSpace(p.TaskID),
			WebhookID: strings.TrimSpace(p.WebhookID),
			Body:      clickUpFallbackBody(kind, clickUpHistoryItem{}),
			RawJSON:   raw,
		}}, nil
	}
	out := make([]ClickUpEvent, 0, len(p.HistoryItems))
	for _, h := range p.HistoryItems {
		historyID := strings.TrimSpace(h.ID)
		if historyID == "" {
			continue
		}
		body := clickUpHistoryBody(kind, h)
		out = append(out, ClickUpEvent{
			Kind:      kind,
			TaskID:    strings.TrimSpace(p.TaskID),
			WebhookID: strings.TrimSpace(p.WebhookID),
			HistoryID: historyID,
			Author:    strings.TrimSpace(h.User.Username),
			Body:      body,
			RawJSON:   raw,
			CreatedAt: strings.TrimSpace(h.Date),
		})
	}
	return out, nil
}

type clickUpWebhookPayload struct {
	Event        string               `json:"event"`
	TaskID       string               `json:"task_id"`
	WebhookID    string               `json:"webhook_id"`
	HistoryItems []clickUpHistoryItem `json:"history_items"`
}

type clickUpHistoryItem struct {
	ID     string          `json:"id"`
	Date   string          `json:"date"`
	Field  string          `json:"field"`
	User   clickUpUser     `json:"user"`
	Before json.RawMessage `json:"before"`
	After  json.RawMessage `json:"after"`
}

type clickUpUser struct {
	ID       any    `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

func supportedClickUpEvent(kind ClickUpEventKind) bool {
	switch kind {
	case ClickUpEventTaskCreated,
		ClickUpEventTaskUpdated,
		ClickUpEventTaskDeleted,
		ClickUpEventTaskStatusUpdated,
		ClickUpEventTaskAssigneeUpdated,
		ClickUpEventTaskPriorityUpdated,
		ClickUpEventTaskDueDateUpdated,
		ClickUpEventTaskTagUpdated,
		ClickUpEventTaskMoved,
		ClickUpEventTaskCommentPosted,
		ClickUpEventTaskCommentUpdated,
		ClickUpEventTaskTimeTracked,
		ClickUpEventTaskTimeEstimate:
		return true
	default:
		return false
	}
}

func clickUpHistoryBody(kind ClickUpEventKind, h clickUpHistoryItem) string {
	if kind == ClickUpEventTaskCommentPosted || kind == ClickUpEventTaskCommentUpdated {
		if s := compactJSONValue(h.After); s != "" {
			return s
		}
	}
	field := strings.TrimSpace(h.Field)
	if field == "" {
		field = string(kind)
	}
	after := compactJSONValue(h.After)
	if after == "" {
		return clickUpFallbackBody(kind, h)
	}
	return field + " changed to " + after
}

func clickUpFallbackBody(kind ClickUpEventKind, h clickUpHistoryItem) string {
	field := strings.TrimSpace(h.Field)
	if field != "" {
		return string(kind) + " (" + field + ")"
	}
	return string(kind)
}
