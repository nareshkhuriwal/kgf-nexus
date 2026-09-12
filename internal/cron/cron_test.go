package cron

import (
	"strings"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func mustParse(t *testing.T, expr string) *Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestNextBasicFields(t *testing.T) {
	cases := []struct {
		expr, from, want string
	}{
		{"* * * * *", "2026-03-01T10:00:30Z", "2026-03-01T10:01:00Z"},
		// Strictly after: a schedule must never re-fire the tick it just ran.
		{"0 2 * * *", "2026-03-01T02:00:00Z", "2026-03-02T02:00:00Z"},
		{"0 2 * * *", "2026-03-01T01:59:00Z", "2026-03-01T02:00:00Z"},
		{"*/15 * * * *", "2026-03-01T10:00:00Z", "2026-03-01T10:15:00Z"},
		{"*/15 * * * *", "2026-03-01T10:46:00Z", "2026-03-01T11:00:00Z"},
		{"30 3 1 * *", "2026-03-01T04:00:00Z", "2026-04-01T03:30:00Z"},
		{"0 0 * * 1", "2026-03-04T00:00:00Z", "2026-03-09T00:00:00Z"}, // next Monday
		{"0 0 1 1 *", "2026-06-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		// Named months and days must work; an operator porting a crontab uses them.
		{"0 0 1 jan *", "2026-06-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"0 0 * * sun", "2026-03-04T00:00:00Z", "2026-03-08T00:00:00Z"},
		// 7 as Sunday is a widespread convention.
		{"0 0 * * 7", "2026-03-04T00:00:00Z", "2026-03-08T00:00:00Z"},
		// Ranges and lists.
		{"0 9-17 * * *", "2026-03-01T08:30:00Z", "2026-03-01T09:00:00Z"},
		{"0 9-17 * * *", "2026-03-01T17:30:00Z", "2026-03-02T09:00:00Z"},
		{"0 0,12 * * *", "2026-03-01T01:00:00Z", "2026-03-01T12:00:00Z"},
		// A bare value with a step runs from that value to the field maximum.
		{"5/15 * * * *", "2026-03-01T10:00:00Z", "2026-03-01T10:05:00Z"},
		{"5/15 * * * *", "2026-03-01T10:06:00Z", "2026-03-01T10:20:00Z"},
	}
	for _, c := range cases {
		got, err := mustParse(t, c.expr).Next(at(c.from))
		if err != nil {
			t.Errorf("%q from %s: %v", c.expr, c.from, err)
			continue
		}
		if want := at(c.want); !got.Equal(want) {
			t.Errorf("%q from %s = %s, want %s", c.expr, c.from, got.Format(time.RFC3339), c.want)
		}
	}
}

func TestDescriptors(t *testing.T) {
	cases := map[string]string{
		"@daily":  "2026-03-02T00:00:00Z",
		"@hourly": "2026-03-01T11:00:00Z",
		"@weekly": "2026-03-08T00:00:00Z", // next Sunday
		"@yearly": "2027-01-01T00:00:00Z",
	}
	from := at("2026-03-01T10:30:00Z")
	for expr, want := range cases {
		s := mustParse(t, expr)
		// The expression round-trips as what the author wrote, not as its expansion:
		// an API that silently rewrote @daily to "0 0 * * *" would be editing input.
		if s.Expr() != expr {
			t.Errorf("Expr() = %q, want the shorthand %q the author wrote", s.Expr(), expr)
		}
		got, err := s.Next(from)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if !got.Equal(at(want)) {
			t.Errorf("%s = %s, want %s", expr, got.Format(time.RFC3339), want)
		}
	}
}

// TestDomAndDowAreUnionedWhenBothRestricted pins cron's one genuine oddity.
//
// When both day fields are restricted a day matches if EITHER matches, so
// "0 0 1 * 1" means the 1st of the month AND every Monday -- not "the 1st when it
// is a Monday". Every crontab does this, so matching it matters more than being
// logical: an operator porting a schedule must get the same days.
func TestDomAndDowAreUnionedWhenBothRestricted(t *testing.T) {
	s := mustParse(t, "0 0 1 * 1")
	// From 2026-03-01 (a Sunday), the next matches are Mon 2026-03-02 (a Monday)
	// then Mon 03-09, and also 2026-04-01 by day-of-month.
	got, err := s.NextN(at("2026-03-01T00:00:00Z"), 3)
	if err != nil {
		t.Fatalf("NextN: %v", err)
	}
	want := []string{"2026-03-02T00:00:00Z", "2026-03-09T00:00:00Z", "2026-03-16T00:00:00Z"}
	for i, w := range want {
		if !got[i].Equal(at(w)) {
			t.Errorf("fire %d = %s, want %s", i, got[i].Format(time.RFC3339), w)
		}
	}

	// With only day-of-month restricted, day-of-week must not narrow it.
	if fire, err := mustParse(t, "0 0 1 * *").Next(at("2026-03-02T00:00:00Z")); err != nil {
		t.Fatal(err)
	} else if !fire.Equal(at("2026-04-01T00:00:00Z")) {
		t.Errorf("dom-only = %s, want 2026-04-01T00:00:00Z", fire.Format(time.RFC3339))
	}
}

// TestLeapDayTerminates is why Next has a bounded lookahead. "29 February" is
// reachable only every four years, and "30 February" is never reachable at all --
// an unbounded search would spin forever inside a timer handler.
func TestLeapDayTerminates(t *testing.T) {
	// 2028 is the next leap year after 2026.
	got, err := mustParse(t, "0 0 29 2 *").Next(at("2026-03-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("leap day: %v", err)
	}
	if !got.Equal(at("2028-02-29T00:00:00Z")) {
		t.Errorf("leap day = %s, want 2028-02-29T00:00:00Z", got.Format(time.RFC3339))
	}

	// A date that never occurs must ERROR rather than hang.
	done := make(chan error, 1)
	go func() {
		_, err := mustParse(t, "0 0 30 2 *").Next(at("2026-03-01T00:00:00Z"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("30 February returned a fire time")
		} else if !strings.Contains(err.Error(), "never occurs") {
			t.Errorf("error should explain the cause, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Next did not terminate on an impossible date; the lookahead bound is not working")
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	bad := []string{
		"",
		"* * * *",     // four fields
		"* * * * * *", // six
		"60 * * * *",  // minute out of range
		"* 24 * * *",  // hour out of range
		"* * 0 * *",   // day-of-month is 1-based
		"* * 32 * *",
		"* * * 13 *",  // month out of range
		"* * * * 8",   // day-of-week out of range
		"*/0 * * * *", // zero step
		"*/-1 * * * *",
		"5-1 * * * *", // inverted range
		"nope * * * *",
		"* * * nope *",
		"@reboot", // no computable fire time
		"@nonsense",
		"1,, * * * *", // empty list element
	}
	for _, expr := range bad {
		if s, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) succeeded (expr %q); a bad expression must be rejected at "+
				"submit time, not silently never fire", expr, s.Expr())
		}
	}
}

// TestErrorsNameTheField matters because a rejected schedule is something a person
// has to fix, and "invalid cron expression" tells them nothing.
func TestErrorsNameTheField(t *testing.T) {
	cases := map[string]string{
		"60 * * * *": "minute",
		"* 24 * * *": "hour",
		"* * 32 * *": "day-of-month",
		"* * * 13 *": "month",
		"* * * * 8":  "day-of-week",
	}
	for expr, field := range cases {
		_, err := Parse(expr)
		if err == nil {
			t.Errorf("Parse(%q) succeeded", expr)
			continue
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("Parse(%q) error %q does not name the %q field", expr, err, field)
		}
	}
}

// TestNextIsStable guards against a schedule drifting: computing the next fire from
// a previous fire must land exactly on the grid, forever.
func TestNextIsStable(t *testing.T) {
	s := mustParse(t, "*/10 * * * *")
	t0 := at("2026-03-01T00:00:00Z")
	cur := t0
	for i := 0; i < 200; i++ {
		next, err := s.Next(cur)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if next.Sub(cur) != 10*time.Minute {
			t.Fatalf("iteration %d: gap %s, want 10m (drift means a schedule slowly leaves its grid)", i, next.Sub(cur))
		}
		if next.Second() != 0 || next.Nanosecond() != 0 {
			t.Fatalf("iteration %d: fire time %s is not minute-aligned", i, next)
		}
		cur = next
	}
}

// TestSubMinutePrecisionDoesNotDoubleFire covers a real hazard: a handler that
// passes time.Now() (which has sub-minute precision) must not get back the same
// minute it is already firing.
func TestSubMinutePrecisionDoesNotDoubleFire(t *testing.T) {
	s := mustParse(t, "* * * * *")
	got, err := s.Next(at("2026-03-01T10:05:00Z").Add(1 * time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(at("2026-03-01T10:06:00Z")) {
		t.Errorf("Next just after 10:05:00 = %s, want 10:06:00", got.Format(time.RFC3339))
	}
}

func TestNextNStopsAtTheError(t *testing.T) {
	// A partial preview is still worth showing, so NextN returns what it computed.
	got, err := mustParse(t, "0 0 30 2 *").NextN(at("2026-03-01T00:00:00Z"), 3)
	if err == nil {
		t.Error("NextN on an impossible date returned no error")
	}
	if len(got) != 0 {
		t.Errorf("NextN returned %d times for an impossible date, want 0", len(got))
	}
}

// TestNextIsUTCRegardlessOfInput pins the documented limitation: the timezone
// column is stored and not consulted, so a caller passing a zoned time still gets
// UTC grid alignment rather than local.
func TestNextIsUTCRegardlessOfInput(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	got, err := mustParse(t, "0 2 * * *").Next(time.Date(2026, 3, 1, 10, 0, 0, 0, tokyo))
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-01 10:00 Tokyo is 01:00 UTC, so the next 02:00 UTC is the same day.
	if !got.Equal(at("2026-03-01T02:00:00Z")) {
		t.Errorf("Next = %s, want 2026-03-01T02:00:00Z (UTC grid)", got.Format(time.RFC3339))
	}
	if got.Location() != time.UTC {
		t.Errorf("fire time location = %s, want UTC", got.Location())
	}
}
