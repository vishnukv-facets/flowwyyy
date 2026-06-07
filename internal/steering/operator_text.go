package steering

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	operatorSentenceRE      = regexp.MustCompile(`[^.!?]+[.!?]*`)
	operatorSlackUserIDRE   = regexp.MustCompile(`\b[UW][0-9][A-Z0-9]{7,}\b`)
	operatorSlackChannelRE  = regexp.MustCompile(`\b[CDG][0-9][A-Z0-9]{7,}\b`)
	operatorWhitespaceRE    = regexp.MustCompile(`\s+`)
	operatorFetchSeparators = []string{", but ", "; but ", " but "}
)

// SanitizeOperatorText removes internal connector-fetch details from text that
// will be shown to the operator. Fetch errors remain in context_json/trace for
// audit, but summaries, reasons, and drafts should explain the work signal, not
// Slack API access mechanics.
func SanitizeOperatorText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	var kept []string
	for _, sentence := range operatorSentenceRE.FindAllString(text, -1) {
		if cleaned := sanitizeOperatorSentence(sentence); cleaned != "" {
			kept = append(kept, cleaned)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	out := strings.Join(kept, " ")
	out = operatorSlackUserIDRE.ReplaceAllString(out, "the sender")
	out = operatorSlackChannelRE.ReplaceAllString(out, "the channel")
	out = operatorWhitespaceRE.ReplaceAllString(out, " ")
	return strings.TrimSpace(out)
}

func sanitizeOperatorSentence(sentence string) string {
	sentence = strings.TrimSpace(sentence)
	if sentence == "" {
		return ""
	}
	lower := strings.ToLower(sentence)
	if !hasInternalFetchMarker(lower) {
		return sentence
	}
	for _, sep := range operatorFetchSeparators {
		idx := strings.Index(lower, sep)
		if idx < 0 {
			continue
		}
		if hasInternalFetchMarker(lower[:idx]) {
			return upperFirst(strings.TrimSpace(sentence[idx+len(sep):]))
		}
	}
	return ""
}

func hasInternalFetchMarker(lower string) bool {
	for _, marker := range []string{
		"context fetch",
		"fetch_status",
		"fetch status",
		"fetch_error",
		"fetch error",
		"not_in_channel",
		"channel_not_found",
		"missing_scope",
		"slack api",
		"slack web api",
		"slack mcp",
		"read token",
		"sender's name couldn't be resolved",
		"sender name couldn't be resolved",
		"name couldn't be resolved",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func upperFirst(s string) string {
	s = strings.TrimLeft(s, " \t\r\n,;:-")
	if s == "" {
		return ""
	}
	runes := []rune(s)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
