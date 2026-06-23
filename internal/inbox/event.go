package inbox

import "strings"

// InboundEvent is the parsed, normalized form of a source event that may land
// in a task inbox.
type InboundEvent struct {
	Kind         string   `json:"kind"`
	Channel      string   `json:"channel,omitempty"`
	ChannelType  string   `json:"channel_type,omitempty"`
	TS           string   `json:"ts,omitempty"`
	ThreadTS     string   `json:"thread_ts,omitempty"`
	UserID       string   `json:"user_id,omitempty"`
	Text         string   `json:"text,omitempty"`
	URL          string   `json:"url,omitempty"`
	EventKey     string   `json:"event_key,omitempty"`
	Reaction     string   `json:"reaction,omitempty"`
	ItemChannel  string   `json:"item_channel,omitempty"`
	ItemTS       string   `json:"item_ts,omitempty"`
	ItemAuthor   string   `json:"item_author,omitempty"`
	TeamID       string   `json:"team_id,omitempty"`
	APIAppID     string   `json:"api_app_id,omitempty"`
	RawJSON      string   `json:"raw_json,omitempty"`
	Participants []string `json:"participants,omitempty"`
	RefChannel   string   `json:"ref_channel,omitempty"`
	RefThreadTS  string   `json:"ref_thread_ts,omitempty"`
	RefTS        string   `json:"ref_ts,omitempty"`
}

// SharedRef reconstructs the shared-message pointer carried by this event, or
// ok=false when there is none.
func (e InboundEvent) SharedRef() (SharedRef, bool) {
	if strings.TrimSpace(e.RefChannel) == "" || strings.TrimSpace(e.RefTS) == "" {
		return SharedRef{}, false
	}
	return SharedRef{Channel: e.RefChannel, ThreadTS: e.RefThreadTS, TS: e.RefTS}, true
}

// SharedRef is a deterministic pointer from one Slack message to another that
// it embeds.
type SharedRef struct {
	Channel  string
	ThreadTS string
	TS       string
}

// ThreadKeys returns candidate task thread keys, most specific first.
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

// ThreadKey returns the partition key flow uses to find or create a task for a
// thread.
func ThreadKey(channel, threadTS string) string {
	channel = strings.TrimSpace(channel)
	threadTS = strings.TrimSpace(threadTS)
	if channel == "" || threadTS == "" {
		return ""
	}
	return channel + ":" + threadTS
}
