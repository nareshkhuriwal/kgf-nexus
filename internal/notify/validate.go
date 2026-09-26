// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"fmt"
	"net/mail"
	"slices"
	"strings"
)

// SecretRequirement says what a kind needs as its secret.
type SecretRequirement int

const (
	SecretNone SecretRequirement = iota
	SecretRequired
	// SecretGenerated: optional; the gateway generates one when absent (the
	// generic webhook's signing key, so every delivery is signed).
	SecretGenerated
)

// SecretFor reports what a kind's secret is and whether it is required.
func SecretFor(kind string) (SecretRequirement, string) {
	switch kind {
	case KindSlackWebhook:
		return SecretRequired, "the Slack incoming-webhook URL"
	case KindSlackBot:
		return SecretRequired, "the Slack bot token (xoxb-…)"
	case KindTeams:
		return SecretRequired, "the Teams Workflows webhook URL"
	case KindPagerDuty:
		return SecretRequired, "the PagerDuty Events API v2 routing key"
	case KindWebhook:
		return SecretGenerated, "the HMAC signing key (generated when omitted)"
	}
	return SecretNone, ""
}

// ValidateChannel checks a channel definition. inlineSecret is the plaintext
// secret when one was supplied inline (nil when it is a reference or absent);
// hasSecret says whether any secret (inline or reference) will be configured.
func ValidateChannel(kind string, config map[string]any, inlineSecret []byte, hasSecret, allowPrivate bool) error {
	if !slices.Contains(Kinds, kind) {
		return fmt.Errorf("kind must be one of %s", strings.Join(Kinds, ", "))
	}
	req, what := SecretFor(kind)
	switch req {
	case SecretRequired:
		if !hasSecret {
			return fmt.Errorf("a %s channel needs a secret: %s (inline as \"secret\" or as \"secret_ref\")", kind, what)
		}
	case SecretNone:
		if hasSecret {
			return fmt.Errorf("a %s channel takes no secret", kind)
		}
	}
	allowed := map[string][]string{
		KindSlackWebhook: {},
		KindSlackBot:     {"channel"},
		KindTeams:        {},
		KindPagerDuty:    {"severity"},
		KindWebhook:      {"url"},
		KindEmail:        {"to"},
	}[kind]
	for k := range config {
		if !slices.Contains(allowed, k) {
			return fmt.Errorf("config.%s is not a setting of a %s channel (allowed: %v); secrets never go in config", k, kind, allowed)
		}
	}
	switch kind {
	case KindSlackWebhook, KindTeams:
		if inlineSecret != nil {
			if err := ValidateURL(string(inlineSecret), allowPrivate); err != nil {
				return fmt.Errorf("secret (webhook URL): %w", err)
			}
		}
	case KindSlackBot:
		if cfgString(config, "channel") == "" {
			return fmt.Errorf("config.channel (a Slack channel id such as C0123ABC) is required")
		}
	case KindPagerDuty:
		if sev := cfgString(config, "severity"); sev != "" && !slices.Contains(PagerDutySeverities, sev) {
			return fmt.Errorf("config.severity must be one of %v", PagerDutySeverities)
		}
	case KindWebhook:
		u := cfgString(config, "url")
		if u == "" {
			return fmt.Errorf("config.url is required")
		}
		if err := ValidateURL(u, allowPrivate); err != nil {
			return fmt.Errorf("config.url: %w", err)
		}
	case KindEmail:
		to := cfgStrings(config, "to")
		if len(to) == 0 || len(to) > 20 {
			return fmt.Errorf("config.to must list 1 to 20 email addresses")
		}
		for _, a := range to {
			if addr, err := mail.ParseAddress(a); err != nil || addr.Address != a {
				return fmt.Errorf("config.to: %q is not a bare email address", a)
			}
		}
	}
	return nil
}
