package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"flow/internal/flowdb"
)

func cmdContext(args []string) int {
	if len(args) == 0 || leadingHelpArg(args) {
		printContextUsage()
		return 0
	}
	switch args[0] {
	case "bind":
		return cmdContextBind(args[1:])
	case "inspect":
		return cmdContextInspect(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown context subcommand %q\n", args[0])
		printContextUsage()
		return 2
	}
}

func printContextUsage() {
	fmt.Println(`flow context — bind tasks/chats/source anchors to shared work context

Usage:
  flow context bind [--context <id-or-slug>] [--title <title>] [--slug <slug>] [--summary <text>]
                    [--task <slug>] [--chat <slug>]
                    [--source slack|github|flow] [--anchor-type <type>] [--external-id <id>] [--url <url>] [--label <text>]
  flow context inspect <id-or-slug|task:<slug>|chat:<slug>>`)
}

func cmdContextBind(args []string) int {
	fs := flagSet("context bind")
	contextRef := fs.String("context", "", "existing work context id or slug")
	title := fs.String("title", "", "title for a new work context")
	slug := fs.String("slug", "", "slug for a new work context")
	summary := fs.String("summary", "", "summary for a new work context")
	taskRef := fs.String("task", "", "task slug to bind")
	chatRef := fs.String("chat", "", "chat slug to bind")
	source := fs.String("source", "", "anchor source: slack|github|flow")
	anchorType := fs.String("anchor-type", "", "source anchor type")
	externalID := fs.String("external-id", "", "source external id")
	url := fs.String("url", "", "source url")
	label := fs.String("label", "", "source label")
	if handled, rc := parseFlagSet(fs, args); handled {
		return rc
	}

	hasAnchor := strings.TrimSpace(*anchorType) != "" || strings.TrimSpace(*externalID) != "" || strings.TrimSpace(*url) != "" || strings.TrimSpace(*label) != ""
	if strings.TrimSpace(*taskRef) == "" && strings.TrimSpace(*chatRef) == "" && !hasAnchor {
		fmt.Fprintln(os.Stderr, "error: context bind requires --task, --chat, or source anchor flags")
		return 2
	}
	if hasAnchor && (strings.TrimSpace(*anchorType) == "" || strings.TrimSpace(*externalID) == "") {
		fmt.Fprintln(os.Stderr, "error: source anchors require --anchor-type and --external-id")
		return 2
	}
	if strings.TrimSpace(*contextRef) == "" && strings.TrimSpace(*title) == "" {
		fmt.Fprintln(os.Stderr, "error: context bind requires --title when --context is not provided")
		return 2
	}

	db, err := openFlowDBForCommand()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()

	var task *flowdb.Task
	if strings.TrimSpace(*taskRef) != "" {
		task, err = ResolveTask(db, *taskRef, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
	}
	var chat *flowdb.Chat
	if strings.TrimSpace(*chatRef) != "" {
		chat, err = flowdb.GetChat(db, strings.TrimSpace(*chatRef))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				fmt.Fprintf(os.Stderr, "error: no chat matching %q\n", strings.TrimSpace(*chatRef))
			} else {
				fmt.Fprintf(os.Stderr, "error: get chat: %v\n", err)
			}
			return 1
		}
	}

	wc, err := resolveOrCreateWorkContext(db, *contextRef, *title, *slug, *summary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	oldIDs := uniqueNonEmpty(oldWorkContextIDs(task, chat, wc.ID))
	var anchorID string
	if hasAnchor {
		anchor, err := flowdb.CreateWorkContextSourceAnchor(db, flowdb.WorkContextSourceAnchor{
			WorkContextID: wc.ID,
			Source:        *source,
			AnchorType:    *anchorType,
			ExternalID:    *externalID,
			URL:           *url,
			Label:         *label,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: create source anchor: %v\n", err)
			return 1
		}
		anchorID = anchor.ID
	}

	if task != nil {
		if err := flowdb.SetTaskWorkContext(db, task.Slug, wc.ID); err != nil {
			fmt.Fprintf(os.Stderr, "error: bind task: %v\n", err)
			return 1
		}
	}
	if chat != nil {
		if err := flowdb.SetChatWorkContext(db, chat.Slug, wc.ID); err != nil {
			fmt.Fprintf(os.Stderr, "error: bind chat: %v\n", err)
			return 1
		}
	}
	for _, oldID := range oldIDs {
		err := flowdb.CreateWorkContextEdge(db, flowdb.WorkContextEdge{
			FromContextID: oldID,
			ToContextID:   wc.ID,
			Kind:          "duplicate",
			Note:          "Rebound to newer active context.",
		})
		if err != nil && !strings.Contains(err.Error(), "constraint failed") {
			fmt.Fprintf(os.Stderr, "error: record context edge: %v\n", err)
			return 1
		}
	}

	eventType := "work_context_bound"
	if len(oldIDs) > 0 {
		eventType = "work_context_rebound"
	}
	if err := appendContextBindEvent(db, eventType, wc.ID, task, chat, anchorID, oldIDs, *anchorType, *externalID, *url); err != nil {
		fmt.Fprintf(os.Stderr, "error: append work event: %v\n", err)
		return 1
	}

	fmt.Printf("bound context %s", wc.ID)
	if wc.Slug.Valid {
		fmt.Printf(" (%s)", wc.Slug.String)
	}
	if task != nil {
		fmt.Printf(" task=%s", task.Slug)
	}
	if chat != nil {
		fmt.Printf(" chat=%s", chat.Slug)
	}
	if len(oldIDs) > 0 {
		fmt.Printf(" rebound_from=%s", strings.Join(oldIDs, ","))
	}
	fmt.Println()
	return 0
}

func cmdContextInspect(args []string) int {
	if leadingHelpArg(args) {
		printContextUsage()
		return 0
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "error: context inspect requires one ref")
		return 2
	}
	db, err := openFlowDBForCommand()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()

	wc, bound, err := resolveInspectWorkContext(db, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	printWorkContextInspect(db, wc, bound)
	return 0
}

func resolveOrCreateWorkContext(db *sql.DB, ref, title, slug, summary string) (flowdb.WorkContext, error) {
	if strings.TrimSpace(ref) != "" {
		return resolveWorkContext(db, ref)
	}
	var ns sql.NullString
	if strings.TrimSpace(slug) != "" {
		ns = sql.NullString{String: strings.TrimSpace(slug), Valid: true}
	}
	return flowdb.CreateWorkContext(db, flowdb.WorkContext{
		Slug:    ns,
		Title:   strings.TrimSpace(title),
		Summary: strings.TrimSpace(summary),
	})
}

func resolveWorkContext(db *sql.DB, ref string) (flowdb.WorkContext, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return flowdb.WorkContext{}, fmt.Errorf("empty work context ref")
	}
	wc, err := flowdb.GetWorkContext(db, ref)
	if err == nil {
		return wc, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return flowdb.WorkContext{}, err
	}
	wc, err = flowdb.WorkContextBySlug(db, ref)
	if err == nil {
		return wc, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return flowdb.WorkContext{}, fmt.Errorf("no work context matching %q", ref)
	}
	return flowdb.WorkContext{}, err
}

type contextBoundRef struct {
	Kind string
	Slug string
}

func resolveInspectWorkContext(db *sql.DB, ref string) (flowdb.WorkContext, contextBoundRef, error) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "task:") {
		slug := strings.TrimSpace(strings.TrimPrefix(ref, "task:"))
		task, err := ResolveTask(db, slug, false)
		if err != nil {
			return flowdb.WorkContext{}, contextBoundRef{}, err
		}
		if !task.WorkContextID.Valid {
			return flowdb.WorkContext{}, contextBoundRef{}, fmt.Errorf("task %q has no work context", task.Slug)
		}
		wc, err := flowdb.GetWorkContext(db, task.WorkContextID.String)
		return wc, contextBoundRef{Kind: "task", Slug: task.Slug}, err
	}
	if strings.HasPrefix(ref, "chat:") {
		slug := strings.TrimSpace(strings.TrimPrefix(ref, "chat:"))
		chat, err := flowdb.GetChat(db, slug)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return flowdb.WorkContext{}, contextBoundRef{}, fmt.Errorf("no chat matching %q", slug)
			}
			return flowdb.WorkContext{}, contextBoundRef{}, err
		}
		if !chat.WorkContextID.Valid {
			return flowdb.WorkContext{}, contextBoundRef{}, fmt.Errorf("chat %q has no work context", chat.Slug)
		}
		wc, err := flowdb.GetWorkContext(db, chat.WorkContextID.String)
		return wc, contextBoundRef{Kind: "chat", Slug: chat.Slug}, err
	}
	wc, err := resolveWorkContext(db, ref)
	return wc, contextBoundRef{}, err
}

func printWorkContextInspect(db *sql.DB, wc flowdb.WorkContext, bound contextBoundRef) {
	fmt.Printf("work_context:  %s\n", wc.Title)
	fmt.Printf("id:            %s\n", wc.ID)
	if wc.Slug.Valid {
		fmt.Printf("slug:          %s\n", wc.Slug.String)
	}
	fmt.Printf("status:        %s\n", wc.Status)
	if wc.Summary != "" {
		fmt.Printf("summary:       %s\n", wc.Summary)
	}
	if bound.Kind != "" {
		fmt.Printf("bound:         %s %s\n", bound.Kind, bound.Slug)
	}

	anchors, _ := flowdb.ListWorkContextSourceAnchors(db, wc.ID)
	fmt.Println("anchors:")
	if len(anchors) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, a := range anchors {
			fmt.Printf("  - %s %s %s", a.Source, a.AnchorType, a.ExternalID)
			if a.URL != "" {
				fmt.Printf(" %s", a.URL)
			}
			if a.Label != "" {
				fmt.Printf(" (%s)", a.Label)
			}
			fmt.Println()
		}
	}

	edges, _ := flowdb.ListWorkContextEdges(db, wc.ID)
	fmt.Println("edges:")
	if len(edges) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, e := range edges {
			fmt.Printf("  - %s -> %s", e.Kind, e.ToContextID)
			if e.Note != "" {
				fmt.Printf(" (%s)", e.Note)
			}
			fmt.Println()
		}
	}

	events, _ := flowdb.ListWorkEventLog(db, flowdb.WorkEventLogFilter{WorkContextID: wc.ID, Limit: 8})
	fmt.Println("events:")
	if len(events) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, ev := range events {
			fmt.Printf("  - %s %s", ev.OccurredAt, ev.EventType)
			if ev.TaskSlug != "" {
				fmt.Printf(" task=%s", ev.TaskSlug)
			}
			if ev.ChatSlug != "" {
				fmt.Printf(" chat=%s", ev.ChatSlug)
			}
			if ev.Source != "" || ev.ExternalID != "" {
				fmt.Printf(" source=%s:%s", ev.Source, ev.ExternalID)
			}
			if ev.ExternalURL != "" {
				fmt.Printf(" %s", ev.ExternalURL)
			}
			fmt.Println()
		}
	}
}

func oldWorkContextIDs(task *flowdb.Task, chat *flowdb.Chat, newID string) []string {
	var out []string
	if task != nil && task.WorkContextID.Valid && strings.TrimSpace(task.WorkContextID.String) != newID {
		out = append(out, strings.TrimSpace(task.WorkContextID.String))
	}
	if chat != nil && chat.WorkContextID.Valid && strings.TrimSpace(chat.WorkContextID.String) != newID {
		out = append(out, strings.TrimSpace(chat.WorkContextID.String))
	}
	return out
}

func uniqueNonEmpty(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func appendContextBindEvent(db *sql.DB, eventType, contextID string, task *flowdb.Task, chat *flowdb.Chat, anchorID string, oldIDs []string, anchorType, externalID, url string) error {
	meta := map[string]any{
		"new_work_context_id": contextID,
	}
	if len(oldIDs) > 0 {
		meta["old_work_context_ids"] = oldIDs
	}
	raw, _ := json.Marshal(meta)
	entry := flowdb.WorkEventLogEntry{
		EventType:      eventType,
		WorkContextID:  contextID,
		ActorKind:      "operator",
		Source:         "flow",
		SourceAnchorID: anchorID,
		ExternalID:     externalID,
		ExternalURL:    url,
		MetadataJSON:   string(raw),
	}
	if strings.TrimSpace(anchorType) != "" {
		entry.Source = strings.Split(strings.TrimSpace(strings.ToLower(anchorType)), "_")[0]
	}
	if task != nil {
		entry.TaskSlug = task.Slug
		if task.ProjectSlug.Valid {
			entry.ProjectSlug = task.ProjectSlug.String
		}
	}
	if chat != nil {
		entry.ChatSlug = chat.Slug
	}
	_, _, err := flowdb.AppendWorkEventLog(db, entry)
	return err
}
