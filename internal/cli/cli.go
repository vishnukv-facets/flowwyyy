// Package cli is the per-process command registry shared by flow-core and the
// flowwyyy product layer. Importing it pulls in no product code (stdlib only),
// so both cmd/flow and cmd/flowwyyy can register their own command sets against
// the same registry type without a dependency-direction violation.
package cli

import "sort"

// SessionTokenFileName is the filename of the Mission Control UI session token
// under the flow root. It lives in core cli so that core call-sites (e.g.
// app/tell.go) can reference it without importing the product server package.
const SessionTokenFileName = ".ui-session-token"

// Command is a single CLI verb. Run receives the args after the verb and
// returns a process exit code (0 success, 1 runtime error, 2 usage error).
type Command struct {
	Name   string
	Run    func(args []string) int
	Help   string
	Hidden bool
}

var registry = map[string]Command{}

// Register adds (or replaces) a command in the registry.
func Register(c Command) { registry[c.Name] = c }

// Lookup returns the command registered under name, if any.
func Lookup(name string) (Command, bool) { c, ok := registry[name]; return c, ok }

// Each invokes fn for every registered command, in name order.
func Each(fn func(Command)) {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fn(registry[n])
	}
}

// Reset clears the registry. Intended for tests.
func Reset() { registry = map[string]Command{} }
