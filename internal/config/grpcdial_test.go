package config

import (
	"testing"
	"time"
)

func TestPeerTargetSelectsTheDNSResolver(t *testing.T) {
	cases := map[string]string{
		// The common case: a bare host:port uses the passthrough resolver, which
		// never re-reads the name, so each reconnect would dial the same stale
		// address forever.
		"scheduler:9090": "dns:///scheduler:9090",
		"10.0.0.4:9090":  "dns:///10.0.0.4:9090",
		// An explicit scheme is the operator's choice and must survive untouched.
		"dns:///scheduler:9090":    "dns:///scheduler:9090",
		"unix:///var/run/nexus.sk": "unix:///var/run/nexus.sk",
		// Unset stays unset: every caller treats "" as "peer not configured" and
		// reports that explicitly rather than dialing something meaningless.
		"": "",
	}
	for in, want := range cases {
		if got := PeerTarget(in); got != want {
			t.Errorf("PeerTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPeerOptionsArePresent guards against the options being dropped in a
// refactor. They are the only thing that recovers a client whose connection is
// healthy but pointed at the wrong service after an address swap, and losing them
// would reintroduce a failure that reads like a version skew.
func TestPeerOptionsArePresent(t *testing.T) {
	if len(PeerDialOptions()) == 0 {
		t.Error("PeerDialOptions is empty; reconnect-after-swap would fall back to the slow default backoff")
	}
	if len(PeerServerOptions()) == 0 {
		t.Error("PeerServerOptions is empty; connections would never rotate and a wrong-peer connection would never be recovered")
	}
}

// TestMaxPeerConnectionAgeIsBounded pins the recovery window. The constant is the
// worst case for how long a client can keep talking to the wrong service, so a
// large value silently turns a self-healing deploy into a manual restart.
func TestMaxPeerConnectionAgeIsBounded(t *testing.T) {
	if MaxPeerConnectionAge > time.Minute {
		t.Errorf("MaxPeerConnectionAge = %s; a bad deploy would take that long to "+
			"self-heal, which is long enough that an operator restarts by hand instead",
			MaxPeerConnectionAge)
	}
	// Also guard the other direction: churning connections every second would
	// spend real CPU on TLS/HTTP2 handshakes for no benefit.
	if MaxPeerConnectionAge < 10*time.Second {
		t.Errorf("MaxPeerConnectionAge = %s; that rotates connections faster than "+
			"any address change needs and just burns handshakes", MaxPeerConnectionAge)
	}
}
