package product

import (
	_ "embed"

	"flow/internal/coreskill"
	"flow/internal/skillinstall"
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
// fragment (from the neutral coreskill package) with the product fragment, then
// runs flowwyyy's own install/update/print/uninstall machinery (internal/
// skillinstall) so the FULL agent skill (core + Attention/Slack/Owners/
// inbox-monitor sections) is installed — preserving today's single-binary
// experience. It no longer imports internal/app (Phase-3 decoupling, seam
// §11.3.1, Tier C); without this, `flowwyyy skill install` would pass through to
// the core binary and install a core-only skill.
func cmdSkill(args []string) int {
	return skillinstall.Run(args, skillinstall.Config{
		Content: ComposeSkill(coreskill.Bytes()),
		Version: Version,
	})
}
