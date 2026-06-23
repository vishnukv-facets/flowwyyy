package product

import _ "embed"

//go:embed skill/SKILL.flowwyyy.md
var embeddedProductSkill []byte

func ComposeSkill(core []byte) []byte {
	out := make([]byte, 0, len(core)+len(embeddedProductSkill))
	out = append(out, core...)
	out = append(out, embeddedProductSkill...)
	return out
}
