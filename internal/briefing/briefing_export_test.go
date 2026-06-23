package briefing_test

// briefing.Build reads the product attention_feed table (via flowdb's attention
// query funcs), and the briefing tests seed/read it. This external test package
// blank-imports productdb so its init() registers the product migration set and
// flowdb.OpenDB creates attention_feed in the briefing test binary. Without it
// the seed step fails with "no such table: attention_feed".
//
// The live single binary already pulls in productdb (via steering/monitor), so
// `flow standup` works in production; only this unit-test binary needed wiring.
// It MUST be the external test package (briefing_test): briefing is a core
// package and must not import productdb in non-test code (productdb imports
// flowdb; briefing_test → productdb → flowdb is acyclic and keeps briefing's
// own deps product-free).
import _ "flow/internal/productdb"
