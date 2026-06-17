package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"flow/internal/flowdb"
	"flow/internal/steering"
)

// cmdAttention implements `flow attention <list|act>` — the terminal surface
// for the attention router's feed (the Mission Control feed panel is P1.4).
func cmdAttention(args []string) int {
	if leadingHelpArg(args) || len(args) == 0 {
		printAttentionUsage()
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdAttentionList(rest)
	case "surface":
		return cmdAttentionSurface(rest)
	case "act":
		return cmdAttentionAct(rest)
	case "handoff":
		return cmdAttentionHandoff(rest)
	case "sent":
		return cmdAttentionSent(rest)
	case "trace":
		return cmdAttentionTrace(rest)
	case "feedback":
		return cmdAttentionFeedback(rest)
	case "calibration":
		return cmdAttentionCalibration(rest)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown attention subcommand %q (want list|surface|act|handoff|sent|trace|feedback|calibration)\n", sub)
		printAttentionUsage()
		return 2
	}
}

func printAttentionUsage() {
	fmt.Println(`flow attention — review and act on the attention feed

  flow attention list [--status new|acted|dismissed|snoozed|all]   (default: new)
  flow attention surface --channel <id> --ts <ts> [--thread-key <key>] [--context-only]
  flow attention act <id> <make-task|forward|confirm-handoff|dismiss>
  flow attention handoff accept <correlation-id> --reason "<why>"
  flow attention handoff decline <correlation-id> --reason "<why>"
  flow attention sent <id> [--close-floating <floating-id>]
  flow attention trace [--since 24h] [--disposition dropped|surfaced|error|all] [--limit 50]
  flow attention feedback [--group source|channel|author|thread-type|suggested-action|confidence-band]
  flow attention calibration   (raw confidence band vs observed operator-agreement rate, per action)`)
}

func cmdAttentionList(args []string) int {
	fs := flagSet("attention list")
	status := fs.String("status", "new", "filter: new|acted|dismissed|snoozed|all")
	if handled, rc := parseFlagSet(fs, args); handled {
		return rc
	}
	db, rc := openAttentionDB()
	if rc != 0 {
		return rc
	}
	defer db.Close()

	filter := strings.TrimSpace(*status)
	if filter == "all" {
		filter = ""
	}
	if _, err := flowdb.ExpireAttentionHandoffs(db, time.Now().UTC().Format(time.RFC3339)); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	items, err := flowdb.ListFeedItems(db, filter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Print(renderAttentionFeed(items))
	return 0
}

func cmdAttentionSurface(args []string) int {
	fs := flagSet("attention surface")
	source := fs.String("source", "slack", "event source: slack|github")
	channel := fs.String("channel", "", "channel/DM/PR id")
	channelType := fs.String("channel-type", "", "channel|im|mpim|github")
	threadKey := fs.String("thread-key", "", "proposed thread_key to continue (validated)")
	ts := fs.String("ts", "", "message ts")
	threadTS := fs.String("thread-ts", "", "parent thread ts (defaults to ts)")
	author := fs.String("author", "", "author id")
	action := fs.String("action", string(steering.ActionDigestOnly), "make_task|capture_kb|forward|reply|digest_only|drop")
	matchedTask := fs.String("matched-task", "", "task slug to forward to")
	summary := fs.String("summary", "", "<=140 char card summary")
	draft := fs.String("draft", "", "drafted reply, if any")
	reason := fs.String("reason", "", "why")
	confidence := fs.Float64("confidence", 0, "0..1")
	contextOnly := fs.Bool("context-only", false, "memory-only: never surface a card")
	if handled, rc := parseFlagSet(fs, args); handled {
		return rc
	}
	if strings.TrimSpace(*channel) == "" || strings.TrimSpace(*ts) == "" {
		fmt.Fprintln(os.Stderr, "error: --channel and --ts are required")
		return 2
	}

	db, rc := openAttentionDB()
	if rc != 0 {
		return rc
	}
	defer db.Close()

	id, surfaced, err := steering.SurfaceCard(context.Background(), db, steering.SurfaceCardParams{
		Source:      *source,
		Channel:     *channel,
		ChannelType: *channelType,
		ThreadKey:   *threadKey,
		TS:          *ts,
		ThreadTS:    *threadTS,
		Author:      *author,
		Action:      *action,
		MatchedTask: *matchedTask,
		Summary:     *summary,
		Draft:       *draft,
		Confidence:  *confidence,
		Reason:      *reason,
		ContextOnly: *contextOnly,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("surfaced=%v id=%s\n", surfaced, id)
	return 0
}

func cmdAttentionAct(args []string) int {
	if leadingHelpArg(args) {
		printAttentionUsage()
		return 0
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "error: act requires <id> and <make-task|forward|confirm-handoff|dismiss>")
		return 2
	}
	id, actionArg := args[0], strings.ToLower(strings.TrimSpace(args[1]))

	db, rc := openAttentionDB()
	if rc != 0 {
		return rc
	}
	defer db.Close()

	item, err := flowdb.GetFeedItem(db, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: no feed item with id %q\n", id)
		return 1
	}

	switch actionArg {
	case "dismiss":
		if err := steering.DismissFeed(db, id); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Printf("dismissed %s\n", id)
		return 0
	case "make-task", "make_task":
		return runAttentionAction(db, item, steering.ActionMakeTask, "made task from")
	case "forward":
		return runAttentionAction(db, item, steering.ActionForward, "forwarded")
	case "confirm-handoff", "confirm_handoff", "handoff":
		h, err := steering.RequestHandoff(context.Background(), db, item, "attention-router")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Printf("requested handoff %s from %s\n", h.ID, h.Receiver)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "error: unknown action %q (want make-task|forward|confirm-handoff|dismiss)\n", actionArg)
		return 2
	}
}

func cmdAttentionHandoff(args []string) int {
	if leadingHelpArg(args) || len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: flow attention handoff <accept|decline|respond> <correlation-id> [verdict] --reason <why>")
		return 2
	}
	verb := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(args[0])), "-", "_")
	rest := args[1:]
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "error: handoff requires a correlation id")
		return 2
	}
	id := rest[0]
	rest = rest[1:]
	verdict := verb
	if verb == "respond" {
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "error: handoff respond requires accept|decline")
			return 2
		}
		verdict = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(rest[0])), "-", "_")
		rest = rest[1:]
	}
	if verdict != "accept" && verdict != "decline" {
		fmt.Fprintf(os.Stderr, "error: unknown handoff verdict %q (want accept|decline)\n", verdict)
		return 2
	}
	fs := flagSet("attention handoff")
	reason := fs.String("reason", "", "reason for accepting or declining")
	if handled, rc := parseFlagSet(fs, rest); handled {
		return rc
	}
	if strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(os.Stderr, "error: handoff response requires --reason")
		return 2
	}

	db, rc := openAttentionDB()
	if rc != 0 {
		return rc
	}
	defer db.Close()
	h, err := steering.RespondHandoff(context.Background(), db, id, verdict, *reason)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("%s %s (%s)\n", h.Status, h.ID, h.Receiver)
	return 0
}

// cmdAttentionSent marks a feed item 'acted' (sent) — the bookkeeping step an
// ephemeral send-reply session runs AFTER it has actually posted the reply via
// its MCP tools. Splitting "post" (the agent, which alone knows it succeeded)
// from "mark sent" (this command) is what stops the old false-"acted" bug: the
// card only resolves when the agent confirms the post by running this. With
// --close-floating it also closes its own watchable floating window (best-effort
// HTTP to the running `flow ui serve`), so a successful send auto-tidies while a
// failure leaves the window open and the card 'new'.
func cmdAttentionSent(args []string) int {
	if leadingHelpArg(args) || len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: flow attention sent <id> [--close-floating <floating-id>]")
		return 2
	}
	id := args[0]
	fs := flagSet("attention sent")
	closeFloating := fs.String("close-floating", "", "floating terminal id to close after marking sent")
	if handled, rc := parseFlagSet(fs, args[1:]); handled {
		return rc
	}

	db, rc := openAttentionDB()
	if rc != 0 {
		return rc
	}
	defer db.Close()

	item, err := flowdb.GetFeedItem(db, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: no feed item with id %q\n", id)
		return 1
	}
	// Preserve any existing task linkage so the card stays attributed to the task
	// it relates to (the ephemeral no-task path simply has none).
	link := strings.TrimSpace(item.LinkedTask)
	if link == "" {
		link = strings.TrimSpace(item.MatchedTask)
	}
	if err := flowdb.SetFeedItemActed(db, id, link, time.Now().UTC().Format(time.RFC3339)); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if err := recordAttentionSentFeedback(db, item); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if cf := strings.TrimSpace(*closeFloating); cf != "" {
		closeFloatingTerminalBestEffort(cf)
	}
	fmt.Printf("marked %s sent\n", id)
	return 0
}

func recordAttentionSentFeedback(db *sql.DB, item flowdb.FeedItem) error {
	existing, err := flowdb.ListAttentionFeedback(db, flowdb.AttentionFeedbackFilter{FeedItemID: item.ID, Limit: 50})
	if err != nil {
		return err
	}
	for _, row := range existing {
		if row.FinalAction == "send_reply" && (row.Outcome == "approved" || row.Outcome == "sent") {
			return nil
		}
	}
	return flowdb.RecordAttentionFeedback(db, flowdb.AttentionFeedbackFromFeed(item, "send_reply", "sent", item.Draft, time.Now().UTC().Format(time.RFC3339)))
}

// closeFloatingTerminalBestEffort asks the running flow server to close a
// floating terminal. It targets the server via FLOW_HOOK_URL (set in every
// flow-spawned session's env) and never fails the caller: closing the watchable
// window is a courtesy, not part of the send. The connection may be reset when
// the server tears down this very session's PTY — that's expected and ignored.
func closeFloatingTerminalBestEffort(id string) {
	base := serverBaseFromHookURL()
	if base == "" {
		return
	}
	payload := fmt.Sprintf(`{"kind":"close-floating-terminal","slug":%q}`, id)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/actions", strings.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := uiSessionToken(); tok != "" {
		req.Header.Set("X-Flow-Session-Token", tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// serverBaseFromHookURL derives the flow server's base URL from FLOW_HOOK_URL
// (".../api/hooks/agent"). Returns "" when unset, which makes the close a no-op.
func serverBaseFromHookURL() string {
	h := strings.TrimSpace(os.Getenv("FLOW_HOOK_URL"))
	if h == "" {
		return ""
	}
	return strings.TrimSuffix(h, "/api/hooks/agent")
}

// runAttentionAction applies an operator-initiated (manual) feed action and
// reports the result. manual=true bypasses the autonomy gate — the operator
// at the terminal is the authorization.
func runAttentionAction(db *sql.DB, item flowdb.FeedItem, action steering.Action, verb string) int {
	if err := steering.ApplyAction(context.Background(), db, item, action, steering.DefaultAutonomy(), true); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("%s %s\n", verb, item.ID)
	return 0
}

func openAttentionDB() (*sql.DB, int) {
	dbPath, err := flowDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return nil, 1
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return nil, 1
	}
	return db, 0
}

// renderAttentionFeed renders feed items as a compact table. Pure (no I/O) so
// it's unit-testable.
func renderAttentionFeed(items []flowdb.FeedItem) string {
	if len(items) == 0 {
		return "No attention items.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-10s  %-7s  %-10s  %-5s  %-7s  %-14s  %s\n",
		"ID", "SOURCE", "ACTION", "CONF", "URGENCY", "MATCHED", "SUMMARY")
	for _, it := range items {
		matched := it.MatchedTask
		if matched == "" {
			matched = "-"
		}
		fmt.Fprintf(&b, "%-10s  %-7s  %-10s  %-5.2f  %-7s  %-14s  %s\n",
			shortID(it.ID), it.Source, it.SuggestedAction, it.Confidence,
			orDash(it.Urgency), matched, it.Summary)
	}
	return b.String()
}

func shortID(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func cmdAttentionTrace(args []string) int {
	fs := flagSet("attention trace")
	since := fs.String("since", "24h", "how far back (Go duration, e.g. 1h, 24h)")
	disposition := fs.String("disposition", "all", "filter: dropped|surfaced|error|all")
	limit := fs.Int("limit", 50, "max rows")
	if handled, rc := parseFlagSet(fs, args); handled {
		return rc
	}
	sinceTS, err := sinceToRFC3339(*since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: bad --since %q: %v\n", *since, err)
		return 2
	}
	db, rc := openAttentionDB()
	if rc != 0 {
		return rc
	}
	defer db.Close()

	disp := strings.TrimSpace(*disposition)
	if disp == "all" {
		disp = ""
	}
	funnel, err := flowdb.SteeringFunnelSince(db, sinceTS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	items, err := flowdb.ListSteeringTrace(db, flowdb.TraceFilter{Disposition: disp, Since: sinceTS, Limit: *limit})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Print(renderTrace(funnel, items))
	return 0
}

func cmdAttentionFeedback(args []string) int {
	fs := flagSet("attention feedback")
	group := fs.String("group", "suggested-action", "group: source|channel|author|thread-type|suggested-action|confidence-band")
	if handled, rc := parseFlagSet(fs, args); handled {
		return rc
	}
	groupKey, err := attentionFeedbackGroupKey(*group)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	db, rc := openAttentionDB()
	if rc != 0 {
		return rc
	}
	defer db.Close()

	rows, err := flowdb.AttentionFeedbackReport(db, groupKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Print(renderAttentionFeedbackReport(rows))
	return 0
}

func attentionFeedbackGroupKey(s string) (string, error) {
	key := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
	switch key {
	case "source", "channel", "author", "thread_type", "suggested_action", "confidence_band":
		return key, nil
	default:
		return "", fmt.Errorf("unsupported feedback group %q", s)
	}
}

func renderAttentionFeedbackReport(rows []flowdb.AttentionFeedbackAggregate) string {
	if len(rows) == 0 {
		return "No attention feedback rows.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-18s  %-5s  %-8s  %-9s  %-5s  %-8s  %-8s\n",
		"GROUP", "TOTAL", "approved", "dismissed", "muted", "approve%", "dismiss%")
	for _, row := range rows {
		fmt.Fprintf(&b, "%-18s  %-5d  %-8d  %-9d  %-5d  %-8s  %-8s\n",
			clipStr(row.Group, 18), row.Total, row.Approved, row.Dismissed, row.Muted,
			percent(row.ApprovalRate), percent(row.DismissRate))
	}
	return b.String()
}

func percent(v float64) string {
	return fmt.Sprintf("%.0f%%", v*100)
}

// cmdAttentionCalibration prints the confidence calibration table: for each
// (action × raw confidence band) the steerer has feedback on, the observed
// operator-agreement rate. This is the audit surface for "is a 0.9 actually a
// 0.9?" — the raw band is what the model emitted, CALIBRATED is what it should
// have meant. GROUNDED=no marks bands with too few samples to trust (the live
// path keeps the raw number there).
func cmdAttentionCalibration(args []string) int {
	if handled, rc := parseFlagSet(flagSet("attention calibration"), args); handled {
		return rc
	}
	db, rc := openAttentionDB()
	if rc != 0 {
		return rc
	}
	defer db.Close()

	cal, err := steering.LoadConfidenceCalibrator(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Print(renderAttentionCalibration(cal.Cells()))
	return 0
}

// renderAttentionCalibration renders the calibration cells as a compact table.
// Pure (no I/O) so it's unit-testable.
func renderAttentionCalibration(cells []steering.CalibrationCell) string {
	if len(cells) == 0 {
		return "No attention feedback to calibrate against yet.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-12s  %-11s  %-7s  %-6s  %-10s  %s\n",
		"ACTION", "RAW-BAND", "SAMPLES", "AGREED", "CALIBRATED", "GROUNDED")
	for _, c := range cells {
		grounded := "yes"
		if !c.Grounded {
			grounded = "no (raw fallback)"
		}
		fmt.Fprintf(&b, "%-12s  %-11s  %-7d  %-6d  %-10s  %s\n",
			clipStr(string(c.Action), 12), c.Band, c.Total, c.Agreed, percent(c.Calibrated), grounded)
	}
	return b.String()
}

// sinceToRFC3339 converts a Go duration string (e.g. "24h") into an RFC3339
// lower bound that many units before now. Empty → 24h.
func sinceToRFC3339(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "24h"
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return "", err
	}
	return time.Now().Add(-d).UTC().Format(time.RFC3339), nil
}

// renderTrace renders the funnel summary line + a compact decision table. Pure
// (no I/O) so it's unit-testable.
func renderTrace(f flowdb.SteeringFunnel, items []flowdb.SteeringTrace) string {
	var b strings.Builder
	fmt.Fprintf(&b, "observed %d · stage0 %d · cache %d · stage1 %d · stage2 %d · surfaced %d · errors %d\n\n",
		f.Observed, f.DroppedStage0, f.DroppedCache, f.DroppedStage1, f.DroppedStage2, f.Surfaced, f.Errors)
	if len(items) == 0 {
		b.WriteString("No trace rows in window.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%-20s  %-8s  %-9s  %-7s  %-5s  %-12s  %s\n",
		"WHEN", "ORIGIN", "DISPOSE", "STAGE", "CONF", "CHANNEL", "REASON / PREVIEW")
	for _, it := range items {
		reason := it.DropReason
		if strings.TrimSpace(reason) == "" {
			reason = it.TextPreview
		}
		fmt.Fprintf(&b, "%-20s  %-8s  %-9s  %-7s  %-5.2f  %-12s  %s\n",
			clipStr(it.CreatedAt, 20), orDash(it.Origin), orDash(it.Disposition),
			orDash(it.StageReached), it.FinalConfidence, orDash(it.Channel), clipStr(reason, 60))
	}
	return b.String()
}

func clipStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
