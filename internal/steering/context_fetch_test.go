package steering

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"flow/internal/monitor"
)

var errTestContextFetch = errors.New("read token unavailable")

type fakeSlackReplies struct {
	msgs []monitor.SlackMessage
	err  error
}

func (f fakeSlackReplies) Replies(context.Context, string, string, string, int) ([]monitor.SlackMessage, error) {
	return f.msgs, f.err
}

func TestSlackContextFetcherBuildsParentRepliesParticipantsAndPermalink(t *testing.T) {
	f := SlackContextFetcher{
		Replies: fakeSlackReplies{msgs: []monitor.SlackMessage{
			{User: "U_ALICE", Text: "Parent request", TS: "1.1", ThreadTS: "1.1"},
			{User: "U_BOB", Text: "Reply with <https://example.com|link>", TS: "1.2", ThreadTS: "1.1"},
		}},
		CleanText: func(_ context.Context, text string) string {
			return strings.ReplaceAll(text, "<https://example.com|link>", "link")
		},
		Permalink: func(_ context.Context, channel, ts string) string {
			if channel == "C1" && ts == "1.1" {
				return "https://example.slack.com/archives/C1/p11"
			}
			return ""
		},
	}
	pack, err := f.FetchContext(context.Background(), monitor.InboundEvent{
		Kind: "message", ChannelType: "channel", Channel: "C1", TS: "1.2", ThreadTS: "1.1", UserID: "U_BOB", Text: "Reply", TeamID: "T1",
	})
	if err != nil {
		t.Fatalf("FetchContext: %v", err)
	}
	if pack.Source != "slack" || pack.ThreadKey != "C1:1.1" || pack.FetchStatus != "ok" {
		t.Fatalf("pack identity mismatch: %+v", pack)
	}
	if pack.Permalink != "https://example.slack.com/archives/C1/p11" {
		t.Errorf("Permalink = %q", pack.Permalink)
	}
	if pack.Parent == nil || pack.Parent.Text != "Parent request" || pack.Parent.Kind != "parent" {
		t.Fatalf("parent mismatch: %+v", pack.Parent)
	}
	if len(pack.Messages) != 1 || pack.Messages[0].Text != "Reply with link" || pack.Messages[0].Kind != "reply" {
		t.Fatalf("messages mismatch: %+v", pack.Messages)
	}
	if strings.Join(pack.Participants, ",") != "U_ALICE,U_BOB" {
		t.Errorf("Participants = %#v", pack.Participants)
	}
	if pack.Summary == "" || !strings.Contains(pack.Summary, "2 Slack messages") {
		t.Errorf("Summary = %q", pack.Summary)
	}
}

func TestSlackContextFetcherIncludesTextFileContent(t *testing.T) {
	f := SlackContextFetcher{
		Replies: fakeSlackReplies{msgs: []monitor.SlackMessage{
			{
				User:     "U_ISHAN",
				TS:       "1780916901.021529",
				ThreadTS: "1780916901.021529",
				SubType:  "file_share",
				Files: []monitor.SlackFile{{
					Title:      "PHASE2-PHASE3-EXECUTION-PLAN.md",
					PrettyType: "Markdown (raw)",
					Content:    "# CSX Phase 2 & 3 Execution Plan\n\nCreate DMS replication instance first.",
				}},
			},
		}},
	}
	pack, err := f.FetchContext(context.Background(), monitor.InboundEvent{
		Kind: "message", ChannelType: "im", Channel: "D03LH2RCZMG", TS: "1780916901.021529", ThreadTS: "1780916901.021529", UserID: "U_ISHAN", Text: "file: PHASE2-PHASE3-EXECUTION-PLAN.md",
	})
	if err != nil {
		t.Fatalf("FetchContext: %v", err)
	}
	if pack.Parent == nil {
		t.Fatalf("parent missing: %+v", pack)
	}
	if !strings.Contains(pack.Parent.Text, "Create DMS replication instance first") {
		t.Fatalf("parent text = %q, want downloaded file content", pack.Parent.Text)
	}
	if !strings.Contains(pack.Parent.Text, "PHASE2-PHASE3-EXECUTION-PLAN.md") {
		t.Fatalf("parent text = %q, want file name retained", pack.Parent.Text)
	}
}

func TestSlackContextFetcherCollectsAttachmentPaths(t *testing.T) {
	f := SlackContextFetcher{
		Replies: fakeSlackReplies{msgs: []monitor.SlackMessage{
			{
				User:     "U_ISHAN",
				TS:       "1780916901.021529",
				ThreadTS: "1780916901.021529",
				Files: []monitor.SlackFile{{
					Title:     "mail-screenshot.png",
					Mimetype:  "image/png",
					LocalPath: "/tmp/flow/tasks/chat-steer-d03/attachments/mail-screenshot.png",
				}},
			},
		}},
	}
	pack, err := f.FetchContext(context.Background(), monitor.InboundEvent{
		Kind: "message", ChannelType: "im", Channel: "D03", TS: "1780916901.021529", ThreadTS: "1780916901.021529", UserID: "U_ISHAN",
	})
	if err != nil {
		t.Fatalf("FetchContext: %v", err)
	}
	if len(pack.AttachmentPaths) != 1 || pack.AttachmentPaths[0] != "/tmp/flow/tasks/chat-steer-d03/attachments/mail-screenshot.png" {
		t.Fatalf("AttachmentPaths = %#v, want local image path", pack.AttachmentPaths)
	}
	raw, err := json.Marshal(pack)
	if err != nil {
		t.Fatalf("marshal context: %v", err)
	}
	if !strings.Contains(string(raw), "attachment_paths") {
		t.Fatalf("context JSON = %s, want attachment_paths", raw)
	}
}

func TestSlackContextFetcherMissingAuthFailsClearly(t *testing.T) {
	f := SlackContextFetcher{}
	_, err := f.FetchContext(context.Background(), monitor.InboundEvent{
		Kind: "message", ChannelType: "channel", Channel: "C1", TS: "1.1", ThreadTS: "1.1", UserID: "U_ALICE", Text: "Parent request",
	})
	if err == nil || !strings.Contains(err.Error(), "slack context fetch unavailable") {
		t.Fatalf("FetchContext error = %v, want missing-auth style error", err)
	}
}

func TestGitHubContextFetcherBuildsIssueAndCommentContext(t *testing.T) {
	old := githubContextRunner
	githubContextRunner = func(_ context.Context, endpoint string) ([]byte, error) {
		switch endpoint {
		case "repos/o/r/issues/5":
			return []byte(`{"title":"Deploy question","body":"Should we roll out today?","html_url":"https://github.com/o/r/issues/5","user":{"login":"maintainer"},"created_at":"2026-06-05T09:00:00Z","updated_at":"2026-06-05T09:30:00Z"}`), nil
		case "repos/o/r/issues/5/comments":
			return []byte(`[{"body":"Please include rollback notes","html_url":"https://github.com/o/r/issues/5#issuecomment-1","user":{"login":"reviewer"},"created_at":"2026-06-05T10:00:00Z","updated_at":"2026-06-05T10:00:00Z"}]`), nil
		default:
			return []byte(`[]`), nil
		}
	}
	t.Cleanup(func() { githubContextRunner = old })

	pack, err := GitHubContextFetcher{}.FetchContext(context.Background(), monitor.InboundEvent{
		Kind: "issue_comment", ChannelType: "github", Channel: "o/r", ThreadTS: "gh-issue:o/r#5", UserID: "reviewer", Text: "Please include rollback notes",
	})
	if err != nil {
		t.Fatalf("FetchContext: %v", err)
	}
	if pack.Source != "github" || pack.ThreadKey != "o/r:gh-issue:o/r#5" || pack.Permalink != "https://github.com/o/r/issues/5" {
		t.Fatalf("pack identity mismatch: %+v", pack)
	}
	if pack.Parent == nil || pack.Parent.Text != "Deploy question\n\nShould we roll out today?" || pack.Parent.Author != "maintainer" {
		t.Fatalf("parent mismatch: %+v", pack.Parent)
	}
	if len(pack.Messages) != 1 || pack.Messages[0].Kind != "comment" || pack.Messages[0].Author != "reviewer" {
		t.Fatalf("messages mismatch: %+v", pack.Messages)
	}
	if strings.Join(pack.Participants, ",") != "maintainer,reviewer" {
		t.Errorf("Participants = %#v", pack.Participants)
	}
}
