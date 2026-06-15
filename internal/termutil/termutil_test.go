package termutil

import (
	"errors"
	"testing"
)

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":         "''",
		"plain":    "'plain'",
		"a b":      "'a b'",
		"it's":     `'it'\''s'`, // embedded single quote is escaped
		"a'b'c":    `'a'\''b'\''c'`,
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEscapeAppleScript(t *testing.T) {
	// backslash first, then quote — order matters so we don't double-escape.
	if got := EscapeAppleScript(`a\b"c`); got != `a\\b\"c` {
		t.Errorf("EscapeAppleScript = %q", got)
	}
}

func TestAccessibilityDenied(t *testing.T) {
	if AccessibilityDenied(nil) {
		t.Error("nil error must not be a denial")
	}
	if AccessibilityDenied(errors.New("some unrelated failure")) {
		t.Error("unrelated error must not be a denial")
	}
	for _, msg := range []string{
		"osascript: execution error: System Events is not allowed to send keystrokes (-1002)",
		"not authorized to send Apple events",
	} {
		if !AccessibilityDenied(errors.New(msg)) {
			t.Errorf("expected denial for %q", msg)
		}
	}
}
