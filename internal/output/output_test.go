package output

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func newTestWriter(format Format) (*Writer, *bytes.Buffer, *bytes.Buffer) {
	var out, errBuf bytes.Buffer
	return &Writer{Out: &out, Err: &errBuf, Format: format}, &out, &errBuf
}

// The verbatim promise: whatever the API sent, `-o json` prints — same
// fields, same names, same numbers. Only the envelope is unwrapped.
func TestJSONIsVerbatim(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "list envelope is unwrapped one level",
			in:   `{"data":[{"id":"a"},{"id":"b"}]}`,
			want: `[{"id":"a"},{"id":"b"}]`,
		},
		{
			name: "single object passes through",
			in:   `{"id":"a","name":"x"}`,
			want: `{"id":"a","name":"x"}`,
		},
		{
			// A resource that legitimately has a `data` field alongside
			// others must not be mistaken for an envelope.
			name: "object with data plus siblings is not an envelope",
			in:   `{"data":"payload","id":"a"}`,
			want: `{"data":"payload","id":"a"}`,
		},
		{
			// The reason we json.Indent instead of Unmarshal+Marshal:
			// a float64 round-trip would destroy this integer.
			name: "large integers survive",
			in:   `{"n":10000000000000001}`,
			want: `{"n":10000000000000001}`,
		},
		{
			name: "unknown future fields survive",
			in:   `{"data":[{"id":"a","fieldTheSpecDoesNotKnow":{"deep":[1,2]}}]}`,
			want: `[{"id":"a","fieldTheSpecDoesNotKnow":{"deep":[1,2]}}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, out, _ := newTestWriter(FormatJSON)
			if err := w.Render([]byte(tt.in), nil); err != nil {
				t.Fatalf("Render: %v", err)
			}
			got := compact(t, out.String())
			if got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

// compact strips the pretty-printing so a comparison is about content,
// not indentation.
func compact(t *testing.T, s string) string {
	t.Helper()
	var b strings.Builder
	inString := false
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			escaped = false
			b.WriteRune(r)
		case r == '\\' && inString:
			escaped = true
			b.WriteRune(r)
		case r == '"':
			inString = !inString
			b.WriteRune(r)
		case !inString && (r == ' ' || r == '\n' || r == '\t'):
			// drop
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestTableRendering(t *testing.T) {
	cols := []Column{
		{Header: "id", Value: func(r map[string]any) string { return Field(r, "id") }},
		{Header: "name", Value: func(r map[string]any) string { return Field(r, "name") }},
		{Header: "cpu", Value: func(r map[string]any) string { return Field(r, "cpuCores") }},
		{Header: "node", Wide: true, Value: func(r map[string]any) string { return Field(r, "node") }},
	}
	body := []byte(`{"data":[
		{"id":"i-1","name":"build","cpuCores":10,"node":"flow-mbm-0901"},
		{"id":"i-2","name":"test","cpuCores":4,"node":null}
	]}`)

	t.Run("table hides wide columns", func(t *testing.T) {
		w, out, _ := newTestWriter(FormatTable)
		if err := w.Render(body, cols); err != nil {
			t.Fatalf("Render: %v", err)
		}
		got := out.String()
		if !strings.Contains(got, "ID") || !strings.Contains(got, "CPU") {
			t.Errorf("missing headers:\n%s", got)
		}
		if strings.Contains(got, "NODE") {
			t.Errorf("wide column leaked into table output:\n%s", got)
		}
		// Integers must not print as 10.000000 or 1e+01.
		if !strings.Contains(got, "10") || strings.Contains(got, "10.0") {
			t.Errorf("integer rendering is wrong:\n%s", got)
		}
	})

	t.Run("wide shows them", func(t *testing.T) {
		w, out, _ := newTestWriter(FormatWide)
		if err := w.Render(body, cols); err != nil {
			t.Fatalf("Render: %v", err)
		}
		got := out.String()
		if !strings.Contains(got, "NODE") || !strings.Contains(got, "flow-mbm-0901") {
			t.Errorf("wide output is missing the wide column:\n%s", got)
		}
		// A null must render as "-", visibly distinct from an empty string.
		if !strings.Contains(got, "-") {
			t.Errorf("null should render as '-':\n%s", got)
		}
	})

	t.Run("quiet prints identifiers only", func(t *testing.T) {
		w, out, _ := newTestWriter(FormatTable)
		w.Quiet = true
		if err := w.Render(body, cols); err != nil {
			t.Fatalf("Render: %v", err)
		}
		if got := out.String(); got != "i-1\ni-2\n" {
			t.Errorf("quiet output = %q, want one id per line", got)
		}
	})

	t.Run("no-headers omits the header row", func(t *testing.T) {
		w, out, _ := newTestWriter(FormatTable)
		w.NoHeaders = true
		if err := w.Render(body, cols); err != nil {
			t.Fatalf("Render: %v", err)
		}
		if strings.Contains(out.String(), "ID") {
			t.Errorf("header row was not suppressed:\n%s", out.String())
		}
	})
}

// "No resources found" is a message for a human and must never land on
// stdout, where it would corrupt a pipeline.
func TestEmptyListMessageGoesToStderr(t *testing.T) {
	w, out, errBuf := newTestWriter(FormatTable)
	cols := []Column{{Header: "id", Value: func(r map[string]any) string { return Field(r, "id") }}}
	if err := w.Render([]byte(`{"data":[]}`), cols); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out.String() != "" {
		t.Errorf("stdout should be empty, got %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "No resources found") {
		t.Errorf("stderr should carry the message, got %q", errBuf.String())
	}
}

// An empty list in JSON must be a valid empty array, so `jq length`
// works instead of erroring.
func TestEmptyListJSONIsAnArray(t *testing.T) {
	w, out, _ := newTestWriter(FormatJSON)
	if err := w.Render([]byte(`{"data":[]}`), nil); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := compact(t, out.String()); got != "[]" {
		t.Errorf("got %q, want []", got)
	}
}

func TestYAMLRendering(t *testing.T) {
	w, out, _ := newTestWriter(FormatYAML)
	if err := w.Render([]byte(`{"data":[{"id":"a","cpu":4}]}`), nil); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "id: a") || !strings.Contains(got, "cpu: 4") {
		t.Errorf("unexpected YAML:\n%s", got)
	}
}

func TestParseFormat(t *testing.T) {
	for _, s := range []string{"table", "TABLE", " json ", "yaml", "wide"} {
		if _, err := ParseFormat(s); err != nil {
			t.Errorf("ParseFormat(%q) failed: %v", s, err)
		}
	}
	if _, err := ParseFormat("xml"); err == nil {
		t.Error("ParseFormat(\"xml\") should fail")
	}
}

// now is frozen so the relative-time rendering is deterministic.
func TestAge(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tests := map[string]string{
		"2026-07-29T11:59:30Z": "30s",
		"2026-07-29T11:30:00Z": "30m",
		"2026-07-29T06:00:00Z": "6h",
		"2026-07-22T12:00:00Z": "7d",
		"":                     "",
		"not-a-timestamp":      "not-a-timestamp",
	}
	for in, want := range tests {
		if got := Age(in, now); got != want {
			t.Errorf("Age(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNoColorHonoursConventions(t *testing.T) {
	t.Run("NO_COLOR wins over a TTY", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		t.Setenv("CLOUDCONSOLE_COLOR", "")
		t.Setenv("CI", "")
		if !NoColor(false, true) {
			t.Error("NO_COLOR should suppress colour even on a TTY")
		}
	})
	t.Run("CI suppresses colour", func(t *testing.T) {
		t.Setenv("NO_COLOR", "")
		t.Setenv("CLOUDCONSOLE_COLOR", "")
		t.Setenv("CI", "true")
		if !NoColor(false, true) {
			t.Error("CI should suppress colour")
		}
	})
	t.Run("explicit always beats CI", func(t *testing.T) {
		t.Setenv("NO_COLOR", "")
		t.Setenv("CI", "true")
		t.Setenv("CLOUDCONSOLE_COLOR", "always")
		if NoColor(false, true) {
			t.Error("CLOUDCONSOLE_COLOR=always should force colour on")
		}
	})
	t.Run("non-tty suppresses colour", func(t *testing.T) {
		t.Setenv("NO_COLOR", "")
		t.Setenv("CLOUDCONSOLE_COLOR", "")
		t.Setenv("CI", "")
		t.Setenv("TERM", "xterm")
		if !NoColor(false, false) {
			t.Error("a non-TTY should suppress colour")
		}
	})
}
