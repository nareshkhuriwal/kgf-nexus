// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package notifier

import (
	"context"
	"errors"
	"fmt"

	"github.com/khuriwalgroup/kgf-nexus/internal/mail"
)

// MailAdapter lets internal/mail satisfy notify.MailSender. One message per
// recipient, because internal/mail sends to a single address: a bad address
// then fails alone instead of failing the whole list.
type MailAdapter struct{ S mail.Sender }

// SendMail sends to every recipient; a permanent failure for any recipient is
// reported after trying the rest.
func (a MailAdapter) SendMail(ctx context.Context, to []string, subject, text string) error {
	if mail.IsDisabled(a.S) {
		return mail.ErrDisabled
	}
	var errs []error
	for _, addr := range to {
		if err := a.S.Send(ctx, mail.Message{To: addr, Subject: subject, Text: text}); err != nil {
			if !mail.IsPermanent(err) {
				// A transient failure retries the whole delivery; recipients that
				// already got it may receive it twice (stated in ADR-0028).
				return fmt.Errorf("to %s: %w", addr, err)
			}
			errs = append(errs, fmt.Errorf("to %s: %w", addr, err))
		}
	}
	if len(errs) > 0 {
		return mail.Permanent(errors.Join(errs...))
	}
	return nil
}

// IsPermanent classifies a mail error.
func (MailAdapter) IsPermanent(err error) bool { return mail.IsPermanent(err) }
