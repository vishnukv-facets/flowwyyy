package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
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
	views := make([]AttentionItemView, 0, len(items))
	for _, it := range items {
		views = append(views, s.attentionItemView(r.Context(), it))
	}
	writeJSON(w, views)
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
		// Prefer a real https permalink (resolved from channel+ts, no team_id
		// needed); fall back to the slack:// deep link.
		v.Permalink = s.slackPermalinker.Permalink(ctx, ch, ts)
		if v.Permalink == "" {
			v.Permalink = connectorPermalink(it.Source, it.TeamID, ch, ts, it.URL)
		}
	}
	return v
}

// splitThreadKey parses a Slack thread_key "<channel>:<thread_ts>". Slack
// channel IDs contain no ':' and the ts is the remainder, so SplitN(2) is safe.
func splitThreadKey(threadKey string) (channel, ts string) {
	if i := strings.Index(threadKey, ":"); i > 0 {
		return threadKey[:i], threadKey[i+1:]
	}
	return "", ""
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
		// Mark in-flight server-side so the spinner + disabled state survive a page
		// refresh and can't be double-fired; clear it when the async run finishes.
		_ = flowdb.SetFeedRetriaging(s.cfg.DB, id, time.Now().UTC().Format(time.RFC3339))
		s.publishUIChange("attention")
		go func(it flowdb.FeedItem) {
			bctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := s.cascade.Retriage(bctx, it); err != nil {
				fmt.Fprintf(os.Stderr, "attention: retriage %s: %v\n", it.ID, err)
			}
			_ = flowdb.SetFeedRetriaging(s.cfg.DB, it.ID, "")
			s.publishUIChange("attention")
		}(item)
		return actionResponse{OK: true, Message: "re-running triage — the card will update with the fresh decision"}, http.StatusOK
	case "make-task", "make_task":
		if err := steering.ApplyAction(context.Background(), s.cfg.DB, item, steering.ActionMakeTask, steering.DefaultAutonomy(), true); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "made task from " + id}, http.StatusOK
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
		if target := strings.TrimSpace(item.MatchedTask); target != "" {
			// A real flow task already owns this thread — hand it the reply (via
			// its inbox) and resume its session so it posts. No new task.
			if err := steering.InjectReplyToTask(context.Background(), s.cfg.DB, item, text, target, instructions); err != nil {
				return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
			}
			s.startSessionAsync(target)
			return actionResponse{OK: true, Message: "reply handed to session " + target + " — that agent is posting it"}, http.StatusOK
		}
		// No task owns this thread. How we post depends on the connector:
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
		// Slack (and any other connector whose posting needs a claude.ai MCP): a
		// headless `claude -p` has NO Slack MCP, so it cannot post. Spin an
		// ephemeral, watchable floating session that DOES (a real interactive
		// bypass Claude session). It posts the approved reply, then marks the card
		// sent and closes itself — and it's not a task, so nothing lands in the
		// Tasks list. On failure it stays open so the operator can see why.
		if s.terminals == nil {
			return actionResponse{OK: false, Message: "terminal hub is not running — cannot open a send session"}, http.StatusServiceUnavailable
		}
		launch, err := s.prepareSendReplyFloatingLaunch(item, text, instructions)
		if err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		ft := s.terminals.registerFloatingLaunch(launch, "Send reply")
		// Start it detached so the reply posts in the background whether or not the
		// operator opens the window. It surfaces as a tray chip they can click to
		// watch; on success it self-closes, on failure it stays for inspection.
		if err := s.terminals.startFloatingDetached(ft.ID); err != nil {
			s.terminals.stopFloating(ft.ID)
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "posting your reply in the background — open the Send reply terminal from the tray to watch", FloatingTerminal: &ft}, http.StatusOK
	default:
		return actionResponse{OK: false, Message: "unknown attention action: " + req.AttentionAction}, http.StatusBadRequest
	}
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
		Stage1Relevant: t.Stage1Relevant, Stage2Action: t.Stage2Action, Stage2Confidence: t.Stage2Confidence,
		Stage3Action: t.Stage3Action, Stage3Confidence: t.Stage3Confidence, FinalAction: t.FinalAction,
		FinalConfidence: t.FinalConfidence, FeedItemID: t.FeedItemID, Error: t.Error, LatencyMS: t.LatencyMS, Model: t.Model,
		TS: t.TS, TeamID: t.TeamID, URL: t.URL,
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
