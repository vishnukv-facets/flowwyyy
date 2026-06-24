package steering

// Test-only: steering's own tests open the DB via flowdb.OpenDB and exercise
// the attention/steering product tables. productdb is flowdb-free and no longer
// self-registers (seam §11), and steering's NON-test code is now flowdb-free
// too — so the runtime registration moved to internal/product. This blank
// import keeps steering's TEST binary creating the product tables.
// productdbreg dedupes per domain, so overlapping triggers are safe.
import _ "flow/internal/productdbreg"
