package api

import "testing"

// The retry policy's safety depends on this list matching the server. If
// somebody mounts idempotency() on a sixth router — or removes one — the
// CLI would either miss a safe retry or, far worse, retry a mutation the
// server will not deduplicate. Pinning the exact set makes that a
// deliberate, reviewed change.
func TestIdempotencyMountsArePinned(t *testing.T) {
	want := []string{
		"/v1/xcloud/instances",
		"/v1/xcloud-security-groups",
		"/v1/xcloud/networks",
		"/v1/resources",
		"/v1/buildkite/stacks",
		"/v1/github-actions/stacks",
	}
	if len(idempotencyMounts) != len(want) {
		t.Fatalf("idempotencyMounts has %d entries, want %d — "+
			"if the server changed, update the list AND this test",
			len(idempotencyMounts), len(want))
	}
	for i, w := range want {
		if idempotencyMounts[i] != w {
			t.Errorf("idempotencyMounts[%d] = %q, want %q", i, idempotencyMounts[i], w)
		}
	}
}

func TestSupportsIdempotencyKey(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/v1/resources", true},
		{"/v1/resources/", true},
		{"/v1/resources/abc-123", true},
		{"/v1/resources/abc-123/start", true},
		{"/v1/resources/abc/snapshots", true},
		{"/v1/xcloud-security-groups", true},
		{"/v1/xcloud-security-groups/web", true},
		{"/v1/xcloud/networks", true},
		{"/v1/buildkite/stacks/1/jobs", true},
		{"/v1/github-actions/stacks", true},

		{"/v1/xcloud/instances", true},
		{"/v1/xcloud/instances/abc/start", true},
		// A prefix that merely starts with the same characters must not
		// match — this is why the check is path-segment aware.
		{"/v1/resources-archive", false},
		{"/v1/xcloud/networks-v2", false},
		{"/v1/xcloud/volumes", false},
		{"/v1/api-keys", false},
		{"/v1/me", false},
		{"/", false},
		{"", false},

		// Query strings and absolute URLs must be tolerated.
		{"/v1/resources?type=vm", true},
		{"https://api.cloud.flow.swiss/v1/resources", true},
		{"https://api.cloud.flow.swiss/v1/xcloud/instances", true},
		// A prefix that merely shares characters must still not match.
		{"/v1/xcloud/instances-archive", false},
	}
	for _, tt := range tests {
		if got := SupportsIdempotencyKey(tt.path); got != tt.want {
			t.Errorf("SupportsIdempotencyKey(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
