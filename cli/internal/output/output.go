// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package output renders results as a table, JSON, or YAML.
//
// Table is the default because the usual reader is a human at a terminal
// diagnosing a run. JSON exists because the second-most-common reader is a shell
// pipeline, and a CLI whose output can only be scraped with awk is a CLI that
// gets wrapped in a fragile script.
//
// Table output is deliberately NOT machine-parseable-by-accident: columns are
// space-padded and can be truncated. Anything scripting against nexusctl should
// use --output json, and the help text says so.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Format is a rendering mode.
type Format string

// The format constants are prefixed because `Table` is also the name of the
// tabular data type below, and one identifier cannot be both.
const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
)

// ParseFormat validates a --output value.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "table", "text", "wide":
		return FormatTable, nil
	case "json":
		return FormatJSON, nil
	case "yaml", "yml":
		return FormatYAML, nil
	default:
		return "", fmt.Errorf("unknown output format %q: expected table, json, or yaml", s)
	}
}

// Printer writes rendered output to a stream.
type Printer struct {
	Format Format
	Out    io.Writer
}

// New builds a Printer.
func New(format Format, out io.Writer) *Printer { return &Printer{Format: format, Out: out} }

// Table is a set of rows with named columns.
type Table struct {
	Columns []string
	Rows    [][]string
	// Empty is shown instead of a bare header when there are no rows. "No runs
	// found." answers the user's question; an empty table leaves them wondering
	// whether the command worked.
	Empty string
}

// Print renders a value in the printer's format.
//
// For JSON and YAML the ORIGINAL object is emitted, never the table's stringified
// cells: a script consuming --output json needs the real types and the fields the
// table left out.
func (p *Printer) Print(raw any, table *Table) error {
	switch p.Format {
	case FormatJSON:
		enc := json.NewEncoder(p.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(raw)
	case FormatYAML:
		enc := yaml.NewEncoder(p.Out)
		enc.SetIndent(2)
		if err := enc.Encode(raw); err != nil {
			return err
		}
		return enc.Close()
	default:
		if table == nil {
			return fmt.Errorf("no table rendering for this result; try --output json")
		}
		return p.printTable(table)
	}
}

// PrintRaw writes text through untouched, for log and event streaming where any
// re-encoding would corrupt what the task actually printed.
func (p *Printer) PrintRaw(s string) error {
	_, err := io.WriteString(p.Out, s)
	return err
}

func (p *Printer) printTable(t *Table) error {
	if len(t.Rows) == 0 {
		msg := t.Empty
		if msg == "" {
			msg = "No results."
		}
		_, err := fmt.Fprintln(p.Out, msg)
		return err
	}

	widths := make([]int, len(t.Columns))
	for i, c := range t.Columns {
		widths[i] = len(c)
	}
	for _, row := range t.Rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	var b strings.Builder
	for i, c := range t.Columns {
		b.WriteString(pad(strings.ToUpper(c), widths[i], i == len(t.Columns)-1))
	}
	b.WriteString("\n")
	for _, row := range t.Rows {
		for i := range t.Columns {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			b.WriteString(pad(cell, widths[i], i == len(t.Columns)-1))
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(p.Out, b.String())
	return err
}

func pad(s string, width int, last bool) string {
	if last {
		// No trailing whitespace on the final column: it is invisible, it breaks
		// `diff`, and it makes copy-pasted output messy.
		return s
	}
	if len(s) >= width {
		return s + "   "
	}
	return s + strings.Repeat(" ", width-len(s)) + "   "
}

// ── cell helpers ─────────────────────────────────────────────────────────────

// Str extracts a string field, tolerating absence.
//
// Every accessor here is forgiving because the CLI must keep working against a
// control plane that is one version ahead or behind; a renamed field should
// degrade one column to "-", not crash the command.
func Str(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return "-"
	}
	switch t := v.(type) {
	case string:
		if t == "" {
			return "-"
		}
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// Int extracts an integer field; JSON numbers arrive as float64.
func Int(m map[string]any, key string) int64 {
	switch t := m[key].(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case string:
		var n int64
		_, _ = fmt.Sscanf(t, "%d", &n)
		return n
	default:
		return 0
	}
}

// Age renders an RFC3339 timestamp as a compact relative duration ("4m", "2d").
//
// Relative beats absolute in a fleet listing: "3h" answers "is this stuck?" at a
// glance, where a timestamp makes the reader do arithmetic.
func Age(m map[string]any, key string) string {
	s := Str(m, key)
	if s == "-" {
		return "-"
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return Duration(time.Since(ts))
}

// Duration renders a duration at one significant unit.
func Duration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
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

// Millis renders a millisecond count the way durations are written in specs.
func Millis(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return Duration(time.Duration(ms) * time.Millisecond)
}

// Bytes renders a byte count with a binary suffix, matching spec authoring units.
func Bytes(n int64) string {
	if n == 0 {
		return "-"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	val := float64(n) / float64(div)
	suffix := []string{"Ki", "Mi", "Gi", "Ti", "Pi"}[exp]
	if val == float64(int64(val)) {
		return fmt.Sprintf("%d%s", int64(val), suffix)
	}
	return fmt.Sprintf("%.1f%s", val, suffix)
}

// CPU renders millicores the way they are authored ("2000m" -> "2").
func CPU(millis int64) string {
	if millis == 0 {
		return "-"
	}
	if millis%1000 == 0 {
		return fmt.Sprintf("%d", millis/1000)
	}
	return fmt.Sprintf("%dm", millis)
}

// Items pulls a list out of a response, tolerating several plausible envelope
// shapes so the CLI does not break on a server-side rename.
func Items(raw any, keys ...string) []map[string]any {
	switch t := raw.(type) {
	case []any:
		return toMaps(t)
	case map[string]any:
		for _, k := range append(keys, "items", "data", "results") {
			if v, ok := t[k]; ok {
				if list, ok := v.([]any); ok {
					return toMaps(list)
				}
			}
		}
	}
	return nil
}

func toMaps(list []any) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, v := range list {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// SortedKeys returns map keys in a stable order, so repeated output is diffable.
func SortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
