package app

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"flow/internal/monitor"
)

// slackSendFn is the in-process fallback for a text post (resolves a token
// locally). Stubbable in tests. identity is "" (global), "bot", or "user".
var slackSendFn = monitor.SendAsThread

// slackFileSendFn is the in-process fallback for a file upload: (channel,
// comment, filePath, identity). Stubbable in tests.
var slackFileSendFn = monitor.SendFileAsThread

// postSlackSendFn POSTs {channel,thread_ts,text} to the running flow server, which holds
// the freshly-validated Slack token. Returns:
//   - (status, body, nil) when the server was reached (caller inspects status)
//   - (0, "", err)        when the server was UNREACHABLE (connection refused,
//     no server, timeout) — the caller falls back to slackSendFn.
//
// Stubbable in tests.
var postSlackSendFn = func(channel, threadTS, text, identity, file string) (status int, body string, err error) {
	url := flowServerURL("/api/slack/send")
	payload := fmt.Sprintf(`{"channel":%q,"thread_ts":%q,"text":%q,"as":%q,"file":%q}`, channel, threadTS, text, identity, file)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
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

func cmdSlack(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: flow slack send --channel <id> --text <message>")
		return 2
	}
	switch args[0] {
	case "send":
		return cmdSlackSend(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown slack subcommand %q\n", args[0])
		return 2
	}
}

func cmdSlackSend(args []string) int {
	fs := flagSet("slack send")
	channel := fs.String("channel", "", "Slack channel/DM id to post to")
	threadTS := fs.String("thread-ts", "", "Slack thread timestamp to reply into")
	text := fs.String("text", "", "message body (or, with --file, the attachment's initial comment)")
	as := fs.String("as", "", "send identity: bot or user (default: server's FLOW_SLACK_SEND_AS). Use 'bot' for automation — the bot token carries chat:write/files:write.")
	file := fs.String("file", "", "local path to a file (image, PDF, …) to upload as an attachment")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	filePath := strings.TrimSpace(*file)
	if strings.TrimSpace(*channel) == "" {
		fmt.Fprintln(os.Stderr, "error: --channel is required")
		return 2
	}
	if strings.TrimSpace(*text) == "" && filePath == "" {
		fmt.Fprintln(os.Stderr, "error: --text or --file is required")
		return 2
	}
	identity := strings.ToLower(strings.TrimSpace(*as))
	if identity != "" && identity != "bot" && identity != "user" {
		fmt.Fprintln(os.Stderr, "error: --as must be 'bot' or 'user'")
		return 2
	}

	// Prefer routing through the running flow server: it holds the
	// freshly-validated Slack token. A tmux-spawned agent may carry a stale
	// token in its environment, so resolving locally would fail
	// (account_inactive). Only fall back to the in-process path when the
	// server is unreachable.
	thread := strings.TrimSpace(*threadTS)
	status, body, err := postSlackSendFn(*channel, thread, *text, identity, filePath)
	if err == nil {
		if status >= 200 && status < 300 {
			return 0
		}
		// Reached the server but Slack rejected the send. The server's token
		// is authoritative — do NOT fall back (a stale local token would just
		// fail again). Surface the server's error.
		msg := serverSlackError(body)
		fmt.Fprintf(os.Stderr, "error: %s\n", msg)
		return 1
	}

	// Server unreachable (no server / connection refused / timeout) — fall
	// back to the in-process send so `flow slack send` still works standalone.
	if filePath != "" {
		if err := slackFileSendFn(*channel, thread, *text, filePath, identity); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		return 0
	}
	if err := slackSendFn(*channel, thread, *text, identity); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
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
