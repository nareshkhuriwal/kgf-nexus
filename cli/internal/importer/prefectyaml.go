// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// applyPrefectYAML reads a prefect.yaml and attaches each deployment's
// schedules to the converted flow its entrypoint names. Deployments are how
// Prefect schedules flows; in Nexus that is a companion schedule command.
func applyPrefectYAML(path string, files []irFile) []Finding {
	var out []Finding
	find := func(line int, status, msg string) {
		out = append(out, Finding{File: path, Line: line, Construct: "prefect.yaml", Status: status, Message: msg})
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		find(0, Unsupported, err.Error())
		return out
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil || len(root.Content) == 0 {
		find(0, Unsupported, fmt.Sprintf("not valid YAML: %v", err))
		return out
	}
	deps := mapGet(root.Content[0], "deployments")
	if deps == nil || deps.Kind != yaml.SequenceNode {
		find(0, Translated, "no deployments")
		return out
	}
	base := filepath.Dir(path)
	for _, d := range deps.Content {
		name := scalar(mapGet(d, "name"))
		ep := mapGet(d, "entrypoint")
		file, fn, ok := strings.Cut(scalar(ep), ":")
		if !ok {
			find(d.Line, Unsupported, fmt.Sprintf("deployment %s: entrypoint %q is not path.py:function", name, scalar(ep)))
			continue
		}
		target := findFlow(files, filepath.Join(base, file), fn)
		if target == nil {
			find(d.Line, Approximated, fmt.Sprintf("deployment %s: flow %s in %s was not converted (not in the input)",
				name, fn, file))
			continue
		}
		var params map[string]any
		if p := mapGet(d, "parameters"); p != nil {
			_ = p.Decode(&params)
		}
		var scheds []*yaml.Node
		if s := mapGet(d, "schedules"); s != nil && s.Kind == yaml.SequenceNode {
			scheds = s.Content
		}
		if s := mapGet(d, "schedule"); s != nil && s.Kind == yaml.MappingNode {
			scheds = append(scheds, s)
		}
		if len(scheds) == 0 {
			target.find(d.Line, "deployment", Translated, fmt.Sprintf("deployment %s has no schedule: runs start on "+
				"demand", name), "")
		}
		for _, s := range scheds {
			is := IRSchedule{Line: s.Line, Via: "prefect.yaml deployment " + name, Params: params,
				Timezone: scalar(mapGet(s, "timezone"))}
			switch {
			case mapGet(s, "cron") != nil:
				is.Cron = scalar(mapGet(s, "cron"))
			case mapGet(s, "interval") != nil:
				v, err := strconv.ParseFloat(scalar(mapGet(s, "interval")), 64)
				if err != nil {
					is.Unsupported = "the interval is not a number of seconds"
				}
				is.IntervalSeconds = v
			case mapGet(s, "rrule") != nil:
				is.Unsupported = "an RRule schedule has no cron equivalent"
			default:
				is.Unsupported = "a schedule without cron or interval"
			}
			if a := mapGet(s, "active"); a != nil && scalar(a) == "false" {
				is.Disabled = true
			}
			target.Schedules = append(target.Schedules, is)
		}
		for _, k := range []string{"work_pool", "build", "push", "pull"} {
			if mapGet(d, k) != nil {
				target.find(mapGet(d, k).Line, k, Approximated, fmt.Sprintf("deployment %s: %s (Prefect "+
					"infrastructure) is not part of a Nexus spec; ignored", name, k), "")
			}
		}
	}
	return out
}

func findFlow(files []irFile, path, fn string) *IRWorkflow {
	want, _ := filepath.Abs(path)
	var byBase *IRWorkflow
	for _, f := range files {
		abs, _ := filepath.Abs(f.Path)
		for _, w := range f.Workflows {
			if w.Function != fn {
				continue
			}
			if abs == want {
				return w
			}
			if filepath.Base(f.Path) == filepath.Base(path) {
				byBase = w
			}
		}
	}
	return byBase
}

// mapGet returns the value node of key in a mapping node.
func mapGet(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func scalar(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}
