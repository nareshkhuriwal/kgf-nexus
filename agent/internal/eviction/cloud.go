// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package eviction

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	defaultAWSBaseURL   = "http://169.254.169.254"
	defaultGCPBaseURL   = "http://metadata.google.internal"
	defaultAzureBaseURL = "http://169.254.169.254"

	// awsTokenTTL is the IMDSv2 session token lifetime requested (the
	// maximum, six hours). The token is refreshed a minute early so a poll
	// never races its expiry.
	awsTokenTTL = 6 * time.Hour

	// gcpPreemptLead is the deadline assumed for a GCP preemption: the
	// documented ACPI G2 soft-off window is 30 s, and the metadata flag
	// flips at its start.
	gcpPreemptLead = 30 * time.Second

	// maxMetadataBody bounds every metadata response read. The real
	// documents are a few hundred bytes.
	maxMetadataBody = 64 << 10
)

// metadataClient is the default HTTP client for instance metadata services.
// Two properties matter: a short timeout, because a metadata service that
// does not answer within seconds is not going to deliver a useful notice, and
// NO proxy, because HTTP(S)_PROXY is commonly set on GPU nodes for egress and
// a link-local metadata request routed through a corporate proxy either fails
// or, worse, reaches some other machine's metadata.
var metadataClient = &http.Client{
	Timeout: 3 * time.Second,
	Transport: &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		MaxIdleConns:        4,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
		TLSHandshakeTimeout: 2 * time.Second,
	},
}

func clientOr(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return metadataClient
}

func baseOr(u, def string) string {
	if u == "" {
		u = def
	}
	return strings.TrimRight(u, "/")
}

// doMetadata performs one request and returns the status and a bounded body.
func doMetadata(ctx context.Context, c *http.Client, method, url string, hdr map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBody))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// ---------------------------------------------------------------------------
// AWS

// AWSSource polls the EC2 spot instance-action document through IMDSv2.
//
// IMDSv2 only: IMDSv1 is disabled on hardened instances and the token flow
// works everywhere IMDS works at all. In a container, IMDSv2 needs the
// instance's metadata hop limit to be at least 2 (the PUT's response is
// otherwise dropped one hop short); the source then just keeps retrying, and
// Merge still has the kubernetes source (aws-node-termination-handler taints)
// to fall back on.
type AWSSource struct {
	BaseURL string
	Client  *http.Client
	Poll    time.Duration

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// Name implements Source.
func (s *AWSSource) Name() string { return SourceAWS }

// Watch implements Source.
func (s *AWSSource) Watch(ctx context.Context, out chan<- Notice) error {
	return pollLoop(ctx, s.Poll, out, s.check)
}

type awsInstanceAction struct {
	Action string `json:"action"`
	Time   string `json:"time"`
}

func (s *AWSSource) check(ctx context.Context) (Notice, bool, error) {
	base := baseOr(s.BaseURL, defaultAWSBaseURL)
	c := clientOr(s.Client)
	tok, err := s.sessionToken(ctx, c, base)
	if err != nil {
		return Notice{}, false, err
	}
	status, body, err := doMetadata(ctx, c, http.MethodGet, base+"/latest/meta-data/spot/instance-action",
		map[string]string{"X-aws-ec2-metadata-token": tok})
	if err != nil {
		return Notice{}, false, fmt.Errorf("aws instance-action: %w", err)
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return Notice{}, false, nil // no interruption scheduled
	case http.StatusUnauthorized:
		// The token was revoked or expired early; fetch a new one next poll.
		s.mu.Lock()
		s.token = ""
		s.mu.Unlock()
		return Notice{}, false, fmt.Errorf("aws instance-action: token rejected (401)")
	default:
		return Notice{}, false, fmt.Errorf("aws instance-action: unexpected status %d", status)
	}

	// The document only exists once an interruption is scheduled, so a 200
	// is the signal even if its body cannot be parsed; parsing only refines
	// the deadline and the reason.
	n := Notice{Source: SourceAWS, Reason: "aws spot instance-action"}
	var ia awsInstanceAction
	if json.Unmarshal(body, &ia) == nil {
		if ia.Action != "" {
			n.Reason = "aws spot instance-action: " + ia.Action
		}
		if t, perr := time.Parse(time.RFC3339, ia.Time); perr == nil {
			n.Deadline = t
		}
	}
	return n, true, nil
}

func (s *AWSSource) sessionToken(ctx context.Context, c *http.Client, base string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.tokenExp) {
		return s.token, nil
	}
	status, body, err := doMetadata(ctx, c, http.MethodPut, base+"/latest/api/token",
		map[string]string{"X-aws-ec2-metadata-token-ttl-seconds": fmt.Sprint(int(awsTokenTTL / time.Second))})
	if err != nil {
		return "", fmt.Errorf("aws imdsv2 token: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("aws imdsv2 token: unexpected status %d", status)
	}
	tok := strings.TrimSpace(string(body))
	if tok == "" {
		return "", fmt.Errorf("aws imdsv2 token: empty token")
	}
	s.token = tok
	s.tokenExp = time.Now().Add(awsTokenTTL - time.Minute)
	return tok, nil
}

// ---------------------------------------------------------------------------
// GCP

// GCPSource polls the GCE `instance/preempted` metadata flag, which reads
// "TRUE" once the VM has been told it is being preempted.
//
// It polls instead of using `wait_for_change=true`: a hanging GET needs a
// client without the short metadata timeout, and a missed change during a
// reconnect would be silent, whereas a poll is simple and loses at most one
// interval.
type GCPSource struct {
	BaseURL string
	Client  *http.Client
	Poll    time.Duration

	// now is overridable by tests; nil means time.Now.
	now func() time.Time
}

// Name implements Source.
func (s *GCPSource) Name() string { return SourceGCP }

// Watch implements Source.
func (s *GCPSource) Watch(ctx context.Context, out chan<- Notice) error {
	return pollLoop(ctx, s.Poll, out, s.check)
}

func (s *GCPSource) check(ctx context.Context) (Notice, bool, error) {
	url := baseOr(s.BaseURL, defaultGCPBaseURL) + "/computeMetadata/v1/instance/preempted"
	status, body, err := doMetadata(ctx, clientOr(s.Client), http.MethodGet, url,
		map[string]string{"Metadata-Flavor": "Google"})
	if err != nil {
		return Notice{}, false, fmt.Errorf("gcp preempted: %w", err)
	}
	if status != http.StatusOK {
		return Notice{}, false, fmt.Errorf("gcp preempted: unexpected status %d", status)
	}
	if !strings.EqualFold(strings.TrimSpace(string(body)), "TRUE") {
		return Notice{}, false, nil
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	return Notice{
		Source:   SourceGCP,
		Reason:   "gcp instance preempted",
		Deadline: now().Add(gcpPreemptLead),
	}, true, nil
}

// ---------------------------------------------------------------------------
// Azure

// AzureSource polls Azure Scheduled Events for a Preempt (spot eviction) or
// Terminate event. Reboot, Redeploy and Freeze do not remove the node, so they
// are not evictions: a Freeze pauses the VM for seconds, and draining for it
// would throw away work that would have survived.
//
// Note that the first Scheduled Events call on a VM enables the feature, and
// Azure may take a few minutes to start delivering events after that, which
// is a reason to start this source at agent start-up rather than lazily.
type AzureSource struct {
	BaseURL string
	Client  *http.Client
	Poll    time.Duration
	// InstanceName, when set, restricts notices to events whose Resources
	// list names this instance. Scheduled Events are per availability set /
	// scale set, so without it a neighbour's eviction would drain this node.
	InstanceName string
}

// Name implements Source.
func (s *AzureSource) Name() string { return SourceAzure }

// Watch implements Source.
func (s *AzureSource) Watch(ctx context.Context, out chan<- Notice) error {
	return pollLoop(ctx, s.Poll, out, s.check)
}

type azureScheduledEvents struct {
	Events []azureEvent `json:"Events"`
}

type azureEvent struct {
	EventID   string   `json:"EventId"`
	EventType string   `json:"EventType"`
	Resources []string `json:"Resources"`
	NotBefore string   `json:"NotBefore"`
}

func (s *AzureSource) check(ctx context.Context) (Notice, bool, error) {
	url := baseOr(s.BaseURL, defaultAzureBaseURL) + "/metadata/scheduledevents?api-version=2020-07-01"
	status, body, err := doMetadata(ctx, clientOr(s.Client), http.MethodGet, url,
		map[string]string{"Metadata": "true"})
	if err != nil {
		return Notice{}, false, fmt.Errorf("azure scheduled events: %w", err)
	}
	if status != http.StatusOK {
		return Notice{}, false, fmt.Errorf("azure scheduled events: unexpected status %d", status)
	}
	var doc azureScheduledEvents
	if err := json.Unmarshal(body, &doc); err != nil {
		return Notice{}, false, fmt.Errorf("azure scheduled events: decode: %w", err)
	}
	for _, ev := range doc.Events {
		if ev.EventType != "Preempt" && ev.EventType != "Terminate" {
			continue
		}
		if s.InstanceName != "" && !slices.ContainsFunc(ev.Resources, func(r string) bool {
			return strings.EqualFold(r, s.InstanceName)
		}) {
			continue
		}
		n := Notice{
			Source: SourceAzure,
			Reason: fmt.Sprintf("azure scheduled event %s %s", ev.EventType, ev.EventID),
		}
		// NotBefore is empty once the event has started; then the
		// deadline is unknown (and imminent).
		if ev.NotBefore != "" {
			if t, perr := time.Parse(time.RFC1123, ev.NotBefore); perr == nil {
				n.Deadline = t
			}
		}
		return n, true, nil
	}
	return Notice{}, false, nil
}
