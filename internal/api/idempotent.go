package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// NewIdempotencyKey returns a fresh key for a create.
//
// Random rather than derived from the request body: two deliberate
// creates with identical parameters are a legitimate thing to want, and
// a content-derived key would make the second one replay the first's
// response instead of provisioning anything.
func NewIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("cloudconsole-cli-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// idempotencyMounts lists the API routers that actually mount the
// idempotency() middleware. Sending an Idempotency-Key anywhere else is
// not merely useless — it is actively misleading, because it makes a
// retry look safe when the server will happily create a second resource.
//
// THIS LIST MUST STAY IN LOCKSTEP WITH THE SERVER. The mounts are:
//
//	apps/api/src/routes/v1/xcloud-instances.ts
//	apps/api/src/routes/v1/xcloud-security-groups.ts
//	apps/api/src/routes/v1/xcloud-networks.ts
//	apps/api/src/routes/v1/resources.ts
//	apps/api/src/routes/v1/buildkite-stacks.ts
//	apps/api/src/routes/v1/github-actions-stacks.ts
//
// The last three are not reachable from this CLI (it covers the Xcloud
// stack only) but are listed because this describes the API, and an
// incomplete picture here would be a trap for whoever extends the CLI
// next.
//
// api-keys.ts is deliberately excluded server-side: its one-time secret
// cannot be replayed, so a stored response would be useless.
//
// idempotent_test.go pins this list, so a server-side change that is not
// mirrored here fails a test rather than silently weakening the retry
// policy.
var idempotencyMounts = []string{
	"/v1/xcloud/instances",
	"/v1/xcloud-security-groups",
	"/v1/xcloud/networks",
	"/v1/resources",
	"/v1/buildkite/stacks",
	"/v1/github-actions/stacks",
}

// SupportsIdempotencyKey reports whether the server honours an
// Idempotency-Key header on the given request path.
func SupportsIdempotencyKey(path string) bool {
	// Normalise away an origin, if the caller passed a full URL.
	if i := strings.Index(path, "://"); i >= 0 {
		if j := strings.Index(path[i+3:], "/"); j >= 0 {
			path = path[i+3+j:]
		} else {
			path = "/"
		}
	}
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	for _, mount := range idempotencyMounts {
		if path == mount || strings.HasPrefix(path, mount+"/") {
			return true
		}
	}
	return false
}
