package product

import (
	"strings"
	"testing"
)

func TestComposeSkillIncludesCoreAndProduct(t *testing.T) {
	got := ComposeSkill([]byte("## 1. What flow is\ncore\n"))
	text := string(got)
	for _, want := range []string{
		"## 1. What flow is",
		"## Product extensions (flowwyyy)",
		"## 10d. Attention Router feed",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("composed skill missing %q", want)
		}
	}
}
