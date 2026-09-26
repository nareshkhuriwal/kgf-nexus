// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/logship"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/secrets"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// resolveSecrets validates and resolves the attempt's declared secrets.
//
// Declarations are validated first, with or without providers, so a
// malformed spec fails VALIDATION everywhere rather than PERMISSION_DENIED on
// an unconfigured agent. A secret may not share a name with a runtime.env
// entry: which one "wins" would be an executor detail an author cannot see.
func (r *Runner) resolveSecrets(ctx context.Context, a *nexusv1.TaskAssignment) ([]secrets.Secret, error) {
	refs := a.GetSpec().GetRuntime().GetSecrets()
	if len(refs) == 0 {
		return nil, nil
	}
	if err := secrets.Validate(refs); err != nil {
		return nil, err
	}
	env := a.GetSpec().GetRuntime().GetEnv()
	for _, s := range refs {
		if s.GetInject() != string(secrets.InjectFile) {
			if _, clash := env[s.GetName()]; clash {
				e := secrets.Invalid("name is also set in runtime.env; remove one of them")
				e.Name, e.Ref = s.GetName(), s.GetRef()
				return nil, e
			}
		}
	}
	return r.Secrets.Resolve(ctx, refs)
}

// secretFailure classifies a resolution error for ReportResult. The message
// names the secret and its ref (both already in the stored spec), never a
// value.
func secretFailure(err error) executor.Classified {
	var se *secrets.Error
	if errors.As(err, &se) {
		return executor.Classified{
			Class: se.Class, Type: "SecretResolutionFailed",
			Message: se.Error(), Retryable: se.Retryable,
		}
	}
	return executor.Classified{
		Class: nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT, Type: "SecretResolutionFailed",
		Message: fmt.Sprintf("secret resolution failed: %v", err), Retryable: true,
	}
}

// buildRedactor indexes the attempt's resolved values (plus any operator-
// supplied extras from SecretValues) for the ADR-0020 redactor. A value too
// short to redact safely is reported by NAME, so an operator knows that
// secret is not protected in logs.
func (r *Runner) buildRedactor(a *nexusv1.TaskAssignment, resolved []secrets.Secret, log *slog.Logger) *logship.Redactor {
	values := secrets.Values(resolved)
	if r.SecretValues != nil {
		for k, v := range r.SecretValues(a) {
			if values == nil {
				values = map[string]string{}
			}
			if _, dup := values[k]; !dup {
				values[k] = v
			}
		}
	}
	red := logship.NewRedactor(values)
	if skipped := red.Skipped(); len(skipped) > 0 {
		log.Warn("some secret values are too short to redact from logs", "secrets", skipped,
			"min_length", logship.MinRedactableLength)
	}
	return red
}

// redactAttempt applies the running attempt's redactor, if any, to s.
func (r *Runner) redactAttempt(attemptID, s string) string {
	r.mu.Lock()
	rt := r.running[attemptID]
	r.mu.Unlock()
	if rt == nil {
		return s
	}
	return redactString(rt.redactor, s)
}

func redactString(red *logship.Redactor, s string) string {
	out, _ := red.Apply(s)
	return out
}
