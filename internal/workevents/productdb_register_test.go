package workevents

// Test-only: workevents' builder tests read the product attention_feed table
// (the activity log surfaces Attention items). productdb is flowdb-free and no
// longer self-registers (seam §11), so blank-import productdbreg to register the
// product migration set — then flowdb.OpenDB creates those tables in workevents'
// TEST binary. workevents' NON-test code stays product-free. productdbreg
// dedupes, so overlapping triggers are safe.
import _ "flow/internal/productdbreg"
