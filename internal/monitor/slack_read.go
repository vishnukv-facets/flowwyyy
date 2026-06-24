package monitor

import (
	"context"
	"fmt"
	"strings"

	"github.com/slack-go/slack"
)

var slackUsersListFn = func(ctx context.Context, token string) ([]slack.User, error) {
	return slack.New(token).GetUsersContext(ctx)
}

var slackUserByEmailFn = func(ctx context.Context, token, email string) (*slack.User, error) {
	return slack.New(token).GetUserByEmailContext(ctx, strings.TrimSpace(email))
}

var slackUserDetailsFn = func(ctx context.Context, token, id string) (*slack.User, error) {
	return slack.New(token).GetUserInfoContext(ctx, strings.TrimSpace(id))
}

// SearchUsers lists users visible to token and filters by a case-insensitive
// substring across id, username, display name, real name, and email.
func SearchUsers(ctx context.Context, token, query string) ([]SlackUser, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrNoToken
	}
	users, err := slackUsersListFn(ctx, token)
	if err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	out := make([]SlackUser, 0, len(users))
	for _, u := range users {
		compact := slackUserFromAPI(u)
		if query == "" || slackUserMatches(compact, query) {
			out = append(out, compact)
		}
	}
	return out, nil
}

// LookupUserByEmail resolves a single Slack user by email.
func LookupUserByEmail(ctx context.Context, token, email string) (SlackUser, error) {
	if strings.TrimSpace(token) == "" {
		return SlackUser{}, ErrNoToken
	}
	u, err := slackUserByEmailFn(ctx, token, email)
	if err != nil {
		return SlackUser{}, err
	}
	if u == nil {
		return SlackUser{}, nil
	}
	return slackUserFromAPI(*u), nil
}

// UserInfo resolves a single Slack user by id.
func UserInfo(ctx context.Context, token, id string) (SlackUser, error) {
	if strings.TrimSpace(token) == "" {
		return SlackUser{}, ErrNoToken
	}
	u, err := slackUserDetailsFn(ctx, token, id)
	if err != nil {
		return SlackUser{}, err
	}
	if u == nil {
		return SlackUser{}, nil
	}
	return slackUserFromAPI(*u), nil
}

// ResolveUserNames returns user id -> display name for the supplied ids. It
// first uses the bulk directory and then the existing bounded users.info
// fallback used for DM names.
func ResolveUserNames(ctx context.Context, token string, ids []string) map[string]string {
	token = strings.TrimSpace(token)
	if token == "" || len(ids) == 0 {
		return map[string]string{}
	}
	dir, err := slackUserDirectoryFn(ctx, token)
	if err != nil {
		dir = nil
	}
	return resolveIMNames(ctx, token, ids, dir, slackUserInfoFn)
}

func slackUserFromAPI(u slack.User) SlackUser {
	return SlackUser{
		ID:          strings.TrimSpace(u.ID),
		Name:        strings.TrimSpace(u.Name),
		DisplayName: strings.TrimSpace(firstNonEmpty(u.Profile.DisplayName, u.Profile.DisplayNameNormalized)),
		RealName:    strings.TrimSpace(firstNonEmpty(u.Profile.RealName, u.RealName, u.Name)),
		Email:       strings.TrimSpace(u.Profile.Email),
		Title:       strings.TrimSpace(u.Profile.Title),
		TeamID:      strings.TrimSpace(firstNonEmpty(u.TeamID, u.Profile.Team)),
		IsBot:       u.IsBot,
		Deleted:     u.Deleted,
	}
}

func slackUserMatches(u SlackUser, query string) bool {
	for _, v := range []string{u.ID, u.Name, u.DisplayName, u.RealName, u.Email} {
		if strings.Contains(strings.ToLower(v), query) {
			return true
		}
	}
	return false
}

// ListSlackChannelsWithToken lists public/private conversations visible to the
// supplied token, optionally appending operator DMs/group DMs via the existing
// user-token path.
func ListSlackChannelsWithToken(ctx context.Context, token string, includeDMs bool) ([]SlackChannelInfo, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrNoToken
	}
	channels, err := slackConversationsFn(ctx, token)
	if err != nil {
		return nil, err
	}
	channels = defaultChannelKind(channels)
	if includeDMs {
		channels = withSlackDMs(ctx, channels)
	}
	return compactSlackChannels(channels), nil
}

// ResolveSlackChannel resolves a Slack id, #name, or bare channel/DM name to a
// channel record using the same enumeration as list-channels.
func ResolveSlackChannel(ctx context.Context, token, ref string, includeDMs bool) (SlackChannelInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return SlackChannelInfo{}, fmt.Errorf("no channel matching %q", ref)
	}
	if isSlackConversationID(ref) {
		id := normalizeSlackReadChannelID(ref)
		return SlackChannelInfo{ID: id, Name: id, Kind: slackKindFromID(id)}, nil
	}
	want := strings.TrimPrefix(ref, "#")
	want = strings.ToLower(strings.TrimSpace(want))
	channels, err := ListSlackChannelsWithToken(ctx, token, includeDMs)
	if err != nil {
		return SlackChannelInfo{}, err
	}
	for _, ch := range channels {
		if strings.ToLower(strings.TrimSpace(ch.Name)) == want {
			return ch, nil
		}
	}
	return SlackChannelInfo{}, fmt.Errorf("no channel matching %q", ref)
}

func isSlackConversationID(ref string) bool {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(strings.ToLower(ref), "slack:") {
		ref = strings.TrimSpace(ref[len("slack:"):])
	}
	if len(ref) < 2 || strings.ContainsAny(ref, " \t\r\n#") {
		return false
	}
	switch ref[0] {
	case 'C', 'G', 'D':
		return true
	default:
		return false
	}
}

func slackKindFromID(id string) string {
	if strings.HasPrefix(id, "D") {
		return "im"
	}
	return "channel"
}

func normalizeSlackReadChannelID(id string) string {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(strings.ToLower(id), "slack:") {
		id = strings.TrimSpace(id[len("slack:"):])
	}
	return normalizeSlackChannelID(id)
}

// SlackHistoryOptions contains conversations.history inputs for the read CLI.
type SlackHistoryOptions struct {
	ChannelID string `json:"channel_id"`
	Oldest    string `json:"oldest,omitempty"`
	Latest    string `json:"latest,omitempty"`
	Limit     int    `json:"limit"`
}

var slackConversationHistoryFn = func(ctx context.Context, token string, opts SlackHistoryOptions) ([]SlackMessage, error) {
	api := slack.New(token)
	out := make([]SlackMessage, 0, opts.Limit)
	cursor := ""
	for len(out) < opts.Limit {
		pageLimit := min(opts.Limit-len(out), slackHistoryPageSize)
		resp, err := api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
			ChannelID: opts.ChannelID,
			Oldest:    strings.TrimSpace(opts.Oldest),
			Latest:    strings.TrimSpace(opts.Latest),
			Inclusive: false,
			Limit:     pageLimit,
			Cursor:    cursor,
		})
		if err != nil {
			return nil, err
		}
		for _, m := range resp.Messages {
			out = append(out, slackMessageFromAPI(ctx, api, opts.ChannelID, m))
		}
		cursor = strings.TrimSpace(resp.ResponseMetaData.NextCursor)
		if cursor == "" || !resp.HasMore {
			break
		}
	}
	return out, nil
}

// ConversationHistory fetches top-level messages from a conversation.
func ConversationHistory(ctx context.Context, token string, opts SlackHistoryOptions) ([]SlackMessage, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrNoToken
	}
	opts.ChannelID = normalizeSlackReadChannelID(opts.ChannelID)
	if strings.TrimSpace(opts.ChannelID) == "" {
		return nil, fmt.Errorf("channel is required")
	}
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	return slackConversationHistoryFn(ctx, token, opts)
}

var slackConversationRepliesFn = func(ctx context.Context, token, channelID, threadTS string, limit int) ([]SlackMessage, error) {
	api := slack.New(token)
	msgs, _, _, err := api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: strings.TrimSpace(threadTS),
		Inclusive: false,
		Limit:     limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SlackMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, slackMessageFromAPI(ctx, api, channelID, m))
	}
	return out, nil
}

// ConversationReplies fetches messages in a Slack thread.
func ConversationReplies(ctx context.Context, token, channelID, threadTS string, limit int) ([]SlackMessage, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrNoToken
	}
	channelID = normalizeSlackReadChannelID(channelID)
	if strings.TrimSpace(channelID) == "" {
		return nil, fmt.Errorf("channel is required")
	}
	threadTS = strings.TrimSpace(threadTS)
	if threadTS == "" {
		return nil, fmt.Errorf("thread ts is required")
	}
	if limit <= 0 {
		limit = 100
	}
	return slackConversationRepliesFn(ctx, token, channelID, threadTS, limit)
}

func slackMessageFromAPI(ctx context.Context, api *slack.Client, channelID string, m slack.Message) SlackMessage {
	files := slackFilesFromAPIWithContent(ctx, api, channelID, m.Files)
	return SlackMessage{
		User:        firstNonEmpty(m.User, m.Username),
		Text:        strings.TrimSpace(m.Text),
		TS:          m.Timestamp,
		ThreadTS:    m.ThreadTimestamp,
		SubType:     m.SubType,
		Files:       files,
		ReplyCount:  m.ReplyCount,
		LatestReply: m.LatestReply,
	}
}

var slackConversationMembersFn = func(ctx context.Context, token, channelID string, limit int) ([]string, error) {
	api := slack.New(token)
	out := make([]string, 0, limit)
	cursor := ""
	for len(out) < limit {
		pageLimit := min(limit-len(out), 200)
		members, next, err := api.GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{
			ChannelID: channelID,
			Cursor:    cursor,
			Limit:     pageLimit,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, members...)
		cursor = strings.TrimSpace(next)
		if cursor == "" {
			break
		}
	}
	return out, nil
}

// ConversationMembers fetches member IDs for a conversation and resolves names
// best-effort through the existing Slack user directory helpers.
func ConversationMembers(ctx context.Context, token, channelID string, limit int) ([]SlackUser, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrNoToken
	}
	channelID = normalizeSlackReadChannelID(channelID)
	if strings.TrimSpace(channelID) == "" {
		return nil, fmt.Errorf("channel is required")
	}
	if limit <= 0 {
		limit = 200
	}
	ids, err := slackConversationMembersFn(ctx, token, channelID, limit)
	if err != nil {
		return nil, err
	}
	names := ResolveUserNames(ctx, token, ids)
	out := make([]SlackUser, 0, len(ids))
	for _, id := range ids {
		name := strings.TrimSpace(names[id])
		out = append(out, SlackUser{
			ID:          id,
			Name:        name,
			DisplayName: name,
			RealName:    name,
		})
	}
	return out, nil
}

// SlackReaction is the compact reaction shape exposed by the read CLI.
type SlackReaction struct {
	Name  string   `json:"name"`
	Count int      `json:"count"`
	Users []string `json:"users,omitempty"`
}

var slackReactionsFn = func(ctx context.Context, token, channelID, ts string) ([]slack.ItemReaction, error) {
	item, err := slack.New(token).GetReactionsContext(ctx, slack.ItemRef{
		Channel:   channelID,
		Timestamp: strings.TrimSpace(ts),
	}, slack.GetReactionsParameters{Full: true})
	if err != nil {
		return nil, err
	}
	return item.Reactions, nil
}

// MessageReactions fetches emoji reactions for a Slack message.
func MessageReactions(ctx context.Context, token, channelID, ts string) ([]SlackReaction, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrNoToken
	}
	channelID = normalizeSlackReadChannelID(channelID)
	if strings.TrimSpace(channelID) == "" {
		return nil, fmt.Errorf("channel is required")
	}
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return nil, fmt.Errorf("ts is required")
	}
	reactions, err := slackReactionsFn(ctx, token, channelID, ts)
	if err != nil {
		return nil, err
	}
	out := make([]SlackReaction, 0, len(reactions))
	for _, r := range reactions {
		out = append(out, SlackReaction{Name: strings.TrimSpace(r.Name), Count: r.Count, Users: r.Users})
	}
	return out, nil
}

// SlackSearchMatch is the compact search.messages result exposed by the CLI.
type SlackSearchMatch struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	User        string `json:"user"`
	Username    string `json:"username,omitempty"`
	TS          string `json:"ts"`
	Text        string `json:"text"`
	Permalink   string `json:"permalink,omitempty"`
}

var slackSearchMessagesFn = func(ctx context.Context, token, query, sort string, limit int) ([]slack.SearchMessage, error) {
	params := slack.NewSearchParameters()
	params.Count = limit
	if strings.TrimSpace(sort) != "" {
		params.Sort = strings.TrimSpace(sort)
	}
	msgs, err := slack.New(token).SearchMessagesContext(ctx, query, params)
	if err != nil {
		return nil, err
	}
	return msgs.Matches, nil
}

// SearchMessages runs Slack search.messages. Slack requires a user token with
// search:read; callers enforce the user-token precondition before this call.
func SearchMessages(ctx context.Context, token, query, sort string, limit int) ([]SlackSearchMatch, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrNoToken
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	if limit <= 0 {
		limit = 20
	}
	if strings.TrimSpace(sort) == "" {
		sort = "score"
	}
	matches, err := slackSearchMessagesFn(ctx, token, query, sort, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SlackSearchMatch, 0, len(matches))
	for _, m := range matches {
		out = append(out, SlackSearchMatch{
			ChannelID:   strings.TrimSpace(m.Channel.ID),
			ChannelName: strings.TrimSpace(m.Channel.Name),
			User:        strings.TrimSpace(firstNonEmpty(m.User, m.Username)),
			Username:    strings.TrimSpace(m.Username),
			TS:          strings.TrimSpace(m.Timestamp),
			Text:        strings.TrimSpace(m.Text),
			Permalink:   strings.TrimSpace(m.Permalink),
		})
	}
	return out, nil
}
