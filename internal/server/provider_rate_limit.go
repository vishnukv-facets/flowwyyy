package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

type providerLimitHold struct {
	Provider string
	Until    time.Time
	Reason   string
}

type queuedOpenTaskPayload struct {
	Slug string `json:"slug"`
}

func (s *Server) providerRateLimitHold(provider string) (providerLimitHold, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		provider = "claude"
	}
	out := s.readProviderUsage(provider)
	if !out.Available || !out.Limited || strings.TrimSpace(out.LimitReset) == "" {
		return providerLimitHold{}, false
	}
	until, err := time.Parse(time.RFC3339, strings.TrimSpace(out.LimitReset))
	if err != nil || !until.After(time.Now()) {
		return providerLimitHold{}, false
	}
	return providerLimitHold{
		Provider: provider,
		Until:    until,
		Reason:   "provider rate limit reached",
	}, true
}

func (s *Server) anyProviderRateLimitHold() (providerLimitHold, bool) {
	var hold providerLimitHold
	for _, provider := range []string{"claude", "codex"} {
		h, ok := s.providerRateLimitHold(provider)
		if !ok {
			continue
		}
		if hold.Until.IsZero() || h.Until.After(hold.Until) {
			hold = h
		}
	}
	return hold, !hold.Until.IsZero()
}

func (s *Server) providerBackfillHoldUntil() (time.Time, bool) {
	hold, ok := s.anyProviderRateLimitHold()
	if !ok {
		return time.Time{}, false
	}
	s.scheduleRateLimitQueueDrain(hold.Until)
	return hold.Until, true
}

func (s *Server) taskProviderRateLimitHold(slug string) (providerLimitHold, bool) {
	if s == nil || s.cfg.DB == nil {
		return providerLimitHold{}, false
	}
	task, err := flowdb.GetTask(s.cfg.DB, slug)
	if err != nil || task == nil {
		return providerLimitHold{}, false
	}
	provider := task.SessionProvider
	if provider == "" {
		provider = "claude"
	}
	return s.providerRateLimitHold(provider)
}

func (s *Server) HoldSlackEvent(ctx context.Context, ev monitor.InboundEvent) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}
	hold, ok := s.anyProviderRateLimitHold()
	if !ok {
		return false, nil
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return false, err
	}
	if err := s.enqueueRateLimitAction(flowdb.RateLimitQueueSlackEvent, hold, payload); err != nil {
		return false, err
	}
	log.Printf("flow monitor: queued Slack event until %s because %s is rate-limited", hold.Until.Format(time.RFC3339), hold.Provider)
	return true, nil
}

func (s *Server) HoldGitHubEvent(ctx context.Context, ev monitor.GitHubEvent) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}
	hold, ok := s.anyProviderRateLimitHold()
	if !ok {
		return false, nil
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return false, err
	}
	if err := s.enqueueRateLimitAction(flowdb.RateLimitQueueGitHubEvent, hold, payload); err != nil {
		return false, err
	}
	log.Printf("flow monitor: queued GitHub event until %s because %s is rate-limited", hold.Until.Format(time.RFC3339), hold.Provider)
	return true, nil
}

func (s *Server) enqueueOpenTaskAfter(slug string, hold providerLimitHold) error {
	payload, err := json.Marshal(queuedOpenTaskPayload{Slug: strings.TrimSpace(slug)})
	if err != nil {
		return err
	}
	if err := s.enqueueRateLimitAction(flowdb.RateLimitQueueOpenTask, hold, payload); err != nil {
		return err
	}
	log.Printf("flow monitor: queued automatic open for %s until %s because %s is rate-limited", slug, hold.Until.Format(time.RFC3339), hold.Provider)
	return nil
}

func (s *Server) enqueueRateLimitAction(kind string, hold providerLimitHold, payload []byte) error {
	if s == nil || s.cfg.DB == nil {
		return errors.New("rate-limit queue unavailable")
	}
	if hold.Until.IsZero() {
		hold.Until = time.Now().Add(5 * time.Minute)
	}
	_, err := flowdb.EnqueueRateLimitQueue(s.cfg.DB, kind, hold.Provider, payload, hold.Until.UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	s.scheduleRateLimitQueueDrain(hold.Until)
	s.publishUIChange("tasks")
	return nil
}

func (s *Server) scheduleNextRateLimitQueueDrain() {
	if s == nil || s.cfg.DB == nil {
		return
	}
	next, ok, err := flowdb.NextRateLimitQueueRunAfter(s.cfg.DB)
	if err != nil || !ok {
		return
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(next))
	if err != nil {
		t = time.Now().Add(time.Minute)
	}
	s.scheduleRateLimitQueueDrain(t)
}

func (s *Server) scheduleRateLimitQueueDrain(at time.Time) {
	if s == nil {
		return
	}
	delay := time.Until(at)
	if delay < 0 {
		delay = 0
	}
	delay += 250 * time.Millisecond
	s.rateLimitQueueMu.Lock()
	defer s.rateLimitQueueMu.Unlock()
	if s.rateLimitQueueTimer != nil && !s.rateLimitQueueAt.IsZero() && s.rateLimitQueueAt.Before(at) {
		return
	}
	if s.rateLimitQueueTimer != nil {
		s.rateLimitQueueTimer.Stop()
	}
	s.rateLimitQueueAt = at
	s.rateLimitQueueTimer = time.AfterFunc(delay, func() {
		s.drainRateLimitQueue(context.Background())
	})
}

func (s *Server) drainRateLimitQueue(ctx context.Context) {
	if s == nil || s.cfg.DB == nil {
		return
	}
	if !s.rateLimitDrainMu.TryLock() {
		return
	}
	defer s.rateLimitDrainMu.Unlock()

	s.rateLimitQueueMu.Lock()
	s.rateLimitQueueAt = time.Time{}
	s.rateLimitQueueTimer = nil
	s.rateLimitQueueMu.Unlock()

	if hold, ok := s.anyProviderRateLimitHold(); ok {
		s.scheduleRateLimitQueueDrain(hold.Until)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	items, err := flowdb.ListReadyRateLimitQueue(s.cfg.DB, now, 100)
	if err != nil {
		log.Printf("flow monitor: load rate-limit queue: %v", err)
		s.scheduleRateLimitQueueDrain(time.Now().Add(time.Minute))
		return
	}
	for _, item := range items {
		select {
		case <-ctx.Done():
			s.scheduleRateLimitQueueDrain(time.Now().Add(time.Minute))
			return
		default:
		}
		if hold, ok := s.anyProviderRateLimitHold(); ok {
			_ = flowdb.RescheduleRateLimitQueue(s.cfg.DB, item.ID, hold.Until.UTC().Format(time.RFC3339), hold.Reason)
			s.scheduleRateLimitQueueDrain(hold.Until)
			return
		}
		if err := s.replayRateLimitQueueItem(ctx, item); err != nil {
			next := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
			_ = flowdb.RescheduleRateLimitQueue(s.cfg.DB, item.ID, next, err.Error())
			log.Printf("flow monitor: replay rate-limit queue row %d: %v", item.ID, err)
			continue
		}
		_ = flowdb.AckRateLimitQueue(s.cfg.DB, item.ID)
	}
	s.scheduleNextRateLimitQueueDrain()
}

func (s *Server) replayRateLimitQueueItem(ctx context.Context, item flowdb.RateLimitQueueItem) error {
	ctx = monitor.WithConnectorHoldBypass(ctx)
	switch item.Kind {
	case flowdb.RateLimitQueueSlackEvent:
		if s.slackDispatcher == nil {
			return errors.New("slack dispatcher unavailable")
		}
		var ev monitor.InboundEvent
		if err := json.Unmarshal([]byte(item.PayloadJSON), &ev); err != nil {
			return fmt.Errorf("decode slack event: %w", err)
		}
		return s.slackDispatcher.Dispatch(ctx, ev)
	case flowdb.RateLimitQueueGitHubEvent:
		if s.githubDispatcher == nil {
			return errors.New("github dispatcher unavailable")
		}
		var ev monitor.GitHubEvent
		if err := json.Unmarshal([]byte(item.PayloadJSON), &ev); err != nil {
			return fmt.Errorf("decode github event: %w", err)
		}
		return s.githubDispatcher.Dispatch(ctx, ev)
	case flowdb.RateLimitQueueOpenTask:
		var payload queuedOpenTaskPayload
		if err := json.Unmarshal([]byte(item.PayloadJSON), &payload); err != nil {
			return fmt.Errorf("decode open task: %w", err)
		}
		return s.openQueuedTask(payload.Slug)
	default:
		return fmt.Errorf("unknown rate-limit queue kind %q", item.Kind)
	}
}

func (s *Server) openQueuedTask(slug string) error {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return errors.New("queued task slug is empty")
	}
	if hold, ok := s.taskProviderRateLimitHold(slug); ok {
		return s.enqueueOpenTaskAfter(slug, hold)
	}
	opener := slackTaskOpener{server: s}
	if err := opener.openInUIWithoutRateLimitCheck(slug); err != nil {
		return err
	}
	if s.terminals != nil {
		s.terminals.flushWakes(slug)
	}
	return nil
}

func (s *Server) queueWakeAfterRateLimit(slug, prompt string, hold providerLimitHold) error {
	if s == nil || s.terminals == nil || s.terminals.wakes == nil {
		return nil
	}
	notBefore := hold.Until.UTC().Format(time.RFC3339)
	if err := s.terminals.wakes.pushAfter(slug, prompt, notBefore); err != nil {
		return err
	}
	s.terminals.scheduleWakeFlush(slug, hold.Until)
	return nil
}

func (s *Server) recheckProviderLimits() (actionResponse, int) {
	if s == nil || s.cfg.DB == nil {
		return actionResponse{OK: false, Message: "database unavailable"}, 500
	}
	s.drainRateLimitQueue(context.Background())
	pending, err := flowdb.CountPendingRateLimitQueue(s.cfg.DB)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, 500
	}
	if pending > 0 {
		return actionResponse{OK: true, Message: fmt.Sprintf("Provider limits rechecked; %d queued action(s) remain held.", pending)}, 200
	}
	return actionResponse{OK: true, Message: "Provider limits rechecked; no queued automation is held."}, 200
}

var _ monitor.ConnectorHoldGate = (*Server)(nil)
