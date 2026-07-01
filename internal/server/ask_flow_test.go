package server

import (
	"strings"
	"testing"

	"flow/internal/monitor"
)

func TestAskFlowNeedsMeUsesWorkEvents(t *testing.T) {
	root, db := testRootDB(t)
	t.Setenv("FLOW_ROOT", root)
	t.Setenv("HOME", root)
	insertProjectTask(t, db, root)
	if err := monitor.AppendInboxEvent("build-ui", monitor.InboundEvent{
		Kind: "pr_head_updated", ChannelType: "github", Channel: "vishnukv-facets/flow-manager",
		ThreadTS: "gh-pr:vishnukv-facets/flow-manager#42",
		URL:      "https://github.com/vishnukv-facets/flow-manager/pull/42",
		Text:     "Pull request head changed. Review the PR again.",
	}); err != nil {
		t.Fatalf("AppendInboxEvent: %v", err)
	}

	res := askFlowTest(t, db, root, "what needs me right now")
	if res.Intent != "needs_action" {
		t.Fatalf("intent = %q", res.Intent)
	}
	if !strings.Contains(res.Answer, "PR head updated") || !strings.Contains(res.Answer, "Task-linked PR changed") {
		t.Fatalf("answer did not include needs-action WorkEvent: %s", res.Answer)
	}
	if !hasAskFlowCitation(res.Citations, "task", "build-ui") {
		t.Fatalf("missing task citation: %#v", res.Citations)
	}
	if !hasAskFlowCitation(res.Citations, "source", "https://github.com/vishnukv-facets/flow-manager/pull/42") {
		t.Fatalf("missing source citation: %#v", res.Citations)
	}
}

func TestAskFlowCloseoutUsesWorkEvents(t *testing.T) {
	root, db := testRootDB(t)
	t.Setenv("FLOW_ROOT", root)
	t.Setenv("HOME", root)
	insertProjectTask(t, db, root)
	if err := monitor.AppendInboxEvent("build-ui", monitor.InboundEvent{
		Kind: "pr_merged", ChannelType: "github", Channel: "vishnukv-facets/flow-manager",
		ThreadTS: "gh-pr:vishnukv-facets/flow-manager#43",
		URL:      "https://github.com/vishnukv-facets/flow-manager/pull/43",
		Text:     "Pull request merged.",
	}); err != nil {
		t.Fatalf("AppendInboxEvent: %v", err)
	}

	res := askFlowTest(t, db, root, "what can i close")
	if res.Intent != "closeout" {
		t.Fatalf("intent = %q", res.Intent)
	}
	if !strings.Contains(res.Answer, "PR merged") || !strings.Contains(res.Answer, "verify and close out") {
		t.Fatalf("answer did not include closeout WorkEvent: %s", res.Answer)
	}
	if !hasAskFlowCitation(res.Citations, "task", "build-ui") {
		t.Fatalf("missing task citation: %#v", res.Citations)
	}
	if !hasAskFlowCitation(res.Citations, "source", "https://github.com/vishnukv-facets/flow-manager/pull/43") {
		t.Fatalf("missing source citation: %#v", res.Citations)
	}
}

func TestAskFlowChangedIncludesInboxWorkEvents(t *testing.T) {
	root, db := testRootDB(t)
	t.Setenv("FLOW_ROOT", root)
	t.Setenv("HOME", root)
	insertProjectTask(t, db, root)
	if err := monitor.AppendInboxEvent("build-ui", monitor.InboundEvent{
		Kind: "message", ChannelType: "slack", Channel: "C1", TS: "1710000000.000100",
		ThreadTS: "1710000000.000100",
		URL:      "https://example.slack.com/archives/C1/p1710000000000100",
		Text:     "The release thread changed while you were away.",
	}); err != nil {
		t.Fatalf("AppendInboxEvent: %v", err)
	}

	res := askFlowTest(t, db, root, "what changed")
	if res.Intent != "changed" {
		t.Fatalf("intent = %q", res.Intent)
	}
	if !strings.Contains(res.Answer, "The release thread changed while you were away") {
		t.Fatalf("answer did not include inbox WorkEvent: %s", res.Answer)
	}
	if !hasAskFlowCitation(res.Citations, "source", "https://example.slack.com/archives/C1/p1710000000000100") {
		t.Fatalf("missing source citation: %#v", res.Citations)
	}
}

func TestAskFlowTaskCitationsUseSessionRoutes(t *testing.T) {
	task := TaskView{Slug: "build-ui", Name: "Build UI"}
	if got := taskCitation(task).URL; got != "/session/build-ui" {
		t.Fatalf("task citation URL = %q", got)
	}
	if got := taskSummaryCitation(TaskSummary{Slug: "build-ui", Name: "Build UI"}).URL; got != "/session/build-ui" {
		t.Fatalf("task summary citation URL = %q", got)
	}
	if got := updateCitation(task, FileRef{Filename: "2026-07-01-note.md"}).URL; got != "/session/build-ui" {
		t.Fatalf("update citation URL = %q", got)
	}
}
