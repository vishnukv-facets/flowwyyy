package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flow/internal/productdb"
	"flow/internal/steering"
)

// attentionAutoAct is the autonomous twin of attentionAct: it applies the
// operator's autonomy policy to a card a per-channel steerer session just
// surfaced (via `flow attention surface`). The cascade auto-acts inline, but the
// session path writes the card and stops — so without this, session-surfaced
// cards never honor the operator's forward/make_task/reply autonomy. The CLI
// calls this (kind=attention-autoact, Target=feed id) right after a surface.
func (s *Server) attentionAutoAct(req actionRequest) (actionResponse, int) {
	id := strings.TrimSpace(req.Target)
	if id == "" {
		return actionResponse{OK: false, Message: "attention-autoact requires a feed item id (target)"}, http.StatusBadRequest
	}
	item, err := productdb.GetFeedItem(s.cfg.DB, id)
	if err != nil {
		return actionResponse{OK: false, Message: "feed item not found: " + id}, http.StatusNotFound
	}
	// Run off the request goroutine: an auto-reply hands off to a chat/agent
	// session, which can outlast the CLI's short POST timeout. The surface already
	// succeeded; gating is best-effort on top of it.
	go s.autoActOnSurfacedCard(item)
	return actionResponse{OK: true, Message: "autonomy gate applied to " + id}, http.StatusOK
}

// autoActOnSurfacedCard applies the operator's autonomy policy to a freshly
// surfaced session card — the session path's equivalent of the cascade's inline
// auto-act. Forward/make_task go through ApplyActionAuto; an opted-in reply is
// posted by the channel's own per-channel chat (Slack) or the gh agent (GitHub).
// Per the operator's request, a card that is BOTH a reply and a match sends the
// reply AND forwards to the task. Autonomous acts record NO attention_feedback
// row (audit is the steering trace + the agent's send confirmation), so they
// cannot inflate the calibration they gate on — same invariant as the cascade.
// Best-effort: any miss leaves the card surfaced for manual handling.
func (s *Server) autoActOnSurfacedCard(item productdb.FeedItem) {
	if s == nil || s.cfg.DB == nil {
		return
	}
	// Only gate a still-open card. A re-surface or an already-acted card must not
	// double-forward / double-send.
	if strings.TrimSpace(item.Status) != "new" {
		return
	}
	pol := steering.AutonomyFnWithFeedback(s.cfg.DB, steering.AutonomyFromEnv)()
	cal, _ := steering.LoadConfidenceCalibrator(s.cfg.DB)
	// gate returns the calibrated confidence and whether the operator's policy
	// allows this action at it — mirroring the cascade's calibrated gate.
	gate := func(a steering.Action) (float64, bool) {
		conf := item.Confidence
		if cal != nil {
			conf, _ = cal.Calibrate(a, item.Confidence)
		}
		return conf, pol.Allow(a, conf)
	}
	action := steering.Action(strings.TrimSpace(item.SuggestedAction))

	// Reply (opt-in, critical risk): post via the channel's own chat / gh agent.
	if action == steering.ActionReply && strings.TrimSpace(item.Draft) != "" {
		if conf, ok := gate(steering.ActionReply); ok {
			s.autoSendReply(item, conf)
		}
	}

	// Forward to the matched task (and the reply→forward chain). When no task is
	// matched but the verdict is make_task, create the backlog task instead.
	kbDir := filepath.Join(s.cfg.FlowRoot, "kb")
	if strings.TrimSpace(item.MatchedTask) != "" {
		if conf, ok := gate(steering.ActionForward); ok {
			if err := steering.ApplyActionAuto(context.Background(), s.cfg.DB, item, steering.ActionForward, kbDir, pol, conf); err != nil {
				fmt.Fprintf(os.Stderr, "[steering] session-card auto-forward %s failed: %v\n", item.ID, err)
			} else {
				fmt.Fprintf(os.Stderr, "[steering] auto-forwarded session card %s to %s (calibrated %.2f)\n", item.ID, item.MatchedTask, conf)
				s.publishUIChange("attention")
			}
		}
	} else if action == steering.ActionMakeTask {
		if conf, ok := gate(steering.ActionMakeTask); ok {
			if err := steering.ApplyActionAuto(context.Background(), s.cfg.DB, item, steering.ActionMakeTask, kbDir, pol, conf); err != nil {
				fmt.Fprintf(os.Stderr, "[steering] session-card auto-make-task %s failed: %v\n", item.ID, err)
			} else {
				fmt.Fprintf(os.Stderr, "[steering] auto-created task from session card %s (calibrated %.2f)\n", item.ID, conf)
				s.publishUIChange("attention")
			}
		}
	}
}

// autoSendReply posts an opted-in autonomous reply through the right path for the
// connector: Slack goes through the channel's own per-channel steerer chat (it
// holds the thread memory + Slack MCP and posts in-thread); GitHub goes through
// the gh agent. Neither records an attention_feedback row. If no Slack chat
// exists yet, the reply is left for manual send rather than spinning a
// context-blind ephemeral session without operator approval.
func (s *Server) autoSendReply(item productdb.FeedItem, conf float64) {
	switch strings.TrimSpace(item.Source) {
	case "slack":
		if s.terminals == nil {
			return
		}
		handled, err := s.postApprovedReplyViaChat(item, item.Draft, "")
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "[steering] auto-reply via channel chat %s failed: %v\n", item.ID, err)
		case handled:
			fmt.Fprintf(os.Stderr, "[steering] auto-sent reply via channel chat for %s (calibrated %.2f)\n", item.ID, conf)
		default:
			fmt.Fprintf(os.Stderr, "[steering] auto-reply skipped for %s: no per-channel chat (left for manual send)\n", item.ID)
		}
	case "github":
		go func() {
			bctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := steering.SendReplyViaAgent(bctx, s.cfg.DB, item, item.Draft, ""); err != nil {
				fmt.Fprintf(os.Stderr, "[steering] auto-reply via gh agent %s failed: %v\n", item.ID, err)
				return
			}
			fmt.Fprintf(os.Stderr, "[steering] auto-sent GitHub reply via gh agent for %s (calibrated %.2f)\n", item.ID, conf)
			s.publishUIChange("attention")
		}()
	}
}
