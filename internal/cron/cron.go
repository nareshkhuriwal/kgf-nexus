// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package cron parses five-field cron expressions and computes fire times.
//
// Written rather than imported because the platform has no cron dependency and
// deliberately keeps a small one: the Python SDK ships with zero runtime deps for
// the same reason. A cron field parser is a bounded, well-specified problem with a
// finite test surface, which is exactly the kind of thing worth owning rather than
// tracking CVEs for.
//
// TIMEZONES AND DST. A schedule is evaluated in its own IANA location, so
// "0 2 * * *" in Europe/London means 02:00 LOCAL every day -- which is what an
// author writing a nightly job means, and it stays 02:00 local across the March and
// October transitions rather than drifting an hour twice a year.
//
// That requires deciding two cases explicitly, because wall-clock time is neither
// total nor unique:
//
//   - THE SPRING GAP. Clocks jump forward, so an hour of local time never occurs.
//     On 2026-03-29 in London, 01:00-02:00 does not exist. A schedule for "0 1 * * *"
//     has no instant to fire at that day.
//   - THE AUTUMN FOLD. Clocks go back, so an hour occurs TWICE. On 2026-10-25 in
//     London, 01:30 happens at both 00:30 UTC (BST) and 01:30 UTC (GMT). A schedule
//     for "30 1 * * *" has two candidate instants.
//
// Go's time.Date does not solve this and silently does something surprising in both
// cases -- measured, not assumed:
//
//   - in the gap it NORMALIZES FORWARD, so Date(01:30) returns 02:30. Local 01:30 and
//     local 02:30 then map to the SAME instant, and a naive walk fires that instant
//     twice.
//   - in the fold it returns the SECOND occurrence, so the first 01:00 (BST) is never
//     produced and an hourly schedule silently skips a tick.
//
// Both were verified against Go's tzdata before this was written, and both are
// exactly the failures a scheduler must not have. So this package handles them
// explicitly:
//
//   - A GAP TICK FIRES ONCE, at the first instant that exists after the skipped wall
//     time. Skipping the day entirely is the other defensible choice and is worse for
//     the common case: a nightly backup scheduled at 01:30 should still run on the
//     day the clocks change, an hour late, rather than not run at all. Firing once is
//     the part that matters -- the normalization collision is suppressed so it cannot
//     double.
//   - A FOLD TICK FIRES ONCE, on the FIRST occurrence. Firing twice would run the job
//     twice for one logical time, which for anything that writes is a duplicate the
//     platform promises not to manufacture; firing on the second occurrence would
//     delay it an hour for no reason.
//
// Both rules are "exactly once per wall-clock time the author wrote", which is the
// only reading that keeps a schedule's logical time meaningful. UTC remains available
// and is the default: it has neither case by construction.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed expression. Each field is a set over its own domain, held
// as a bitmask because the domains are tiny and membership is the only question
// ever asked of them.
type Schedule struct {
	expr string
	// loc is the location the expression is evaluated in. Never nil: Parse defaults
	// it to UTC, so no call site has to nil-check a location.
	loc    *time.Location
	minute uint64 // 0-59
	hour   uint64 // 0-23
	dom    uint64 // 1-31
	month  uint64 // 1-12
	dow    uint64 // 0-6, Sunday = 0
	// domRestricted and dowRestricted record whether the author constrained the
	// field, because the two interact in a way no other pair does: see Next.
	domRestricted bool
	dowRestricted bool
}

// Expr returns the expression this schedule was parsed from.
func (s *Schedule) Expr() string { return s.expr }

type fieldSpec struct {
	name     string
	min, max int
	// names maps the accepted three-letter aliases to their numeric value.
	names map[string]int
}

var (
	minuteSpec = fieldSpec{name: "minute", min: 0, max: 59}
	hourSpec   = fieldSpec{name: "hour", min: 0, max: 23}
	domSpec    = fieldSpec{name: "day-of-month", min: 1, max: 31}
	monthSpec  = fieldSpec{name: "month", min: 1, max: 12, names: map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}}
	dowSpec = fieldSpec{name: "day-of-week", min: 0, max: 6, names: map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}}
)

// descriptors are the conventional shorthands. Accepted because an author who
// writes @daily should not be told the grammar does not include it, and because
// every other cron implementation takes them.
var descriptors = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// Parse compiles a five-field expression to be evaluated in UTC.
//
// Kept for callers that genuinely mean UTC, and because it is the safe default: UTC
// has neither a gap nor a fold, so a schedule parsed this way cannot hit either case.
func Parse(expr string) (*Schedule, error) {
	return ParseIn(expr, time.UTC)
}

// ParseIn compiles an expression to be evaluated in loc.
//
// A nil loc means UTC rather than being an error, so a zero-value config is safe.
//
// Errors name the field and the offending token, because a rejected schedule is
// something a person has to fix and "invalid cron expression" tells them nothing.
func ParseIn(expr string, loc *time.Location) (*Schedule, error) {
	if loc == nil {
		loc = time.UTC
	}
	s, err := parseFields(expr)
	if err != nil {
		return nil, err
	}
	s.loc = loc
	return s, nil
}

// LoadLocation resolves an IANA name, rejecting the empty string as UTC.
//
// Wrapped so every caller gets the same error shape, and so a missing tzdata is
// reported as what it is rather than as a bad schedule: a deployment without zoneinfo
// cannot evaluate a local schedule at all, and that is an operator problem, not an
// authoring one.
func LoadLocation(name string) (*time.Location, error) {
	if name == "" || name == "UTC" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q: %w (it must be an IANA name such as Europe/London or America/New_York, and the host must have zoneinfo installed)", name, err)
	}
	return loc, nil
}

// Location reports the location this schedule is evaluated in.
func (s *Schedule) Location() *time.Location { return s.loc }

func parseFields(expr string) (*Schedule, error) {
	raw := strings.TrimSpace(expr)
	if raw == "" {
		return nil, fmt.Errorf("cron expression is empty")
	}
	if expanded, ok := descriptors[strings.ToLower(raw)]; ok {
		s, err := parseFields(expanded)
		if err != nil {
			return nil, err
		}
		// Report the shorthand the author wrote, not what it expanded to, so a
		// round trip through the API does not silently rewrite their input.
		s.expr = raw
		return s, nil
	}
	// @reboot has no fire time a durable scheduler could compute; rejecting it is
	// clearer than accepting it and never firing.
	if strings.HasPrefix(strings.ToLower(raw), "@") {
		return nil, fmt.Errorf("unsupported cron descriptor %q; supported: @yearly, @annually, @monthly, @weekly, @daily, @midnight, @hourly", raw)
	}

	fields := strings.Fields(raw)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression has %d fields, want 5 (minute hour day-of-month month day-of-week): %q", len(fields), raw)
	}

	s := &Schedule{expr: raw}
	var err error
	if s.minute, err = parseField(fields[0], minuteSpec); err != nil {
		return nil, err
	}
	if s.hour, err = parseField(fields[1], hourSpec); err != nil {
		return nil, err
	}
	if s.dom, err = parseField(fields[2], domSpec); err != nil {
		return nil, err
	}
	if s.month, err = parseField(fields[3], monthSpec); err != nil {
		return nil, err
	}
	if s.dow, err = parseField(fields[4], dowSpec); err != nil {
		return nil, err
	}
	s.domRestricted = fields[2] != "*"
	s.dowRestricted = fields[4] != "*"
	return s, nil
}

// parseField parses one comma-separated field into a bitmask.
func parseField(field string, spec fieldSpec) (uint64, error) {
	var mask uint64
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0, fmt.Errorf("%s field has an empty element in %q", spec.name, field)
		}
		m, err := parseRange(part, spec)
		if err != nil {
			return 0, err
		}
		mask |= m
	}
	if mask == 0 {
		return 0, fmt.Errorf("%s field %q matches nothing", spec.name, field)
	}
	return mask, nil
}

// parseRange handles one element: *, n, a-b, and any of those with /step.
func parseRange(part string, spec fieldSpec) (uint64, error) {
	step := 1
	if slash := strings.Index(part, "/"); slash >= 0 {
		stepStr := part[slash+1:]
		part = part[:slash]
		n, err := strconv.Atoi(stepStr)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%s field has an invalid step %q; a step must be a positive integer", spec.name, stepStr)
		}
		step = n
		// "*/n" and "a/n" are both conventional; a bare "a/n" means from a to the
		// field's maximum, which is what most implementations do and what an author
		// writing "5/15" intends.
		if part == "" {
			return 0, fmt.Errorf("%s field has a step with no range in %q", spec.name, part+"/"+stepStr)
		}
	}

	if part == "*" {
		return maskRange(spec.min, spec.max, step), nil
	}

	lo, hi := part, part
	if dash := strings.Index(part, "-"); dash > 0 {
		lo, hi = part[:dash], part[dash+1:]
	} else if step > 1 {
		// "a/n" with no dash: from a through the maximum.
		hi = strconv.Itoa(spec.max)
	}

	loN, err := parseValue(lo, spec)
	if err != nil {
		return 0, err
	}
	hiN, err := parseValue(hi, spec)
	if err != nil {
		return 0, err
	}
	if loN > hiN {
		return 0, fmt.Errorf("%s field range %q is inverted (%d > %d)", spec.name, part, loN, hiN)
	}
	return maskRange(loN, hiN, step), nil
}

func parseValue(tok string, spec fieldSpec) (int, error) {
	tok = strings.TrimSpace(tok)
	if spec.names != nil {
		if v, ok := spec.names[strings.ToLower(tok)]; ok {
			return v, nil
		}
	}
	n, err := strconv.Atoi(tok)
	if err != nil {
		return 0, fmt.Errorf("%s field has an unrecognised value %q", spec.name, tok)
	}
	// 7 as Sunday is a widespread convention, and rejecting it would surprise
	// anyone porting an existing crontab.
	if spec.name == dowSpec.name && n == 7 {
		return 0, nil
	}
	if n < spec.min || n > spec.max {
		return 0, fmt.Errorf("%s field value %d is out of range (%d-%d)", spec.name, n, spec.min, spec.max)
	}
	return n, nil
}

func maskRange(lo, hi, step int) uint64 {
	var mask uint64
	for i := lo; i <= hi; i += step {
		mask |= 1 << uint(i)
	}
	return mask
}

func isSet(mask uint64, v int) bool { return mask&(1<<uint(v)) != 0 }

// maxLookahead bounds the search in Next.
//
// Four years covers every reachable combination including the leap-day case
// ("0 0 29 2 *" fires at most once every four years), and terminating is essential:
// a satisfiable-looking expression such as "0 0 30 2 *" matches no real date, and
// an unbounded loop would spin forever inside a timer handler.
const maxLookahead = 4 * 366 * 24 * time.Hour

// Next returns the first fire time strictly after `after`.
//
// The returned instant is in the schedule's location, so its wall-clock reading is
// the one the author wrote. Strictly after, so calling Next with a previous fire time
// always advances -- an inclusive boundary would make a schedule re-fire the tick it
// just ran every time the loop re-read it.
//
// The walk is over LOCAL wall-clock minutes, because that is what a cron expression
// describes. Each candidate wall time is then resolved to a real instant, which is
// where the gap and the fold are handled (see the package comment).
func (s *Schedule) Next(after time.Time) (time.Time, error) {
	// Truncate to the minute and step once: cron's resolution is a minute, and any
	// sub-minute remainder in `after` would otherwise make the first candidate the
	// same minute it was called in.
	start := after.In(s.loc).Truncate(time.Minute).Add(time.Minute)
	limit := start.Add(maxLookahead)

	// Walk wall-clock fields rather than adding durations. Adding an hour of real time
	// across a transition lands on a different wall-clock hour than intended, which is
	// precisely the drift local scheduling exists to avoid.
	y, mo, d := start.Date()
	h, mi := start.Hour(), start.Minute()

	for {
		cand := time.Date(y, mo, d, h, mi, 0, 0, s.loc)
		if cand.After(limit) {
			return time.Time{}, fmt.Errorf("cron expression %q has no fire time within %s of %s; it likely describes a date that never occurs (for example 30 February)",
				s.expr, maxLookahead, after.In(s.loc).Format(time.RFC3339))
		}

		if !isSet(s.month, int(mo)) {
			// Skip to the first instant of the next month rather than the next
			// minute: a month that does not match cannot contain a fire time, and
			// stepping through it a minute at a time is ~44,000 wasted iterations.
			y, mo, d, h, mi = nextMonth(y, mo)
			continue
		}
		if !s.dayMatches(time.Date(y, mo, d, 12, 0, 0, 0, s.loc)) {
			// Noon, deliberately: the day-of-week of a date is what is being tested,
			// and midday is never inside a DST transition in any zone, so the weekday
			// read is never distorted by one.
			y, mo, d, h, mi = nextDay(y, mo, d)
			continue
		}
		if !isSet(s.hour, h) {
			h, mi = h+1, 0
			if h > 23 {
				y, mo, d, h, mi = nextDay(y, mo, d)
			}
			continue
		}
		if !isSet(s.minute, mi) {
			mi++
			if mi > 59 {
				h, mi = h+1, 0
				if h > 23 {
					y, mo, d, h, mi = nextDay(y, mo, d)
				}
			}
			continue
		}

		// The wall time matches. Resolve it to a real instant.
		fire, ok := s.resolve(y, mo, d, h, mi)
		if ok && fire.After(after) {
			return fire, nil
		}
		// Either this wall time does not exist as its own instant (a gap minute that
		// normalizes onto another matching minute -- suppressed so it fires once), or
		// the resolved instant is not actually after `after`, which happens on a fold
		// when the earlier occurrence has already passed. Advance a wall minute.
		mi++
		if mi > 59 {
			h, mi = h+1, 0
			if h > 23 {
				y, mo, d, h, mi = nextDay(y, mo, d)
			}
		}
	}
}

// resolve turns a local wall-clock time into the instant it should fire at, reporting
// false when this wall time must not fire in its own right.
//
// THE TWO DST CASES, both measured against Go's tzdata rather than assumed:
//
// A GAP. time.Date normalizes a nonexistent wall time FORWARD, so asking for 01:30 on
// a spring-forward day returns 02:30. That is usable -- the job runs at the first
// instant that exists, an hour late, which beats not running -- but it creates a
// collision: local 01:30 and local 02:30 both resolve to 02:30. If both were allowed
// to fire, an hourly schedule would fire that one instant twice. So a normalized time
// fires only if the wall time it landed on is NOT itself a matching tick; otherwise the
// later, genuine one owns the instant and this one is suppressed.
//
// A FOLD. time.Date returns the SECOND occurrence of a repeated wall time, so the
// first (still in the pre-transition offset) is never produced and a tick is silently
// skipped. This checks for an earlier instant with the same wall reading and prefers
// it, so the tick fires once, on the first occurrence.
func (s *Schedule) resolve(y int, mo time.Month, d, h, mi int) (time.Time, bool) {
	cand := time.Date(y, mo, d, h, mi, 0, 0, s.loc)

	if cand.Hour() != h || cand.Minute() != mi {
		// Normalized: this wall time does not exist. It fires at the normalized
		// instant only if that instant's own wall time is not also a tick -- if it is,
		// that tick owns the instant and this one would be a duplicate.
		if isSet(s.hour, cand.Hour()) && isSet(s.minute, cand.Minute()) &&
			s.dayMatches(cand) && isSet(s.month, int(cand.Month())) {
			return time.Time{}, false
		}
		return cand, true
	}

	// An hour earlier in real time with the SAME wall reading means cand is the second
	// occurrence of a folded hour. Prefer the first.
	if earlier := cand.Add(-time.Hour); earlier.Hour() == h && earlier.Minute() == mi {
		return earlier, true
	}
	return cand, true
}

func nextMonth(y int, mo time.Month) (int, time.Month, int, int, int) {
	if mo == time.December {
		return y + 1, time.January, 1, 0, 0
	}
	return y, mo + 1, 1, 0, 0
}

func nextDay(y int, mo time.Month, d int) (int, time.Month, int, int, int) {
	// Constructed in UTC purely to do calendar arithmetic: AddDate on a zoned time can
	// shift the wall clock across a transition, and here only the DATE is wanted.
	t := time.Date(y, mo, d, 12, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	ny, nmo, nd := t.Date()
	return ny, nmo, nd, 0, 0
}

// dayMatches implements cron's one genuine oddity: when BOTH day-of-month and
// day-of-week are restricted, a day matches if EITHER matches, not both.
//
// So "0 0 1 * 1" is "the 1st of the month, and also every Monday" rather than "the
// 1st when it falls on a Monday". This is surprising and it is what every crontab
// implementation does, so matching it matters more than being logical: an operator
// porting an existing schedule must get the same days.
func (s *Schedule) dayMatches(t time.Time) bool {
	dom := isSet(s.dom, t.Day())
	dow := isSet(s.dow, int(t.Weekday()))
	switch {
	case s.domRestricted && s.dowRestricted:
		return dom || dow
	case s.domRestricted:
		return dom
	case s.dowRestricted:
		return dow
	default:
		return true
	}
}

// NextN returns up to n fire times after `after`, for a next-run preview.
//
// A preview is the only way an author can check a cron expression means what they
// think before it starts creating runs, which is why the API exposes it on create
// as well as on read.
func (s *Schedule) NextN(after time.Time, n int) ([]time.Time, error) {
	out := make([]time.Time, 0, n)
	t := after
	for i := 0; i < n; i++ {
		next, err := s.Next(t)
		if err != nil {
			// Whatever was computed so far is still useful to show.
			return out, err
		}
		out = append(out, next)
		t = next
	}
	return out, nil
}
