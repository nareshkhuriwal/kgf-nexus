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

// ── timezones and DST ───────────────────────────────────────────────────────

func london(t *testing.T) *time.Location {
	t.Helper()
	loc, err := LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return loc
}

func mustParseIn(t *testing.T, expr string, loc *time.Location) *Schedule {
	t.Helper()
	s, err := ParseIn(expr, loc)
	if err != nil {
		t.Fatalf("ParseIn(%q): %v", expr, err)
	}
	return s
}

// TestLocalTimeStaysOnTheWallClock is the reason timezone support exists. A nightly
// job at 02:00 London must remain 02:00 London across a DST transition, not drift to
// 03:00 for half the year -- which is exactly what evaluating in UTC does.
func TestLocalTimeStaysOnTheWallClock(t *testing.T) {
	loc := london(t)
	s := mustParseIn(t, "0 2 * * *", loc)

	// Before the spring transition (GMT) and after it (BST).
	for _, from := range []string{"2026-03-25T12:00:00Z", "2026-04-05T12:00:00Z"} {
		got, err := s.Next(at(from))
		if err != nil {
			t.Fatalf("from %s: %v", from, err)
		}
		inLoc := got.In(loc)
		if inLoc.Hour() != 2 || inLoc.Minute() != 0 {
			t.Errorf("from %s: fired at %s local, want 02:00 local", from, inLoc.Format("15:04 -0700"))
		}
	}

	// The same schedule in UTC drifts relative to local time, which is the behaviour
	// being replaced. Asserted so the difference is documented rather than implied.
	utc := mustParseIn(t, "0 2 * * *", time.UTC)
	after, err := utc.Next(at("2026-04-05T12:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if h := after.In(loc).Hour(); h != 3 {
		t.Errorf("a UTC schedule in April reads %02d:00 London, want 03:00 (the drift local scheduling fixes)", h)
	}
}

// TestSpringGapFiresExactlyOnce covers the hour that does not exist.
//
// On 2026-03-29 London, 01:00-02:00 is skipped. time.Date normalizes a nonexistent
// wall time FORWARD, so local 01:30 and local 02:30 both resolve to 02:30 -- and a
// naive walk fires that one instant twice. Measured against Go's tzdata before this
// was written.
func TestSpringGapFiresExactlyOnce(t *testing.T) {
	loc := london(t)

	// A schedule at 01:30, which does not exist that day. It must still fire, once,
	// at the first instant that does exist: not running a nightly backup because the
	// clocks changed is worse than running it an hour late.
	s := mustParseIn(t, "30 1 * * *", loc)
	got, err := s.Next(at("2026-03-28T12:00:00Z"))
	if err != nil {
		t.Fatalf("next across the gap: %v", err)
	}
	if got.In(loc).Day() != 29 {
		t.Fatalf("fired on day %d, want 29 (the transition day must not be skipped)", got.In(loc).Day())
	}
	// 01:30 does not exist, so the fire lands at 02:30 BST = 01:30 UTC.
	if want := at("2026-03-29T01:30:00Z"); !got.Equal(want) {
		t.Errorf("gap fire = %s (%s local), want %s", got.Format(time.RFC3339),
			got.In(loc).Format("15:04 -0700"), want.Format(time.RFC3339))
	}

	// The collision case: an HOURLY schedule crosses the gap. Every instant it fires
	// must be distinct -- local 01:00 and local 02:00 both normalize to 02:00 BST, and
	// firing both would run the same instant twice.
	hourly := mustParseIn(t, "0 * * * *", loc)
	cur := at("2026-03-28T22:00:00Z")
	seen := map[int64]int{}
	for i := 0; i < 10; i++ {
		n, err := hourly.Next(cur)
		if err != nil {
			t.Fatalf("hourly walk: %v", err)
		}
		seen[n.Unix()]++
		if seen[n.Unix()] > 1 {
			t.Fatalf("instant %s fired twice while crossing the spring gap",
				n.In(loc).Format(time.RFC3339))
		}
		if !n.After(cur) {
			t.Fatalf("Next did not advance: %s -> %s", cur.Format(time.RFC3339), n.Format(time.RFC3339))
		}
		cur = n
	}
}

// TestAutumnFoldFiresExactlyOnce covers the hour that happens twice.
//
// On 2026-10-25 London, 01:00-02:00 occurs at both BST and GMT offsets. time.Date
// returns the SECOND occurrence, so a naive walk never produces the first and silently
// skips a tick. Firing twice would be worse still: for anything that writes, that is a
// duplicate the platform promises not to manufacture.
func TestAutumnFoldFiresExactlyOnce(t *testing.T) {
	loc := london(t)
	s := mustParseIn(t, "30 1 * * *", loc)

	got, err := s.Next(at("2026-10-24T12:00:00Z"))
	if err != nil {
		t.Fatalf("next across the fold: %v", err)
	}
	// The FIRST 01:30 is 00:30 UTC (BST); the second is 01:30 UTC (GMT). Firing on the
	// first means the job runs when the author's wall clock first reads 01:30, rather
	// than an hour later for no reason.
	if want := at("2026-10-25T00:30:00Z"); !got.Equal(want) {
		t.Errorf("fold fire = %s (%s local), want the FIRST occurrence %s",
			got.Format(time.RFC3339), got.In(loc).Format("15:04 -0700"), want.Format(time.RFC3339))
	}

	// And it must not fire again for that wall time: the next fire is the following
	// day, not the second occurrence of the same 01:30.
	next, err := s.Next(got)
	if err != nil {
		t.Fatalf("next after the fold: %v", err)
	}
	if next.In(loc).Day() == 25 {
		t.Errorf("fired twice for one wall-clock time: %s then %s",
			got.In(loc).Format(time.RFC3339), next.In(loc).Format(time.RFC3339))
	}

	// An hourly schedule across the fold must produce distinct instants and must not
	// skip the folded hour entirely.
	hourly := mustParseIn(t, "0 * * * *", loc)
	cur := at("2026-10-24T22:00:00Z")
	seen := map[int64]int{}
	for i := 0; i < 10; i++ {
		n, err := hourly.Next(cur)
		if err != nil {
			t.Fatalf("hourly walk: %v", err)
		}
		if seen[n.Unix()] > 0 {
			t.Fatalf("instant %s fired twice while crossing the autumn fold",
				n.In(loc).Format(time.RFC3339))
		}
		seen[n.Unix()]++
		if !n.After(cur) {
			t.Fatalf("Next did not advance: %s -> %s", cur.Format(time.RFC3339), n.Format(time.RFC3339))
		}
		cur = n
	}
}

// TestNextAlwaysAdvancesAcrossBothTransitions is the property that keeps the fire loop
// from spinning. The loop computes the next tick from the last one, so a Next that ever
// returns a time not strictly after its argument is an infinite loop in production.
func TestNextAlwaysAdvancesAcrossBothTransitions(t *testing.T) {
	loc := london(t)
	for _, expr := range []string{"* * * * *", "*/15 * * * *", "0 * * * *", "30 1 * * *", "0 2 * * *"} {
		s := mustParseIn(t, expr, loc)
		for _, window := range []string{"2026-03-28T20:00:00Z", "2026-10-24T20:00:00Z"} {
			cur := at(window)
			for i := 0; i < 200; i++ {
				n, err := s.Next(cur)
				if err != nil {
					t.Fatalf("%q from %s iteration %d: %v", expr, window, i, err)
				}
				if !n.After(cur) {
					t.Fatalf("%q from %s iteration %d: Next returned %s which is not after %s",
						expr, window, i, n.Format(time.RFC3339), cur.Format(time.RFC3339))
				}
				cur = n
			}
		}
	}
}

// TestSouthernHemisphereTransitionsWork guards against having special-cased the
// northern calendar: Sydney's transitions run the other way round in the year.
func TestSouthernHemisphereTransitionsWork(t *testing.T) {
	loc, err := LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	s := mustParseIn(t, "0 * * * *", loc)
	// April is the fold there, October the gap -- inverted relative to London.
	for _, window := range []string{"2026-04-04T12:00:00Z", "2026-10-03T12:00:00Z"} {
		cur := at(window)
		seen := map[int64]bool{}
		for i := 0; i < 60; i++ {
			n, err := s.Next(cur)
			if err != nil {
				t.Fatalf("from %s: %v", window, err)
			}
			if seen[n.Unix()] {
				t.Fatalf("instant %s fired twice in Sydney", n.In(loc).Format(time.RFC3339))
			}
			seen[n.Unix()] = true
			if !n.After(cur) {
				t.Fatalf("Next did not advance in Sydney: %s -> %s", cur, n)
			}
			cur = n
		}
	}
}

// TestZoneWithoutDSTIsUnaffected: a fixed-offset zone must behave exactly like UTC
// with a shift, so the DST machinery cannot cost correctness where there is no DST.
func TestZoneWithoutDSTIsUnaffected(t *testing.T) {
	loc, err := LoadLocation("Asia/Kolkata") // UTC+05:30 year round
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	s := mustParseIn(t, "0 2 * * *", loc)
	got, err := s.Next(at("2026-03-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	// 02:00 IST is 20:30 UTC the previous day.
	if want := at("2026-03-01T20:30:00Z"); !got.Equal(want) {
		t.Errorf("Kolkata 02:00 = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestLoadLocationRejectsUnknownZones(t *testing.T) {
	if _, err := LoadLocation("Mars/Olympus_Mons"); err == nil {
		t.Error("an unknown zone was accepted; a schedule would then evaluate in the wrong time")
	}
	// Empty and UTC both mean UTC, so a zero-value config is safe.
	for _, name := range []string{"", "UTC"} {
		loc, err := LoadLocation(name)
		if err != nil || loc != time.UTC {
			t.Errorf("LoadLocation(%q) = (%v, %v), want (UTC, nil)", name, loc, err)
		}
	}
}

// TestParseDefaultsToUTC keeps the existing contract: Parse without a location is UTC,
// which has neither a gap nor a fold, so an unmigrated caller cannot regress.
func TestParseDefaultsToUTC(t *testing.T) {
	s := mustParse(t, "0 2 * * *")
	if s.Location() != time.UTC {
		t.Errorf("Parse location = %v, want UTC", s.Location())
	}
	if s2, err := ParseIn("0 2 * * *", nil); err != nil || s2.Location() != time.UTC {
		t.Errorf("ParseIn with a nil location = (%v, %v), want UTC", s2, err)
	}
}

// TestGapCollisionIsSuppressedNotJustFilteredByOrdering exercises the suppression
// directly, because the chained walk in TestSpringGapFiresExactlyOnce does not.
//
// On 2026-03-29 London an hourly schedule has TWO matching wall times that resolve to
// the same instant: 01:00 (which does not exist, normalizing forward to 02:00 BST) and
// 02:00 (which is that instant). A chained Next() hides the problem, because the
// `After(after)` guard discards the second one for free -- so removing the suppression
// still passes that test. It does NOT hide it here: asking from a point BEFORE both,
// the schedule must return the instant once and then move on to 03:00, never returning
// 02:00 BST a second time.
//
// Written after a mutation test showed the collision suppression could be deleted with
// every other test still green. A guard nothing exercises is a guard that will be
// removed by someone tidying up.
func TestGapCollisionIsSuppressedNotJustFilteredByOrdering(t *testing.T) {
	loc := london(t)
	s := mustParseIn(t, "0 * * * *", loc)

	// 00:00 GMT on the transition day, before both colliding wall times.
	first, err := s.Next(at("2026-03-29T00:00:00Z"))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// The only instant available for both 01:00 and 02:00 wall time is 01:00 UTC.
	if want := at("2026-03-29T01:00:00Z"); !first.Equal(want) {
		t.Fatalf("first fire = %s, want %s", first.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	second, err := s.Next(first)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Equal(first) {
		t.Fatal("the same instant fired twice across the gap")
	}
	// 03:00 BST = 02:00 UTC. If the collision were not suppressed, the walk would have
	// a second matching wall time (02:00) resolving to the instant just returned, and
	// the only reason it would not fire is the ordering guard -- which is precisely the
	// accident this test refuses to rely on.
	if want := at("2026-03-29T02:00:00Z"); !second.Equal(want) {
		t.Errorf("second fire = %s (%s local), want %s",
			second.Format(time.RFC3339), second.In(loc).Format("15:04 -0700"), want.Format(time.RFC3339))
	}

	// The direct form: resolve() must refuse the gap wall time whose normalized instant
	// belongs to a later matching tick.
	if _, ok := s.resolve(2026, time.March, 29, 1, 0); ok {
		t.Error("resolve accepted wall 01:00 on the gap day; its instant belongs to the 02:00 tick, " +
			"so accepting both is how one instant fires twice")
	}
	// And it must ACCEPT a gap wall time that collides with nothing, or a nightly job
	// would silently not run on the transition day.
	half := mustParseIn(t, "30 1 * * *", loc)
	if _, ok := half.resolve(2026, time.March, 29, 1, 30); !ok {
		t.Error("resolve refused wall 01:30, which collides with no other tick; the schedule " +
			"would skip the transition day entirely")
	}
}
