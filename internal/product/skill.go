package product

import (
	_ "embed"

	"flow/internal/app"
)

//go:embed skill/SKILL.flowwyyy.md
var embeddedProductSkill []byte

func ComposeSkill(core []byte) []byte {
	out := make([]byte, 0, len(core)+len(embeddedProductSkill))
	out = append(out, core...)
	out = append(out, embeddedProductSkill...)
	return out
}

// cmdSkill is flowwyyy's native `skill` handler. It composes the core skill
// fragment with the product fragment, then reuses the core install/update/
// print/uninstall logic so the FULL agent skill (core + Attention/Slack/Owners/
// inbox-monitor sections) is installed — preserving today's single-binary
// experience. Without this, `flowwyyy skill install` would pass through to the
// core binary and install a core-only skill.
func cmdSkill(args []string) int {
	app.SetEmbeddedSkill(ComposeSkill(app.EmbeddedCoreSkill()))
	return app.RunSkillCommand(args)
}
