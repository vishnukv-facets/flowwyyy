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

// Per-kind caps for the trusted-sources picker. They are SEPARATE because the
// costs differ: a 1:1 DM (im) needs a users.info call to resolve its peer name
// (so slackIMListLimit bounds cold-cache resolution cost), while a group DM
// (mpim) carries a Slack-provided name and is free to list. A single shared cap
// let numerous group DMs starve the 1:1 DMs out of the list entirely.
const (
	slackIMListLimit   = 300
	slackMPIMListLimit = 300
)

// slackDirectoryTimeout bounds the bulk users.list fetch used to name DM peers,
// so a very large workspace degrades to raw ids instead of hanging the request.
const slackDirectoryTimeout = 15 * time.Second

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
	// Resolve every DM peer name from ONE bulk users.list directory rather than a
	// users.info call per DM: a 200+ DM operator would otherwise make 200+ sequential
	// calls and blow the request timeout. Best-effort — a directory error degrades to
	// raw U… ids (the DMs still list; they just aren't named until next refresh).
	// Bound the directory fetch so a very large workspace can't hang the request:
	// on timeout it degrades to raw ids (DMs still list), and the rest completes.
	dctx, cancel := context.WithTimeout(ctx, slackDirectoryTimeout)
	defer cancel()
	dir, derr := slackUserDirectoryFn(dctx, SlackUserToken())
	if derr != nil {
		log.Printf("flow: trusted-source picker user directory: %v (DMs will show ids)", derr)
		dir = nil
	}
	var out []SlackChannelInfo
	ims, mpims := 0, 0
	cursor := ""
	for {
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
			if c.IsMpIM {
				if mpims >= slackMPIMListLimit {
					continue
				}
				out = append(out, SlackChannelInfo{ID: c.ID, IsPrivate: true, IsMember: true, Kind: "mpim", Name: firstNonEmpty(strings.TrimSpace(c.Name), "group DM")})
				mpims++
			} else {
				if ims >= slackIMListLimit {
					continue
				}
				out = append(out, SlackChannelInfo{ID: c.ID, IsPrivate: true, IsMember: true, Kind: "im", Name: firstNonEmpty(dir[c.User], c.User)})
				ims++
			}
		}
		// Keep paging until the cursor is exhausted so a flood of group DMs can
		// never crowd out 1:1 DMs; stop early only once BOTH per-kind caps are met.
		if strings.TrimSpace(next) == "" || (ims >= slackIMListLimit && mpims >= slackMPIMListLimit) {
			break
		}
		cursor = next
	}
	// Diagnostic: if the API returns groups but zero DMs, the user token is missing
	// im:read — a connector-scope problem, not a labelling one.
	log.Printf("flow: trusted-source picker listed %d DM(s) + %d group DM(s)", ims, mpims)
	return out, nil
}

// slackUserDirectoryFn builds a userID→display-name map from a single bulk
// users.list (GetUsersContext paginates internally), so DM peer names resolve
// without a per-DM users.info call. Mockable in tests.
var slackUserDirectoryFn = func(ctx context.Context, token string) (map[string]string, error) {
	users, err := slack.New(token).GetUsersContext(ctx)
	if err != nil {
		return nil, err
	}
	dir := make(map[string]string, len(users))
	for _, u := range users {
		if name := firstNonEmpty(u.Profile.DisplayName, u.Profile.RealName, u.RealName, u.Name); name != "" {
			dir[u.ID] = name
		}
	}
	return dir, nil
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
	// Serve a fresh cache without re-listing. The live fetch is several Slack API
	// calls (bot channels + DM/group list + the bulk user directory), and the UI
	// polls this endpoint, so a short TTL keeps it snappy and well under the request
	// timeout. A stale/missing/empty cache falls through to a live fetch + rewrite.
	if cached, ok := readFreshSlackChannelCache(); ok {
		return cached, nil
	}
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

// slackChannelCacheTTL bounds how long a written channel list is served without
// a live re-fetch. Short enough that a newly created DM/channel shows soon, long
// enough that the expensive live listing runs rarely.
const slackChannelCacheTTL = 10 * time.Minute

// readFreshSlackChannelCache returns the cached list only when it was written
// within slackChannelCacheTTL; otherwise ok=false so the caller re-fetches.
func readFreshSlackChannelCache() ([]SlackChannelInfo, bool) {
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
	at, err := time.Parse(time.RFC3339, cache.CachedAt)
	if err != nil || time.Since(at) > slackChannelCacheTTL {
		return nil, false
	}
	channels := compactSlackChannels(cache.Channels)
	if len(channels) == 0 {
		return nil, false
	}
	return channels, true
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
