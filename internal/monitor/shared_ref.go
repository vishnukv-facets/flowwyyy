package monitor

import (
	"encoding/json"
	"net/url"
	"strings"
)

// SharedRef is a deterministic pointer from one Slack message to another that
// it embeds — a forwarded/shared message ("Forward to…") or a pasted-permalink
// unfurl. Slack delivers both as a legacy `attachments[]` entry on the carrier
// message, carrying the original conversation + timestamps. slack-go's typed
// slackevents.MessageEvent has no Attachments field, so we recover the ref
// straight from the raw Socket Mode payload before it's lost.
//
// We use it to correlate a reply that arrives in a *different* conversation than
// the one a task tracks. Example: a task is anchored on a #channel thread, but a
// teammate answers by forwarding that thread message into a DM. The DM's shared
// attachment points back at the original thread, so we can route the DM as
// activity on the tracked thread.
type SharedRef struct {
	Channel  string // original conversation id (C…/D…/G…)
	ThreadTS string // original thread parent ts (from from_url ?thread_ts=, else == TS)
	TS       string // original message ts that was shared
}

// ThreadKeys returns the candidate thread keys this ref could match, most
// specific first: the (channel, thread_ts) parent key, then the
// (channel, ts) key for when the shared message is itself a thread root or the
// permalink omitted thread_ts. Both are exact tag lookups downstream, so
// returning both is safe — a match is a match, never a false positive.
func (r SharedRef) ThreadKeys() []string {
	var out []string
	seen := map[string]bool{}
	for _, ts := range []string{r.ThreadTS, r.TS} {
		if k := ThreadKey(r.Channel, ts); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// parseSharedRef extracts the first shared-message reference from a raw Socket
// Mode Events-API payload (the json.RawMessage in socketmode.Request.Payload).
// It is deliberately defensive about Slack's attachment shape: a forward sets
// is_share, a pasted-permalink unfurl sets is_msg_unfurl, and either way the
// useful fields are channel_id + ts (+ from_url for the thread parent). We
// accept any attachment that carries both channel_id and ts so we degrade
// gracefully if Slack relabels the flag. Returns ok=false when the payload has
// no such attachment (the overwhelmingly common case).
func parseSharedRef(raw []byte) (SharedRef, bool) {
	if len(raw) == 0 {
		return SharedRef{}, false
	}
	var env struct {
		Event struct {
			Attachments []struct {
				IsShare     bool   `json:"is_share"`
				IsMsgUnfurl bool   `json:"is_msg_unfurl"`
				ChannelID   string `json:"channel_id"`
				TS          string `json:"ts"`
				FromURL     string `json:"from_url"`
				ThreadTS    string `json:"thread_ts"`
			} `json:"attachments"`
		} `json:"event"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return SharedRef{}, false
	}
	for _, a := range env.Event.Attachments {
		channel := strings.TrimSpace(a.ChannelID)
		ts := strings.TrimSpace(a.TS)
		if channel == "" || ts == "" {
			continue
		}
		threadTS := strings.TrimSpace(a.ThreadTS)
		if threadTS == "" {
			threadTS = threadTSFromPermalink(a.FromURL)
		}
		if threadTS == "" {
			threadTS = ts
		}
		return SharedRef{Channel: channel, ThreadTS: threadTS, TS: ts}, true
	}
	return SharedRef{}, false
}

// threadTSFromPermalink pulls the thread_ts query param out of a Slack message
// permalink. Slack permalinks to a thread reply look like
//
//	https://acme.slack.com/archives/C123/p1700000001000200?thread_ts=1700000000.000100&cid=C123
//
// where p<digits> is the reply ts and thread_ts is the parent. We want the
// parent so the key matches the task's slack-thread tag (which is keyed on the
// thread root). Returns "" when the URL has no thread_ts (a top-level message).
func threadTSFromPermalink(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Query().Get("thread_ts"))
}
