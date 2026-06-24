package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"flow/internal/monitor"
	"flow/internal/server"
)

var slackHydrateFn = server.HydrateSlackSecretsFromKeyring

var slackSearchUsersFn = func(token, query string) ([]monitor.SlackUser, error) {
	return monitor.SearchUsers(context.Background(), token, query)
}

var slackLookupUserByEmailFn = func(token, email string) (monitor.SlackUser, error) {
	return monitor.LookupUserByEmail(context.Background(), token, email)
}

var slackUserInfoReadFn = func(token, id string) (monitor.SlackUser, error) {
	return monitor.UserInfo(context.Background(), token, id)
}

var slackListChannelsFn = func(token string, includeDMs bool) ([]monitor.SlackChannelInfo, error) {
	return monitor.ListSlackChannelsWithToken(context.Background(), token, includeDMs)
}

var slackResolveChannelFn = func(token, ref string, includeDMs bool) (monitor.SlackChannelInfo, error) {
	return monitor.ResolveSlackChannel(context.Background(), token, ref, includeDMs)
}

var slackHistoryReadFn = func(token string, opts monitor.SlackHistoryOptions) ([]monitor.SlackMessage, error) {
	return monitor.ConversationHistory(context.Background(), token, opts)
}

var slackThreadReadFn = func(token, channelID, threadTS string, limit int) ([]monitor.SlackMessage, error) {
	return monitor.ConversationReplies(context.Background(), token, channelID, threadTS, limit)
}

var slackMembersReadFn = func(token, channelID string, limit int) ([]monitor.SlackUser, error) {
	return monitor.ConversationMembers(context.Background(), token, channelID, limit)
}

var slackReactionsReadFn = func(token, channelID, ts string) ([]monitor.SlackReaction, error) {
	return monitor.MessageReactions(context.Background(), token, channelID, ts)
}

var slackSearchMessagesReadFn = func(token, query, sort string, limit int) ([]monitor.SlackSearchMatch, error) {
	return monitor.SearchMessages(context.Background(), token, query, sort, limit)
}

var (
	errSlackNoToken     = errors.New("no Slack token configured — connect Slack in Mission Control, or set FLOW_SLACK_TOKEN / FLOW_SLACK_USER_TOKEN")
	errSlackNoUserToken = errors.New("message search needs a Slack user token with the search:read scope — connect Slack (user token) in Mission Control or set FLOW_SLACK_USER_TOKEN")
)

func slackReadToken(as string, requireUser bool) (string, error) {
	slackHydrateFn()
	as = strings.ToLower(strings.TrimSpace(as))
	if requireUser {
		if as == "bot" {
			return "", errSlackNoUserToken
		}
		if tok := strings.TrimSpace(monitor.SlackUserToken()); tok != "" {
			return tok, nil
		}
		return "", errSlackNoUserToken
	}
	switch as {
	case "user":
		if tok := strings.TrimSpace(monitor.SlackUserToken()); tok != "" {
			return tok, nil
		}
	case "bot":
		if tok := strings.TrimSpace(monitor.SlackBotToken()); tok != "" {
			return tok, nil
		}
	default:
		if tok := strings.TrimSpace(monitor.SlackUserToken()); tok != "" {
			return tok, nil
		}
		if tok := strings.TrimSpace(monitor.SlackBotToken()); tok != "" {
			return tok, nil
		}
	}
	return "", errSlackNoToken
}

func validSlackFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "table", "json", "tsv":
		return true
	default:
		return false
	}
}

func validSlackAs(as string) bool {
	switch strings.ToLower(strings.TrimSpace(as)) {
	case "", "bot", "user":
		return true
	default:
		return false
	}
}

func emitSlack(format string, headers []string, rows [][]string, jsonVal any) int {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json":
		b, err := json.MarshalIndent(jsonVal, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Fprintln(os.Stdout, string(b))
		return 0
	case "tsv":
		fmt.Fprintln(os.Stdout, strings.Join(headers, "\t"))
		for _, row := range rows {
			fmt.Fprintln(os.Stdout, strings.Join(row, "\t"))
		}
		return 0
	case "", "table":
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, strings.Join(headers, "\t"))
		for _, row := range rows {
			fmt.Fprintln(w, strings.Join(row, "\t"))
		}
		if err := w.Flush(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "error: --format must be table, json, or tsv")
		return 2
	}
}

func cmdSlackSearchUsers(args []string) int {
	fs := flagSet("slack search-users")
	format := fs.String("format", "table", "output format: table, json, or tsv")
	as := fs.String("as", "", "read identity: bot or user (default prefers user, falls back to bot)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !validSlackFormat(*format) {
		return emitSlack(*format, nil, nil, nil)
	}
	if !validSlackAs(*as) {
		fmt.Fprintln(os.Stderr, "error: --as must be 'bot' or 'user'")
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		fmt.Fprintln(os.Stderr, "error: search-users requires a query")
		return 2
	}
	token, err := slackReadToken(*as, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	query := strings.TrimSpace(rest[0])
	var users []monitor.SlackUser
	if strings.Contains(query, "@") {
		u, err := slackLookupUserByEmailFn(token, query)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		if strings.TrimSpace(u.ID) != "" {
			users = []monitor.SlackUser{u}
		}
	} else {
		users, err = slackSearchUsersFn(token, query)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
	}
	return emitSlack(*format, slackUserHeaders(), slackUserRows(users), users)
}

func cmdSlackUser(args []string) int {
	fs := flagSet("slack user")
	id := fs.String("id", "", "Slack user id")
	email := fs.String("email", "", "Slack user email")
	format := fs.String("format", "table", "output format: table, json, or tsv")
	as := fs.String("as", "", "read identity: bot or user (default prefers user, falls back to bot)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !validSlackFormat(*format) {
		return emitSlack(*format, nil, nil, nil)
	}
	if !validSlackAs(*as) {
		fmt.Fprintln(os.Stderr, "error: --as must be 'bot' or 'user'")
		return 2
	}
	userID := strings.TrimSpace(*id)
	userEmail := strings.TrimSpace(*email)
	if (userID == "") == (userEmail == "") {
		fmt.Fprintln(os.Stderr, "error: exactly one of --id or --email is required")
		return 2
	}
	token, err := slackReadToken(*as, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	var user monitor.SlackUser
	if userEmail != "" {
		user, err = slackLookupUserByEmailFn(token, userEmail)
	} else {
		user, err = slackUserInfoReadFn(token, userID)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return emitSlack(*format, []string{"FIELD", "VALUE"}, slackUserDetailRows(user), user)
}

func cmdSlackListChannels(args []string) int {
	fs := flagSet("slack list-channels")
	kind := fs.String("kind", "", "comma-separated channel kinds: public,private,im,mpim,groups")
	match := fs.String("match", "", "case-insensitive id/name substring filter")
	format := fs.String("format", "table", "output format: table, json, or tsv")
	as := fs.String("as", "", "read identity: bot or user (default prefers user, falls back to bot)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return runSlackListChannels(*format, *as, *kind, *match)
}

func cmdSlackSearchChannels(args []string) int {
	fs := flagSet("slack search-channels")
	format := fs.String("format", "table", "output format: table, json, or tsv")
	as := fs.String("as", "", "read identity: bot or user (default prefers user, falls back to bot)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		fmt.Fprintln(os.Stderr, "error: search-channels requires a query")
		return 2
	}
	return runSlackListChannels(*format, *as, "", rest[0])
}

func cmdSlackHistory(args []string) int {
	fs := flagSet("slack history")
	channel := fs.String("channel", "", "Slack channel id, #name, or name")
	limit := fs.Int("limit", 50, "maximum messages to fetch")
	oldest := fs.String("oldest", "", "Slack ts lower bound")
	latest := fs.String("latest", "", "Slack ts upper bound")
	format := fs.String("format", "table", "output format: table, json, or tsv")
	as := fs.String("as", "", "read identity: bot or user (default prefers user, falls back to bot)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*channel) == "" {
		fmt.Fprintln(os.Stderr, "error: --channel is required")
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(os.Stderr, "error: --limit must be >= 0")
		return 2
	}
	token, includeDMs, rc := slackReadTokenAndDMs(*as, false, *format)
	if rc != -1 {
		return rc
	}
	ch, err := slackResolveChannelFn(token, *channel, includeDMs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	msgs, err := slackHistoryReadFn(token, monitor.SlackHistoryOptions{
		ChannelID: ch.ID,
		Oldest:    strings.TrimSpace(*oldest),
		Latest:    strings.TrimSpace(*latest),
		Limit:     *limit,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return emitSlack(*format, slackMessageHeaders(), slackMessageRows(msgs), msgs)
}

func cmdSlackThread(args []string) int {
	fs := flagSet("slack thread")
	channel := fs.String("channel", "", "Slack channel id, #name, or name")
	ts := fs.String("ts", "", "thread root timestamp")
	limit := fs.Int("limit", 100, "maximum replies to fetch")
	format := fs.String("format", "table", "output format: table, json, or tsv")
	as := fs.String("as", "", "read identity: bot or user (default prefers user, falls back to bot)")
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
	if *limit < 0 {
		fmt.Fprintln(os.Stderr, "error: --limit must be >= 0")
		return 2
	}
	token, includeDMs, rc := slackReadTokenAndDMs(*as, false, *format)
	if rc != -1 {
		return rc
	}
	ch, err := slackResolveChannelFn(token, *channel, includeDMs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	msgs, err := slackThreadReadFn(token, ch.ID, strings.TrimSpace(*ts), *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return emitSlack(*format, slackMessageHeaders(), slackMessageRows(msgs), msgs)
}

func cmdSlackMembers(args []string) int {
	fs := flagSet("slack members")
	channel := fs.String("channel", "", "Slack channel id, #name, or name")
	limit := fs.Int("limit", 200, "maximum members to fetch")
	format := fs.String("format", "table", "output format: table, json, or tsv")
	as := fs.String("as", "", "read identity: bot or user (default prefers user, falls back to bot)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*channel) == "" {
		fmt.Fprintln(os.Stderr, "error: --channel is required")
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(os.Stderr, "error: --limit must be >= 0")
		return 2
	}
	token, includeDMs, rc := slackReadTokenAndDMs(*as, false, *format)
	if rc != -1 {
		return rc
	}
	ch, err := slackResolveChannelFn(token, *channel, includeDMs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	members, err := slackMembersReadFn(token, ch.ID, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return emitSlack(*format, slackUserHeaders(), slackUserRows(members), members)
}

func cmdSlackReactions(args []string) int {
	fs := flagSet("slack reactions")
	channel := fs.String("channel", "", "Slack channel id, #name, or name")
	ts := fs.String("ts", "", "message timestamp")
	format := fs.String("format", "table", "output format: table, json, or tsv")
	as := fs.String("as", "", "read identity: bot or user (default prefers user, falls back to bot)")
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
	token, includeDMs, rc := slackReadTokenAndDMs(*as, false, *format)
	if rc != -1 {
		return rc
	}
	ch, err := slackResolveChannelFn(token, *channel, includeDMs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	reactions, err := slackReactionsReadFn(token, ch.ID, strings.TrimSpace(*ts))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return emitSlack(*format, slackReactionHeaders(), slackReactionRows(reactions), reactions)
}

func cmdSlackSearchMessages(args []string) int {
	fs := flagSet("slack search")
	limit := fs.Int("limit", 20, "maximum matches to fetch")
	sort := fs.String("sort", "score", "sort order: score or timestamp")
	format := fs.String("format", "table", "output format: table, json, or tsv")
	as := fs.String("as", "", "read identity: user only for search.messages")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		fmt.Fprintln(os.Stderr, "error: search requires a query")
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(os.Stderr, "error: --limit must be >= 0")
		return 2
	}
	sortVal := strings.ToLower(strings.TrimSpace(*sort))
	if sortVal == "" {
		sortVal = "score"
	}
	if sortVal != "score" && sortVal != "timestamp" {
		fmt.Fprintln(os.Stderr, "error: --sort must be score or timestamp")
		return 2
	}
	token, _, rc := slackReadTokenAndDMs(*as, true, *format)
	if rc != -1 {
		return rc
	}
	matches, err := slackSearchMessagesReadFn(token, strings.TrimSpace(rest[0]), sortVal, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return emitSlack(*format, slackSearchHeaders(), slackSearchRows(matches), matches)
}

func runSlackListChannels(format, as, kind, match string) int {
	kinds, err := parseSlackChannelKinds(kind)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	token, includeDMs, rc := slackReadTokenAndDMs(as, false, format)
	if rc != -1 {
		return rc
	}
	channels, err := slackListChannelsFn(token, includeDMs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	channels = filterSlackChannels(channels, kinds, match)
	return emitSlack(format, slackChannelHeaders(), slackChannelRows(channels), channels)
}

func slackReadTokenAndDMs(as string, requireUser bool, format string) (token string, includeDMs bool, rc int) {
	if !validSlackFormat(format) {
		return "", false, emitSlack(format, nil, nil, nil)
	}
	if !validSlackAs(as) {
		fmt.Fprintln(os.Stderr, "error: --as must be 'bot' or 'user'")
		return "", false, 2
	}
	token, err := slackReadToken(as, requireUser)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return "", false, 1
	}
	return token, slackEffectiveIdentity(as, token) == "user", -1
}

func slackUserHeaders() []string {
	return []string{"ID", "NAME", "REAL NAME", "BOT"}
}

func slackUserRows(users []monitor.SlackUser) [][]string {
	rows := make([][]string, 0, len(users))
	for _, u := range users {
		rows = append(rows, []string{u.ID, u.Name, firstNonEmptyApp(u.RealName, u.DisplayName), fmt.Sprintf("%t", u.IsBot)})
	}
	return rows
}

func slackUserDetailRows(u monitor.SlackUser) [][]string {
	return [][]string{
		{"ID", u.ID},
		{"Name", u.Name},
		{"Display name", u.DisplayName},
		{"Real name", u.RealName},
		{"Email", u.Email},
		{"Title", u.Title},
		{"Team", u.TeamID},
		{"Bot", fmt.Sprintf("%t", u.IsBot)},
		{"Deleted", fmt.Sprintf("%t", u.Deleted)},
	}
}

func slackEffectiveIdentity(as, token string) string {
	as = strings.ToLower(strings.TrimSpace(as))
	if as == "user" || as == "bot" {
		return as
	}
	if userTok := strings.TrimSpace(monitor.SlackUserToken()); userTok != "" && strings.TrimSpace(token) == userTok {
		return "user"
	}
	return "bot"
}

func parseSlackChannelKinds(kind string) (map[string]bool, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil, nil
	}
	out := map[string]bool{}
	for _, part := range strings.Split(kind, ",") {
		k := strings.ToLower(strings.TrimSpace(part))
		switch k {
		case "public", "private", "im", "mpim":
			out[k] = true
		case "groups":
			out["private"] = true
			out["mpim"] = true
		case "channel":
			out["public"] = true
			out["private"] = true
		default:
			return nil, fmt.Errorf("--kind must be public, private, im, mpim, groups, or comma-separated")
		}
	}
	return out, nil
}

func filterSlackChannels(channels []monitor.SlackChannelInfo, kinds map[string]bool, match string) []monitor.SlackChannelInfo {
	match = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(match, "#")))
	out := make([]monitor.SlackChannelInfo, 0, len(channels))
	for _, ch := range channels {
		kind := slackChannelKind(ch)
		if len(kinds) > 0 && !kinds[kind] {
			continue
		}
		if match != "" && !strings.Contains(strings.ToLower(ch.ID), match) && !strings.Contains(strings.ToLower(ch.Name), match) {
			continue
		}
		out = append(out, ch)
	}
	return out
}

func slackChannelKind(ch monitor.SlackChannelInfo) string {
	switch strings.ToLower(strings.TrimSpace(ch.Kind)) {
	case "im":
		return "im"
	case "mpim":
		return "mpim"
	}
	if ch.IsPrivate {
		return "private"
	}
	return "public"
}

func slackChannelHeaders() []string {
	return []string{"ID", "KIND", "NAME", "MEMBER"}
}

func slackChannelRows(channels []monitor.SlackChannelInfo) [][]string {
	rows := make([][]string, 0, len(channels))
	for _, ch := range channels {
		rows = append(rows, []string{ch.ID, slackChannelKind(ch), ch.Name, fmt.Sprintf("%t", ch.IsMember)})
	}
	return rows
}

func slackMessageHeaders() []string {
	return []string{"TS", "USER", "TEXT"}
}

func slackMessageRows(msgs []monitor.SlackMessage) [][]string {
	rows := make([][]string, 0, len(msgs))
	for _, msg := range msgs {
		rows = append(rows, []string{msg.TS, msg.User, truncateSlackCell(msg.DisplayText(), 160)})
	}
	return rows
}

func slackReactionHeaders() []string {
	return []string{"EMOJI", "COUNT", "USERS"}
}

func slackReactionRows(reactions []monitor.SlackReaction) [][]string {
	rows := make([][]string, 0, len(reactions))
	for _, r := range reactions {
		rows = append(rows, []string{r.Name, fmt.Sprintf("%d", r.Count), strings.Join(r.Users, ",")})
	}
	return rows
}

func slackSearchHeaders() []string {
	return []string{"TS", "CHANNEL", "USER", "TEXT"}
}

func slackSearchRows(matches []monitor.SlackSearchMatch) [][]string {
	rows := make([][]string, 0, len(matches))
	for _, m := range matches {
		channel := firstNonEmptyApp(m.ChannelName, m.ChannelID)
		rows = append(rows, []string{m.TS, channel, m.User, truncateSlackCell(m.Text, 160)})
	}
	return rows
}

func truncateSlackCell(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 3 {
		return string(r[:max])
	}
	return strings.TrimSpace(string(r[:max-3])) + "..."
}

func firstNonEmptyApp(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
