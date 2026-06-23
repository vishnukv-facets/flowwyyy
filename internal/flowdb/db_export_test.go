package flowdb_test

// This external test package (flowdb_test, not flowdb) blank-imports productdb
// so its init() registers the product migration set before any test runs.
// Without it, OpenDB in the flowdb test binary would create a core-only DB and
// every product-table test here (chats_test.go, attention_test.go,
// steering_trace_test.go, …) would fail with "no such table".
//
// It MUST live in the external test package: productdb imports flowdb, so an
// in-package (package flowdb) test file importing productdb would be an import
// cycle. flowdb_test → productdb → flowdb is acyclic.
import _ "flow/internal/productdb"
