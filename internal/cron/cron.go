// Package cron parses five-field cron expressions and computes fire times.
//
// Written rather than imported because the platform has no cron dependency and
// deliberately keeps a small one: the Python SDK ships with zero runtime deps for
// the same reason. A cron field parser is a bounded, well-specified problem with a
// finite test surface, which is exactly the kind of thing worth owning rather than
// tracking CVEs for.
//
// UTC ONLY in this release. Schedule rows carry a timezone column that is stored
// and not yet consulted, so every expression here is evaluated in UTC. That is a
// real limitation an operator must know about, not an implementation detail: "0 2 *
// * *" means 02:00 UTC, which is 03:00 in London for half the year. Local-time
// semantics need an explicit DST policy for the spring gap (an hour that does not
// exist) and the autumn fold (an hour that happens twice), and guessing at those
// silently either skips a run or doubles it.
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
	expr   string
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

// Parse compiles a five-field expression, or one of the @ descriptors.
//
// Errors name the field and the offending token, because a rejected schedule is
// something a person has to fix and "invalid cron expression" tells them nothing.
func Parse(expr string) (*Schedule, error) {
	raw := strings.TrimSpace(expr)
	if raw == "" {
		return nil, fmt.Errorf("cron expression is empty")
	}
	if expanded, ok := descriptors[strings.ToLower(raw)]; ok {
		s, err := Parse(expanded)
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

// Next returns the first fire time strictly after `after`, in UTC.
//
// Strictly after, so calling Next with a previous fire time always advances. An
// inclusive boundary would make a schedule re-fire the tick it just ran every time
// the loop re-read it.
func (s *Schedule) Next(after time.Time) (time.Time, error) {
	// Truncate to the minute and step once: cron's resolution is a minute, and any
	// sub-minute remainder in `after` would otherwise make the first candidate the
	// same minute it was called in.
	t := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.Add(maxLookahead)

	for t.Before(limit) {
		if !isSet(s.month, int(t.Month())) {
			// Skip to the first instant of the next month rather than the next
			// minute: a month that does not match cannot contain a fire time, and
			// stepping through it a minute at a time is ~44,000 wasted iterations.
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
			continue
		}
		if !isSet(s.hour, t.Hour()) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC).Add(time.Hour)
			continue
		}
		if !isSet(s.minute, t.Minute()) {
			t = t.Add(time.Minute)
			continue
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cron expression %q has no fire time within %s of %s; it likely describes a date that never occurs (for example 30 February)",
		s.expr, maxLookahead, after.UTC().Format(time.RFC3339))
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
