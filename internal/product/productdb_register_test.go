package product

// Test-only: product tests (e.g. attention_test.go) open the DB via
// flowdb.OpenDB and exercise the product tables. The runtime product binary
// creates those tables via productdb.Open, so product's NON-test code no longer
// blank-imports productdbreg (which would re-introduce a transitive flowdb
// import). This test-only blank import keeps the TEST binary registering the
// product migration set with flowdb.OpenDB. productdbreg dedupes per domain.
import _ "flow/internal/productdbreg"
