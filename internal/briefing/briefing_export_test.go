package briefing_test

// briefing.Build reads the product attention_feed table (via flowdb's attention
// query funcs), and the briefing tests seed/read it. This external test package
// blank-imports productdbreg so its init() registers the product migration set
// and flowdb.OpenDB creates attention_feed in the briefing test binary. Without
// it the seed step fails with "no such table: attention_feed".
//
// The live single/product binary already pulls in productdbreg (via
// steering/monitor), so `flow standup` works in production; only this unit-test
// binary needed wiring. It MUST be the external test package (briefing_test):
// briefing is a core package and must not import productdbreg in non-test code
// (productdbreg imports flowdb+productdb; briefing_test → productdbreg keeps
// briefing's own deps product-free).
import _ "flow/internal/productdbreg"
