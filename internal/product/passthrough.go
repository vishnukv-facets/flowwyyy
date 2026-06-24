package product

import (
	"flow/internal/cli"
	"flow/internal/flowclient"
)

// productVerbs are the commands the flowwyyy binary handles natively (in
// process). Every other verb is a core engine verb and is passed through to the
// resolved `flow` binary, so the full command surface is preserved with
// byte-identical behavior. Note: the core verbs are ALSO present in the shared
// cli registry (app.init → registerCore runs because product imports app), so we
// gate on this explicit product set rather than the registry to decide
// in-process vs passthrough.
var productVerbs = map[string]bool{
	"ui":        true,
	"serve":     true,
	"attention": true,
	"slack":     true,
	// skill is handled in-process so the composed (core + product) skill is
	// installed; passing it through would install the core-only skill.
	"skill": true,
}

// RunWithPassthrough dispatches a product command in-process, or execs the core
// flow binary for any other verb (and for help/usage/version), inheriting stdio
// so the experience is identical to running `flow` directly.
func RunWithPassthrough(bin string, args []string) int {
	FlowBin = bin
	if len(args) > 0 && productVerbs[args[0]] {
		if c, ok := cli.Lookup(args[0]); ok {
			return c.Run(args[1:])
		}
	}
	// Core verb (or no args / --help / version) → exec the resolved flow binary.
	return flowclient.Exec(bin, args)
}
