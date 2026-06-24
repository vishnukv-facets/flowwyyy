package flowdb_test

// This external test package (flowdb_test, not flowdb) blank-imports productdbreg
// so its init() registers the product migration set before any test runs, and
// OpenDB in the flowdb test binary creates the product tables that flowdb's
// product-CRUD tests (chats_test.go, attention_test.go, steering_trace_test.go,
// …) rely on. Without it, OpenDB would create a core-only DB and every
// product-table test would fail with "no such table".
//
// It MUST be the external test package: productdbreg imports flowdb, so an
// in-package (package flowdb) file importing it would be an import cycle.
// flowdb_test → productdbreg → {flowdb, productdb} is acyclic (productdb is
// flowdb-free; see seam §11).
import _ "flow/internal/productdbreg"
