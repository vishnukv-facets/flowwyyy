package monitor

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// SlackChannelInfo is the compact channel shape used by the channel picker.
// Kind distinguishes a public/private channel ("channel") from a direct message
// ("im") or a group DM ("mpim") so the picker can label and group them — needed
// by the trusted-sources picker, which lets the operator trust DMs and groups,
// not just channels. Empty Kind is treated as "channel" (legacy/cached rows).
type SlackChannelInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsPrivate bool   `json:"is_private"`
	IsMember  bool   `json:"is_member"`
	Kind      string `json:"kind,omitempty"`
}

// slackDMListLimit caps how many DMs/group-DMs the trusted-sources picker lists,
// bounding the per-peer users.info resolution cost on a cold name cache.
const slackDMListLimit = 100

type slackChannelCache struct {
	CachedAt string             `json:"cached_at"`
	Channels []SlackChannelInfo `json:"channels"`
}

// slackConversationsFn is the mockable seam that hits conversations.list.
var slackConversationsFn = func(ctx context.Context, token string) ([]SlackChannelInfo, error) {
	api := slack.New(token)
	var out []SlackChannelInfo
	cursor := ""
	for {
		channels, next, err := api.GetConversationsContext(ctx, &slack.GetConversationsParameters{
			Types:           []string{"public_channel", "private_channel"},
			ExcludeArchived: true,
			Limit:           200,
			Cursor:          cursor,
		})
		if err != nil {
			return nil, err
		}
		for _, c := range channels {
			out = append(out, SlackChannelInfo{
				ID:        c.ID,
				Name:      c.Name,
				IsPrivate: c.IsPrivate,
				IsMember:  c.IsMember,
			})
		}
		if strings.TrimSpace(next) == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// slackDMConversationsFn lists the operator's DMs (im) and group DMs (mpim) via
// the USER token — a bot token can't see the operator's personal DMs. Best-effort
// and mockable: returns nil,nil when no user token is configured so the picker
// degrades to channels-only. An im is labelled by its peer's display name; an
// mpim by Slack's group name. Capped at slackDMListLimit to bound resolution cost.
var slackDMConversationsFn = func(ctx context.Context) ([]SlackChannelInfo, error) {
	if strings.TrimSpace(SlackUserToken()) == "" {
		return nil, nil
	}
	api := slack.New(SlackUserToken())
	// Resolve a DM peer's display name. UserName uses the resolver's PRIMARY client
	// only, so prefer the bot's users.info but fall back to the user token when no
	// bot token is configured — otherwise every 1:1 DM is labelled by its raw U… id,
	// which a name search ("manan") can't match and the operator can't recognize.
	resolver := NewSlackNameResolver()
	if resolver == nil {
		resolver = NewSlackNameResolverWithClient(NewSlackTitleUserClient())
	}
	var out []SlackChannelInfo
	ims, mpims := 0, 0
	cursor := ""
	for len(out) < slackDMListLimit {
		convs, next, err := api.GetConversationsContext(ctx, &slack.GetConversationsParameters{
			Types:           []string{"im", "mpim"},
			ExcludeArchived: true,
			Limit:           200,
			Cursor:          cursor,
		})
		if err != nil {
			return out, err
		}
		for _, c := range convs {
			info := SlackChannelInfo{ID: c.ID, IsPrivate: true, IsMember: true}
			if c.IsMpIM {
				info.Kind = "mpim"
				info.Name = firstNonEmpty(strings.TrimSpace(c.Name), "group DM")
				mpims++
			} else {
				info.Kind = "im"
				info.Name = firstNonEmpty(resolver.UserName(ctx, c.User), c.User)
				ims++
			}
			out = append(out, info)
			if len(out) >= slackDMListLimit {
				break
			}
		}
		if strings.TrimSpace(next) == "" {
			break
		}
		cursor = next
	}
	// Diagnostic: if the API returns groups but zero DMs, the user token is missing
	// im:read — a connector-scope problem, not a labelling one.
	log.Printf("flow: trusted-source picker listed %d DM(s) + %d group DM(s)", ims, mpims)
	return out, nil
}

// withSlackDMs appends the operator's DMs/group-DMs (best-effort) to a channel
// list. A listing error degrades to channels-only — the picker still works, the
// operator just can't trust a DM until the user-token scopes are in place.
func withSlackDMs(ctx context.Context, channels []SlackChannelInfo) []SlackChannelInfo {
	dms, err := slackDMConversationsFn(ctx)
	if err != nil {
		log.Printf("flow: list Slack DMs/groups for trusted-source picker: %v", err)
		return channels
	}
	return append(channels, dms...)
}

// defaultChannelKind stamps Kind="channel" on any entry that lacks one (the
// channel-listing seam and legacy cache rows don't set it).
func defaultChannelKind(channels []SlackChannelInfo) []SlackChannelInfo {
	for i := range channels {
		if strings.TrimSpace(channels[i].Kind) == "" {
			channels[i].Kind = "channel"
		}
	}
	return channels
}

// ListSlackChannels returns the channels visible to the bot token plus the
// operator's DMs/group-DMs (via the user token, best-effort). When no token is
// configured it returns an empty list (not an error) so the UI can render a
// "configure Slack" empty state gracefully.
func ListSlackChannels(ctx context.Context) ([]SlackChannelInfo, error) {
	token := SlackBotToken()
	if strings.TrimSpace(token) == "" {
		cached, _ := readSlackChannelCache() // may be nil; DMs are still worth trying
		return withSlackDMs(ctx, defaultChannelKind(cached)), nil
	}
	channels, err := slackConversationsFn(ctx, token)
	if err != nil {
		if cached, ok := readSlackChannelCache(); ok {
			return cached, nil
		}
		return nil, err
	}
	channels = withSlackDMs(ctx, defaultChannelKind(channels))
	writeSlackChannelCache(channels)
	return channels, nil
}

func slackChannelCachePath() string {
	root := strings.TrimSpace(os.Getenv("FLOW_ROOT"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			return ""
		}
		root = filepath.Join(home, ".flow")
	}
	return filepath.Join(root, "cache", "slack_channels.json")
}

func readSlackChannelCache() ([]SlackChannelInfo, bool) {
	path := slackChannelCachePath()
	if path == "" {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var cache slackChannelCache
	if err := json.Unmarshal(raw, &cache); err != nil {
		return nil, false
	}
	channels := compactSlackChannels(cache.Channels)
	if len(channels) == 0 {
		return nil, false
	}
	return channels, true
}

func writeSlackChannelCache(channels []SlackChannelInfo) {
	path := slackChannelCachePath()
	if path == "" {
		return
	}
	channels = compactSlackChannels(channels)
	if len(channels) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	cache := slackChannelCache{
		CachedAt: time.Now().UTC().Format(time.RFC3339),
		Channels: channels,
	}
	raw, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

func compactSlackChannels(channels []SlackChannelInfo) []SlackChannelInfo {
	var out []SlackChannelInfo
	seen := map[string]bool{}
	for _, ch := range channels {
		id := strings.TrimSpace(ch.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(ch.Name)
		out = append(out, SlackChannelInfo{
			ID:        id,
			Name:      name,
			IsPrivate: ch.IsPrivate,
			IsMember:  ch.IsMember,
			Kind:      strings.TrimSpace(ch.Kind),
		})
	}
	return out
}
