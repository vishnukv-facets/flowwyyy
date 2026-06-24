// Package coreskill embeds the core flow agent skill (SKILL.core.md) as a
// neutral, dependency-free source of bytes. Both the core `flow` binary (via
// internal/app) and the flowwyyy product binary (which composes it with its own
// fragment) read it from here, so there is a single copy of the core skill
// markdown in the repo — no duplication, no drift. It imports nothing, so any
// package may depend on it without pulling flowdb or app (Phase-3 decoupling).
package coreskill

import _ "embed"

//go:embed SKILL.core.md
var bytes []byte

// Bytes returns the embedded core skill fragment (SKILL.core.md).
func Bytes() []byte { return bytes }
