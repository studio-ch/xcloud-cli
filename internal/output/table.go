package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
)

// newIndentBuffer exists so output.go can indent without importing
// bytes directly at every call site.
func newIndentBuffer() *bytes.Buffer { return &bytes.Buffer{} }

// renderTable decodes the payload into generic maps and renders the
// requested columns.
//
// Decoding generically rather than into typed structs is deliberate: the
// generated client models every response as an anonymous inline struct
// (the API's OpenAPI document declares almost no named components), so
// there is no nameable Go type to hand a renderer. Going through
// map[string]any keeps rendering decoupled from codegen churn — a new
// field in the API cannot break the table, it just is not shown.
func (w *Writer) renderTable(body []byte, columns []Column) error {
	rows, err := decodeRows(body)
	if err != nil {
		return err
	}

	// Quiet mode is the scripting idiom: one identifying value per line,
	// so `for id in $(xcloud instance list -q)` works.
	if w.Quiet {
		for _, r := range rows {
			fmt.Fprintln(w.Out, identifier(r))
		}
		return nil
	}

	visible := make([]Column, 0, len(columns))
	for _, c := range columns {
		if c.Wide && w.Format != FormatWide {
			continue
		}
		visible = append(visible, c)
	}
	if len(visible) == 0 {
		return w.renderJSON(body)
	}

	if len(rows) == 0 {
		fmt.Fprintln(w.Err, "No resources found.")
		return nil
	}

	tw := tabwriter.NewWriter(w.Out, 0, 4, 3, ' ', 0)
	if !w.NoHeaders {
		headers := make([]string, len(visible))
		for i, c := range visible {
			headers[i] = strings.ToUpper(c.Header)
		}
		fmt.Fprintln(tw, strings.Join(headers, "\t"))
	}
	for _, r := range rows {
		cells := make([]string, len(visible))
		for i, c := range visible {
			v := strings.TrimSpace(c.Value(r))
			if v == "" {
				v = "-"
			}
			cells[i] = v
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// decodeRows normalises both shapes an endpoint can return — a
// {"data":[…]} list or a single object — into a slice of rows.
func decodeRows(body []byte) ([]map[string]any, error) {
	payload := unwrapEnvelope(body)
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, nil
	}

	var list []map[string]any
	if err := json.Unmarshal(payload, &list); err == nil {
		return list, nil
	}
	var single map[string]any
	if err := json.Unmarshal(payload, &single); err == nil {
		return []map[string]any{single}, nil
	}
	return nil, fmt.Errorf("cannot render this response as a table; try --output json")
}

// identifier picks the value -q prints for a row, preferring the field a
// user would pass back to another command.
func identifier(row map[string]any) string {
	for _, key := range []string{"id", "name", "slug", "key"} {
		if v, ok := row[key]; ok {
			if s := Str(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// Str renders a decoded JSON value as a table cell. json.Unmarshal turns
// every number into a float64, so integers must be printed without a
// spurious ".0" — hence the integral check rather than a plain %v.
func Str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "yes"
		}
		return "no"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// Field reads a possibly-absent key.
func Field(row map[string]any, key string) string {
	v, ok := row[key]
	if !ok {
		return ""
	}
	return Str(v)
}

// Age renders an RFC 3339 timestamp as a compact relative duration, the
// way kubectl does. `now` is a parameter rather than a call to
// time.Now() so golden tests can freeze it.
func Age(value string, now time.Time) string {
	if value == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	d := now.Sub(t)
	if d < 0 {
		return "0s"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
