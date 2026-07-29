package exitcode

import "testing"

// The numeric values are a public contract. This test exists so that
// renumbering one is a deliberate act with a failing test attached,
// rather than a silent break of somebody's CI pipeline.
func TestCodeValuesAreStable(t *testing.T) {
	want := map[string]int{
		"OK": 0, "Unexpected": 1, "Usage": 2, "Auth": 3, "Forbidden": 4,
		"NotFound": 5, "Conflict": 6, "Precondition": 7, "Invalid": 8,
		"RateLimited": 9, "Server": 10, "Network": 11, "WaitTimeout": 12,
		"Config": 13,
	}
	got := map[string]int{
		"OK": int(OK), "Unexpected": int(Unexpected), "Usage": int(Usage),
		"Auth": int(Auth), "Forbidden": int(Forbidden), "NotFound": int(NotFound),
		"Conflict": int(Conflict), "Precondition": int(Precondition),
		"Invalid": int(Invalid), "RateLimited": int(RateLimited),
		"Server": int(Server), "Network": int(Network),
		"WaitTimeout": int(WaitTimeout), "Config": int(Config),
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %d, want %d (exit codes are a public contract)", name, got[name], w)
		}
	}
	if len(All) != len(want) {
		t.Errorf("All has %d entries, want %d — add new codes to All", len(All), len(want))
	}
}

func TestEveryCodeHasDescription(t *testing.T) {
	for _, c := range All {
		if d := c.Description(); d == "" || d == "unknown" {
			t.Errorf("code %d has no description", c)
		}
	}
}

func TestFromHTTPStatus(t *testing.T) {
	tests := []struct {
		status int
		want   Code
	}{
		{200, OK},
		{201, OK},
		{202, OK},
		{204, OK},
		{400, Invalid},
		{401, Auth},
		{403, Forbidden},
		{404, NotFound},
		{409, Conflict},
		{410, NotFound}, // upstream_missing: the row is stale, treat as gone
		{412, Precondition},
		{422, Invalid},
		{429, RateLimited},
		{500, Server},
		{502, Server},
		{503, Server},
		{504, Server},
	}
	for _, tt := range tests {
		if got := FromHTTPStatus(tt.status); got != tt.want {
			t.Errorf("FromHTTPStatus(%d) = %d, want %d", tt.status, got, tt.want)
		}
	}
}
