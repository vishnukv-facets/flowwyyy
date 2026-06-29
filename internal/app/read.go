package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"flow/internal/flowdb"
)

func cmdRead(args []string) int {
	if len(args) == 0 || leadingHelpArg(args) {
		printReadUsage()
		return 0
	}
	switch args[0] {
	case "ask", "say":
		return cmdReadAppend(args[0], args[1:])
	case "list":
		return cmdReadList(args[1:])
	case "mark":
		return cmdReadMark(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown read subcommand %q\n", args[0])
		printReadUsage()
		return 2
	}
}

func printReadUsage() {
	fmt.Println(`flow read — structured notes/questions from live sessions

Usage:
  flow read ask "<question>" [--key <dedupe-key>]
  flow read say "<note>"     [--key <dedupe-key>]
  flow read list [--status pending|unread|read|answered|all] [--format table|json]
  flow read mark <id> (--read|--answered)`)
}

func cmdReadAppend(kind string, args []string) int {
	fs := flagSet("read " + kind)
	key := fs.String("key", "", "idempotency key")
	if leadingHelpArg(args) {
		fs.Usage()
		return 0
	}
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "error: read %s requires text\n", kind)
		return 2
	}
	text := strings.TrimSpace(args[0])
	if text == "" {
		fmt.Fprintf(os.Stderr, "error: read %s requires non-empty text\n", kind)
		return 2
	}
	if handled, rc := parseFlagSet(fs, args[1:]); handled {
		return rc
	}
	db, err := openFlowDBForCommand()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()

	ctx := resolveReadContext(db)
	item := flowdb.SessionReadItem{
		Kind:          kind,
		Text:          text,
		Provider:      ctx.provider,
		SessionID:     ctx.sessionID,
		TaskSlug:      ctx.taskSlug,
		ChatSlug:      ctx.chatSlug,
		ProjectSlug:   ctx.projectSlug,
		WorkContextID: ctx.workContextID,
		DedupeKey:     *key,
	}
	if len(ctx.dependencies) > 0 {
		raw, _ := json.Marshal(ctx.dependencies)
		item.DependenciesJSON = string(raw)
	}
	got, inserted, err := flowdb.AppendSessionReadItem(db, item)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: append read item: %v\n", err)
		return 1
	}
	if err := recordReadWorkEvent(db, got); err != nil {
		fmt.Fprintf(os.Stderr, "error: append work event: %v\n", err)
		return 1
	}
	if inserted {
		fmt.Printf("recorded %s %s (%s)\n", got.Kind, got.ID, got.Status)
	} else {
		fmt.Printf("already recorded %s %s (%s)\n", got.Kind, got.ID, got.Status)
	}
	return 0
}

func cmdReadList(args []string) int {
	fs := flagSet("read list")
	status := fs.String("status", "open", "pending|unread|read|answered|all")
	format := fs.String("format", "table", "table|json")
	limit := fs.Int("limit", 50, "max rows")
	if handled, rc := parseFlagSet(fs, args); handled {
		return rc
	}
	if !validReadListStatus(*status) {
		fmt.Fprintf(os.Stderr, "error: invalid read status %q\n", *status)
		return 2
	}
	if *format != "table" && *format != "json" {
		fmt.Fprintf(os.Stderr, "error: invalid read format %q\n", *format)
		return 2
	}
	listStatus := strings.TrimSpace(strings.ToLower(*status))
	db, err := openFlowDBForCommand()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()
	rows, err := flowdb.ListSessionReadItems(db, flowdb.SessionReadItemFilter{Status: readStatusFilter(listStatus), Limit: *limit})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: list read items: %v\n", err)
		return 1
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintf(os.Stderr, "error: encode read items: %v\n", err)
			return 1
		}
		return 0
	}
	for _, item := range rows {
		printReadItem(item)
	}
	if len(rows) == 0 {
		fmt.Println("(no read items)")
	}
	return 0
}

func cmdReadMark(args []string) int {
	fs := flagSet("read mark")
	read := fs.Bool("read", false, "mark read")
	answered := fs.Bool("answered", false, "mark answered")
	if leadingHelpArg(args) {
		fs.Usage()
		return 0
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "error: read mark requires an item id")
		return 2
	}
	id := args[0]
	if handled, rc := parseFlagSet(fs, args[1:]); handled {
		return rc
	}
	if *read == *answered {
		fmt.Fprintln(os.Stderr, "error: pass exactly one of --read or --answered")
		return 2
	}
	db, err := openFlowDBForCommand()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()
	status := "read"
	if *answered {
		status = "answered"
	}
	if err := flowdb.MarkSessionReadItem(db, id, status); err != nil {
		fmt.Fprintf(os.Stderr, "error: mark read item: %v\n", err)
		return 1
	}
	fmt.Printf("marked %s %s\n", id, status)
	return 0
}

type readContext struct {
	provider      string
	sessionID     string
	taskSlug      string
	chatSlug      string
	projectSlug   string
	workContextID string
	dependencies  []flowdb.DependencyRef
}

func resolveReadContext(db *sql.DB) readContext {
	var out readContext
	session := currentSessionForProvider("")
	out.provider = session.Provider
	out.sessionID = session.ID
	if session.ID != "" {
		if task, err := flowdb.TaskBySessionID(db, session.ID); err == nil && task != nil {
			applyReadTaskContext(db, &out, task)
			return out
		}
		if chat, err := flowdb.ChatBySessionID(db, session.Provider, session.ID); err == nil && chat != nil {
			out.chatSlug = chat.Slug
			if chat.WorkContextID.Valid {
				out.workContextID = chat.WorkContextID.String
			}
			return out
		}
		if state, err := flowdb.AgentRuntimeStateBySessionID(db, session.Provider, session.ID); err == nil && state != nil {
			if state.WorkContextID.Valid {
				out.workContextID = state.WorkContextID.String
			}
			if state.TaskSlug.Valid {
				if task, err := flowdb.GetTask(db, state.TaskSlug.String); err == nil && task != nil {
					applyReadTaskContext(db, &out, task)
					return out
				}
				out.taskSlug = state.TaskSlug.String
			}
		}
	}
	if slug := strings.TrimSpace(os.Getenv("FLOW_TASK")); slug != "" {
		if task, err := flowdb.GetTask(db, slug); err == nil && task != nil {
			applyReadTaskContext(db, &out, task)
		}
	}
	return out
}

func applyReadTaskContext(db *sql.DB, out *readContext, task *flowdb.Task) {
	out.taskSlug = task.Slug
	if task.ProjectSlug.Valid {
		out.projectSlug = task.ProjectSlug.String
	}
	if task.WorkContextID.Valid {
		out.workContextID = task.WorkContextID.String
	}
	if deps, err := flowdb.LoadDependencyRefs(db, task.Slug); err == nil {
		out.dependencies = deps
	}
}

func recordReadWorkEvent(db *sql.DB, item flowdb.SessionReadItem) error {
	actorKind := "user"
	actorID := "user"
	if item.SessionID != "" {
		actorKind = "agent"
		actorID = item.Provider + ":" + item.SessionID
	}
	meta, _ := json.Marshal(map[string]any{
		"kind":       item.Kind,
		"status":     item.Status,
		"dedupe_key": item.DedupeKey,
	})
	_, _, err := flowdb.AppendWorkEventLog(db, flowdb.WorkEventLogEntry{
		EventID:       "flow-read:" + item.ID,
		EventType:     "flow_read_" + item.Kind,
		OccurredAt:    item.CreatedAt,
		Provider:      item.Provider,
		SessionID:     item.SessionID,
		TaskSlug:      item.TaskSlug,
		ChatSlug:      item.ChatSlug,
		ProjectSlug:   item.ProjectSlug,
		WorkContextID: item.WorkContextID,
		ActorKind:     actorKind,
		ActorID:       actorID,
		Source:        "flow",
		ExternalID:    item.ID,
		MetadataJSON:  string(meta),
	})
	return err
}

func printReadItem(item flowdb.SessionReadItem) {
	fmt.Printf("%s [%s %s] %s\n", item.ID, item.Kind, item.Status, item.Text)
	if item.TaskSlug != "" {
		fmt.Printf("  task: %s\n", item.TaskSlug)
	}
	if item.ChatSlug != "" {
		fmt.Printf("  chat: %s\n", item.ChatSlug)
	}
	if item.Provider != "" || item.SessionID != "" {
		fmt.Printf("  session: %s/%s\n", item.Provider, item.SessionID)
	}
	if item.WorkContextID != "" {
		fmt.Printf("  work_context: %s\n", item.WorkContextID)
	}
	if deps := readItemDependencySummary(item.DependenciesJSON); deps != "" {
		fmt.Printf("  deps: %s\n", deps)
	}
	if item.Kind == "ask" && item.TaskSlug != "" {
		fmt.Printf("  reply: flow tell %s \"<answer>\"\n", item.TaskSlug)
	}
}

func readItemDependencySummary(raw string) string {
	var deps []flowdb.DependencyRef
	if err := json.Unmarshal([]byte(raw), &deps); err != nil || len(deps) == 0 {
		return ""
	}
	parts := make([]string, 0, len(deps))
	for _, d := range deps {
		if d.Status != "" {
			parts = append(parts, d.Slug+"("+d.Status+")")
		} else {
			parts = append(parts, d.Slug)
		}
	}
	return strings.Join(parts, ", ")
}

func readStatusFilter(status string) string {
	status = strings.TrimSpace(strings.ToLower(status))
	switch status {
	case "", "open":
		return "open"
	default:
		return status
	}
}

func validReadListStatus(status string) bool {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case "", "open", "pending", "unread", "read", "answered", "all":
		return true
	default:
		return false
	}
}

func openFlowDBForCommand() (*sql.DB, error) {
	dbPath, err := flowDBPath()
	if err != nil {
		return nil, err
	}
	return flowdb.OpenDB(dbPath)
}
