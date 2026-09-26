// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

var sourceTitle = map[string]string{Airflow: "Airflow", Prefect: "Prefect", Argo: "Argo Workflows"}

// Markdown renders the full report: every finding, translated ones included.
func Markdown(r *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# nexusctl import %s report\n\n", r.Source)
	fmt.Fprintf(&b, "- Input: `%s` (%d file(s))\n", filepath.ToSlash(r.Input), len(r.Files))
	fmt.Fprintf(&b, "- Namespace: `%s`\n", r.Namespace)
	fmt.Fprintf(&b, "- Workflows: %d\n", len(r.Workflows))
	fmt.Fprintf(&b, "- Constructs: %d translated, %d approximated, %d unsupported\n", r.Summary.Translated,
		r.Summary.Approximated, r.Summary.Unsupported)
	fmt.Fprintf(&b, "- Result: %s\n", outcome(r))
	if len(r.Findings) > 0 {
		b.WriteString("\n## Outside any workflow\n\n")
		table(&b, r.Findings)
	}
	for _, w := range r.Workflows {
		fmt.Fprintf(&b, "\n## %s -> `%s`\n\n", w.SourceID, w.Name)
		fmt.Fprintf(&b, "Source `%s:%d`. %d task(s), %d edge(s). %d translated, %d approximated, %d unsupported.",
			filepath.ToSlash(w.File), w.Line, w.Tasks, w.Edges, w.Counts.Translated, w.Counts.Approximated,
			w.Counts.Unsupported)
		if w.Output != "" {
			fmt.Fprintf(&b, " Spec: `%s`.", filepath.ToSlash(w.Output))
		}
		if w.Hash != "" {
			fmt.Fprintf(&b, " Content address `%s`.", w.Hash[:12])
		}
		b.WriteString("\n\n")
		if len(w.Problems) > 0 {
			b.WriteString("**The generated spec failed validation. This is a converter bug; please report it:**\n\n")
			for _, p := range w.Problems {
				fmt.Fprintf(&b, "- %s\n", p)
			}
			b.WriteString("\n")
		}
		table(&b, w.Findings)
		b.WriteString("\n### After import\n\n```sh\n")
		out := w.Output
		if out == "" {
			out = w.Name + ".yaml"
		}
		fmt.Fprintf(&b, "nexusctl workflow submit %s --dry-run --namespace %s\n", filepath.ToSlash(out), r.Namespace)
		for _, c := range w.Companions {
			b.WriteString(c.Command + "\n")
			if c.Backfill != "" {
				b.WriteString(c.Backfill + "\n")
			}
		}
		b.WriteString("```\n")
	}
	return b.String()
}

func outcome(r *Result) string {
	switch {
	case len(r.InvalidWorkflows()) > 0:
		return "nothing written: a generated spec failed validation (converter bug)"
	case r.Written && r.Partial:
		return "PARTIAL specs written (--allow-partial): unsupported constructs are failing placeholder tasks"
	case r.Written:
		return "specs written"
	case r.Unsupported():
		return "nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)"
	case len(r.Workflows) == 0:
		return "nothing written: no workflow found"
	}
	return "specs not written"
}

func table(b *strings.Builder, fs []Finding) {
	if len(fs) == 0 {
		b.WriteString("No findings.\n")
		return
	}
	b.WriteString("| Status | Source | Construct | Nexus | Note |\n|---|---|---|---|---|\n")
	for _, f := range fs {
		fmt.Fprintf(b, "| %s | `%s:%d` | %s | %s | %s |\n", f.Status, filepath.Base(f.File), f.Line, cell(f.Construct),
			cell(f.Nexus), cell(f.Message))
	}
}

func cell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// Summary is the human text for stderr: what was converted, every finding
// that needs attention (translated ones are only counted), and next steps.
func Summary(r *Result) string {
	var b strings.Builder
	title := sourceTitle[r.Source]
	fmt.Fprintf(&b, "Imported %d %s workflow(s) from %s: %d translated, %d approximated, %d unsupported.\n",
		len(r.Workflows), title, filepath.ToSlash(r.Input), r.Summary.Translated, r.Summary.Approximated,
		r.Summary.Unsupported)
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	row := func(f Finding) {
		fmt.Fprintf(tw, "  %s\t%s:%d\t%s\t%s\n", strings.ToUpper(f.Status), filepath.Base(f.File), f.Line,
			f.Construct, oneLine(f.Message, 1000))
	}
	for _, f := range r.Findings {
		if f.Status != Translated {
			row(f)
		}
	}
	_ = tw.Flush()
	for _, w := range r.Workflows {
		fmt.Fprintf(&b, "\n%s -> %s (%d tasks, %d edges)", w.SourceID, w.Name, w.Tasks, w.Edges)
		if w.Output != "" {
			fmt.Fprintf(&b, " -> %s", filepath.ToSlash(w.Output))
		}
		fmt.Fprintf(&b, "\n  %d translated, %d approximated, %d unsupported\n", w.Counts.Translated,
			w.Counts.Approximated, w.Counts.Unsupported)
		for _, p := range w.Problems {
			fmt.Fprintf(&b, "  CONVERTER BUG: %s\n", p)
		}
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, f := range w.Findings {
			if f.Status == Unsupported {
				fmt.Fprintf(tw, "  UNSUPPORTED\t%s:%d\t%s\t%s\n", filepath.Base(f.File), f.Line, f.Construct,
					oneLine(f.Message, 1000))
			}
		}
		for _, f := range w.Findings {
			if f.Status == Approximated {
				fmt.Fprintf(tw, "  approximated\t%s:%d\t%s\t%s\n", filepath.Base(f.File), f.Line, f.Construct,
					oneLine(f.Message, 1000))
			}
		}
		_ = tw.Flush()
		for _, c := range w.Companions {
			fmt.Fprintf(&b, "  schedule: %s\n", c.Command)
			if c.Backfill != "" {
				fmt.Fprintf(&b, "  backfill: %s\n", c.Backfill)
			}
		}
	}
	fmt.Fprintf(&b, "\n%s.\n", strings.ToUpper(outcome(r)[:1])+outcome(r)[1:])
	return b.String()
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-3] + "..."
	}
	return s
}
