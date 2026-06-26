package monitor

import (
	"encoding/json"
	"fmt"
	"strings"
)

type ClickUpEventKind string

const (
	ClickUpEventTaskCreated         ClickUpEventKind = "taskCreated"
	ClickUpEventTaskUpdated         ClickUpEventKind = "taskUpdated"
	ClickUpEventTaskDeleted         ClickUpEventKind = "taskDeleted"
	ClickUpEventTaskStatusUpdated   ClickUpEventKind = "taskStatusUpdated"
	ClickUpEventTaskAssigneeUpdated ClickUpEventKind = "taskAssigneeUpdated"
	ClickUpEventTaskPriorityUpdated ClickUpEventKind = "taskPriorityUpdated"
	ClickUpEventTaskDueDateUpdated  ClickUpEventKind = "taskDueDateUpdated"
	ClickUpEventTaskTagUpdated      ClickUpEventKind = "taskTagUpdated"
	ClickUpEventTaskMoved           ClickUpEventKind = "taskMoved"
	ClickUpEventTaskCommentPosted   ClickUpEventKind = "taskCommentPosted"
	ClickUpEventTaskCommentUpdated  ClickUpEventKind = "taskCommentUpdated"
	ClickUpEventTaskTimeTracked     ClickUpEventKind = "taskTimeTrackedUpdated"
	ClickUpEventTaskTimeEstimate    ClickUpEventKind = "taskTimeEstimateUpdated"
)

type ClickUpEvent struct {
	Kind      ClickUpEventKind
	TeamID    string
	TaskID    string
	TaskName  string
	TaskURL   string
	WebhookID string
	HistoryID string
	Author    string
	Body      string
	RawJSON   string
	CreatedAt string
}

func (ev ClickUpEvent) LinkTag() string {
	taskID := strings.TrimSpace(ev.TaskID)
	if taskID == "" {
		return ""
	}
	return fmt.Sprintf("clickup-task:%s:%s", strings.TrimSpace(ev.TeamID), taskID)
}

func (ev ClickUpEvent) EventKeyValue() string {
	if ev.WebhookID != "" && ev.HistoryID != "" {
		return strings.TrimSpace(ev.WebhookID) + ":" + strings.TrimSpace(ev.HistoryID)
	}
	if ev.WebhookID != "" && ev.TaskID != "" && ev.Kind != "" {
		return strings.TrimSpace(ev.WebhookID) + ":" + string(ev.Kind) + ":" + strings.TrimSpace(ev.TaskID)
	}
	return ""
}

func ClickUpSlugForEvent(ev ClickUpEvent) string {
	id := strings.TrimSpace(ev.TaskID)
	if id == "" {
		return ""
	}
	return sanitizeGitHubSlug("clickup-" + id)
}

func clickUpEventToInboxEvent(ev ClickUpEvent) InboundEvent {
	return InboundEvent{
		Kind:        string(ev.Kind),
		Channel:     strings.TrimSpace(ev.TaskID),
		ChannelType: "clickup",
		TS:          strings.TrimSpace(ev.CreatedAt),
		ThreadTS:    ev.LinkTag(),
		UserID:      strings.TrimSpace(ev.Author),
		Text:        strings.TrimSpace(ev.Body),
		URL:         strings.TrimSpace(ev.TaskURL),
		EventKey:    ev.EventKeyValue(),
		ItemChannel: strings.TrimSpace(ev.TaskID),
		ItemTS:      strings.TrimSpace(ev.HistoryID),
		ItemAuthor:  strings.TrimSpace(ev.Author),
		TeamID:      strings.TrimSpace(ev.TeamID),
		RawJSON:     strings.TrimSpace(ev.RawJSON),
	}
}

func compactJSONValue(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		for _, k := range []string{"comment_text", "text", "username", "status", "priority", "name"} {
			if v, ok := m[k]; ok {
				if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
					return s
				}
			}
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(raw))
}
