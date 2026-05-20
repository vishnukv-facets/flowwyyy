package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"

	"github.com/slack-go/slack/slackevents"
)

type slackAutoReplyCall struct {
	ActionType string
	Channel    string
	TS         string
	ThreadTS   string
	Emoji      string
	Text       string
	TaskSlug   string
}

var slackAutoReplyRunner = func(ctx context.Context, call slackAutoReplyCall) error {
	writer := monitor.NewSlackWriter()
	switch call.ActionType {
	case "reaction_add":
		return writer.AddReaction(ctx, call.Channel, call.TS, call.Emoji)
	case "status_reply", "clarifying_question", "final_answer":
		return writer.PostMessage(ctx, call.Channel, call.ThreadTS, call.Text)
	default:
		return fmt.Errorf("unsupported slack auto-reply action %q", call.ActionType)
	}
}

var slackAutoReplyMentionRE = regexp.MustCompile(`<@([A-Z0-9]+)(?:\|[^>]+)?>`)

func (s *Server) recordSlackExternalMessagesAndMaybeReply(ctx context.Context, event slackevents.EventsAPIEvent, inputs []flowdb.MonitorEventInput) {
	if s == nil || s.cfg.DB == nil || len(inputs) == 0 {
		return
	}
	meta, ok := slackEventContextFromEvent(event)
	if !ok {
		return
	}
	for _, input := range inputs {
		if !strings.EqualFold(input.Source, "slack") {
			continue
		}
		eventID := flowdb.MonitorEventID(input.Source, input.SourceID)
		stored, err := flowdb.GetMonitorEvent(s.cfg.DB, eventID)
		if err != nil || stored == nil {
			continue
		}
		msg, isNew, err := s.recordSlackExternalMessage(ctx, meta, input, *stored)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: record slack external message: %v\n", err)
			continue
		}
		if isNew {
			s.maybeAutoReplyToSlackQuestion(ctx, *stored, meta, msg)
		}
	}
}

func (s *Server) recordSlackExternalMessage(ctx context.Context, meta slackEventContext, input flowdb.MonitorEventInput, event flowdb.MonitorEvent) (*flowdb.ExternalMessage, bool, error) {
	channel := strings.TrimSpace(meta.ChannelID)
	ts := strings.TrimSpace(meta.TS)
	if channel == "" || ts == "" {
		origin, ok := monitor.SlackOriginFromEvent(event)
		if !ok {
			return nil, false, fmt.Errorf("slack event has no channel/ts")
		}
		channel = origin.Channel
		ts = origin.TS
	}
	threadTS := strings.TrimSpace(meta.ThreadTS)
	if threadTS == "" {
		threadTS = ts
	}
	text := slackReadableMessageText(firstNonEmpty(input.Body, meta.Text))
	intent := "message"
	basis := ""
	if slackLooksLikeQuestion(text) {
		intent = "question"
		basis = "question heuristic"
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return flowdb.RecordExternalMessage(s.cfg.DB, flowdb.ExternalMessageInput{
		Source:          "slack",
		EventID:         event.ID,
		ConversationID:  channel,
		ChannelID:       channel,
		ThreadTS:        threadTS,
		MessageTS:       ts,
		Direction:       "inbound",
		SenderID:        firstNonEmpty(meta.UserID, meta.BotID),
		SenderName:      slackDisplayUser(resolveCtx, meta.UserID, meta.BotID),
		Text:            text,
		NormalizedText:  normalizeExternalText(text),
		Intent:          intent,
		ConfidenceBasis: basis,
		RawJSON:         input.RawJSON,
	})
}

func (s *Server) maybeAutoReplyToSlackQuestion(ctx context.Context, event flowdb.MonitorEvent, meta slackEventContext, msg *flowdb.ExternalMessage) {
	if !slackAutoReplyEnabled() || msg == nil || msg.Intent.String != "question" {
		return
	}
	origin, ok := monitor.SlackOriginFromEvent(event)
	if !ok {
		return
	}
	s.runSlackAutoReplyAction(ctx, event.ID, msg.ID, slackAutoReplyCall{
		ActionType: "reaction_add",
		Channel:    origin.Channel,
		TS:         origin.TS,
		Emoji:      slackAutoReplyEmoji(),
	})
	_, threadTS := origin.PostTarget()
	if answer, taskSlug, ok := s.flowBackedSlackAnswer(event, msg.Text); ok {
		s.runSlackAutoReplyAction(ctx, event.ID, msg.ID, slackAutoReplyCall{
			ActionType: "final_answer",
			Channel:    origin.Channel,
			ThreadTS:   threadTS,
			Text:       answer,
			TaskSlug:   taskSlug,
		})
		return
	}
	s.runSlackAutoReplyAction(ctx, event.ID, msg.ID, slackAutoReplyCall{
		ActionType: "clarifying_question",
		Channel:    origin.Channel,
		ThreadTS:   threadTS,
		Text:       "I can answer from Flow, but I do not have enough Flow context for this thread yet. Which Flow task or project should I check?",
	})
}

func (s *Server) flowBackedSlackAnswer(event flowdb.MonitorEvent, text string) (string, string, bool) {
	if slackQuestionAsksCurrentWork(text) {
		if answer, ok := s.currentWorkSlackAnswer(); ok {
			return answer, "", true
		}
	}
	if !slackQuestionAsksTaskStatus(text) {
		return "", "", false
	}
	task, ok := s.taskForSlackThread(event)
	if !ok || task == nil {
		return "", "", false
	}
	return slackTaskStatusAnswer(task, monitor.FlowBaseURL()), task.Slug, true
}

func (s *Server) currentWorkSlackAnswer() (string, bool) {
	if s == nil || s.cfg.DB == nil {
		return "", false
	}
	tasks, err := flowdb.ListTasks(s.cfg.DB, flowdb.TaskFilter{
		Status:          "in-progress",
		Kind:            "",
		IncludeArchived: false,
		IncludeDeleted:  false,
	})
	if err != nil {
		return "", false
	}
	if len(tasks) == 0 {
		return "From Flow: I do not see any in-progress tasks right now.", true
	}
	const maxTasks = 5
	var b strings.Builder
	b.WriteString("From Flow: current in-progress tasks are:")
	baseURL := monitor.FlowBaseURL()
	for i, task := range tasks {
		if i >= maxTasks {
			fmt.Fprintf(&b, "\n- ...and %d more", len(tasks)-maxTasks)
			break
		}
		fmt.Fprintf(&b, "\n- %s (`%s`)", task.Name, task.Slug)
		if task.WaitingOn.Valid {
			b.WriteString(" - waiting on " + task.WaitingOn.String)
		}
		if baseURL != "" {
			b.WriteString(" - " + strings.TrimRight(baseURL, "/") + "/tasks/" + task.Slug)
		}
	}
	return b.String(), true
}

func (s *Server) taskForSlackThread(event flowdb.MonitorEvent) (*flowdb.Task, bool) {
	origin, ok := monitor.SlackOriginFromEvent(event)
	if !ok {
		return nil, false
	}
	_, threadTS := origin.PostTarget()
	events, err := flowdb.ListMonitorEvents(s.cfg.DB, 500)
	if err != nil {
		return nil, false
	}
	for _, candidate := range events {
		if !strings.EqualFold(candidate.Source, "slack") || candidate.ID == event.ID {
			continue
		}
		action, err := flowdb.GetMonitorEventAction(s.cfg.DB, candidate.ID)
		if err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			return nil, false
		}
		if !action.TaskSlug.Valid {
			continue
		}
		candidateOrigin, ok := monitor.SlackOriginFromEvent(candidate)
		if !ok || candidateOrigin.Channel != origin.Channel {
			continue
		}
		_, candidateThreadTS := candidateOrigin.PostTarget()
		if candidateThreadTS != threadTS {
			continue
		}
		task, err := flowdb.GetTask(s.cfg.DB, action.TaskSlug.String)
		if err == nil && task != nil {
			return task, true
		}
	}
	return nil, false
}

func (s *Server) runSlackAutoReplyAction(ctx context.Context, eventID, messageID string, call slackAutoReplyCall) {
	if s == nil || s.cfg.DB == nil {
		return
	}
	if exists, err := flowdb.ExternalActionExists(s.cfg.DB, eventID, call.ActionType); err == nil && exists {
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"channel":     call.Channel,
		"ts":          call.TS,
		"thread_ts":   call.ThreadTS,
		"emoji":       call.Emoji,
		"text":        call.Text,
		"task_slug":   call.TaskSlug,
		"action_type": call.ActionType,
		"basis":       "flow_backed_only",
	})
	status := "sent"
	errText := ""
	if err := slackAutoReplyRunner(ctx, call); err != nil {
		status = "failed"
		errText = err.Error()
	}
	if err := flowdb.RecordExternalAction(s.cfg.DB, flowdb.ExternalActionInput{
		Source:       "slack",
		EventID:      eventID,
		MessageID:    messageID,
		ActionType:   call.ActionType,
		Status:       status,
		TaskSlug:     call.TaskSlug,
		PayloadJSON:  string(payload),
		Error:        errText,
		AutoApproved: status == "sent",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: record slack auto-reply action: %v\n", err)
	}
}

func slackAutoReplyEnabled() bool {
	return envEnabled("FLOW_SLACK_AUTO_REPLY_ENABLED") && envEnabled("FLOW_SLACK_WRITES_ENABLED")
}

func slackAutoReplyEmoji() string {
	emoji := strings.Trim(strings.TrimSpace(os.Getenv("FLOW_SLACK_AUTO_REPLY_EMOJI")), ":")
	if emoji == "" {
		emoji = "eyes"
	}
	return emoji
}

func envEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func slackReadableMessageText(text string) string {
	text = strings.TrimSpace(text)
	text = slackAutoReplyMentionRE.ReplaceAllString(text, "@$1")
	return strings.Join(strings.Fields(text), " ")
}

func normalizeExternalText(text string) string {
	text = strings.ToLower(slackReadableMessageText(text))
	text = strings.Trim(text, " \t\r\n?.!,")
	return strings.Join(strings.Fields(text), " ")
}

func slackLooksLikeQuestion(text string) bool {
	normalized := normalizeExternalText(text)
	if normalized == "" {
		return false
	}
	if strings.Contains(text, "?") {
		return true
	}
	for _, prefix := range []string{"can ", "could ", "is ", "are ", "do ", "does ", "did ", "have ", "has ", "what ", "when ", "where ", "who ", "why ", "how "} {
		if strings.HasPrefix(normalized, prefix) || strings.Contains(normalized, " "+prefix) {
			return true
		}
	}
	return false
}

func slackQuestionAsksTaskStatus(text string) bool {
	normalized := normalizeExternalText(text)
	if normalized == "" {
		return false
	}
	for _, kw := range []string{"done", "finished", "finish", "finsihed", "finsih", "complete", "completed", "status", "progress", "working", "waiting", "blocked"} {
		if strings.Contains(normalized, kw) {
			return true
		}
	}
	return false
}

func slackQuestionAsksCurrentWork(text string) bool {
	normalized := normalizeExternalText(text)
	if normalized == "" {
		return false
	}
	return strings.Contains(normalized, "working on") &&
		(strings.Contains(normalized, "what ") || strings.Contains(normalized, "task"))
}

func slackTaskStatusAnswer(task *flowdb.Task, baseURL string) string {
	status := strings.ReplaceAll(task.Status, "-", " ")
	text := fmt.Sprintf("From Flow: task %q (`%s`) is %s.", task.Name, task.Slug, status)
	if task.WaitingOn.Valid {
		text += " It is waiting on: " + task.WaitingOn.String + "."
	}
	if baseURL != "" {
		text += " " + strings.TrimRight(baseURL, "/") + "/tasks/" + task.Slug
	}
	return text
}
