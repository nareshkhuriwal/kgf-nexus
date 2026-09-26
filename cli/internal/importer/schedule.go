// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Companion is a schedule (and optional backfill) suggested for a converted
// workflow. Nexus schedules are API objects of their own, not part of the
// spec, so the importer never puts one in the spec: it prints the commands.
type Companion struct {
	Schedule string            `json:"schedule"`
	Workflow string            `json:"workflow"`
	Cron     string            `json:"cron"`
	Timezone string            `json:"timezone"`
	Overlap  string            `json:"overlap,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
	Command  string            `json:"command"`
	Backfill string            `json:"backfill,omitempty"`
	Line     int               `json:"line"`
}

var cronPresets = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@monthly":  "0 0 1 * *",
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
}

// schedules turns the IR's schedule facts into companion commands and findings.
func schedules(ir *IRWorkflow, name, namespace, source string) ([]Companion, []Finding) {
	var out []Companion
	var fs []Finding
	for _, s := range ir.Schedules {
		file := ir.File
		if s.File != "" {
			file = s.File
		}
		find := func(line int, construct, status, msg, nexus string) {
			fs = append(fs, Finding{File: file, Line: line, Construct: construct, Status: status, Message: msg,
				Nexus: nexus})
		}
		construct := "schedule"
		if s.Via != "" {
			construct = s.Via
		}
		switch {
		case s.Unsupported != "":
			find(s.Line, construct, Unsupported, s.Unsupported, "")
			continue
		case s.None:
			find(s.Line, construct, Translated, "no schedule: runs start on demand (`nexusctl run create`)", "")
			continue
		case s.Unspecified:
			if source == "airflow" {
				find(s.Line, construct, Approximated, "no schedule argument: Airflow 2 defaults to daily, Airflow 3 "+
					"to none; no schedule was suggested", "")
			}
			continue
		}
		cron, status, note := s.Cron, Translated, ""
		switch {
		case s.Cron == "@once":
			find(s.Line, construct, Approximated, "@once: start the single run yourself with `nexusctl run create "+
				name+"`", "")
			continue
		case s.Cron == "@continuous":
			find(s.Line, construct, Unsupported, "@continuous (start a new run as soon as one ends) has no Nexus "+
				"equivalent", "")
			continue
		case strings.HasPrefix(s.Cron, "@"):
			c, ok := cronPresets[s.Cron]
			if !ok {
				find(s.Line, construct, Unsupported, fmt.Sprintf("schedule preset %s is not known", s.Cron), "")
				continue
			}
			cron, note = c, fmt.Sprintf("%s is %q", s.Cron, c)
		case s.Cron != "":
			if len(strings.Fields(s.Cron)) != 5 {
				find(s.Line, construct, Unsupported, fmt.Sprintf("cron %q does not have 5 fields; Nexus schedules "+
					"take standard 5-field cron", s.Cron), "")
				continue
			}
			cron = strings.Join(strings.Fields(s.Cron), " ")
		case s.IntervalSeconds > 0:
			c, ok := intervalCron(s.IntervalSeconds)
			if !ok {
				find(s.Line, construct, Unsupported, fmt.Sprintf("an interval of %gs has no cron equivalent (it does "+
					"not divide a minute, hour or day evenly)", s.IntervalSeconds), "")
				continue
			}
			cron, status = c, Approximated
			note = fmt.Sprintf("an interval of %s became cron %q, which fires on wall-clock boundaries rather than "+
				"every interval from the start date", humanSeconds(s.IntervalSeconds), c)
		default:
			continue
		}
		if s.Note != "" {
			note = strings.TrimPrefix(note+"; "+s.Note, "; ")
			status = Approximated
		}
		tz := s.Timezone
		if tz == "" {
			tz = "UTC"
		}
		sched := name
		if len(out) > 0 {
			sched = fmt.Sprintf("%s-%d", name, len(out)+1)
		}
		c := Companion{Schedule: sched, Workflow: name, Cron: cron, Timezone: tz, Disabled: s.Disabled, Line: s.Line}
		overlapStatus := ""
		switch {
		case s.Overlap != "":
			c.Overlap = s.Overlap
		case source == "airflow" && s.MaxActiveRuns != nil && *s.MaxActiveRuns == 1:
			c.Overlap = "queue_one"
			overlapStatus = "max_active_runs=1 queues later runs in Airflow; overlap queue_one keeps at most one waiting"
		case source == "airflow":
			c.Overlap = "allow"
		}
		if s.OverlapNote != "" {
			overlapStatus = s.OverlapNote
		}
		if len(s.Params) > 0 {
			c.Params = map[string]string{}
			for k, v := range s.Params {
				b, _ := json.Marshal(v)
				c.Params[sanitizeIdentifier(k)] = string(b)
			}
		}
		c.Command = scheduleCommand(c, namespace)
		msg := fmt.Sprintf("schedule %q in %s", cron, tz)
		if note != "" {
			msg += " (" + note + ")"
		}
		find(s.Line, construct, status, msg, "companion: nexusctl schedule create "+sched)
		if overlapStatus != "" {
			find(s.Line, "overlap", Approximated, overlapStatus, "schedule --overlap "+c.Overlap)
		}
		if s.Catchup != nil && *s.Catchup && s.StartDate != "" {
			to := "\"$(date -u +%Y-%m-%d)\""
			if s.EndDate != "" {
				to = s.EndDate[:10]
			}
			c.Backfill = fmt.Sprintf("nexusctl backfill start %s --from %s --to %s --namespace %s", sched,
				s.StartDate[:10], to, namespace)
		}
		out = append(out, c)
	}
	return out, fs
}

func scheduleCommand(c Companion, namespace string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "nexusctl schedule create %s --workflow %s --cron %s --timezone %s", c.Schedule, c.Workflow,
		shellQuote(c.Cron), shellQuote(c.Timezone))
	if c.Overlap != "" {
		fmt.Fprintf(&b, " --overlap %s", c.Overlap)
	}
	if c.Disabled {
		b.WriteString(" --disabled")
	}
	keys := make([]string, 0, len(c.Params))
	for k := range c.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, " --param %s", shellQuote(k+"="+c.Params[k]))
	}
	fmt.Fprintf(&b, " --namespace %s", namespace)
	return b.String()
}

// intervalCron expresses a fixed interval as cron when it divides a larger
// unit evenly; otherwise there is no faithful cron.
func intervalCron(sec float64) (string, bool) {
	if sec <= 0 || sec != math.Trunc(sec) {
		return "", false
	}
	s := int64(sec)
	switch {
	case s == 604800:
		return "0 0 * * 0", true
	case s%86400 == 0:
		d := s / 86400
		if d == 1 {
			return "0 0 * * *", true
		}
		if d <= 28 {
			return fmt.Sprintf("0 0 */%d * *", d), true
		}
	case s%3600 == 0:
		h := s / 3600
		if h == 1 {
			return "0 * * * *", true
		}
		if 24%h == 0 {
			return fmt.Sprintf("0 */%d * * *", h), true
		}
	case s%60 == 0:
		m := s / 60
		if m == 1 {
			return "* * * * *", true
		}
		if 60%m == 0 {
			return fmt.Sprintf("*/%d * * * *", m), true
		}
	}
	return "", false
}

func humanSeconds(s float64) string {
	switch {
	case s >= 86400 && math.Mod(s, 86400) == 0:
		return fmt.Sprintf("%gd", s/86400)
	case s >= 3600 && math.Mod(s, 3600) == 0:
		return fmt.Sprintf("%gh", s/3600)
	case s >= 60 && math.Mod(s, 60) == 0:
		return fmt.Sprintf("%gm", s/60)
	}
	return fmt.Sprintf("%gs", s)
}

// shellQuote quotes for a POSIX shell when needed.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
