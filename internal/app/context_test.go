package app

import (
	"database/sql"
	"strings"
	"testing"

	"flow/internal/flowdb"
)

func TestCmdContextBindInspectAndRebind(t *testing.T) {
	setupFlowRoot(t)
	wd := t.TempDir()
	if rc := cmdAdd([]string{"task", "Build", "--slug", "build", "--work-dir", wd, "--agent", "codex"}); rc != 0 {
		t.Fatalf("cmdAdd build rc=%d", rc)
	}
	db := openFlowDB(t)
	now := flowdb.NowISO()
	if err := flowdb.InsertChat(db, flowdb.Chat{
		Slug:           "chat-build",
		Title:          "Build chat",
		Provider:       "claude",
		Origin:         "ui",
		SessionID:      sql.NullString{String: "chat-session-1", Valid: true},
		CreatedAt:      now,
		LastActivityAt: now,
	}); err != nil {
		t.Fatalf("InsertChat: %v", err)
	}

	if rc := cmdContext([]string{
		"bind",
		"--task", "build",
		"--chat", "chat-build",
		"--title", "Shared incident",
		"--slug", "shared-incident",
		"--anchor-type", "slack_channel_thread",
		"--external-id", "C1:1719500000.000100",
		"--url", "https://slack.example/archives/C1/p1719500000000100",
		"--label", "customer thread",
	}); rc != 0 {
		t.Fatalf("cmdContext bind rc=%d", rc)
	}

	task, err := flowdb.GetTask(db, "build")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	chat, err := flowdb.GetChat(db, "chat-build")
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if !task.WorkContextID.Valid || !chat.WorkContextID.Valid || task.WorkContextID.String != chat.WorkContextID.String {
		t.Fatalf("task/chat context mismatch: task=%v chat=%v", task.WorkContextID, chat.WorkContextID)
	}
	ctx, err := flowdb.GetWorkContext(db, task.WorkContextID.String)
	if err != nil {
		t.Fatalf("GetWorkContext: %v", err)
	}
	if ctx.Title != "Shared incident" || !ctx.Slug.Valid || ctx.Slug.String != "shared-incident" {
		t.Fatalf("context = %+v, want shared incident slug", ctx)
	}
	anchors, err := flowdb.ListWorkContextSourceAnchors(db, ctx.ID)
	if err != nil {
		t.Fatalf("ListWorkContextSourceAnchors: %v", err)
	}
	if len(anchors) != 1 || anchors[0].AnchorType != "slack_channel_thread" || anchors[0].ExternalID != "C1:1719500000.000100" {
		t.Fatalf("anchors = %+v", anchors)
	}
	events, err := flowdb.ListWorkEventLog(db, flowdb.WorkEventLogFilter{EventType: "work_context_bound", TaskSlug: "build", WorkContextID: ctx.ID})
	if err != nil {
		t.Fatalf("ListWorkEventLog bound: %v", err)
	}
	if len(events) != 1 || events[0].ChatSlug != "chat-build" || events[0].SourceAnchorID != anchors[0].ID {
		t.Fatalf("bound events = %+v", events)
	}

	out := captureStdout(t, func() {
		if rc := cmdContext([]string{"inspect", "task:build"}); rc != 0 {
			t.Fatalf("cmdContext inspect rc=%d", rc)
		}
	})
	for _, want := range []string{"Shared incident", "shared-incident", "slack_channel_thread", "work_context_bound"} {
		if !strings.Contains(out, want) {
			t.Fatalf("inspect output missing %q:\n%s", want, out)
		}
	}

	if rc := cmdContext([]string{
		"bind",
		"--task", "build",
		"--title", "Narrower root cause",
		"--slug", "narrower-root-cause",
	}); rc != 0 {
		t.Fatalf("cmdContext rebind rc=%d", rc)
	}
	task, err = flowdb.GetTask(db, "build")
	if err != nil {
		t.Fatalf("GetTask after rebind: %v", err)
	}
	if !task.WorkContextID.Valid || task.WorkContextID.String == ctx.ID {
		t.Fatalf("task context after rebind = %v, want new context", task.WorkContextID)
	}
	edges, err := flowdb.ListWorkContextEdges(db, ctx.ID)
	if err != nil {
		t.Fatalf("ListWorkContextEdges: %v", err)
	}
	if len(edges) != 1 || edges[0].Kind != "duplicate" || edges[0].ToContextID != task.WorkContextID.String {
		t.Fatalf("edges = %+v, want duplicate edge to new context", edges)
	}
	rebound, err := flowdb.ListWorkEventLog(db, flowdb.WorkEventLogFilter{EventType: "work_context_rebound", TaskSlug: "build", WorkContextID: task.WorkContextID.String})
	if err != nil {
		t.Fatalf("ListWorkEventLog rebound: %v", err)
	}
	if len(rebound) != 1 || !strings.Contains(rebound[0].MetadataJSON, ctx.ID) {
		t.Fatalf("rebound events = %+v, want old context in metadata", rebound)
	}
}
