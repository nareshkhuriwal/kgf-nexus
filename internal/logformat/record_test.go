// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package logformat

import (
	"strings"
	"testing"
)

func TestRecordRoundTripSurvivesHostileContent(t *testing.T) {
	in := []Record{
		{N: 1, TS: 10, S: StreamStdout, M: "plain"},
		{N: 2, TS: 11, S: StreamStderr, M: "quotes \" and \\ and \t tabs and \x00 nul"},
		{N: 3, TS: 12, S: StreamStdout, M: "bad utf8 \xff\xfe", P: true},
		{N: 4, TS: 13, S: StreamNexus, M: "[nexus] marker", K: KindTruncated},
	}
	var buf []byte
	for _, r := range in {
		buf = append(buf, r.Encode()...)
	}
	if strings.Count(string(buf), "\n") != len(in) {
		t.Fatal("each record must be exactly one line; an embedded newline would break range reads")
	}
	out, bad := DecodeRecords(buf)
	if bad != 0 || len(out) != len(in) {
		t.Fatalf("decoded %d records, %d bad", len(out), bad)
	}
	if out[2].M != "bad utf8 �" && !strings.HasPrefix(out[2].M, "bad utf8 ") {
		t.Fatalf("invalid UTF-8 not replaced: %q", out[2].M)
	}
	if out[3].K != KindTruncated || !out[2].P {
		t.Fatal("meta fields lost in the round trip")
	}
}

func TestDecodeSkipsDamagedLinesInsteadOfFailing(t *testing.T) {
	buf := append(Record{N: 1, M: "a"}.Encode(), []byte("{not json\n\n")...)
	buf = append(buf, Record{N: 2, M: "b"}.Encode()...)
	out, bad := DecodeRecords(buf)
	if len(out) != 2 || bad != 1 {
		t.Fatalf("got %d records, %d bad; one damaged line must not hide the rest", len(out), bad)
	}
}
