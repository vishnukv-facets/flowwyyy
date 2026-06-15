// Package termutil holds string/error helpers shared by the terminal
// backends (iterm, kitty, ghostty, warp, zellij, terminal). They used to be
// copy-pasted byte-for-byte into each backend; this is their single home.
package termutil

import "strings"

// ShellQuote wraps s in single quotes for safe use as one POSIX sh word,
// escaping any embedded single quotes.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// EscapeAppleScript escapes backslashes and double quotes so s can be
// embedded inside an AppleScript double-quoted string literal.
func EscapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// AccessibilityDenied reports whether err is a macOS Accessibility /
// Apple-events permission denial returned by osascript.
func AccessibilityDenied(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, pat := range []string{
		"not allowed assistive access",
		"is not allowed to send keystrokes",
		"is not allowed sending keystrokes",
		"not authorized to send Apple events",
		"(-1002)",
		"(-1719)",
		"(-1743)",
		"(-25211)",
	} {
		if strings.Contains(msg, pat) {
			return true
		}
	}
	return false
}
