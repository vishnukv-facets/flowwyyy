package server

// Test-only: server's tests open the DB via flowdb.OpenDB and exercise the
// attention / steering / chats / github / remote_devices product tables.
// productdb is flowdb-free and no longer self-registers (seam §11); at runtime
// the product binary registers via internal/product, and server is handed an
// already-migrated *sql.DB. server's NON-test code stays flowdb-free — so this
// blank import lives in the TEST binary only, to create the product tables that
// server tests read/write. productdbreg dedupes per domain.
import _ "flow/internal/productdbreg"
