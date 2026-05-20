package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"flow/internal/flowdb"
	flowmonitor "flow/internal/monitor"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

type slackSocketListener struct {
	srv *Server

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func newSlackSocketListener(srv *Server) *slackSocketListener {
	return &slackSocketListener{srv: srv}
}

func (l *slackSocketListener) start() {
	if l == nil || !flowmonitor.SlackSocketModeEnabled() {
		return
	}
	appToken := flowmonitor.SlackAppToken()
	botToken := flowmonitor.SlackBotToken()
	if appToken == "" || botToken == "" {
		fmt.Fprintln(os.Stderr, "slack socket: disabled; set SLACK_APP_TOKEN and SLACK_BOT_TOKEN")
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.done = make(chan struct{})
	go l.loop(ctx, appToken, botToken)
}

func (l *slackSocketListener) stop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	cancel := l.cancel
	done := l.done
	l.cancel = nil
	l.done = nil
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (l *slackSocketListener) loop(ctx context.Context, appToken, botToken string) {
	defer close(l.done)
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	client := socketmode.New(api, socketmode.OptionDebug(monitorDebugEnabled()))
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.RunContext(ctx)
	}()
	fmt.Fprintln(os.Stderr, "slack socket: started")
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-errCh:
			if err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "slack socket: stopped: %v\n", err)
			}
			return
		case evt, ok := <-client.Events:
			if !ok {
				return
			}
			l.handleEvent(ctx, client, evt)
		}
	}
}

func (l *slackSocketListener) handleEvent(ctx context.Context, client *socketmode.Client, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeConnected:
		fmt.Fprintln(os.Stderr, "slack socket: connected")
		l.recordConnected()
	case socketmode.EventTypeConnectionError, socketmode.EventTypeInvalidAuth, socketmode.EventTypeIncomingError:
		fmt.Fprintf(os.Stderr, "slack socket: %s: %v\n", evt.Type, evt.Data)
	case socketmode.EventTypeEventsAPI:
		if evt.Request != nil {
			if err := client.Ack(*evt.Request); err != nil {
				fmt.Fprintf(os.Stderr, "slack socket: ack: %v\n", err)
			}
		}
		event, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			fmt.Fprintf(os.Stderr, "slack socket: ignored unexpected Events API payload: %T\n", evt.Data)
			return
		}
		eventType := slackEventType(event)
		fmt.Fprintf(os.Stderr, "slack socket: event type=%s payload=%T\n", eventType, event.InnerEvent.Data)
		kept, newCount, err := l.srv.handleSlackSocketEventsAPI(ctx, event)
		if err != nil {
			fmt.Fprintf(os.Stderr, "slack socket: store event type=%s: %v\n", eventType, err)
			return
		}
		if kept == 0 {
			fmt.Fprintf(os.Stderr, "slack socket: ignored event type=%s payload=%T\n", eventType, event.InnerEvent.Data)
			return
		}
		if newCount > 0 {
			fmt.Fprintf(os.Stderr, "slack socket: stored event type=%s kept=%d new=%d\n", eventType, kept, newCount)
		}
	}
}

func slackEventType(event slackevents.EventsAPIEvent) string {
	if event.InnerEvent.Type != "" {
		return event.InnerEvent.Type
	}
	if event.Type != "" {
		return event.Type
	}
	return strings.TrimPrefix(fmt.Sprintf("%T", event.InnerEvent.Data), "*")
}

func (l *slackSocketListener) recordConnected() {
	if l == nil || l.srv == nil || l.srv.cfg.DB == nil {
		return
	}
	state, err := flowdb.RecordMonitorSyncEnd(l.srv.cfg.DB, "slack", "ok", "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "slack socket: record connected: %v\n", err)
		return
	}
	l.srv.publishMonitorSync(state)
}

func (s *Server) handleSlackSocketEventsAPI(ctx context.Context, event slackevents.EventsAPIEvent) (int, int, error) {
	if ctx.Err() != nil {
		return 0, 0, ctx.Err()
	}
	if s == nil || s.cfg.DB == nil {
		return 0, 0, nil
	}
	inputs := flowmonitor.SlackEventsAPIEventInputsForUsers(event, s.slackMentionUserIDs(ctx))
	if len(inputs) == 0 {
		return 0, 0, nil
	}
	inputs = s.enrichSlackEventInputs(ctx, event, inputs)
	if state, err := flowdb.RecordMonitorSyncStart(s.cfg.DB, "slack"); err == nil {
		s.publishMonitorSync(state)
	}
	poller := flowmonitor.Poller{
		DB:         s.cfg.DB,
		OnNewEvent: s.publishInboxItem,
	}
	kept, newCount, err := poller.StoreSlackEvents(inputs)
	if err == nil {
		s.recordSlackExternalMessagesAndMaybeReply(ctx, event, inputs)
	}
	status := "ok"
	errMsg := ""
	if err != nil {
		status = "error"
		errMsg = err.Error()
	}
	if state, endErr := flowdb.RecordMonitorSyncEnd(s.cfg.DB, "slack", status, errMsg); endErr == nil {
		s.publishMonitorSync(state)
	}
	return kept, newCount, err
}
