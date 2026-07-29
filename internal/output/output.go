// Package output renders command results.
//
// The central decision: `--output json` emits the server's response body
// VERBATIM. No renaming, no re-casing, no computed fields, no pruning —
// only the documented {"data": [...]} envelope is unwrapped one level so
// a list is a JSON array and a get is an object.
//
// The reason is a contract one, not a convenience one. The OpenAPI
// document stays the single source of truth, every jq recipe transfers
// 1:1 between curl and xcloud, and the API's additive-only guarantee
// (docs/public-api.md §1) is inherited for free. A CLI-owned JSON shape
// would be a SECOND public schema with its own 90-day deprecation cycle
// — two contracts to maintain for one API.
//
// Table output carries no such promise and is documented as unstable,
// human-only, do-not-parse.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Format is a rendering mode.
type Format string

const (
	FormatTable Format = "table"
	FormatWide  Format = "wide"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
)

// ParseFormat validates a --output value.
func ParseFormat(s string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case FormatTable:
		return FormatTable, nil
	case FormatWide:
		return FormatWide, nil
	case FormatJSON:
		return FormatJSON, nil
	case FormatYAML:
		return FormatYAML, nil
	default:
		return "", fmt.Errorf("unknown output format %q (want table, wide, json or yaml)", s)
	}
}

// IsStructured reports whether the format is machine-oriented. Progress
// output and human hints are suppressed for these on stdout — they go to
// stderr instead, so `xcloud … -o json | jq` is never corrupted.
func (f Format) IsStructured() bool {
	return f == FormatJSON || f == FormatYAML
}

// Writer renders to a destination with a fixed format.
type Writer struct {
	Out    io.Writer
	Err    io.Writer
	Format Format
	Color  bool
	Quiet  bool
	// NoHeaders suppresses the table header row, for `| while read`.
	NoHeaders bool
}

// Column describes one table column. Value extracts the cell from a row,
// already formatted; returning "" renders as "-" so an empty column is
// visually distinct from a missing one.
type Column struct {
	Header string
	// Wide marks a column shown only with --output wide.
	Wide  bool
	Value func(row map[string]any) string
}

// Render writes the payload. `body` is the raw API response; `columns`
// and `rows` drive table rendering only.
//
// Passing the raw bytes through for the structured formats is what makes
// the verbatim promise enforceable: there is no decode step in which a
// field could be lost.
func (w *Writer) Render(body []byte, columns []Column) error {
	switch w.Format {
	case FormatJSON:
		return w.renderJSON(body)
	case FormatYAML:
		return w.renderYAML(body)
	default:
		return w.renderTable(body, columns)
	}
}

// renderJSON unwraps a single {"data": …} envelope and writes the rest
// through untouched. json.Indent rather than Unmarshal+Marshal, so
// numbers keep their exact source representation — a 64-bit id or a
// high-precision float must not be mangled by a float64 round-trip.
func (w *Writer) renderJSON(body []byte) error {
	payload := unwrapEnvelope(body)
	if len(payload) == 0 {
		return nil
	}
	var buf strings.Builder
	if err := indentInto(&buf, payload); err != nil {
		// Not valid JSON (a 204, or a streaming endpoint): emit as-is
		// rather than failing — the user asked for the payload.
		_, err := w.Out.Write(payload)
		return err
	}
	_, err := fmt.Fprintln(w.Out, buf.String())
	return err
}

// renderYAML necessarily re-encodes, and is documented as a convenience
// with that caveat. Key order follows the JSON source rather than being
// sorted, so the two formats read the same way.
func (w *Writer) renderYAML(body []byte) error {
	payload := unwrapEnvelope(body)
	if len(payload) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		_, err := w.Out.Write(payload)
		return err
	}
	enc := yaml.NewEncoder(w.Out)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return err
	}
	return enc.Close()
}

// unwrapEnvelope strips exactly one level of {"data": …} — the shape
// every list endpoint uses. Exactly one level, and only when `data` is
// the sole key, so a resource that legitimately has a `data` field is
// never mistaken for an envelope.
func unwrapEnvelope(body []byte) []byte {
	trimmed := strings.TrimSpace(string(body))
	if !strings.HasPrefix(trimmed, "{") {
		return body
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return body
	}
	if len(probe) != 1 {
		return body
	}
	if data, ok := probe["data"]; ok {
		return data
	}
	return body
}

func indentInto(sb *strings.Builder, payload []byte) error {
	var out []byte
	buf := newIndentBuffer()
	if err := json.Indent(buf, payload, "", "  "); err != nil {
		return err
	}
	out = buf.Bytes()
	sb.Write(out)
	return nil
}

// NoColor reports whether colour should be suppressed, honouring the
// informal cross-tool conventions users already rely on.
func NoColor(explicitNoColor bool, isTTY bool) bool {
	if explicitNoColor {
		return true
	}
	if os.Getenv("NO_COLOR") != "" {
		return true
	}
	switch strings.ToLower(os.Getenv("XCLOUD_COLOR")) {
	case "never":
		return true
	case "always":
		return false
	}
	if os.Getenv("CI") != "" {
		return true
	}
	if os.Getenv("TERM") == "dumb" {
		return true
	}
	return !isTTY
}

// IsTTY reports whether f is an interactive terminal. Uses the file mode
// rather than a dependency: a character device is the definition every
// isatty implementation ultimately reduces to.
func IsTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
