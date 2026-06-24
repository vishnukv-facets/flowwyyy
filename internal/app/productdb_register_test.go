package app

// Test-only: app's standup/briefing tests exercise product-table reads
// (briefing.Build reads attention_feed). productdb is flowdb-free and no longer
// self-registers (seam §11), so blank-import productdbreg to register the
// product migration set — then flowdb.OpenDB creates those tables in app's TEST
// binary. app's NON-test code stays product-free, so cmd/flow's
// product-free guard is unaffected (test files are excluded from the package
// import graph). productdbreg dedupes, so overlapping triggers are safe.
import _ "flow/internal/productdbreg"
