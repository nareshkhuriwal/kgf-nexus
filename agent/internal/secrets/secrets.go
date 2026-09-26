// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package secrets resolves a task's SecretRefs on the worker, at attempt start
// (ADR-0025).
//
// The control plane stores and ships references only (`vault://kv/ml/s3`);
// the VALUE is fetched here, by the agent, with the agent's own per-tenant
// provider credentials, and handed to the executor for injection. Values are
// held in memory for the attempt and never written to the WAL, the result
// store, a report or an agent log line: Secret formats itself as [REDACTED]
// for every printing path, and errors carry the name and the ref, never a
// value or a provider response body.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Inject is how a resolved value reaches the workload.
type Inject string

const (
	// InjectEnv sets the environment variable Secret.Name. The default.
	InjectEnv Inject = "env"
	// InjectFile writes $NEXUS_SECRETS_DIR/<Name> (0400) and keeps the value
	// out of the environment.
	InjectFile Inject = "file"
)

// EnvSecretsDir is the workload variable naming the directory of file secrets.
const EnvSecretsDir = "NEXUS_SECRETS_DIR"

// MaxValueBytes bounds one resolved value. A secret is a credential, not a
// payload; a 50 MiB "secret" is a misconfigured ref, and holding it in agent
// memory (and a Kubernetes Secret, which caps at 1 MiB total) helps nobody.
const MaxValueBytes = 512 << 10

// MaxSecretsPerTask bounds the fan-out of one attempt's resolution.
const MaxSecretsPerTask = 64

// Secret is one resolved value. It deliberately prints as [REDACTED] through
// fmt, slog and String, so a careless log line or %v cannot leak it.
type Secret struct {
	Name   string
	Ref    string
	Inject Inject
	Value  []byte
}

// String implements fmt.Stringer without the value.
func (s Secret) String() string {
	return fmt.Sprintf("secret(%s from %s as %s: [REDACTED])", s.Name, s.Ref, s.Inject)
}

// GoString implements fmt.GoStringer (%#v) without the value.
func (s Secret) GoString() string { return s.String() }

// Format implements fmt.Formatter so every verb (%v, %+v, %s, %q, %x) goes
// through String: %x on a struct would otherwise hex-dump the value bytes.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.String())) }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value {
	return slog.GroupValue(slog.String("name", s.Name), slog.String("ref", s.Ref),
		slog.String("inject", string(s.Inject)), slog.String("value", "[REDACTED]"))
}

// Values returns name -> value for the log redactor (logship.NewRedactor).
func Values(ss []Secret) map[string]string {
	if len(ss) == 0 {
		return nil
	}
	out := make(map[string]string, len(ss))
	for _, s := range ss {
		out[s.Name] = string(s.Value)
	}
	return out
}

// Wipe overwrites the value bytes. Best effort: Go may have copied them (a
// string conversion for env injection, the redactor's index), so this narrows
// the window rather than guaranteeing erasure.
func Wipe(ss []Secret) {
	for i := range ss {
		for j := range ss[i].Value {
			ss[i].Value[j] = 0
		}
	}
}

// ── Errors ────────────────────────────────────────────────────────────────

// Error is a classified resolution failure. It never contains a value.
type Error struct {
	Class     nexusv1.ErrorClass
	Retryable bool
	Name      string // the SecretRef name, when known
	Ref       string // the SecretRef ref, when known
	Msg       string
	Err       error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("secret")
	if e.Name != "" {
		fmt.Fprintf(&b, " %q", e.Name)
	}
	if e.Ref != "" {
		fmt.Fprintf(&b, " (%s)", e.Ref)
	}
	b.WriteString(": ")
	b.WriteString(e.Msg)
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Constructors for the four classes ADR-0025 names. A provider returns one of
// these; the resolver fills in Name and Ref.

// Invalid is a malformed reference or declaration. Terminal.
func Invalid(format string, a ...any) *Error {
	return &Error{Class: nexusv1.ErrorClass_ERROR_CLASS_VALIDATION, Msg: fmt.Sprintf(format, a...)}
}

// Denied is a policy or provider authorization refusal. Terminal: retrying
// the same credentials against the same policy cannot succeed.
func Denied(format string, a ...any) *Error {
	return &Error{Class: nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED, Msg: fmt.Sprintf(format, a...)}
}

// NotFound is an absent secret, field or version. Terminal.
func NotFound(format string, a ...any) *Error {
	return &Error{Class: nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND, Msg: fmt.Sprintf(format, a...)}
}

// Transient is an unreachable, throttled, sealed or failing provider.
// Retryable: another attempt (possibly on another worker) may well succeed.
func Transient(err error, format string, a ...any) *Error {
	return &Error{Class: nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT, Retryable: true,
		Msg: fmt.Sprintf(format, a...), Err: err}
}

// ── Resolver ──────────────────────────────────────────────────────────────

// Provider resolves one scheme's references.
type Provider interface {
	// Scheme is the URI scheme this provider answers, without "://".
	Scheme() string
	// Resolve returns the value. Errors SHOULD be *Error; anything else is
	// treated as Transient.
	Resolve(ctx context.Context, ref Ref) ([]byte, error)
}

// Resolver dispatches references to the tenant's configured providers.
//
// A nil *Resolver is valid and has no providers: every reference then fails
// PERMISSION_DENIED, which is the right answer for an agent whose operator
// configured no secret backend -- a task that declared a secret must not run
// without it.
type Resolver struct {
	providers map[string]Provider
	// Timeout bounds the whole resolution of one attempt.
	Timeout time.Duration
}

// DefaultTimeout bounds one attempt's resolution when unconfigured.
const DefaultTimeout = 30 * time.Second

// NewResolver builds a resolver over providers (nil entries are skipped).
func NewResolver(timeout time.Duration, providers ...Provider) *Resolver {
	r := &Resolver{providers: map[string]Provider{}, Timeout: timeout}
	for _, p := range providers {
		if p != nil {
			r.providers[p.Scheme()] = p
		}
	}
	return r
}

// Schemes lists the configured schemes, for the start-up log line.
func (r *Resolver) Schemes() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.providers))
	for s := range r.providers {
		out = append(out, s)
	}
	return out
}

var (
	// envName is a POSIX-portable environment variable name.
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,254}$`)
	// fileName is a single path component with no leading dot, so a secret
	// can never be named "..", ".hidden" or "a/b".
	fileName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,254}$`)
)

// Validate checks the declarations without contacting any provider: names,
// injection modes, duplicates and ref syntax. It is also what the runner
// calls first, so a malformed spec fails VALIDATION even on an agent with no
// providers configured.
func Validate(refs []*nexusv1.SecretRef) error {
	if len(refs) > MaxSecretsPerTask {
		return Invalid("a task may declare at most %d secrets, this one declares %d", MaxSecretsPerTask, len(refs))
	}
	seen := make(map[string]bool, len(refs))
	for _, sr := range refs {
		name, raw := sr.GetName(), sr.GetRef()
		wrap := func(e *Error) *Error { e.Name, e.Ref = name, raw; return e }
		inj, err := parseInject(sr.GetInject())
		if err != nil {
			return wrap(err)
		}
		switch inj {
		case InjectEnv:
			if !envName.MatchString(name) {
				return wrap(Invalid("name is not a valid environment variable name"))
			}
		case InjectFile:
			if !fileName.MatchString(name) {
				return wrap(Invalid("name is not a valid file name (letters, digits, '_', '.', '-'; no leading dot)"))
			}
		}
		// The platform variables are the workload's view of its own identity
		// (idempotency key, output paths); a secret must not be able to shadow
		// them, and NEXUS_SECRETS_DIR in particular.
		if strings.HasPrefix(strings.ToUpper(name), "NEXUS_") {
			return wrap(Invalid("names starting with NEXUS_ are reserved for platform variables"))
		}
		key := string(inj) + ":" + name
		if seen[key] {
			return wrap(Invalid("declared more than once"))
		}
		seen[key] = true
		if _, err := ParseRef(raw); err != nil {
			pe := Invalid("ref is malformed")
			errors.As(err, &pe)
			return wrap(pe)
		}
	}
	return nil
}

func parseInject(s string) (Inject, *Error) {
	switch s {
	case "", string(InjectEnv):
		return InjectEnv, nil
	case string(InjectFile):
		return InjectFile, nil
	default:
		return "", Invalid("inject must be \"env\" or \"file\", got %q", s)
	}
}

// Resolve validates and resolves every reference, in declaration order. It
// fails fast on the first error: a task either gets all its secrets or does
// not start.
func (r *Resolver) Resolve(ctx context.Context, refs []*nexusv1.SecretRef) ([]Secret, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if err := Validate(refs); err != nil {
		return nil, err
	}
	timeout := DefaultTimeout
	if r != nil && r.Timeout > 0 {
		timeout = r.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := make([]Secret, 0, len(refs))
	fail := func(err error) ([]Secret, error) {
		Wipe(out)
		return nil, err
	}
	for _, sr := range refs {
		name, raw := sr.GetName(), sr.GetRef()
		inj, _ := parseInject(sr.GetInject())
		ref, _ := ParseRef(raw) // validated above
		var p Provider
		if r != nil {
			p = r.providers[ref.Scheme]
		}
		if p == nil {
			return fail(&Error{Class: nexusv1.ErrorClass_ERROR_CLASS_PERMISSION_DENIED, Name: name, Ref: raw,
				Msg: fmt.Sprintf("no %q provider is configured for this worker's tenant", ref.Scheme)})
		}
		v, err := p.Resolve(ctx, ref)
		if err != nil {
			return fail(classify(ctx, err, name, raw))
		}
		if len(v) > MaxValueBytes {
			return fail(&Error{Class: nexusv1.ErrorClass_ERROR_CLASS_VALIDATION, Name: name, Ref: raw,
				Msg: fmt.Sprintf("value is %d bytes, over the %d-byte limit for a secret", len(v), MaxValueBytes)})
		}
		out = append(out, Secret{Name: name, Ref: raw, Inject: inj, Value: v})
	}
	return out, nil
}

// classify turns a provider error into a named, classified *Error.
func classify(ctx context.Context, err error, name, raw string) *Error {
	var se *Error
	if errors.As(err, &se) {
		c := *se
		c.Name, c.Ref = name, raw
		return &c
	}
	if ctx.Err() != nil {
		return &Error{Class: nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT, Retryable: true, Name: name, Ref: raw,
			Msg: "resolution timed out", Err: err}
	}
	return &Error{Class: nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT, Retryable: true, Name: name, Ref: raw,
		Msg: "provider error", Err: err}
}
