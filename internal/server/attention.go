package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
	"flow/internal/steering"
)

// attentionMakeTask spawns a flow task from a feed item (operator-initiated →
// manual=true bypasses the autonomy gate) and marks the row acted+linked. It is
// a package var so tests can stub the shell-out spawn.
var attentionMakeTask = func(s *Server, item flowdb.FeedItem) error {
	return steering.ApplyAction(context.Background(), s.cfg.DB, item, steering.ActionMakeTask, steering.DefaultAutonomy(), true)
}

// attentionRequestHandoff asks a matched task's agent to accept/decline
// ownership before the feed card is resolved.
var attentionRequestHandoff = func(s *Server, item flowdb.FeedItem) (flowdb.AttentionHandoff, error) {
	return steering.RequestHandoff(context.Background(), s.cfg.DB, item, "attention-router")
}

// attentionStartSession attaches the just-made task to a server-managed PTY so
// its agent session streams into the UI ("start the session"). Package var so
// tests can stub the PTY attach.
var attentionStartSession = func(s *Server, slug string) error {
	return (&slackTaskOpener{server: s}).OpenInUI(slug)
}

// startSessionAsync opens/resumes a task's live session WITHOUT blocking the
// caller. Spawning a Claude/Codex session (PTY + prime) routinely takes longer
// than the UI's 30s RPC timeout, and the agent posts/acts on its own once the
// session is up — so an action that only needs the agent to *eventually* run
// must return immediately and let the open proceed in the background. Errors are
// logged, never surfaced: the task already exists and the operator can open it
// manually. This is what keeps "Send reply" from timing out while Claude boots.
func (s *Server) startSessionAsync(slug string) {
	go func() {
		if err := attentionStartSession(s, slug); err != nil {
			fmt.Fprintf(os.Stderr, "attention: background session open %s: %v\n", slug, err)
		}
	}()
}

// handleAttention serves GET /api/attention[?status=new|acted|dismissed|all]
// (default: new). 'all' returns every row.
func (s *Server) handleAttention(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = "new"
	}
	if status == "all" {
		status = ""
	}
	if _, err := flowdb.ExpireAttentionHandoffs(s.cfg.DB, time.Now().UTC().Format(time.RFC3339)); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	items, err := flowdb.ListFeedItems(s.cfg.DB, status)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	// Warm the distinct channel/author names concurrently so the per-row view
	// loop below hits the cache instead of making one serial Slack API call per
	// row (the cold-render stall).
	users := make([]string, 0, len(items))
	chans := make([]string, 0, len(items))
	for _, it := range items {
		if it.Source == "github" {
			continue
		}
		users = append(users, it.Author)
		// attentionItemView CleanText's the summary/reason/draft — warm every
		// mention in them too so those resolve from cache, not serial network.
		users = append(users, monitor.MentionedUserIDs(it.Summary)...)
		users = append(users, monitor.MentionedUserIDs(it.Reason)...)
		users = append(users, monitor.MentionedUserIDs(it.Draft)...)
		chans = append(chans, it.Channel)
	}
	s.warmSlackNames(r.Context(), users, chans)
	// Warm permalinks concurrently too: the per-row lookup is CachedPermalink (no
	// network), so without this a cold tab (e.g. dismissed, hundreds of rows in
	// many channels) resolved one chat.getPermalink each — serially — and stalled
	// for tens of seconds. Warm does them in parallel, time-boxed; unresolved rows
	// fall back to the deep-link and resolve on a later load.
	s.warmSlackPermalinks(r.Context(), items)
	views := make([]AttentionItemView, 0, len(items))
	for _, it := range items {
		views = append(views, s.attentionItemView(r.Context(), it))
	}
	writeJSON(w, views)
}

// warmSlackPermalinks concurrently pre-resolves the (channel, ts) permalinks for
// the feed rows so the per-row CachedPermalink lookups are all cache hits. Derives
// channel/ts the same way attentionItemView does (column, else thread_key).
func (s *Server) warmSlackPermalinks(ctx context.Context, items []flowdb.FeedItem) {
	if s.slackPermalinker == nil || len(items) == 0 {
		return
	}
	chans := make([]string, 0, len(items))
	tss := make([]string, 0, len(items))
	for _, it := range items {
		if it.Source == "github" {
			continue
		}
		ch, ts := it.Channel, it.TS
		if ch == "" || ts == "" {
			tkChan, tkTS := splitThreadKey(it.ThreadKey)
			if ch == "" {
				ch = tkChan
			}
			if ts == "" {
				ts = tkTS
			}
		}
		chans = append(chans, ch)
		tss = append(tss, ts)
	}
	wctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	s.slackPermalinker.Warm(wctx, chans, tss)
}

// warmSlackNames concurrently pre-resolves the given user/channel IDs (bounded,
// time-boxed) so a subsequent batch of per-row name lookups is all cache hits.
// No-op when no resolver is configured or nothing to warm.
func (s *Server) warmSlackNames(ctx context.Context, users, chans []string) {
	if s.nameResolver == nil || (len(users) == 0 && len(chans) == 0) {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	s.nameResolver.Warm(wctx, users, chans)
}

// attentionItemView maps a feed row to its UI shape. As a display safety-net it
// runs the operator-facing text fields (summary, reason, draft) through the
// Slack name resolver so any residual <@U…> markup renders as a name; the
// ingest-time cleaning (Cascade.TextClean) is the primary fix for new items.
// The resolver is nil-safe — a nil resolver leaves the text unchanged.
func (s *Server) attentionItemView(ctx context.Context, it flowdb.FeedItem) AttentionItemView {
	summary, reason, draft := it.Summary, it.Reason, it.Draft
	if s.nameResolver != nil {
		summary = s.nameResolver.CleanText(ctx, summary)
		reason = s.nameResolver.CleanText(ctx, reason)
		draft = s.nameResolver.CleanText(ctx, draft)
	}
	summary = steering.SanitizeOperatorText(summary)
	reason = steering.SanitizeOperatorText(reason)
	draft = steering.SanitizeOperatorText(draft)
	v := AttentionItemView{
		ID: it.ID, Source: it.Source, ThreadKey: it.ThreadKey, Summary: summary,
		SuggestedAction: it.SuggestedAction, MatchedTask: it.MatchedTask,
		SuggestedProject: it.SuggestedProject, SuggestedPriority: it.SuggestedPriority,
		Urgency: it.Urgency, IsVIP: it.IsVIP, Confidence: it.Confidence,
		Draft: draft, Reason: reason, Status: it.Status, LinkedTask: it.LinkedTask,
		Retriaging: it.RetriagingAt != "",
		CreatedAt:  it.CreatedAt, ActedAt: it.ActedAt,
	}
	v.Channel, v.ChannelType, v.Author = it.Channel, it.ChannelType, it.Author
	if it.Source == "github" {
		v.ChannelName = it.Channel // owner/repo, already human
		v.AuthorName = it.Author   // login
		v.Permalink = it.URL
	} else {
		// Channel/ts may be empty on items captured before those columns
		// existed — every item still carries thread_key "<channel>:<ts>", so
		// derive from it. This makes the channel name + permalink work for ALL
		// Slack items, old and new.
		ch, ts := it.Channel, it.TS
		if ch == "" || ts == "" {
			tkChan, tkTS := splitThreadKey(it.ThreadKey)
			if ch == "" {
				ch = tkChan
			}
			if ts == "" {
				ts = tkTS
			}
		}
		if v.Channel == "" {
			v.Channel = ch // so the UI's channel_name || channel fallback shows something
		}
		if s.nameResolver != nil {
			v.ChannelName = s.nameResolver.ChannelName(ctx, ch)
			v.AuthorName = s.nameResolver.UserName(ctx, it.Author)
		}
		// DMs have no channel name — label by the person instead.
		if v.ChannelName == "" && (it.ChannelType == "im" || it.ChannelType == "mpim") {
			if v.AuthorName != "" {
				v.ChannelName = "DM · " + v.AuthorName
			} else {
				v.ChannelName = "Direct message"
			}
		}
		// Prefer a real https permalink — but CACHE-ONLY here (warmSlackPermalinks
		// did the network resolution concurrently up front). A per-row network
		// getPermalink would stall a cold tab for tens of seconds. On a cache miss,
		// fall back to the slack:// deep link (and it resolves on a later load).
		v.Permalink = s.slackPermalinker.CachedPermalink(ch, ts)
		if v.Permalink == "" {
			v.Permalink = connectorPermalink(it.Source, it.TeamID, ch, ts, it.URL)
		}
	}
	v.Why = s.attentionWhyView(ctx, it)
	v.ActionPreviews = attentionActionPreviews(it)
	if h, ok, err := flowdb.LatestAttentionHandoffForFeed(s.cfg.DB, it.ID); err == nil && ok {
		v.Handoff = attentionHandoffView(h)
	}
	return v
}

func attentionHandoffView(h flowdb.AttentionHandoff) *AttentionHandoffView {
	return &AttentionHandoffView{
		ID:               h.ID,
		FeedItemID:       h.FeedItemID,
		Sender:           h.Sender,
		Receiver:         h.Receiver,
		RequestedVerdict: h.RequestedVerdict,
		Status:           h.Status,
		Reason:           h.Reason,
		RequestedAt:      h.RequestedAt,
		ExpiresAt:        h.ExpiresAt,
		RespondedAt:      h.RespondedAt,
	}
}

func (s *Server) attentionWhyView(ctx context.Context, it flowdb.FeedItem) AttentionWhyView {
	v := AttentionWhyView{
		Source:            it.Source,
		Reason:            steering.SanitizeOperatorText(it.Reason),
		Confidence:        it.Confidence,
		SuggestedProject:  it.SuggestedProject,
		SuggestedPriority: it.SuggestedPriority,
	}
	if strings.TrimSpace(it.ContextJSON) != "" {
		var pack steering.ThreadContext
		if err := json.Unmarshal([]byte(it.ContextJSON), &pack); err != nil {
			v.FetchStatus = "invalid"
			v.FetchError = err.Error()
		} else {
			if pack.Source != "" {
				v.Source = pack.Source
			}
			v.ContextSummary = pack.Summary
			v.FetchStatus = pack.FetchStatus
			v.FetchError = pack.FetchError
			v.Participants = pack.Participants
			v.EvidenceCount = contextEvidenceCount(pack)
			if pack.Parent != nil {
				v.ParentPreview = previewText(pack.Parent.Text, 180)
			}
			for i := len(pack.Messages) - 1; i >= 0; i-- {
				if text := previewText(pack.Messages[i].Text, 180); text != "" {
					v.LatestPreview = text
					break
				}
			}
		}
	}
	if matched := strings.TrimSpace(it.MatchedTask); matched != "" {
		v.MatchedTask = s.attentionTaskMatch(ctx, matched)
	}
	if s.cfg.DB != nil {
		if tr, err := flowdb.GetSteeringTraceByFeedItem(s.cfg.DB, it.ID); err == nil {
			v.StageReached = tr.StageReached
			v.Stage1Relevant = tr.Stage1Relevant
			v.StageAction, v.StageConfidence = attentionStageOutcome(tr)
		}
	}
	return v
}

func (s *Server) attentionTaskMatch(_ context.Context, slug string) *AttentionTaskMatchView {
	v := &AttentionTaskMatchView{Slug: slug}
	if s.cfg.DB == nil {
		return v
	}
	task, err := flowdb.GetTask(s.cfg.DB, slug)
	if err != nil {
		return v
	}
	v.Name = task.Name
	v.Status = task.Status
	v.Priority = task.Priority
	v.SessionProvider = task.SessionProvider
	if task.ProjectSlug.Valid {
		v.ProjectSlug = task.ProjectSlug.String
	}
	return v
}

func contextEvidenceCount(pack steering.ThreadContext) int {
	n := 0
	if pack.Parent != nil && strings.TrimSpace(pack.Parent.Text) != "" {
		n++
	}
	for _, msg := range pack.Messages {
		if strings.TrimSpace(msg.Text) != "" {
			n++
		}
	}
	return n
}

func attentionStageOutcome(t flowdb.SteeringTrace) (string, float64) {
	switch {
	case t.Stage3Action != "":
		return t.Stage3Action, t.Stage3Confidence
	case t.FinalAction != "":
		return t.FinalAction, t.FinalConfidence
	case t.Stage2Action != "":
		return t.Stage2Action, t.Stage2Confidence
	default:
		return "", 0
	}
}

func attentionActionPreviews(it flowdb.FeedItem) []AttentionActionPreview {
	if it.Status != "new" {
		return nil
	}
	matched := strings.TrimSpace(it.MatchedTask)
	source := strings.TrimSpace(it.Source)
	if source == "" {
		source = "source"
	}
	out := []AttentionActionPreview{
		{
			Action:      "make_task",
			Label:       "Make task",
			Description: "Creates a backlog task from this card and links the card to it.",
			Primary:     attentionActionPrimary(it, "make_task"),
		},
		{
			Action:      "make_task_start",
			Label:       "Make task & start",
			Description: "Creates the task and starts its agent session in the UI.",
		},
		{
			Action:      "capture_kb",
			Label:       "Save to KB",
			Description: "Records the durable knowledge in this card into your KB (kb/*.md) via an agent — no task created.",
			Primary:     attentionActionPrimary(it, "capture_kb"),
		},
	}
	if matched != "" {
		out = append(out, AttentionActionPreview{
			Action:      "confirm_handoff",
			Label:       "Ask task agent",
			Target:      matched,
			Description: "Asks the matched task's agent to accept or decline this handoff before forwarding.",
		})
		out = append(out, AttentionActionPreview{
			Action:      "forward",
			Label:       "Forward",
			Target:      matched,
			Description: "Adds the context to the matched task and wakes that task's session.",
			Primary:     attentionActionPrimary(it, "forward"),
		})
	}
	if strings.TrimSpace(it.Draft) != "" {
		desc := "Posts the approved reply to the " + source + " thread through an agent session."
		if matched != "" {
			desc = "Hands the approved reply to the matched task's agent session to post."
		}
		out = append(out, AttentionActionPreview{
			Action:      "send_reply",
			Label:       "Send reply",
			Target:      "source thread",
			Description: desc,
			Primary:     attentionActionPrimary(it, "reply") || attentionActionPrimary(it, "send_reply"),
		})
	}
	out = append(out,
		AttentionActionPreview{
			Action:      "dismiss",
			Label:       "Dismiss",
			Description: "Marks this card handled without contacting the source or creating work.",
		},
		AttentionActionPreview{
			Action:      "retriage",
			Label:       "Re-triage",
			Description: "Re-runs the cascade with the latest source context and task evidence.",
		},
	)
	if strings.TrimSpace(it.Channel) != "" {
		out = append(out, AttentionActionPreview{
			Action:      "mute_channel",
			Label:       "Mute channel",
			Target:      it.Channel,
			Description: "Dismisses matching open cards and suppresses future cards from this channel.",
			Destructive: true,
		})
	}
	if strings.TrimSpace(it.Author) != "" {
		out = append(out, AttentionActionPreview{
			Action:      "mute_sender",
			Label:       "Mute sender",
			Target:      it.Author,
			Description: "Dismisses matching open cards and suppresses future cards from this sender.",
			Destructive: true,
		})
	}
	out = append(out, AttentionActionPreview{
		Action:      "mute_thread",
		Label:       "Mute thread",
		Target:      it.ThreadKey,
		Description: "Dismisses this thread and suppresses future cards from the same thread.",
		Destructive: true,
	})
	return out
}

func attentionActionPrimary(it flowdb.FeedItem, action string) bool {
	got := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(it.SuggestedAction)), "-", "_")
	want := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(action)), "-", "_")
	return got == want
}

func previewText(text string, max int) string {
	text = strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if max <= 0 || len(text) <= max {
		return text
	}
	if max <= 3 {
		return text[:max]
	}
	return strings.TrimSpace(text[:max-3]) + "..."
}

// splitThreadKey parses a Slack thread_key "<channel>:<thread_ts>". Slack
// channel IDs contain no ':' and the ts is the remainder, so SplitN(2) is safe.
func splitThreadKey(threadKey string) (channel, ts string) {
	if i := strings.Index(threadKey, ":"); i > 0 {
		return threadKey[:i], threadKey[i+1:]
	}
	return "", ""
}

var launchAttentionRetriage = func(s *Server, item flowdb.FeedItem) {
	go func(it flowdb.FeedItem) {
		bctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.cascade.Retriage(bctx, it); err != nil {
			fmt.Fprintf(os.Stderr, "attention: retriage %s: %v\n", it.ID, err)
		}
		_ = flowdb.SetFeedRetriaging(s.cfg.DB, it.ID, "")
		s.publishUIChange("attention")
	}(item)
}

// launchAttentionCorrectionRetriage re-triages a card after an operator
// correction. Same async shape as launchAttentionRetriage, but routes through
// RetriageFromCorrection so the corrected verdict NEVER auto-acts (always
// re-surfaces). The correction must already be persisted to thread memory.
var launchAttentionCorrectionRetriage = func(s *Server, item flowdb.FeedItem) {
	go func(it flowdb.FeedItem) {
		bctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.cascade.RetriageFromCorrection(bctx, it); err != nil {
			fmt.Fprintf(os.Stderr, "attention: correction retriage %s: %v\n", it.ID, err)
		}
		_ = flowdb.SetFeedRetriaging(s.cfg.DB, it.ID, "")
		s.publishUIChange("attention")
	}(item)
}

// attentionAct handles the attention-act action: make-task | forward | dismiss
// on a feed item (Target = feed id). Operator-initiated → manual=true bypasses
// the autonomy gate.
func (s *Server) attentionAct(req actionRequest) (actionResponse, int) {
	id := strings.TrimSpace(req.Target)
	if id == "" {
		return actionResponse{OK: false, Message: "attention-act requires a feed item id (target)"}, http.StatusBadRequest
	}
	item, err := flowdb.GetFeedItem(s.cfg.DB, id)
	if err != nil {
		return actionResponse{OK: false, Message: "feed item not found: " + id}, http.StatusNotFound
	}
	switch strings.ToLower(strings.TrimSpace(req.AttentionAction)) {
	case "dismiss":
		if err := steering.DismissFeed(s.cfg.DB, id); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "dismissed " + id}, http.StatusOK
	case "mute-channel", "mute-sender", "mute-thread":
		return s.attentionMute(req.AttentionAction, item)
	case "retriage", "re-triage":
		// Force a fresh cascade decision on this card (re-reads task context, etc.).
		// Runs in the background — deep triage outlasts the RPC timeout; the card
		// updates in place when it finishes.
		if s.cascade == nil {
			return actionResponse{OK: false, Message: "steering cascade is not running"}, http.StatusServiceUnavailable
		}
		_ = s.recordAttentionFeedback(item, "retriage", "retriaged", "")
		// Mark in-flight server-side so the spinner + disabled state survive a page
		// refresh and can't be double-fired; clear it when the async run finishes.
		_ = flowdb.SetFeedRetriaging(s.cfg.DB, id, time.Now().UTC().Format(time.RFC3339))
		s.publishUIChange("attention")
		launchAttentionRetriage(s, item)
		return actionResponse{OK: true, Message: "re-running triage — the card will update with the fresh decision"}, http.StatusOK
	case "correct":
		// The steerer misread this thread; the operator supplies the real context.
		// Store it on the thread's running understanding (authoritative ground truth
		// for future triage), optionally promote it to the KB, then re-triage with it
		// folded in. The correction-triggered re-triage NEVER auto-acts — it always
		// re-surfaces for the operator (operator decision).
		if s.cascade == nil {
			return actionResponse{OK: false, Message: "steering cascade is not running"}, http.StatusServiceUnavailable
		}
		text := strings.TrimSpace(req.CorrectionText)
		if text == "" {
			return actionResponse{OK: false, Message: "correction requires context text"}, http.StatusBadRequest
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if err := flowdb.AppendThreadOperatorCorrection(s.cfg.DB, item.ThreadKey, flowdb.ThreadOperatorCorrection{At: now, Text: text}); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		if req.Remember {
			// Distill into a durable cross-thread KB fact (best-effort, background —
			// runs the same capture agent as the capture-kb action).
			kbDir := filepath.Join(s.cfg.FlowRoot, "kb")
			go func(tk, src, txt string) {
				bctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				if err := steering.PromoteCorrectionToKB(bctx, tk, src, txt, kbDir); err != nil {
					fmt.Fprintf(os.Stderr, "attention: promote correction to KB: %v\n", err)
				}
			}(item.ThreadKey, item.Source, text)
		}
		// Under the session model the channel's own chat owns this thread's
		// understanding — teach IT directly (context_only) instead of a stateless
		// re-triage. Falls back to the cold correction-retriage when no chat exists.
		if handled, herr := s.postCorrectionToChat(item, text); herr != nil {
			return actionResponse{OK: false, Message: herr.Error()}, http.StatusInternalServerError
		} else if handled {
			s.publishUIChange("attention")
			msg := "told this channel's steering session — it'll update its read"
			if req.Remember {
				msg += "; saving it to your KB"
			}
			return actionResponse{OK: true, Message: msg}, http.StatusOK
		}
		_ = flowdb.SetFeedRetriaging(s.cfg.DB, id, now)
		s.publishUIChange("attention")
		launchAttentionCorrectionRetriage(s, item)
		msg := "got it — re-reading this thread with your context"
		if req.Remember {
			msg += "; saving it to your KB"
		}
		return actionResponse{OK: true, Message: msg}, http.StatusOK
	case "make-task", "make_task":
		if err := steering.ApplyAction(context.Background(), s.cfg.DB, item, steering.ActionMakeTask, steering.DefaultAutonomy(), true); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "made task from " + id}, http.StatusOK
	case "capture-kb", "capture_kb":
		// A hidden bypass agent distills the card into a durable KB fact and writes
		// it to kb/*.md — pure local filesystem work, so a headless `claude -p`
		// handles it (no connector MCP, unlike send-reply on Slack). The agent runs
		// for seconds-to-minutes, so resolve the card IMMEDIATELY (like make-task)
		// rather than waiting for the write — otherwise the operator clicks "Save to
		// KB" and the card just sits there. If the background write fails, re-surface
		// the card so they see it wasn't captured and can retry.
		kbDir := filepath.Join(s.cfg.FlowRoot, "kb")
		if err := flowdb.SetFeedItemActed(s.cfg.DB, id, "", time.Now().UTC().Format(time.RFC3339)); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		s.publishUIChange("attention")
		// Prefer the per-channel chat that SURFACED this card: it writes the fact in
		// its own context and so knows it captured (instead of a context-blind
		// ephemeral agent doing it invisibly). Fall back to the ephemeral agent when
		// the session model is off or no chat exists for the conversation.
		if handled, herr := s.captureKBViaChat(item, kbDir); herr != nil {
			fmt.Fprintf(os.Stderr, "attention: capture-kb via chat: %v (falling back to agent)\n", herr)
		} else if handled {
			_ = s.recordAttentionFeedback(item, "capture_kb", "captured", "")
			return actionResponse{OK: true, Message: "handed it to this conversation's steering chat to save to your KB"}, http.StatusOK
		}
		go func(it flowdb.FeedItem) {
			bctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := steering.CaptureKBViaAgent(bctx, s.cfg.DB, it, kbDir); err != nil {
				fmt.Fprintf(os.Stderr, "attention: capture-kb agent: %v\n", err)
				// The write failed — undo the optimistic resolve so the card returns.
				if rerr := flowdb.SetFeedItemStatus(s.cfg.DB, it.ID, "new", ""); rerr != nil {
					fmt.Fprintf(os.Stderr, "attention: re-surface capture-kb card %s: %v\n", it.ID, rerr)
				}
				s.publishUIChange("attention")
				return
			}
			s.publishUIChange("attention")
		}(item)
		return actionResponse{OK: true, Message: "saving to your KB via an agent — no task created"}, http.StatusOK
	case "make-task-start", "make_task_start":
		if err := attentionMakeTask(s, item); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		slug := steering.FeedTaskSlug(item)
		// Open the session in the background — starting the agent can outlast the
		// UI's RPC timeout, and the task is already created either way.
		s.startSessionAsync(slug)
		return actionResponse{OK: true, Message: "made task " + slug + " — starting its session"}, http.StatusOK
	case "forward":
		if err := steering.ApplyAction(context.Background(), s.cfg.DB, item, steering.ActionForward, steering.DefaultAutonomy(), true); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "forwarded " + id}, http.StatusOK
	case "confirm-handoff", "confirm_handoff", "handoff":
		h, err := attentionRequestHandoff(s, item)
		if err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "handoff requested from " + h.Receiver + " (" + h.ID + ")"}, http.StatusOK
	case "send-reply", "send_reply":
		// An AGENT sends the reply via its own MCP tools — the server never posts
		// to Slack/GitHub directly.
		text := strings.TrimSpace(req.ReplyText)
		if text == "" {
			text = strings.TrimSpace(item.Draft)
		}
		if text == "" {
			return actionResponse{OK: false, Message: "send-reply needs a draft or reply_text"}, http.StatusBadRequest
		}
		// Optional extra guidance: when present, the agent revises the draft per
		// these instructions before posting; empty → post the draft as-is.
		instructions := strings.TrimSpace(req.ReplyInstructions)
		// A reply is ALWAYS posted by the connector's own send path — never by
		// forwarding to a matched task and asking it to send. Slack posts via the
		// channel's own per-channel steerer chat (it holds the thread memory + Slack
		// MCP and posts in-thread); GitHub posts via the gh agent. A matched task may
		// lack the Slack MCP / PR context, and the operator wants the reply to come
		// straight from the channel/PR, so the match is irrelevant to HOW we send.
		if item.Source == "github" {
			// GitHub posts go through the `gh` CLI, which works in a headless
			// `claude -p` (no MCP needed). Run it in the background — claude -p can
			// outlast the UI's RPC timeout; the card flips to 'acted' once the agent
			// confirms it posted. No visible task is spawned.
			go func(it flowdb.FeedItem, reply, ins string) {
				bctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				if err := steering.SendReplyViaAgent(bctx, s.cfg.DB, it, reply, ins); err != nil {
					fmt.Fprintf(os.Stderr, "attention: send-reply via gh agent: %v\n", err)
					return
				}
				s.publishUIChange("attention")
			}(item, text, instructions)
			return actionResponse{OK: true, Message: "posting your reply via the gh agent — no task created"}, http.StatusOK
		}
		// Slack: the channel's own per-channel steerer chat posts the reply — it holds
		// the thread memory + Slack MCP and posts in-thread. There is no ephemeral
		// fallback: the session model is the master switch, so a Slack card only ever
		// exists (and is only repliable) while the session model is on and a chat
		// exists. If no chat is found, surface that rather than spinning a
		// context-blind ephemeral session.
		if s.terminals == nil {
			return actionResponse{OK: false, Message: "terminal hub is not running — cannot post the reply"}, http.StatusServiceUnavailable
		}
		handled, herr := s.postApprovedReplyViaChat(item, text, instructions)
		if herr != nil {
			return actionResponse{OK: false, Message: herr.Error()}, http.StatusInternalServerError
		}
		if !handled {
			return actionResponse{OK: false, Message: "no per-channel chat for this conversation — the per-channel session model must be on to post replies"}, http.StatusConflict
		}
		_ = s.recordAttentionFeedback(item, "send_reply", "approved", text)
		return actionResponse{OK: true, Message: "handed your reply to this channel's steering session — it's posting in-thread"}, http.StatusOK
	case "open-source", "open_source", "open-session", "open_session":
		action := strings.ToLower(strings.TrimSpace(req.AttentionAction))
		if err := s.recordAttentionFeedback(item, action, "opened", ""); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "recorded " + action + " for " + id}, http.StatusOK
	default:
		return actionResponse{OK: false, Message: "unknown attention action: " + req.AttentionAction}, http.StatusBadRequest
	}
}

func (s *Server) recordAttentionFeedback(item flowdb.FeedItem, finalAction, outcome, draftAfter string) error {
	return flowdb.RecordAttentionFeedback(s.cfg.DB, flowdb.AttentionFeedbackFromFeed(item, finalAction, outcome, draftAfter, time.Now().UTC().Format(time.RFC3339)))
}

// attentionMute records a permanent suppression from a feed card and sweeps any
// open cards it matches. The scope is derived from the verb: mute-channel uses
// the card's channel, mute-sender its author, mute-thread its thread key. The
// cascade re-reads steering_mutes per event (ConfigFn), so future matching
// events drop at Stage 0 with no restart.
func (s *Server) attentionMute(verb string, item flowdb.FeedItem) (actionResponse, int) {
	var scope, value, what string
	switch strings.ToLower(strings.TrimSpace(verb)) {
	case "mute-channel":
		scope, value, what = flowdb.MuteScopeChannel, item.Channel, "channel"
	case "mute-sender":
		scope, value, what = flowdb.MuteScopeAuthor, item.Author, "sender"
	case "mute-thread":
		scope, value, what = flowdb.MuteScopeThread, item.ThreadKey, "thread"
	default:
		return actionResponse{OK: false, Message: "unknown mute verb: " + verb}, http.StatusBadRequest
	}
	if strings.TrimSpace(value) == "" {
		return actionResponse{OK: false, Message: "this card has no " + what + " to mute"}, http.StatusBadRequest
	}
	swept, err := steering.MuteAndSweep(s.cfg.DB, scope, value)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	_ = s.recordAttentionFeedback(item, verb, "muted", "")
	return actionResponse{OK: true, Message: fmt.Sprintf("muted %s — %d card(s) cleared; future ones won't surface", what, swept)}, http.StatusOK
}

// handleAttentionTrace serves GET /api/attention/trace?since=&disposition=&limit=
// — the steering decision-log funnel + recent trace rows. Defaults: since = 24h
// ago, disposition = all, limit = 200.
func (s *Server) handleAttentionTrace(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	q := r.URL.Query()
	since := strings.TrimSpace(q.Get("since"))
	if since == "" {
		since = time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	}
	disposition := strings.TrimSpace(q.Get("disposition"))
	if disposition == "all" {
		disposition = ""
	}
	source := strings.TrimSpace(q.Get("source"))
	if source == "all" {
		source = ""
	}
	limit := 200
	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("limit"))); err == nil && n > 0 {
		limit = n
	}
	funnel, err := flowdb.SteeringFunnelSince(s.cfg.DB, since)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	traces, err := flowdb.ListSteeringTrace(s.cfg.DB, flowdb.TraceFilter{Disposition: disposition, Source: source, Since: since, Limit: limit})
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	// Warm distinct channel/author names concurrently so the per-row view loop
	// below is all cache hits — otherwise a cold render blocks on one serial
	// Slack API call per distinct ID (the load stall).
	users := make([]string, 0, len(traces))
	chans := make([]string, 0, len(traces))
	for _, t := range traces {
		if t.Source == "github" {
			continue
		}
		users = append(users, t.Author)
		users = append(users, monitor.MentionedUserIDs(t.TextPreview)...) // CleanText resolves these per row
		chans = append(chans, t.Channel)
	}
	s.warmSlackNames(r.Context(), users, chans)
	resp := AttentionTraceResponse{Funnel: steeringFunnelView(funnel), Items: make([]SteeringTraceView, 0, len(traces))}
	for _, t := range traces {
		resp.Items = append(resp.Items, s.steeringTraceView(r.Context(), t))
	}
	writeJSON(w, resp)
}

// handleAttentionDecision serves GET /api/attention/decision?feed_id=<id> —
// the resolved cascade-decision trace for one feed item (the "why"). 404 when
// the feed item predates tracing.
func (s *Server) handleAttentionDecision(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	feedID := strings.TrimSpace(r.URL.Query().Get("feed_id"))
	if feedID == "" {
		writeError(w, fmt.Errorf("feed_id required"), http.StatusBadRequest)
		return
	}
	t, err := flowdb.GetSteeringTraceByFeedItem(s.cfg.DB, feedID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	// Warm author + channel AND every @mention in the text in ONE concurrent
	// batch. Without the mentions, steeringTraceView → CleanText resolves each
	// mention with a serial UserInfo network call, which is what stalled this
	// modal. After the batch warm, the view resolves entirely from cache.
	users := append([]string{t.Author}, monitor.MentionedUserIDs(t.TextPreview)...)
	s.warmSlackNames(r.Context(), users, []string{t.Channel})
	writeJSON(w, s.steeringTraceView(r.Context(), t))
}

func steeringFunnelView(f flowdb.SteeringFunnel) SteeringFunnelView {
	return SteeringFunnelView{
		Observed:      f.Observed,
		DroppedStage0: f.DroppedStage0,
		DroppedCache:  f.DroppedCache,
		DroppedStage1: f.DroppedStage1,
		DroppedStage2: f.DroppedStage2,
		Surfaced:      f.Surfaced,
		Errors:        f.Errors,
	}
}

func (s *Server) steeringTraceView(ctx context.Context, t flowdb.SteeringTrace) SteeringTraceView {
	v := SteeringTraceView{
		ID: t.ID, CreatedAt: t.CreatedAt, Origin: t.Origin, Source: t.Source,
		Channel: t.Channel, ChannelType: t.ChannelType, Author: t.Author, ThreadKey: t.ThreadKey,
		TextPreview: t.TextPreview, Disposition: t.Disposition, StageReached: t.StageReached, DropReason: t.DropReason,
		Stage1Relevant: t.Stage1Relevant, Stage1Reason: t.Stage1Reason, Stage2Action: t.Stage2Action, Stage2Confidence: t.Stage2Confidence,
		Stage3Action: t.Stage3Action, Stage3Confidence: t.Stage3Confidence, FinalAction: t.FinalAction,
		FinalConfidence: t.FinalConfidence, FeedItemID: t.FeedItemID, Error: t.Error, LatencyMS: t.LatencyMS, Model: t.Model,
		AutonomyAction: t.AutonomyAction, AutonomyDecision: t.AutonomyDecision, AutonomyReason: t.AutonomyReason,
		TS: t.TS, TeamID: t.TeamID, URL: t.URL,
	}
	if strings.TrimSpace(t.FeedItemID) != "" && s.cfg.DB != nil {
		if item, err := flowdb.GetFeedItem(s.cfg.DB, t.FeedItemID); err == nil {
			target := strings.TrimSpace(item.LinkedTask)
			if target == "" {
				target = strings.TrimSpace(item.MatchedTask)
			}
			v.LinkedTask = target
			if target != "" {
				v.MatchedTask = s.attentionTaskMatch(ctx, target)
			}
		}
	}
	if t.Source == "github" {
		// GitHub fields are already human: owner/repo channel, GitHub login
		// author, the item URL is the canonical permalink. No resolver needed.
		v.ChannelName = t.Channel
		v.AuthorName = t.Author
		v.Text = t.TextPreview
		v.Permalink = t.URL
	} else {
		// Channel/ts may be empty on traces recorded before those columns
		// existed — derive from thread_key "<channel>:<ts>" so the channel name
		// + permalink work for ALL Slack traces, old and new.
		ch, ts := t.Channel, t.TS
		if ch == "" || ts == "" {
			tkChan, tkTS := splitThreadKey(t.ThreadKey)
			if ch == "" {
				ch = tkChan
			}
			if ts == "" {
				ts = tkTS
			}
		}
		if v.Channel == "" {
			v.Channel = ch
		}
		if s.nameResolver != nil {
			v.ChannelName = s.nameResolver.ChannelName(ctx, ch)
			v.AuthorName = s.nameResolver.UserName(ctx, t.Author)
			v.Text = s.nameResolver.CleanText(ctx, t.TextPreview)
		}
		// The trace LIST can be hundreds of rows and does NOT display a
		// permalink (only the detail modal does), so resolve the cheap slack://
		// deep link here — never the network getPermalink per row, which would
		// re-introduce the serial-per-row load stall. New traces carry team_id;
		// older ones fall back to no link in the trace view (the FEED resolves a
		// real https permalink, since cards show it and the feed is small).
		v.Permalink = connectorPermalink(t.Source, t.TeamID, ch, ts, t.URL)
	}
	if v.Text == "" {
		v.Text = t.TextPreview
	}
	if v.Permalink == "" {
		v.Permalink = steeringPermalink(t)
	}
	return v
}

// steeringPermalink builds a best-effort deep link to the traced message,
// delegating to the connector-blind connectorPermalink.
func steeringPermalink(t flowdb.SteeringTrace) string {
	return connectorPermalink(t.Source, t.TeamID, t.Channel, t.TS, t.URL)
}

// connectorPermalink builds a best-effort deep link to a source message. For
// GitHub the canonical permalink is the stored item URL; for Slack it is a
// slack:// deep link built from team+channel+ts (empty when any is missing).
func connectorPermalink(source, teamID, channel, ts, url string) string {
	if source == "github" {
		return strings.TrimSpace(url)
	}
	team, ch, t := strings.TrimSpace(teamID), strings.TrimSpace(channel), strings.TrimSpace(ts)
	if source == "slack" && team != "" && ch != "" && t != "" {
		return fmt.Sprintf("slack://channel?team=%s&id=%s&message=%s", team, ch, t)
	}
	return ""
}

// listSlackChannelsFn is the mockable seam for the channel-list endpoint.
var listSlackChannelsFn = monitor.ListSlackChannels

// handleSlackChannels serves GET /api/slack/channels — the channel list for
// the steering watch-channel picker.
func (s *Server) handleSlackChannels(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	channels, err := listSlackChannelsFn(r.Context())
	if err != nil {
		writeError(w, err, http.StatusBadGateway)
		return
	}
	if channels == nil {
		channels = []monitor.SlackChannelInfo{}
	}
	writeJSON(w, channels)
}
