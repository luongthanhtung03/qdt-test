package httpapi

import "time"

// rfc3339Milli is the wire format for every timestamp the API emits.
//
// Fixed-width milliseconds, unlike time.RFC3339Nano, which trims trailing
// zeros and so produces inconsistent-width output. Timestamps are stored as
// integer Unix millis (see docs/DESIGN.md section 2); this is purely the
// presentation format.
const rfc3339Milli = "2006-01-02T15:04:05.000Z07:00"

// dbTimeout bounds any single database operation made on behalf of a request,
// so a stuck write cannot pin a connection from the one-slot write pool
// indefinitely.
const dbTimeout = 5 * time.Second

// maxBodyBytes caps request bodies. Content bodies are prose, not uploads.
const maxBodyBytes = 1 << 20 // 1 MiB
