// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package cmd

// nexusctl trigger | sensor | notify | asset (ADR-0028).

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/cli/internal/client"
	"github.com/khuriwalgroup/kgf-nexus/cli/internal/output"
	"github.com/khuriwalgroup/kgf-nexus/internal/notify"
)

// ── shared plumbing ─────────────────────────────────────────────────────────

// simple builds a command whose run needs a client, a printer and N operands.
func simple(name, summary, usage string, operands int, flags func(fs *flag.FlagSet),
	do func(ctx context.Context, env *Environment, c *client.Client, p *output.Printer, args []string) error) *command {
	return &command{name: name, summary: summary, usage: usage,
		run: func(ctx context.Context, env *Environment, args []string) error {
			rest, err := env.parseFlags(args, flags)
			if err != nil {
				return err
			}
			if len(rest) != operands {
				return usagef("%s takes %d operand(s), got %d\nUsage: %s", name, operands, len(rest), strings.SplitN(usage, "\n", 2)[0])
			}
			c, err := env.Client()
			if err != nil {
				return err
			}
			p, err := env.Printer()
			if err != nil {
				return err
			}
			return do(ctx, env, c, p, rest)
		}}
}

// kv renders one object as a two-column table of the named fields.
func kv(raw any, keys ...string) *output.Table {
	m, _ := raw.(map[string]any)
	t := &output.Table{Columns: []string{"field", "value"}}
	for _, k := range keys {
		v := "-"
		if x, ok := m[k]; ok && x != nil {
			switch y := x.(type) {
			case map[string]any, []any:
				b, _ := json.Marshal(y)
				v = string(b)
			default:
				v = output.Str(m, k)
			}
		}
		t.Rows = append(t.Rows, []string{k, v})
	}
	return t
}

// mappings parses repeated --map PARAM=EXPR into a params mapping. EXPR is kept
// as a string (a $.path, a {{ }} template or a literal); use --map-json to bind
// a non-string literal.
func mappings(strs, jsons stringSlice) (map[string]any, error) {
	out := map[string]any{}
	for _, raw := range strs {
		k, v, ok := strings.Cut(raw, "=")
		if !ok || k == "" {
			return nil, usagef("--map %q is not PARAM=EXPR (e.g. --map repo='$.payload.repository.name')", raw)
		}
		out[k] = v
	}
	for _, raw := range jsons {
		k, v, ok := strings.Cut(raw, "=")
		if !ok || k == "" {
			return nil, usagef("--map-json %q is not PARAM=JSON", raw)
		}
		var parsed any
		if err := json.Unmarshal([]byte(v), &parsed); err != nil {
			return nil, usagef("--map-json %s: %v", k, err)
		}
		out[k] = parsed
	}
	return out, nil
}

func visited(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// ── trigger ─────────────────────────────────────────────────────────────────

func triggerGroup() *group {
	return &group{name: "trigger", summary: "Manage inbound webhook triggers that start runs.",
		commands: []*command{triggerListCmd(), triggerGetCmd(), triggerSaveCmd(true), triggerSaveCmd(false),
			triggerDeleteCmd(), triggerRotateCmd(), triggerDeliveriesCmd(), triggerFireCmd()}}
}

func triggerListCmd() *command {
	return simple("list", "List webhook triggers.", "nexusctl trigger list", 0, nil,
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, _ []string) error {
			res, err := c.ListTriggers(ctx)
			if err != nil {
				return err
			}
			t := &output.Table{Columns: []string{"name", "workflow", "enabled", "fired", "last_fired", "path"},
				Empty: fmt.Sprintf("No triggers in namespace %q. Create one with: nexusctl trigger create <name> --workflow W", c.Namespace())}
			for _, x := range output.Items(res, "triggers") {
				t.Rows = append(t.Rows, []string{output.Str(x, "name"), output.Str(x, "workflow"), output.Str(x, "enabled"),
					output.Str(x, "fire_count"), output.Str(x, "last_fired_at"), output.Str(x, "path")})
			}
			return p.Print(res, t)
		})
}

func triggerGetCmd() *command {
	return simple("get", "Show a webhook trigger (never its secret).", "nexusctl trigger get <name>", 1, nil,
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, a []string) error {
			res, err := c.GetTrigger(ctx, a[0])
			if err != nil {
				return err
			}
			return p.Print(res, kv(res, "name", "id", "workflow", "workflow_version", "enabled", "path", "url",
				"params_mapping", "idempotency_key_path", "tolerance_seconds", "secret_rotated_at",
				"previous_secret_expires_at", "fire_count", "last_fired_at", "created_by", "signature_scheme"))
		})
}

func triggerSaveCmd(create bool) *command {
	name, summary := "create", "Create a webhook trigger; prints its secret ONCE."
	usage := `nexusctl trigger create <name> --workflow W [--map PARAM=EXPR ...] [--idempotency-key-path '$.payload.id']
                        [--workflow-version N] [--tolerance 300] [--priority N] [--disabled]

EXPR is a JSONPath into {"payload": <body>, "trigger": {...}} ('$.payload.ref'),
a template ('build {{ $.payload.ref }}') or a literal. The secret is shown once;
senders sign with X-Nexus-Timestamp and
X-Nexus-Signature: v1=hex(HMAC-SHA256(secret, timestamp + "." + body)).`
	if !create {
		name, summary = "update", "Change a webhook trigger (only the flags given)."
		usage = `nexusctl trigger update <name> [--workflow W] [--map PARAM=EXPR ...] [--idempotency-key-path P] [--enable|--disable] ...`
	}
	var (
		workflow, idemPath      string
		version, tolerance, pri int
		maps, mapJSON           stringSlice
		disabled, enable        bool
		fs                      *flag.FlagSet
	)
	return simple(name, summary, usage, 1, func(f *flag.FlagSet) {
		fs = f
		f.StringVar(&workflow, "workflow", "", "Workflow (spec name) to run")
		f.IntVar(&version, "workflow-version", 0, "Pin a workflow version (default: latest)")
		f.Var(&maps, "map", "Map a run parameter: PARAM=EXPR (repeatable)")
		f.Var(&mapJSON, "map-json", "Bind a parameter to a JSON literal: PARAM=JSON (repeatable)")
		f.StringVar(&idemPath, "idempotency-key-path", "", "JSONPath to the payload's delivery id; retries converge on one run")
		f.IntVar(&tolerance, "tolerance", 0, "Timestamp tolerance in seconds (default 300)")
		f.IntVar(&pri, "priority", 0, "Priority of created runs")
		f.BoolVar(&disabled, "disable", false, "Disable the trigger")
		if !create {
			f.BoolVar(&enable, "enable", false, "Enable the trigger")
		} else {
			f.BoolVar(&disabled, "disabled", false, "Create it disabled")
		}
	}, func(ctx context.Context, env *Environment, c *client.Client, p *output.Printer, a []string) error {
		set := visited(fs)
		body := map[string]any{}
		if workflow != "" {
			body["workflow"] = workflow
		}
		if set["workflow-version"] {
			body["workflow_version"] = version
		}
		if set["map"] || set["map-json"] {
			m, err := mappings(maps, mapJSON)
			if err != nil {
				return err
			}
			body["params_mapping"] = m
		}
		if set["idempotency-key-path"] {
			body["idempotency_key_path"] = idemPath
		}
		if set["tolerance"] {
			body["tolerance_seconds"] = tolerance
		}
		if set["priority"] {
			body["priority"] = pri
		}
		if disabled {
			body["enabled"] = false
		} else if enable {
			body["enabled"] = true
		}
		var res any
		var err error
		if create {
			if workflow == "" {
				return usagef("--workflow is required")
			}
			body["name"] = a[0]
			res, err = c.CreateTrigger(ctx, body)
		} else {
			res, err = c.UpdateTrigger(ctx, a[0], body)
		}
		if err != nil {
			return err
		}
		if create {
			env.Infof("Save the secret now; it cannot be shown again (rotate it with: nexusctl trigger rotate-secret %s).", a[0])
			return p.Print(res, kv(res, "name", "id", "workflow", "path", "url", "secret"))
		}
		return p.Print(res, kv(res, "name", "workflow", "enabled", "params_mapping", "idempotency_key_path", "revision"))
	})
}

func triggerDeleteCmd() *command {
	return simple("delete", "Delete a webhook trigger; its URL stops working.", "nexusctl trigger delete <name>", 1, nil,
		func(ctx context.Context, env *Environment, c *client.Client, p *output.Printer, a []string) error {
			res, err := c.DeleteTrigger(ctx, a[0])
			if err != nil {
				return err
			}
			env.Infof("Deleted trigger %s.", a[0])
			return p.Print(res, &output.Table{Columns: []string{"deleted"}, Rows: [][]string{{a[0]}}})
		})
}

func triggerRotateCmd() *command {
	var grace time.Duration
	return simple("rotate-secret", "Issue a new trigger secret; the old one keeps working for --grace.",
		"nexusctl trigger rotate-secret <name> [--grace 24h]  (--grace 0 revokes the old secret now)", 1,
		func(fs *flag.FlagSet) {
			fs.DurationVar(&grace, "grace", 24*time.Hour, "How long the old secret still verifies")
		},
		func(ctx context.Context, env *Environment, c *client.Client, p *output.Printer, a []string) error {
			res, err := c.RotateTriggerSecret(ctx, a[0], int(grace.Seconds()))
			if err != nil {
				return err
			}
			env.Infof("Save the new secret now; it cannot be shown again.")
			return p.Print(res, kv(res, "name", "secret", "secret_rotated_at", "previous_secret_expires_at"))
		})
}

func triggerDeliveriesCmd() *command {
	var limit int
	return simple("deliveries", "Show a trigger's recent inbound deliveries.", "nexusctl trigger deliveries <name> [--limit N]", 1,
		func(fs *flag.FlagSet) { fs.IntVar(&limit, "limit", 50, "Maximum rows") },
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, a []string) error {
			res, err := c.TriggerDeliveries(ctx, a[0], limit)
			if err != nil {
				return err
			}
			t := &output.Table{Columns: []string{"received", "outcome", "status", "run", "idempotency_key", "detail"}, Empty: "No deliveries received yet."}
			for _, x := range output.Items(res, "deliveries") {
				t.Rows = append(t.Rows, []string{output.Str(x, "received_at"), output.Str(x, "outcome"), output.Str(x, "http_status"),
					output.Str(x, "run_id"), output.Str(x, "idempotency_key"), output.Str(x, "detail")})
			}
			return p.Print(res, t)
		})
}

// triggerFireCmd signs and sends a payload exactly as an external sender would:
// the fastest way to prove a trigger, its mapping and its workflow end to end.
func triggerFireCmd() *command {
	var data, secretEnv string
	return simple("fire", "Sign and send a test payload to a trigger's hook URL.",
		`nexusctl trigger fire <name> --secret-env VAR [--data JSON | --data @file]

The secret is read from the named environment variable (never a flag value, so it
stays out of shell history and process listings).`, 1,
		func(fs *flag.FlagSet) {
			fs.StringVar(&data, "data", "{}", "JSON body, or @path to read it from a file")
			fs.StringVar(&secretEnv, "secret-env", "NEXUS_TRIGGER_SECRET", "Environment variable holding the trigger secret")
		},
		func(ctx context.Context, env *Environment, c *client.Client, p *output.Printer, a []string) error {
			secret := env.Env(secretEnv)
			if secret == "" {
				return usagef("$%s is empty; export the trigger secret there (or name another variable with --secret-env)", secretEnv)
			}
			body := []byte(data)
			if strings.HasPrefix(data, "@") {
				var err error
				if strings.TrimPrefix(data, "@") == "-" {
					body, err = io.ReadAll(env.IO.In)
				} else {
					body, err = os.ReadFile(strings.TrimPrefix(data, "@"))
				}
				if err != nil {
					return err
				}
			}
			if !json.Valid(body) {
				return usagef("--data is not valid JSON")
			}
			t, err := c.GetTrigger(ctx, a[0])
			if err != nil {
				return err
			}
			path := output.Str(t.(map[string]any), "path")
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			res, err := c.FireHook(ctx, path, body, map[string]string{
				"Content-Type":      "application/json",
				"X-Nexus-Timestamp": ts,
				"X-Nexus-Signature": notify.Signature([]byte(secret), ts, body),
			})
			if err != nil {
				return err
			}
			return p.Print(res, kv(res, "run_id", "created", "idempotency_key"))
		})
}

// ── sensor ──────────────────────────────────────────────────────────────────

func sensorGroup() *group {
	return &group{name: "sensor", summary: "Manage sensors that start runs on data arrival or upstream success.",
		commands: []*command{sensorListCmd(), sensorGetCmd(), sensorSaveCmd(true), sensorSaveCmd(false),
			sensorDeleteCmd(), sensorToggleCmd(true), sensorToggleCmd(false), sensorObservationsCmd()}}
}

func sensorListCmd() *command {
	return simple("list", "List sensors and their status.", "nexusctl sensor list", 0, nil,
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, _ []string) error {
			res, err := c.ListSensors(ctx)
			if err != nil {
				return err
			}
			t := &output.Table{Columns: []string{"name", "kind", "workflow", "status", "triggered", "pending", "last_poll", "last_error"},
				Empty: fmt.Sprintf("No sensors in namespace %q.", c.Namespace())}
			for _, x := range output.Items(res, "sensors") {
				st, _ := x["state"].(map[string]any)
				if st == nil {
					st = map[string]any{}
				}
				t.Rows = append(t.Rows, []string{output.Str(x, "name"), output.Str(x, "kind"), output.Str(x, "workflow"),
					output.Str(x, "status"), output.Str(st, "triggered_total"), output.Str(st, "pending"),
					output.Str(st, "last_poll_at"), output.Str(st, "last_error")})
			}
			return p.Print(res, t)
		})
}

func sensorGetCmd() *command {
	return simple("get", "Show a sensor, its cursor and health.", "nexusctl sensor get <name>", 1, nil,
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, a []string) error {
			res, err := c.GetSensor(ctx, a[0])
			if err != nil {
				return err
			}
			return p.Print(res, kv(res, "name", "kind", "workflow", "status", "enabled", "config", "params_mapping",
				"poll_interval_seconds", "backfill", "state", "created_by"))
		})
}

func sensorSaveCmd(create bool) *command {
	name, summary := "create", "Create a sensor."
	usage := `nexusctl sensor create <name> --workflow W (--uri s3://bucket/prefix/ [--pattern '*.parquet'] | --upstream WORKFLOW)
                       [--map PARAM=EXPR ...] [--poll 30s] [--backfill]

Object sensors expose $.object.{uri,bucket,key,relative_key,size,etag,last_modified};
upstream sensors expose $.run.{id,workflow,params,logical_date,ended_at}.
Without --backfill, what already exists is recorded as a baseline and only new
arrivals trigger.`
	if !create {
		name, summary, usage = "update", "Change a sensor (only the flags given).", "nexusctl sensor update <name> [--uri U] [--pattern P] [--map ...] [--poll D]"
	}
	var (
		workflow, uri, pattern, upstream string
		poll                             time.Duration
		maps, mapJSON                    stringSlice
		backfill                         bool
		fs                               *flag.FlagSet
	)
	return simple(name, summary, usage, 1, func(f *flag.FlagSet) {
		fs = f
		f.StringVar(&workflow, "workflow", "", "Workflow (spec name) to run")
		f.StringVar(&uri, "uri", "", "Object-store prefix to watch, e.g. s3://bucket/landing/")
		f.StringVar(&pattern, "pattern", "", "Glob the key (relative to the prefix) must match: *, **, ?")
		f.StringVar(&upstream, "upstream", "", "Start a run when a run of this workflow succeeds")
		f.Var(&maps, "map", "Map a run parameter: PARAM=EXPR (repeatable)")
		f.Var(&mapJSON, "map-json", "Bind a parameter to a JSON literal (repeatable)")
		f.DurationVar(&poll, "poll", 30*time.Second, "Poll interval (5s to 24h)")
		f.BoolVar(&backfill, "backfill", false, "Also trigger for what already exists")
	}, func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, a []string) error {
		set := visited(fs)
		body := map[string]any{}
		if workflow != "" {
			body["workflow"] = workflow
		}
		cfg := map[string]any{}
		kind := ""
		switch {
		case uri != "" && upstream != "":
			return usagef("give --uri or --upstream, not both")
		case uri != "":
			kind, cfg["uri"] = "object_store", uri
			if pattern != "" {
				cfg["pattern"] = pattern
			}
		case upstream != "":
			kind, cfg["upstream_workflow"] = "run_succeeded", upstream
		}
		if len(cfg) > 0 {
			body["config"] = cfg
		}
		if set["map"] || set["map-json"] {
			m, err := mappings(maps, mapJSON)
			if err != nil {
				return err
			}
			body["params_mapping"] = m
		}
		if create || set["poll"] {
			body["poll_interval_seconds"] = int(poll.Seconds())
		}
		if create || set["backfill"] {
			body["backfill"] = backfill
		}
		var res any
		var err error
		if create {
			if workflow == "" || kind == "" {
				return usagef("--workflow and one of --uri or --upstream are required")
			}
			body["name"], body["kind"] = a[0], kind
			res, err = c.CreateSensor(ctx, body)
		} else {
			res, err = c.UpdateSensor(ctx, a[0], body)
		}
		if err != nil {
			return err
		}
		return p.Print(res, kv(res, "name", "kind", "workflow", "status", "config", "params_mapping", "poll_interval_seconds", "backfill"))
	})
}

func sensorDeleteCmd() *command {
	return simple("delete", "Delete a sensor and its observation history.", "nexusctl sensor delete <name>", 1, nil,
		func(ctx context.Context, env *Environment, c *client.Client, p *output.Printer, a []string) error {
			res, err := c.DeleteSensor(ctx, a[0])
			if err != nil {
				return err
			}
			env.Infof("Deleted sensor %s.", a[0])
			return p.Print(res, &output.Table{Columns: []string{"deleted"}, Rows: [][]string{{a[0]}}})
		})
}

func sensorToggleCmd(on bool) *command {
	name := "disable"
	if on {
		name = "enable"
	}
	return simple(name, strings.ToUpper(name[:1])+name[1:]+" a sensor.", "nexusctl sensor "+name+" <name>", 1, nil,
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, a []string) error {
			res, err := c.UpdateSensor(ctx, a[0], map[string]any{"enabled": on})
			if err != nil {
				return err
			}
			return p.Print(res, kv(res, "name", "enabled", "status"))
		})
}

func sensorObservationsCmd() *command {
	var all bool
	var limit int
	return simple("observations", "Show what a sensor observed and which runs it started.",
		"nexusctl sensor observations <name> [--all] [--limit N]", 1,
		func(fs *flag.FlagSet) {
			fs.BoolVar(&all, "all", false, "Include baseline observations")
			fs.IntVar(&limit, "limit", 50, "Maximum rows")
		},
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, a []string) error {
			res, err := c.SensorObservations(ctx, a[0], all, limit)
			if err != nil {
				return err
			}
			t := &output.Table{Columns: []string{"item", "outcome", "run", "observed", "error"}, Empty: "Nothing observed yet."}
			for _, x := range output.Items(res, "observations") {
				t.Rows = append(t.Rows, []string{output.Str(x, "item_key"), output.Str(x, "outcome"), output.Str(x, "run_id"),
					output.Str(x, "observed_at"), output.Str(x, "last_error")})
			}
			return p.Print(res, t)
		})
}

// ── notify ──────────────────────────────────────────────────────────────────

func notifyGroup() *group {
	return &group{name: "notify", summary: "Manage notification channels, rules and deliveries.",
		commands: []*command{notifyChannelsCmd(), notifyChannelGetCmd(), notifyChannelCreateCmd(), notifyChannelDeleteCmd(),
			notifyChannelTestCmd(), notifyRulesCmd(), notifyRuleCreateCmd(), notifyRuleDeleteCmd(),
			notifyDeliveriesCmd(), notifyRedriveCmd()}}
}

func notifyChannelsCmd() *command {
	return simple("channels", "List notification channels.", "nexusctl notify channels", 0, nil,
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, _ []string) error {
			res, err := c.ListChannels(ctx)
			if err != nil {
				return err
			}
			t := &output.Table{Columns: []string{"name", "kind", "enabled", "secret", "rate/min"}, Empty: "No notification channels."}
			for _, x := range output.Items(res, "channels") {
				sec, _ := x["secret"].(map[string]any)
				s := "-"
				if sec != nil && sec["configured"] == true {
					s = output.Str(sec, "source") + " " + output.Str(sec, "hint")
				}
				t.Rows = append(t.Rows, []string{output.Str(x, "name"), output.Str(x, "kind"), output.Str(x, "enabled"), s,
					output.Str(x, "rate_limit_per_minute")})
			}
			return p.Print(res, t)
		})
}

func notifyChannelGetCmd() *command {
	return simple("channel-get", "Show a channel (never its secret).", "nexusctl notify channel-get <name>", 1, nil,
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, a []string) error {
			res, err := c.GetChannel(ctx, a[0])
			if err != nil {
				return err
			}
			return p.Print(res, kv(res, "name", "kind", "enabled", "config", "secret", "rate_limit_per_minute", "created_by", "updated_at"))
		})
}

func notifyChannelCreateCmd() *command {
	var (
		kind, url, slackChannel, severity, secretEnv, secretRef string
		to                                                      stringSlice
		rate                                                    int
	)
	return simple("channel-create", "Create a notification channel.",
		`nexusctl notify channel-create <name> --kind KIND [settings] [--secret-env VAR | --secret-ref env:NAME|file:/path]

KIND and its settings:
  slack_webhook   secret = the incoming-webhook URL
  slack_bot       secret = bot token; --slack-channel C0123
  teams           secret = the Workflows webhook URL
  pagerduty       secret = Events v2 routing key; --severity critical|error|warning|info
  webhook         --url https://...; signing secret generated and shown once
  email           --to a@example.com (repeatable)
Secrets are read from a local environment variable (--secret-env), never from a
flag value, or given as a reference the event-service resolves.`, 1,
		func(fs *flag.FlagSet) {
			fs.StringVar(&kind, "kind", "", "Channel kind")
			fs.StringVar(&url, "url", "", "Generic webhook URL")
			fs.StringVar(&slackChannel, "slack-channel", "", "Slack channel id (slack_bot)")
			fs.StringVar(&severity, "severity", "", "PagerDuty severity")
			fs.Var(&to, "to", "Email recipient (repeatable)")
			fs.StringVar(&secretEnv, "secret-env", "", "Read the secret from this local environment variable")
			fs.StringVar(&secretRef, "secret-ref", "", "Reference resolved by the event-service: env:NAME or file:/path")
			fs.IntVar(&rate, "rate", 30, "Maximum sends per minute on this channel")
		},
		func(ctx context.Context, env *Environment, c *client.Client, p *output.Printer, a []string) error {
			if kind == "" {
				return usagef("--kind is required (%s)", strings.Join(notify.Kinds, ", "))
			}
			cfg := map[string]any{}
			if url != "" {
				cfg["url"] = url
			}
			if slackChannel != "" {
				cfg["channel"] = slackChannel
			}
			if severity != "" {
				cfg["severity"] = severity
			}
			if len(to) > 0 {
				cfg["to"] = []string(to)
			}
			body := map[string]any{"name": a[0], "kind": kind, "config": cfg, "rate_limit_per_minute": rate}
			switch {
			case secretEnv != "" && secretRef != "":
				return usagef("give --secret-env or --secret-ref, not both")
			case secretEnv != "":
				v := env.Env(secretEnv)
				if v == "" {
					return usagef("$%s is empty", secretEnv)
				}
				body["secret"] = v
			case secretRef != "":
				body["secret_ref"] = secretRef
			}
			res, err := c.CreateChannel(ctx, body)
			if err != nil {
				return err
			}
			if m, ok := res.(map[string]any); ok && m["signing_secret"] != nil {
				env.Infof("Save the signing secret now; receivers verify X-Nexus-Signature with it and it cannot be shown again.")
			}
			return p.Print(res, kv(res, "name", "kind", "enabled", "config", "secret", "signing_secret"))
		})
}

func notifyChannelDeleteCmd() *command {
	return simple("channel-delete", "Delete a channel (and its delivery history).", "nexusctl notify channel-delete <name>", 1, nil,
		func(ctx context.Context, env *Environment, c *client.Client, p *output.Printer, a []string) error {
			res, err := c.DeleteChannel(ctx, a[0])
			if err != nil {
				return err
			}
			env.Infof("Deleted channel %s.", a[0])
			return p.Print(res, &output.Table{Columns: []string{"deleted"}, Rows: [][]string{{a[0]}}})
		})
}

func notifyChannelTestCmd() *command {
	var wait time.Duration
	return simple("channel-test", "Send a test notification through a channel and report the outcome.",
		"nexusctl notify channel-test <name> [--wait 30s]", 1,
		func(fs *flag.FlagSet) {
			fs.DurationVar(&wait, "wait", 30*time.Second, "How long to wait for the outcome (0 = do not wait)")
		},
		func(ctx context.Context, env *Environment, c *client.Client, p *output.Printer, a []string) error {
			res, err := c.TestChannel(ctx, a[0])
			if err != nil {
				return err
			}
			id := output.Str(res.(map[string]any), "delivery_id")
			deadline := time.Now().Add(wait)
			for wait > 0 && time.Now().Before(deadline) {
				d, err := c.GetDelivery(ctx, id)
				if err != nil {
					return err
				}
				m := d.(map[string]any)
				if m["state"] != "pending" || output.Str(m, "last_error") != "-" {
					res = d
					if m["state"] == "pending" {
						env.Infof("The first attempt failed and will be retried; see: nexusctl notify deliveries --channel %s", a[0])
					}
					break
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
			}
			return p.Print(res, kv(res, "delivery_id", "id", "state", "attempts", "last_status", "last_error", "delivered_at"))
		})
}

func notifyRulesCmd() *command {
	return simple("rules", "List notification rules.", "nexusctl notify rules", 0, nil,
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, _ []string) error {
			res, err := c.ListRules(ctx)
			if err != nil {
				return err
			}
			t := &output.Table{Columns: []string{"name", "events", "workflows", "labels", "channels", "enabled"}, Empty: "No notification rules."}
			for _, x := range output.Items(res, "rules") {
				join := func(k string) string {
					arr, _ := x[k].([]any)
					var s []string
					for _, v := range arr {
						s = append(s, fmt.Sprint(v))
					}
					if len(s) == 0 {
						return "*"
					}
					return strings.Join(s, ",")
				}
				labels, _ := json.Marshal(x["labels"])
				t.Rows = append(t.Rows, []string{output.Str(x, "name"), join("events"), join("workflows"), string(labels),
					join("channels"), output.Str(x, "enabled")})
			}
			return p.Print(res, t)
		})
}

func notifyRuleCreateCmd() *command {
	var (
		events, channels, workflows, labels stringSlice
		sla                                 time.Duration
	)
	return simple("rule-create", "Create a notification rule.",
		`nexusctl notify rule-create <name> --event E [--event ...] --channel C [--channel ...]
                             [--workflow W ...] [--label k=v ...] [--sla 30m]

Events: `+strings.Join(notify.EventTypes, ", ")+`. --sla is required with run.sla_exceeded.`, 1,
		func(fs *flag.FlagSet) {
			fs.Var(&events, "event", "Event type (repeatable)")
			fs.Var(&channels, "channel", "Channel name (repeatable)")
			fs.Var(&workflows, "workflow", "Only these workflows (repeatable; default all)")
			fs.Var(&labels, "label", "Only workflows whose spec labels include k=v (repeatable)")
			fs.DurationVar(&sla, "sla", 0, "Overdue threshold for run.sla_exceeded")
		},
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, a []string) error {
			lab := map[string]string{}
			for _, l := range labels {
				k, v, ok := strings.Cut(l, "=")
				if !ok || k == "" {
					return usagef("--label %q is not k=v", l)
				}
				lab[k] = v
			}
			body := map[string]any{"name": a[0], "events": []string(events), "channels": []string(channels),
				"workflows": []string(workflows), "labels": lab}
			if sla > 0 {
				body["sla_seconds"] = int(sla.Seconds())
			}
			res, err := c.CreateRule(ctx, body)
			if err != nil {
				return err
			}
			return p.Print(res, kv(res, "name", "events", "workflows", "labels", "sla_seconds", "channels", "enabled"))
		})
}

func notifyRuleDeleteCmd() *command {
	return simple("rule-delete", "Delete a notification rule.", "nexusctl notify rule-delete <name>", 1, nil,
		func(ctx context.Context, env *Environment, c *client.Client, p *output.Printer, a []string) error {
			res, err := c.DeleteRule(ctx, a[0])
			if err != nil {
				return err
			}
			env.Infof("Deleted rule %s.", a[0])
			return p.Print(res, &output.Table{Columns: []string{"deleted"}, Rows: [][]string{{a[0]}}})
		})
}

func notifyDeliveriesCmd() *command {
	var state, channel, run string
	var limit int
	return simple("deliveries", "List notification deliveries (use --state dead for the dead-letter queue).",
		"nexusctl notify deliveries [--state pending|delivered|dead] [--channel C] [--run ID] [--limit N]", 0,
		func(fs *flag.FlagSet) {
			fs.StringVar(&state, "state", "", "pending, delivered or dead")
			fs.StringVar(&channel, "channel", "", "Only this channel")
			fs.StringVar(&run, "run", "", "Only this run")
			fs.IntVar(&limit, "limit", 50, "Maximum rows")
		},
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, _ []string) error {
			res, err := c.ListDeliveries(ctx, state, channel, run, limit)
			if err != nil {
				return err
			}
			t := &output.Table{Columns: []string{"id", "event", "channel", "state", "attempts", "run", "created", "last_error"}, Empty: "No deliveries."}
			for _, x := range output.Items(res, "deliveries") {
				t.Rows = append(t.Rows, []string{output.Str(x, "id"), output.Str(x, "event_type"), output.Str(x, "channel"),
					output.Str(x, "state"), output.Str(x, "attempts"), output.Str(x, "run_id"), output.Str(x, "created_at"),
					output.Str(x, "last_error")})
			}
			return p.Print(res, t)
		})
}

func notifyRedriveCmd() *command {
	return simple("redrive", "Re-send a dead-lettered delivery (keeps its dedup key).", "nexusctl notify redrive <delivery-id>", 1, nil,
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, a []string) error {
			res, err := c.RedriveDelivery(ctx, a[0])
			if err != nil {
				return err
			}
			return p.Print(res, kv(res, "delivery_id", "redrive_of", "state"))
		})
}

// ── asset ───────────────────────────────────────────────────────────────────

func assetGroup() *group {
	return &group{name: "asset", summary: "Query declared data-asset lineage.",
		commands: []*command{assetListCmd(), assetGetCmd(), assetGraphCmd()}}
}

func assetListCmd() *command {
	var search string
	return simple("list", "List assets.", "nexusctl asset list [--search TEXT]", 0,
		func(fs *flag.FlagSet) { fs.StringVar(&search, "search", "", "Substring of the URI or name") },
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, _ []string) error {
			res, err := c.ListAssets(ctx, search)
			if err != nil {
				return err
			}
			t := &output.Table{Columns: []string{"uri", "name", "producers", "consumers", "last_materialized"},
				Empty: "No assets recorded. Declare them with assets.produces/consumes on tasks (SDK: produces=, consumes=)."}
			for _, x := range output.Items(res, "assets") {
				t.Rows = append(t.Rows, []string{output.Str(x, "uri"), output.Str(x, "name"), output.Str(x, "producers"),
					output.Str(x, "consumers"), output.Str(x, "last_materialized_at")})
			}
			return p.Print(res, t)
		})
}

func assetGetCmd() *command {
	return simple("get", "Show an asset's producers, consumers and latest materialization.", "nexusctl asset get <uri>", 1, nil,
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, a []string) error {
			res, err := c.GetAsset(ctx, a[0])
			if err != nil {
				return err
			}
			return p.Print(res, kv(res, "uri", "name", "producer_tasks", "consumer_tasks", "latest_materialization", "last_declared_at"))
		})
}

func assetGraphCmd() *command {
	var uri string
	var depth int
	return simple("graph", "Print the lineage graph as edges.", "nexusctl asset graph [--uri U --depth N]", 0,
		func(fs *flag.FlagSet) {
			fs.StringVar(&uri, "uri", "", "Center the graph on this asset")
			fs.IntVar(&depth, "depth", 2, "Asset hops from --uri")
		},
		func(ctx context.Context, _ *Environment, c *client.Client, p *output.Printer, _ []string) error {
			res, err := c.AssetGraph(ctx, uri, depth)
			if err != nil {
				return err
			}
			t := &output.Table{Columns: []string{"from", "to"}, Empty: "No lineage recorded."}
			for _, x := range output.Items(res, "edges") {
				t.Rows = append(t.Rows, []string{output.Str(x, "from"), output.Str(x, "to")})
			}
			return p.Print(res, t)
		})
}
