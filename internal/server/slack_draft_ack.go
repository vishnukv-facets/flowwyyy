package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// slackDraftAckRunner is the package-level seam tests swap to capture the
// outgoing reaction + private reply without spinning up an httptest server. The
// production path constructs one writer per call (NewSlackWriter is cheap;
// it just reads env), reacts, then replies. Returning the first error
// short-circuits — reacting before replying matches the user-facing intent
// ("I see this, here's what I'm doing"), so if the reaction fails we don't
// proceed to a reply.
var slackDraftAckRunner = func(ctx context.Context, channel, ts, userID, threadTS, emoji, text string) error {
	writer := monitor.NewSlackWriter()
	if err := writer.AddReaction(ctx, channel, ts, emoji); err != nil {
		return fmt.Errorf("slack draft ack reaction: %w", err)
	}
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("slack draft ack target user required")
	}
	if err := writer.PostEphemeral(ctx, channel, userID, threadTS, text); err != nil {
		return fmt.Errorf("slack draft ack reply: %w", err)
	}
	return nil
}

// postSlackDraftAck adds a :eyes: reaction on the originating Slack message
// and posts a one-line private thread reply naming the drafted task slug.
func (s *Server) postSlackDraftAck(event flowdb.MonitorEvent, slug string) {
	text := slackDraftAckText(slug, monitor.FlowBaseURL())
	s.postSlackThreadAck(event, slug, "draft_ack", "slack draft ack", "FLOW_SLACK_DRAFT_EMOJI", text)
}

// postSlackWorkingAck tells the originating Slack thread that flow has
// started working on the approved item. This is intentionally separate from
// the later close-out / waiting notices: it gives immediate feedback after
// the user approves a Slack-origin item.
func (s *Server) postSlackWorkingAck(event flowdb.MonitorEvent, slug string) {
	text := slackWorkingAckText(slug, monitor.FlowBaseURL())
	s.postSlackThreadAck(event, slug, "working_ack", "slack working ack", "FLOW_SLACK_WORKING_EMOJI", text)
}

func (s *Server) postSlackThreadAck(event flowdb.MonitorEvent, slug, actionType, warningLabel, emojiEnv, text string) {
	origin, ok := monitor.SlackOriginFromEvent(event)
	if !ok {
		return
	}
	emoji := strings.Trim(strings.TrimSpace(os.Getenv(emojiEnv)), ":")
	if emoji == "" {
		emoji = "eyes"
	}
	_, threadTS := origin.PostTarget()
	userID := s.slackPrivateNoticeUserID(origin)
	payload, _ := json.Marshal(map[string]string{
		"channel":   origin.Channel,
		"user":      userID,
		"ts":        origin.TS,
		"thread_ts": threadTS,
		"emoji":     emoji,
		"text":      text,
		"task_slug": slug,
	})
	status := "sent"
	errText := ""
	if err := slackDraftAckRunner(context.Background(), origin.Channel, origin.TS, userID, threadTS, emoji, text); err != nil {
		status = "failed"
		errText = err.Error()
		fmt.Fprintf(os.Stderr, "warning: %s failed: %v\n", warningLabel, err)
	}
	if s != nil && s.cfg.DB != nil {
		taskSlug := ""
		if strings.TrimSpace(slug) != "" {
			if _, err := flowdb.GetTask(s.cfg.DB, slug); err == nil {
				taskSlug = slug
			}
		}
		if err := flowdb.RecordExternalAction(s.cfg.DB, flowdb.ExternalActionInput{
			Source:       "slack",
			EventID:      event.ID,
			ActionType:   actionType,
			Status:       status,
			TaskSlug:     taskSlug,
			PayloadJSON:  string(payload),
			Error:        errText,
			AutoApproved: status == "sent",
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: record %s action: %v\n", warningLabel, err)
		}
	}
}

// slackDraftAckText renders the reply. Pulled out as a pure function so
// tests can assert both URL-present and URL-absent shapes. Empty baseURL
// falls back to a slug-only message — still informative ("flow drafted
// task X"), just without a clickable link.
func slackDraftAckText(slug, baseURL string) string {
	if baseURL == "" {
		return fmt.Sprintf("flow drafted this as task `%s` (awaiting your approval).", slug)
	}
	return fmt.Sprintf("flow drafted this as task `%s` (awaiting your approval): %s/tasks/%s",
		slug, baseURL, slug)
}

func slackWorkingAckText(slug, baseURL string) string {
	if baseURL == "" {
		return fmt.Sprintf("flow is working on this as task `%s`.", slug)
	}
	return fmt.Sprintf("flow is working on this as task `%s`: %s/tasks/%s", slug, baseURL, slug)
}
