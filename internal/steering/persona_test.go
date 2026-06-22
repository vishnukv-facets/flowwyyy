package steering

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The operator voice directive is injected into drafting/sending prompts so
// replies read like the operator. It must be empty (a no-op) when no persona is
// configured, and otherwise carry the operator's voice text + the anti-bot rule.
func TestOperatorVoiceDirective(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)

	// No persona file → the built-in DEFAULT voice is the floor (a sensible human
	// tone applied globally so replies never sound like a bot), so the directive
	// carries the default, not nothing.
	if d := operatorVoiceDirective(); d == "" || !strings.Contains(d, "real person") {
		t.Fatalf("no persona configured: directive should carry the default voice, got %q", d)
	}

	const voice = "Warm but terse. Lowercase. No corporate filler. Sign off with '— V'."
	if err := os.WriteFile(filepath.Join(root, "persona.md"), []byte("# voice\n\n"+voice+"\n"), 0o644); err != nil {
		t.Fatalf("write persona: %v", err)
	}
	d := operatorVoiceDirective()
	if d == "" {
		t.Fatal("persona configured: directive should be non-empty")
	}
	if !strings.Contains(d, voice) {
		t.Fatalf("directive should include the operator's voice text; got %q", d)
	}
	// It must carry the anti-bot / no-footer guard.
	low := strings.ToLower(d)
	if !strings.Contains(low, "operator") || !strings.Contains(low, "footer") {
		t.Fatalf("directive should instruct human voice + no footer; got %q", d)
	}
}

// The seeded persona.md carries editing guidance inside an HTML comment; that
// guidance must be stripped so it never leaks into the drafting/send prompt.
func TestOperatorVoiceStripsHTMLComments(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	body := "<!--\nEDITING GUIDANCE: do not inject this\n-->\n\nLowercase, terse, friendly."
	if err := os.WriteFile(filepath.Join(root, "persona.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write persona: %v", err)
	}
	v := OperatorVoice()
	if strings.Contains(v, "EDITING GUIDANCE") {
		t.Fatalf("HTML comment guidance must be stripped; got %q", v)
	}
	if !strings.Contains(v, "Lowercase, terse, friendly.") {
		t.Fatalf("voice content should remain; got %q", v)
	}
}
