// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseFormat(t *testing.T) {
	cases := map[string]Format{
		"":      FormatTable,
		"table": FormatTable,
		"TABLE": FormatTable,
		"text":  FormatTable,
		"json":  FormatJSON,
		" json": FormatJSON,
		"yaml":  FormatYAML,
		"yml":   FormatYAML,
	}
	for in, want := range cases {
		got, err := ParseFormat(in)
		if err != nil {
			t.Errorf("ParseFormat(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseFormat(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := ParseFormat("xml"); err == nil {
		t.Error("expected an error for an unknown format")
	} else if !strings.Contains(err.Error(), "table, json, or yaml") {
		t.Errorf("error should list the valid formats, got: %v", err)
	}
}

func TestTableAlignsColumnsAndUppercasesHeaders(t *testing.T) {
	var buf bytes.Buffer
	err := New(FormatTable, &buf).Print(nil, &Table{
		Columns: []string{"id", "state"},
		Rows:    [][]string{{"short", "RUNNING"}, {"a-much-longer-id", "FAILED"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got %d lines: %q", len(lines), buf.String())
	}
	if !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[0], "STATE") {
		t.Errorf("header = %q", lines[0])
	}
	// The state column must start at the same offset on both rows, or the table is
	// unreadable at a glance -- which is the only reason table mode exists.
	if strings.Index(lines[1], "RUNNING") != strings.Index(lines[2], "FAILED") {
		t.Errorf("columns not aligned:\n%q\n%q", lines[1], lines[2])
	}
}

func TestTableHasNoTrailingWhitespace(t *testing.T) {
	// Trailing spaces are invisible, break `diff`, and make pasted output messy.
	var buf bytes.Buffer
	if err := New(FormatTable, &buf).Print(nil, &Table{
		Columns: []string{"a", "b"},
		Rows:    [][]string{{"1", "2"}, {"longer", "x"}},
	}); err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("line %d has trailing whitespace: %q", i, line)
		}
	}
}

func TestEmptyTablePrintsAnAnswerNotJustAHeader(t *testing.T) {
	// A bare header leaves the user wondering whether the command worked.
	var buf bytes.Buffer
	if err := New(FormatTable, &buf).Print(nil, &Table{
		Columns: []string{"id"},
		Empty:   "No runs found in namespace \"prod\".",
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No runs found") {
		t.Errorf("want the empty message, got %q", buf.String())
	}
	if strings.Contains(buf.String(), "ID") {
		t.Errorf("a header with no rows is noise, got %q", buf.String())
	}
}

func TestEmptyTableHasADefaultMessage(t *testing.T) {
	var buf bytes.Buffer
	if err := New(FormatTable, &buf).Print(nil, &Table{Columns: []string{"id"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No results") {
		t.Errorf("got %q", buf.String())
	}
}

func TestJSONEmitsTheOriginalObjectNotTableCells(t *testing.T) {
	// A script consuming --output json needs real types and the fields the table
	// left out; re-emitting stringified cells would be a silent data loss.
	raw := map[string]any{"id": "run-1", "attempts": 3, "nested": map[string]any{"k": true}}
	var buf bytes.Buffer
	if err := New(FormatJSON, &buf).Print(raw, &Table{
		Columns: []string{"id"},
		Rows:    [][]string{{"run-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("output is not valid JSON: %v (%q)", err, buf.String())
	}
	if back["attempts"].(float64) != 3 {
		t.Errorf("numeric field lost its type: %#v", back["attempts"])
	}
	if _, ok := back["nested"]; !ok {
		t.Error("nested object dropped")
	}
}

func TestYAMLRendersTheOriginalObject(t *testing.T) {
	var buf bytes.Buffer
	if err := New(FormatYAML, &buf).Print(map[string]any{"id": "run-1", "state": "RUNNING"}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "id: run-1") {
		t.Errorf("got %q", buf.String())
	}
}

func TestTableModeWithNoTableSuggestsJSON(t *testing.T) {
	var buf bytes.Buffer
	err := New(FormatTable, &buf).Print(map[string]any{"x": 1}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--output json") {
		t.Errorf("error should suggest a way forward, got: %v", err)
	}
}

func TestStrToleratesMissingAndOddFields(t *testing.T) {
	// The CLI must survive a control plane one version ahead: a renamed field
	// should degrade one column to "-", not crash the command.
	m := map[string]any{
		"s": "value", "empty": "", "n": float64(42), "f": 1.5, "b": true, "nil": nil,
	}
	cases := map[string]string{
		"s": "value", "empty": "-", "n": "42", "f": "1.5", "b": "true",
		"nil": "-", "absent": "-",
	}
	for key, want := range cases {
		if got := Str(m, key); got != want {
			t.Errorf("Str(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestIntHandlesJSONNumbers(t *testing.T) {
	m := map[string]any{"f": float64(1024), "i": 7, "s": "99", "bad": "nope"}
	for key, want := range map[string]int64{"f": 1024, "i": 7, "s": 99, "bad": 0, "absent": 0} {
		if got := Int(m, key); got != want {
			t.Errorf("Int(%q) = %d, want %d", key, got, want)
		}
	}
}

func TestBytesUsesBinarySuffixesMatchingSpecUnits(t *testing.T) {
	// The spec is authored in "8Gi"; rendering it as "8.6GB" would make the CLI
	// and the spec disagree about the same number.
	cases := map[int64]string{
		0:             "-",
		512:           "512B",
		1024:          "1Ki",
		536870912:     "512Mi",
		8589934592:    "8Gi",
		1099511627776: "1Ti",
		1610612736:    "1.5Gi",
	}
	for in, want := range cases {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestCPURendersMillicoresAsAuthored(t *testing.T) {
	for in, want := range map[int64]string{0: "-", 250: "250m", 1000: "1", 2000: "2", 1500: "1500m"} {
		if got := CPU(in); got != want {
			t.Errorf("CPU(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestDurationAndMillis(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "500ms"},
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
	}
	for _, tc := range cases {
		if got := Duration(tc.d); got != tc.want {
			t.Errorf("Duration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
	if got := Millis(0); got != "-" {
		t.Errorf("Millis(0) = %q, want %q", got, "-")
	}
	if got := Millis(1800000); got != "30m" {
		t.Errorf("Millis(1800000) = %q, want %q", got, "30m")
	}
}

func TestAgeRendersRelativeTime(t *testing.T) {
	m := map[string]any{
		"recent":  time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
		"garbage": "not-a-timestamp",
	}
	if got := Age(m, "recent"); got != "5m" && got != "4m" {
		t.Errorf("Age(recent) = %q, want about 5m", got)
	}
	// An unparseable value passes through rather than becoming "-": seeing the raw
	// value is what lets someone notice the server changed its format.
	if got := Age(m, "garbage"); got != "not-a-timestamp" {
		t.Errorf("Age(garbage) = %q", got)
	}
	if got := Age(m, "absent"); got != "-" {
		t.Errorf("Age(absent) = %q", got)
	}
}

func TestItemsToleratesSeveralEnvelopeShapes(t *testing.T) {
	// The CLI should not break because the server wraps a list under a different
	// key than expected.
	row := map[string]any{"id": "a"}
	for name, raw := range map[string]any{
		"bare list":   []any{row},
		"runs key":    map[string]any{"runs": []any{row}},
		"items key":   map[string]any{"items": []any{row}},
		"data key":    map[string]any{"data": []any{row}},
		"results key": map[string]any{"results": []any{row}},
	} {
		got := Items(raw, "runs")
		if len(got) != 1 || got[0]["id"] != "a" {
			t.Errorf("%s: Items = %#v", name, got)
		}
	}
	if got := Items(map[string]any{"unrelated": 1}, "runs"); got != nil {
		t.Errorf("unknown shape should give nil, got %#v", got)
	}
}

func TestSortedKeysIsStable(t *testing.T) {
	// Repeated output must be diffable, which means map iteration order can never
	// reach the terminal.
	m := map[string]any{"z": 1, "a": 2, "m": 3}
	for i := 0; i < 20; i++ {
		got := SortedKeys(m)
		if strings.Join(got, ",") != "a,m,z" {
			t.Fatalf("SortedKeys = %v", got)
		}
	}
}
