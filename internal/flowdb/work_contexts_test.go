package flowdb

import (
	"database/sql"
	"testing"
)

func TestWorkContextRegistryAnchorsAndEdges(t *testing.T) {
	db := openTempDB(t)

	ctx1, err := CreateWorkContext(db, WorkContext{
		Slug:    sql.NullString{String: "billing-follow-up", Valid: true},
		Title:   "Billing follow-up",
		Summary: "Refund question from the customer thread.",
	})
	if err != nil {
		t.Fatalf("CreateWorkContext ctx1: %v", err)
	}
	if ctx1.ID == "" {
		t.Fatal("CreateWorkContext generated empty ID")
	}
	if ctx1.Status != "active" {
		t.Fatalf("Status = %q, want active", ctx1.Status)
	}
	if ctx1.CreatedAt == "" || ctx1.UpdatedAt == "" {
		t.Fatalf("timestamps not filled: %+v", ctx1)
	}

	bySlug, err := WorkContextBySlug(db, "billing-follow-up")
	if err != nil {
		t.Fatalf("WorkContextBySlug: %v", err)
	}
	if bySlug.ID != ctx1.ID {
		t.Fatalf("WorkContextBySlug ID = %q, want %q", bySlug.ID, ctx1.ID)
	}

	ctx2, err := CreateWorkContext(db, WorkContext{
		Title:   "Unrelated deployment issue",
		Summary: "A second problem discussed in the same Slack thread.",
	})
	if err != nil {
		t.Fatalf("CreateWorkContext ctx2: %v", err)
	}

	for _, id := range []string{ctx1.ID, ctx2.ID} {
		if _, err := CreateWorkContextSourceAnchor(db, WorkContextSourceAnchor{
			WorkContextID: id,
			Source:        "slack",
			AnchorType:    "slack_channel_thread",
			ExternalID:    "C123:1719500000.000100",
			URL:           "https://example.slack.com/archives/C123/p1719500000000100",
			Label:         "customer thread",
			MetadataJSON:  `{"channel":"C123","thread_ts":"1719500000.000100"}`,
		}); err != nil {
			t.Fatalf("CreateWorkContextSourceAnchor for %s: %v", id, err)
		}
	}

	anchors, err := ListWorkContextSourceAnchors(db, ctx1.ID)
	if err != nil {
		t.Fatalf("ListWorkContextSourceAnchors: %v", err)
	}
	if len(anchors) != 1 {
		t.Fatalf("anchors = %d, want 1", len(anchors))
	}
	if anchors[0].AnchorType != "slack_channel_thread" || anchors[0].ExternalID == "" {
		t.Fatalf("unexpected anchor: %+v", anchors[0])
	}

	if err := CreateWorkContextEdge(db, WorkContextEdge{
		FromContextID: ctx2.ID,
		ToContextID:   ctx1.ID,
		Kind:          "follow-up",
		Note:          "Deployment issue came up while discussing billing.",
	}); err != nil {
		t.Fatalf("CreateWorkContextEdge: %v", err)
	}
	if err := CreateWorkContextEdge(db, WorkContextEdge{
		FromContextID: ctx1.ID,
		ToContextID:   ctx2.ID,
		Kind:          "depends",
	}); err == nil {
		t.Fatal("CreateWorkContextEdge accepted unsupported kind")
	}

	edges, err := ListWorkContextEdges(db, ctx2.ID)
	if err != nil {
		t.Fatalf("ListWorkContextEdges: %v", err)
	}
	if len(edges) != 1 || edges[0].Kind != "follow-up" || edges[0].ToContextID != ctx1.ID {
		t.Fatalf("edges = %+v, want follow-up to %s", edges, ctx1.ID)
	}
}

func TestWorkContextAttachmentsAreNullableAndSettable(t *testing.T) {
	db := openTempDB(t)
	now := NowISO()
	insertTask(t, db, "ctx-task", "Context task", "backlog", "medium", t.TempDir(), nil)
	if err := InsertChat(db, Chat{
		Slug:           "ctx-chat",
		Title:          "Context chat",
		Provider:       "claude",
		Origin:         "ui",
		CreatedAt:      now,
		LastActivityAt: now,
	}); err != nil {
		t.Fatalf("InsertChat: %v", err)
	}
	if err := UpsertAgentRuntimeState(db, AgentRuntimeStateInput{
		Provider:  "claude",
		SessionID: "session-1",
		TaskSlug:  "ctx-task",
		Status:    "running",
		EventKind: "session_start",
	}); err != nil {
		t.Fatalf("UpsertAgentRuntimeState without context: %v", err)
	}

	task, err := GetTask(db, "ctx-task")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.WorkContextID.Valid {
		t.Fatalf("new task WorkContextID = %v, want NULL", task.WorkContextID)
	}
	chat, err := GetChat(db, "ctx-chat")
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if chat.WorkContextID.Valid {
		t.Fatalf("new chat WorkContextID = %v, want NULL", chat.WorkContextID)
	}
	state, err := AgentRuntimeStateBySessionID(db, "claude", "session-1")
	if err != nil {
		t.Fatalf("AgentRuntimeStateBySessionID: %v", err)
	}
	if state.WorkContextID.Valid {
		t.Fatalf("new runtime WorkContextID = %v, want NULL", state.WorkContextID)
	}

	ctx, err := CreateWorkContext(db, WorkContext{Title: "Shared context"})
	if err != nil {
		t.Fatalf("CreateWorkContext: %v", err)
	}
	if err := SetTaskWorkContext(db, "ctx-task", ctx.ID); err != nil {
		t.Fatalf("SetTaskWorkContext: %v", err)
	}
	if err := SetChatWorkContext(db, "ctx-chat", ctx.ID); err != nil {
		t.Fatalf("SetChatWorkContext: %v", err)
	}
	if err := UpsertAgentRuntimeState(db, AgentRuntimeStateInput{
		Provider:      "claude",
		SessionID:     "session-1",
		TaskSlug:      "ctx-task",
		Status:        "idle",
		EventKind:     "stop",
		WorkContextID: ctx.ID,
		Seq:           2,
	}); err != nil {
		t.Fatalf("UpsertAgentRuntimeState with context: %v", err)
	}

	task, _ = GetTask(db, "ctx-task")
	if !task.WorkContextID.Valid || task.WorkContextID.String != ctx.ID {
		t.Fatalf("task WorkContextID = %v, want %s", task.WorkContextID, ctx.ID)
	}
	chat, _ = GetChat(db, "ctx-chat")
	if !chat.WorkContextID.Valid || chat.WorkContextID.String != ctx.ID {
		t.Fatalf("chat WorkContextID = %v, want %s", chat.WorkContextID, ctx.ID)
	}
	state, _ = AgentRuntimeStateBySessionID(db, "claude", "session-1")
	if !state.WorkContextID.Valid || state.WorkContextID.String != ctx.ID {
		t.Fatalf("runtime WorkContextID = %v, want %s", state.WorkContextID, ctx.ID)
	}

	if err := SetTaskWorkContext(db, "ctx-task", ""); err != nil {
		t.Fatalf("clear task context: %v", err)
	}
	task, _ = GetTask(db, "ctx-task")
	if task.WorkContextID.Valid {
		t.Fatalf("cleared task WorkContextID = %v, want NULL", task.WorkContextID)
	}
}
