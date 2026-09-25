// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package paramap

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func doc(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestApply(t *testing.T) {
	d := doc(t, `{"repo":{"name":"etl","odd key":"x"},"commits":[{"id":"a1"},{"id":"b2"}],"n":3,"ok":true}`)
	got, err := Apply(map[string]any{
		"repo":   "$.repo.name",
		"first":  "$.commits[0].id",
		"last":   "$.commits[-1].id",
		"odd":    "$.repo['odd key']",
		"n":      "$.n",
		"label":  "build {{ $.repo.name }}#{{$.n}} ok={{ $.ok }}",
		"static": "plain",
		"num":    float64(7),
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"repo": "etl", "first": "a1", "last": "b2", "odd": "x", "n": float64(3),
		"label": "build etl#3 ok=true", "static": "plain", "num": float64(7)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestApplyMissingIsAnError(t *testing.T) {
	_, err := Apply(map[string]any{"a": "$.nope", "b": "x {{ $.also.nope }}"}, doc(t, `{}`))
	var me *MissingError
	if !errors.As(err, &me) || len(me.Paths) != 2 {
		t.Fatalf("err = %v", err)
	}
}

func TestValidate(t *testing.T) {
	for _, bad := range []map[string]any{
		{"a": "$.x[*]"}, {"a": "$..x"}, {"a": "$.x[?(@.y)]"}, {"a": "{{ nope }}"}, {"": "x"}, {"a": "$.x["},
	} {
		if Validate(bad) == nil {
			t.Errorf("Validate(%v) accepted", bad)
		}
	}
	if err := Validate(map[string]any{"a": "$.x[0].y", "b": "{{ $.k }}-z", "c": 1}); err != nil {
		t.Fatal(err)
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"", "anything/at/all", true},
		{"*.parquet", "a.parquet", true},
		{"*.parquet", "d/a.parquet", false},
		{"**/*.parquet", "a.parquet", true},
		{"**/*.parquet", "d/e/a.parquet", true},
		{"day=??/*.csv", "day=01/x.csv", true},
		{"day=??/*.csv", "day=1/x.csv", false},
		{"a.b", "axb", false},
	}
	for _, c := range cases {
		g, err := CompileGlob(c.pat)
		if err != nil {
			t.Fatal(err)
		}
		if got := g.Match(c.name); got != c.want {
			t.Errorf("%q ~ %q = %v, want %v", c.pat, c.name, got, c.want)
		}
	}
}

func TestStringify(t *testing.T) {
	if Stringify(float64(12)) != "12" || Stringify(1.5) != "1.5" || Stringify(map[string]any{"a": 1.0}) != `{"a":1}` {
		t.Fatal("stringify")
	}
}
