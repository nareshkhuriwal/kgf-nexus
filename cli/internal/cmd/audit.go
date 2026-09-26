// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/khuriwalgroup/kgf-nexus/internal/auditchain"
)

// ExitVerifyFailed is `nexusctl audit verify` finding the chain broken.
// Distinct from ExitError so a scheduled evidence job can tell "the trail is
// tampered with" from "the API was down".
const ExitVerifyFailed = 8

type verifyFailed struct{ msg string }

func (e *verifyFailed) Error() string { return e.msg }
func (e *verifyFailed) ExitCode() int { return ExitVerifyFailed }

func auditGroup() *group {
	return &group{
		name:    "audit",
		summary: "Export and verify the tamper-evident audit trail.",
		commands: []*command{
			auditExportCmd(),
			auditVerifyCmd(),
		},
	}
}

func auditQueryFlags(fs *flag.FlagSet, q *url.Values) {
	for _, name := range []string{"since", "until", "action", "outcome", "actor-id"} {
		name := name
		key := name
		if name == "actor-id" {
			key = "actor_id"
		}
		fs.Func(name, "Filter: "+key+" (since/until are RFC 3339)", func(v string) error {
			q.Set(key, v)
			return nil
		})
	}
}

func auditExportCmd() *command {
	return &command{
		name:    "export",
		summary: "Download the audit trail as NDJSON or CSV.",
		usage: `nexusctl audit export [--format ndjson|csv] [--since T] [--until T] [--action A]
                      [--outcome O] [--actor-id ID] [--file PATH]

Streams the namespace's audit trail from one consistent snapshot. NDJSON is
the verifiable form: a header naming the chain's anchor, every row in chain
order with its hash, and a trailer with the chain head. An UNFILTERED NDJSON
export is complete: "nexusctl audit verify --file" can then prove nothing was
altered, inserted or removed. CSV is for spreadsheets and SIEM imports; cells a
spreadsheet would evaluate as formulas are prefixed with a quote.

Needs the audit.view permission (Administrator or Auditor).`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			q := url.Values{}
			format, file := "ndjson", ""
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.StringVar(&format, "format", "ndjson", "ndjson (verifiable) or csv")
				fs.StringVar(&file, "file", "", "Write to PATH instead of stdout")
				auditQueryFlags(fs, &q)
			})
			if err != nil {
				return err
			}
			if len(rest) > 0 {
				return usagef("audit export takes no operands")
			}
			if format != "ndjson" && format != "csv" {
				return usagef("--format must be ndjson or csv")
			}
			q.Set("format", format)
			c, err := env.Client()
			if err != nil {
				return err
			}
			body, err := c.AuditExport(ctx, q)
			if err != nil {
				return err
			}
			defer body.Close()
			var out io.Writer = env.IO.Out
			if file != "" {
				f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
				if err != nil {
					return err
				}
				defer f.Close()
				out = f
			}
			n, err := io.Copy(out, body)
			if err != nil {
				return err
			}
			if file != "" {
				fmt.Fprintf(env.IO.Err, "wrote %d bytes to %s\n", n, file)
			}
			return nil
		},
	}
}

func auditVerifyCmd() *command {
	return &command{
		name:    "verify",
		summary: "Verify the audit hash chain, from the API or offline from an export.",
		usage: `nexusctl audit verify [--file EXPORT.ndjson] [--allow-unchained]

Recomputes every row's hash (canonical_v1, OPERATIONS §25.2) and checks the
chain links from its anchor (genesis, or the last retention purge) to the
current head. With --file it verifies an NDJSON export OFFLINE and needs no
credentials; without it, it fetches a complete export from the API.

Exit status: 0 intact; 8 broken (the first broken sequence number is printed);
other non-zero codes as for every command (unreachable, not authorized).

Rows written after the chain started but without a chain position (an older
gateway replica during a rolling upgrade, or a direct SQL insert) fail the
verification unless --allow-unchained. Rows older than the chain (written
before migration 0033) are counted and reported, never failed.

The chain proves the retained rows were not changed relative to each other. It
cannot stop a database superuser from rewriting the whole tail: keep an export
or a SIEM sink outside the database and compare its last row_hash with this
command's "head".`,
		run: func(ctx context.Context, env *Environment, args []string) error {
			var file string
			var allowUnchained bool
			rest, err := env.parseFlags(args, func(fs *flag.FlagSet) {
				fs.StringVar(&file, "file", "", "Verify this NDJSON export offline")
				fs.BoolVar(&allowUnchained, "allow-unchained", false, "Accept rows without a chain position")
			})
			if err != nil {
				return err
			}
			if len(rest) > 0 {
				return usagef("audit verify takes no operands")
			}
			var in io.Reader
			if file != "" {
				f, err := os.Open(file)
				if err != nil {
					return err
				}
				defer f.Close()
				in = f
			} else {
				c, err := env.Client()
				if err != nil {
					return err
				}
				body, err := c.AuditExport(ctx, url.Values{"format": {"ndjson"}})
				if err != nil {
					return err
				}
				defer body.Close()
				in = body
			}
			res, err := auditchain.Verify(in)
			if err != nil {
				return &verifyFailed{msg: "cannot verify: " + err.Error()}
			}
			if !env.IsTable() {
				p, err := env.Printer()
				if err != nil {
					return err
				}
				if err := p.Print(res, nil); err != nil {
					return err
				}
			} else {
				anchor := "none"
				if res.Anchor != nil {
					anchor = fmt.Sprintf("%s seq %d", res.Anchor.Kind, res.Anchor.Seq)
				}
				fmt.Fprintf(env.IO.Out, "namespace:  %s\ncomplete:   %v\nrows:       %d (chained %d, pre-chain %d, unchained %d)\n"+
					"anchor:     %s\nhead:       seq %d %s\n",
					res.Namespace, res.Complete, res.Rows, res.Chained, res.PreChain, res.Unchained, anchor, res.LastSeq, res.LastHash)
			}
			if p := res.FirstProblem(allowUnchained); p != nil {
				for i, q := range res.Problems {
					if i >= 20 {
						fmt.Fprintf(env.IO.Err, "... and %d more\n", len(res.Problems)-20)
						break
					}
					fmt.Fprintf(env.IO.Err, "BROKEN: %s\n", q)
				}
				return &verifyFailed{msg: "audit chain verification FAILED at " + p.String()}
			}
			if !res.Complete {
				fmt.Fprintln(env.IO.Err, "note: the export was filtered; row hashes were verified but completeness was not")
			}
			env.Infof("audit chain intact")
			return nil
		},
	}
}
