package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"flow/internal/monitor"
)

// slackSendFn is the in-process fallback for a text post (resolves a token
// locally). Stubbable in tests. identity is "" (global), "bot", or "user".
var slackSendFn = monitor.SendAsThread

var slackScheduleSendFn = monitor.ScheduleAsThread

// slackFileSendFn is the in-process fallback for a file upload: (channel,
// comment, filePath, identity). Stubbable in tests.
var slackFileSendFn = monitor.SendFileAsThread

// slackReactFn is the in-process fallback for a reaction: (channel, ts, emoji,
// identity). Stubbable in tests.
var slackReactFn = monitor.ReactAsThread

var slackSendNow = time.Now

// postSlackSendFn POSTs to the running flow server, which holds
// the freshly-validated Slack token. Returns:
//   - (status, body, nil) when the server was reached (caller inspects status)
//   - (0, "", err)        when the server was UNREACHABLE (connection refused,
//     no server, timeout) — the caller falls back to slackSendFn.
//
// Stubbable in tests.
var postSlackSendFn = func(channel, threadTS, text, identity, file string, postAt int64) (status int, body string, err error) {
	url := flowServerURL("/api/slack/send")
	payload, err := json.Marshal(slackSendPayload{
		Channel:  channel,
		ThreadTS: threadTS,
		Text:     text,
		As:       identity,
		File:     file,
		PostAt:   postAt,
	})
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := uiSessionToken(); tok != "" {
		req.Header.Set("X-Flow-Session-Token", tok)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Server unreachable — signal fallback.
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, strings.TrimSpace(string(b)), nil
}

type slackSendPayload struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	Text     string `json:"text"`
	As       string `json:"as"`
	File     string `json:"file"`
	PostAt   int64  `json:"post_at,omitempty"`
}

type slackReactPayload struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	Emoji   string `json:"emoji"`
	As      string `json:"as"`
}

// postSlackReactFn POSTs a reaction to the running flow server (which holds the
// fresh token), mirroring postSlackSendFn. Same return contract: (0,"",err) when
// the server is UNREACHABLE so the caller falls back to the in-process path.
// Stubbable in tests.
var postSlackReactFn = func(channel, ts, emoji, identity string) (status int, body string, err error) {
	payload, err := json.Marshal(slackReactPayload{Channel: channel, TS: ts, Emoji: emoji, As: identity})
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequest(http.MethodPost, flowServerURL("/api/slack/react"), strings.NewReader(string(payload)))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := uiSessionToken(); tok != "" {
		req.Header.Set("X-Flow-Session-Token", tok)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, strings.TrimSpace(string(b)), nil
}

func cmdSlack(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: flow slack send --channel <id> (--text <message>|--text-file <path>|--file <path>) [--at <when>]")
		fmt.Fprintln(os.Stderr, "       flow slack react --channel <id> --ts <ts> [--emoji +1]")
		return 2
	}
	switch args[0] {
	case "send":
		return cmdSlackSend(args[1:])
	case "react":
		return cmdSlackReact(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown slack subcommand %q\n", args[0])
		return 2
	}
}

// cmdSlackReact adds an emoji reaction to a Slack message — the agent's
// lightweight 👍-ack for thread messages that need acknowledgement, not a reply.
// Routes through the running server (fresh token) and falls back to the
// in-process path when the server is unreachable, mirroring `flow slack send`.
func cmdSlackReact(args []string) int {
	fs := flagSet("slack react")
	channel := fs.String("channel", "", "Slack channel/DM id of the message")
	ts := fs.String("ts", "", "timestamp of the message to react to")
	emoji := fs.String("emoji", "+1", "emoji short name without colons (default +1 — thumbs up)")
	as := fs.String("as", "user", "react identity: bot or user (default user, so it lands when the bot isn't in-channel)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*channel) == "" {
		fmt.Fprintln(os.Stderr, "error: --channel is required")
		return 2
	}
	if strings.TrimSpace(*ts) == "" {
		fmt.Fprintln(os.Stderr, "error: --ts is required")
		return 2
	}
	identity := strings.ToLower(strings.TrimSpace(*as))
	if identity != "" && identity != "bot" && identity != "user" {
		fmt.Fprintln(os.Stderr, "error: --as must be 'bot' or 'user'")
		return 2
	}
	emojiName := strings.Trim(strings.TrimSpace(*emoji), ":")
	if emojiName == "" {
		fmt.Fprintln(os.Stderr, "error: --emoji is required")
		return 2
	}

	// Prefer the running server (fresh token); fall back in-process when unreachable.
	status, respBody, err := postSlackReactFn(*channel, strings.TrimSpace(*ts), emojiName, identity)
	if err == nil {
		if status >= 200 && status < 300 {
			return 0
		}
		fmt.Fprintf(os.Stderr, "error: %s\n", serverSlackError(respBody))
		return 1
	}
	if ferr := slackReactFn(*channel, strings.TrimSpace(*ts), emojiName, identity); ferr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", ferr)
		return 1
	}
	return 0
}

func cmdSlackSend(args []string) int {
	fs := flagSet("slack send")
	channel := fs.String("channel", "", "Slack channel/DM id to post to")
	threadTS := fs.String("thread-ts", "", "Slack thread timestamp to reply into")
	text := fs.String("text", "", "message body (or, with --file, the attachment's initial comment)")
	textFile := fs.String("text-file", "", "read message body from a file; use '-' for stdin")
	as := fs.String("as", "", "send identity: bot or user (default: server's FLOW_SLACK_SEND_AS). Use 'bot' for automation — the bot token carries chat:write/files:write.")
	file := fs.String("file", "", "local path to a file (image, PDF, …) to upload as an attachment")
	at := fs.String("at", "", "schedule delivery at local YYYY-MM-DD HH:MM, +30m/+2h, or Unix epoch")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	filePath := strings.TrimSpace(*file)
	if strings.TrimSpace(*channel) == "" {
		fmt.Fprintln(os.Stderr, "error: --channel is required")
		return 2
	}
	body := *text
	if source := strings.TrimSpace(*textFile); source != "" {
		if strings.TrimSpace(*text) != "" {
			fmt.Fprintln(os.Stderr, "error: --text and --text-file are mutually exclusive")
			return 2
		}
		read, err := readSlackTextFile(source)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: read --text-file: %v\n", err)
			return 1
		}
		body = read
	}
	if strings.TrimSpace(body) == "" && filePath == "" {
		fmt.Fprintln(os.Stderr, "error: --text, --text-file, or --file is required")
		return 2
	}
	identity := strings.ToLower(strings.TrimSpace(*as))
	if identity != "" && identity != "bot" && identity != "user" {
		fmt.Fprintln(os.Stderr, "error: --as must be 'bot' or 'user'")
		return 2
	}
	var postAt int64
	if strings.TrimSpace(*at) != "" {
		if filePath != "" {
			fmt.Fprintln(os.Stderr, "error: --at cannot be used with --file (Slack cannot schedule uploads)")
			return 2
		}
		parsed, err := parseSlackPostAt(*at)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: --at: %v\n", err)
			return 2
		}
		postAt = parsed
	}

	// Prefer routing through the running flow server: it holds the
	// freshly-validated Slack token. A tmux-spawned agent may carry a stale
	// token in its environment, so resolving locally would fail
	// (account_inactive). Only fall back to the in-process path when the
	// server is unreachable.
	thread := strings.TrimSpace(*threadTS)
	status, respBody, err := postSlackSendFn(*channel, thread, body, identity, filePath, postAt)
	if err == nil {
		if status == 202 {
			// External-channel send gate: the server parked this for the
			// operator's approval in the inbox rather than sending it. Make it
			// unambiguous that nothing was sent — an agent must NOT treat this
			// as delivered (e.g. do not mark an attention card sent).
			printSlackQueuedConfirmation(respBody)
			return 0
		}
		if status >= 200 && status < 300 {
			if postAt != 0 {
				printSlackScheduleConfirmation(slackScheduledMessageID(respBody), postAt)
			}
			return 0
		}
		// Reached the server but Slack rejected the send. The server's token
		// is authoritative — do NOT fall back (a stale local token would just
		// fail again). Surface the server's error.
		msg := serverSlackError(respBody)
		fmt.Fprintf(os.Stderr, "error: %s\n", msg)
		return 1
	}

	// Server unreachable (no server / connection refused / timeout) — fall
	// back to the in-process send so `flow slack send` still works standalone.
	if postAt != 0 {
		id, err := slackScheduleSendFn(*channel, thread, body, identity, postAt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		printSlackScheduleConfirmation(id, postAt)
		return 0
	}
	if filePath != "" {
		if err := slackFileSendFn(*channel, thread, body, filePath, identity); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		return 0
	}
	if err := slackSendFn(*channel, thread, body, identity); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func readSlackTextFile(path string) (string, error) {
	if path == "-" {
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

func parseSlackPostAt(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("value is required")
	}
	now := slackSendNow()
	var postAt int64
	if strings.HasPrefix(s, "+") {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("relative value must look like +30m or +2h")
		}
		postAt = now.Add(d).Unix()
	} else if allDigits(s) {
		epoch, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid Unix epoch")
		}
		postAt = epoch
	} else {
		t, err := time.ParseInLocation("2006-01-02 15:04", s, now.Location())
		if err != nil {
			return 0, fmt.Errorf("use YYYY-MM-DD HH:MM, +30m/+2h, or Unix epoch")
		}
		postAt = t.Unix()
	}
	if postAt < now.Add(2*time.Minute).Unix() || postAt > now.Add(120*24*time.Hour).Unix() {
		return 0, fmt.Errorf("scheduled time must be between 2 minutes and 120 days from now")
	}
	return postAt, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func slackScheduledMessageID(body string) string {
	var resp struct {
		ScheduledMessageID string `json:"scheduled_message_id"`
	}
	_ = json.Unmarshal([]byte(body), &resp)
	return resp.ScheduledMessageID
}

func printSlackScheduleConfirmation(id string, postAt int64) {
	if strings.TrimSpace(id) != "" {
		fmt.Fprintf(os.Stdout, "scheduled_message_id: %s\n", id)
	}
	fmt.Fprintf(os.Stdout, "scheduled_for: %s\n", time.Unix(postAt, 0).In(slackSendNow().Location()).Format(time.RFC3339))
}

// printSlackQueuedConfirmation reports that the server held an external-channel
// send for the operator's approval (HTTP 202 from the send gate). It prints to
// stdout but is explicit that the message was NOT sent, so a human or agent
// reading the output does not assume delivery.
func printSlackQueuedConfirmation(body string) {
	var resp struct {
		Reason       string `json:"reason"`
		ChannelLabel string `json:"channel_label"`
		Channel      string `json:"channel"`
	}
	_ = json.Unmarshal([]byte(body), &resp)
	target := "the channel"
	for _, c := range []string{resp.ChannelLabel, resp.Channel} {
		if c = strings.TrimSpace(c); c != "" {
			target = c
			break
		}
	}
	reason := strings.TrimSpace(resp.Reason)
	if reason == "" {
		reason = "outside your org"
	}
	fmt.Fprintf(os.Stdout, "QUEUED (not sent): %s is %s — held for your approval in the flow inbox; it will send only when you approve it there.\n", target, reason)
}

// serverSlackError pulls a human message out of the server's error body
// ({"error":"..."} from writeError), falling back to the raw body.
func serverSlackError(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return "slack send failed (server)"
	}
	const key = `"error":`
	if i := strings.Index(body, key); i >= 0 {
		rest := strings.TrimSpace(body[i+len(key):])
		rest = strings.TrimPrefix(rest, `"`)
		if j := strings.Index(rest, `"`); j >= 0 {
			return rest[:j]
		}
	}
	return body
}
