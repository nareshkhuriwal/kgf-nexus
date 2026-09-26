// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package eviction

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testPoll = 10 * time.Millisecond

// runWatch starts s.Watch and returns the notice channel and the Watch
// result channel.
func runWatch(t *testing.T, ctx context.Context, s Source) (<-chan Notice, <-chan error) {
	t.Helper()
	out := make(chan Notice, 1)
	done := make(chan error, 1)
	go func() { done <- s.Watch(ctx, out) }()
	return out, done
}

func waitNotice(t *testing.T, ch <-chan Notice) Notice {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("no notice within 5s")
		return Notice{}
	}
}

func expectNoNotice(t *testing.T, ch <-chan Notice, d time.Duration) {
	t.Helper()
	select {
	case n := <-ch:
		t.Fatalf("unexpected notice %+v", n)
	case <-time.After(d):
	}
}

func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return within 5s")
		return nil
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestFileSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evict")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, done := runWatch(t, ctx, &FileSource{Path: path, Poll: testPoll})
	expectNoNotice(t, out, 5*testPoll)

	if err := os.WriteFile(path, []byte("  2026-09-26T12:00:00Z  \nignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := waitNotice(t, out)
	if n.Source != SourceFile || n.Reason != "file "+path {
		t.Fatalf("notice = %+v", n)
	}
	if want := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC); !n.Deadline.Equal(want) {
		t.Fatalf("deadline = %v, want %v", n.Deadline, want)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatalf("Watch err = %v", err)
	}
}

func TestFileSourceEmptyAndBadDeadline(t *testing.T) {
	for _, body := range []string{"", "not a time\n"} {
		path := filepath.Join(t.TempDir(), "evict")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out, done := runWatch(t, context.Background(), &FileSource{Path: path, Poll: testPoll})
		n := waitNotice(t, out)
		if !n.Deadline.IsZero() {
			t.Fatalf("body %q: deadline = %v, want zero", body, n.Deadline)
		}
		if err := waitDone(t, done); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFileSourceStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	out, done := runWatch(t, ctx, &FileSource{Path: filepath.Join(t.TempDir(), "x"), Poll: testPoll})
	time.Sleep(3 * testPoll)
	cancel()
	if err := waitDone(t, done); err != nil {
		t.Fatalf("cancel should not be an error, got %v", err)
	}
	expectNoNotice(t, out, 0)
}

func TestAWSSource(t *testing.T) {
	var polls atomic.Int32
	var tokens atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/latest/api/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") != "21600" {
			http.Error(w, "ttl", http.StatusBadRequest)
			return
		}
		tokens.Add(1)
		io.WriteString(w, "tok-123")
	})
	mux.HandleFunc("/latest/meta-data/spot/instance-action", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-aws-ec2-metadata-token") != "tok-123" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if polls.Add(1) < 3 {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"action":"terminate","time":"2026-09-26T12:02:00Z"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out, done := runWatch(t, context.Background(), &AWSSource{BaseURL: srv.URL, Client: srv.Client(), Poll: testPoll})
	n := waitNotice(t, out)
	if n.Source != SourceAWS || !strings.Contains(n.Reason, "terminate") {
		t.Fatalf("notice = %+v", n)
	}
	if want := time.Date(2026, 9, 26, 12, 2, 0, 0, time.UTC); !n.Deadline.Equal(want) {
		t.Fatalf("deadline = %v, want %v", n.Deadline, want)
	}
	if got := polls.Load(); got != 3 {
		t.Fatalf("polls = %d, want 3 (two 404s then the action)", got)
	}
	if got := tokens.Load(); got != 1 {
		t.Fatalf("token fetched %d times, want 1 (cached)", got)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestAWSSourceTransientErrorsDoNotNotify(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	out, done := runWatch(t, ctx, &AWSSource{BaseURL: srv.URL, Client: srv.Client(), Poll: testPoll})
	expectNoNotice(t, out, 10*testPoll)
	cancel()
	if err := waitDone(t, done); err != nil {
		t.Fatalf("transient errors should end as nil on cancel, got %v", err)
	}
}

func TestGCPSource(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/computeMetadata/v1/instance/preempted" || r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "bad request", http.StatusForbidden)
			return
		}
		if polls.Add(1) < 3 {
			io.WriteString(w, "FALSE")
			return
		}
		io.WriteString(w, "TRUE")
	}))
	defer srv.Close()

	fixed := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	s := &GCPSource{BaseURL: srv.URL, Client: srv.Client(), Poll: testPoll, now: func() time.Time { return fixed }}
	out, done := runWatch(t, context.Background(), s)
	n := waitNotice(t, out)
	if n.Source != SourceGCP {
		t.Fatalf("notice = %+v", n)
	}
	if want := fixed.Add(30 * time.Second); !n.Deadline.Equal(want) {
		t.Fatalf("deadline = %v, want %v", n.Deadline, want)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestAzureSource(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metadata/scheduledevents" || r.URL.Query().Get("api-version") != "2020-07-01" ||
			r.Header.Get("Metadata") != "true" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch polls.Add(1) {
		case 1:
			io.WriteString(w, `{"DocumentIncarnation":1,"Events":[]}`)
		case 2:
			// A Reboot for us and a Preempt for a neighbour: neither is ours.
			io.WriteString(w, `{"Events":[
				{"EventId":"r1","EventType":"Reboot","Resources":["vm-a"],"NotBefore":"Mon, 19 Sep 2016 18:29:47 GMT"},
				{"EventId":"p0","EventType":"Preempt","Resources":["vm-b"],"NotBefore":"Mon, 19 Sep 2016 18:29:47 GMT"}]}`)
		default:
			io.WriteString(w, `{"Events":[
				{"EventId":"r1","EventType":"Reboot","Resources":["vm-a"],"NotBefore":"Mon, 19 Sep 2016 18:29:47 GMT"},
				{"EventId":"p1","EventType":"Preempt","Resources":["vm-a"],"NotBefore":"Mon, 19 Sep 2016 18:30:17 GMT"}]}`)
		}
	}))
	defer srv.Close()

	out, done := runWatch(t, context.Background(),
		&AzureSource{BaseURL: srv.URL, Client: srv.Client(), Poll: testPoll, InstanceName: "vm-a"})
	n := waitNotice(t, out)
	if n.Source != SourceAzure || !strings.Contains(n.Reason, "Preempt") || !strings.Contains(n.Reason, "p1") {
		t.Fatalf("notice = %+v", n)
	}
	if want := time.Date(2016, 9, 19, 18, 30, 17, 0, time.UTC); !n.Deadline.Equal(want) {
		t.Fatalf("deadline = %v, want %v", n.Deadline, want)
	}
	if got := polls.Load(); got != 3 {
		t.Fatalf("polls = %d, want 3", got)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestAzureSourceIgnoresNonEvictionEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Events":[{"EventId":"f","EventType":"Freeze","Resources":["vm-a"],"NotBefore":""},
			{"EventId":"r","EventType":"Reboot","Resources":["vm-a"],"NotBefore":""}]}`)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	out, done := runWatch(t, ctx, &AzureSource{BaseURL: srv.URL, Client: srv.Client(), Poll: testPoll})
	expectNoNotice(t, out, 10*testPoll)
	cancel()
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestKubernetesNodeSourceTaint(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	client := fake.NewClientset(node)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, done := runWatch(t, ctx, &KubernetesNodeSource{Client: client, NodeName: "n1", Poll: testPoll})
	expectNoNotice(t, out, 5*testPoll)

	// An unrelated taint is not an eviction.
	node.Spec.Taints = []corev1.Taint{{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule}}
	if _, err := client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	expectNoNotice(t, out, 5*testPoll)

	node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
		Key: "aws-node-termination-handler/spot-itn", Value: "itn-1", Effect: corev1.TaintEffectNoSchedule,
	})
	if _, err := client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	n := waitNotice(t, out)
	if n.Source != SourceKubernetes || !strings.Contains(n.Reason, "aws-node-termination-handler/spot-itn=itn-1:NoSchedule") {
		t.Fatalf("notice = %+v", n)
	}
	if !n.Deadline.IsZero() {
		t.Fatalf("deadline = %v, want zero", n.Deadline)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestKubernetesNodeSourceCustomTaints(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{
			{Key: "karpenter.sh/disruption", Effect: corev1.TaintEffectNoSchedule},
		}},
	}
	client := fake.NewClientset(node)
	ctx, cancel := context.WithCancel(context.Background())
	// A custom list replaces the defaults, so the Karpenter taint no longer counts.
	out, done := runWatch(t, ctx, &KubernetesNodeSource{Client: client, NodeName: "n1", Taints: []string{"example.com/evict"}, Poll: testPoll})
	expectNoNotice(t, out, 5*testPoll)
	cancel()
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestKubernetesNodeSourceDeletion(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	client := fake.NewClientset(node)
	ctx := context.Background()
	out, done := runWatch(t, ctx, &KubernetesNodeSource{Client: client, NodeName: "n1", Poll: testPoll})
	expectNoNotice(t, out, 5*testPoll)
	if err := client.CoreV1().Nodes().Delete(ctx, "n1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	n := waitNotice(t, out)
	if !strings.Contains(n.Reason, "deleted") {
		t.Fatalf("notice = %+v", n)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}

	// Deletion in progress (finalizers pending) is a notice as well.
	now := metav1.Now()
	deleting := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2", DeletionTimestamp: &now, Finalizers: []string{"x"}}}
	out, done = runWatch(t, ctx, &KubernetesNodeSource{Client: fake.NewClientset(deleting), NodeName: "n2", Poll: testPoll})
	if n := waitNotice(t, out); !strings.Contains(n.Reason, "being deleted") {
		t.Fatalf("notice = %+v", n)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestKubernetesNodeSourceUnknownNodeIsPermanent(t *testing.T) {
	out, done := runWatch(t, context.Background(),
		&KubernetesNodeSource{Client: fake.NewClientset(), NodeName: "missing", Poll: testPoll})
	err := waitDone(t, done)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want not found", err)
	}
	expectNoNotice(t, out, 0)
}

// fakeSource is a controllable Source for Merge tests.
type fakeSource struct {
	name    string
	after   time.Duration // send a notice after this long; 0 = never
	err     error         // returned immediately when set
	stopped atomic.Bool
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Watch(ctx context.Context, out chan<- Notice) error {
	if f.err != nil {
		return f.err
	}
	defer f.stopped.Store(true)
	if f.after == 0 {
		<-ctx.Done()
		return nil
	}
	select {
	case <-time.After(f.after):
		send(ctx, out, Notice{Reason: "from " + f.name + strings.Repeat("x", 300)})
	case <-ctx.Done():
	}
	return nil
}

func TestMergeFirstNoticeWins(t *testing.T) {
	fast := &fakeSource{name: "fast", after: 20 * time.Millisecond}
	slow := &fakeSource{name: "slow", after: 2 * time.Second}
	never := &fakeSource{name: "never"}
	broken := &fakeSource{name: "broken", err: errors.New("imds unreachable")}

	ch := Merge(context.Background(), quietLogger(), broken, slow, fast, never)
	n := waitNotice(t, ch)
	if n.Source != "fast" {
		t.Fatalf("first notice from %q, want fast", n.Source)
	}
	if len(n.Reason) != MaxReasonLen {
		t.Fatalf("reason len = %d, want bounded to %d", len(n.Reason), MaxReasonLen)
	}

	// The others are cancelled promptly, long before slow would have fired.
	deadline := time.Now().Add(time.Second)
	for !(slow.stopped.Load() && never.stopped.Load()) {
		if time.Now().After(deadline) {
			t.Fatal("other sources were not stopped after the first notice")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Exactly one notice is ever delivered.
	expectNoNotice(t, ch, 50*time.Millisecond)
}

func TestMergeNoSourcesNeverFires(t *testing.T) {
	ch := Merge(context.Background(), nil)
	expectNoNotice(t, ch, 20*time.Millisecond)
}

func TestMergeStopsOnParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	never := &fakeSource{name: "never"}
	ch := Merge(ctx, quietLogger(), never)
	cancel()
	deadline := time.Now().Add(time.Second)
	for !never.stopped.Load() {
		if time.Now().After(deadline) {
			t.Fatal("source not stopped after parent cancel")
		}
		time.Sleep(5 * time.Millisecond)
	}
	expectNoNotice(t, ch, 20*time.Millisecond)
}

func TestFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	kube := fake.NewClientset()

	srcs, err := FromEnv(env(nil), nil)
	if err != nil || len(srcs) != 0 {
		t.Fatalf("empty: %v %v", srcs, err)
	}

	srcs, err = FromEnv(env(map[string]string{
		EnvSources:  " Kubernetes, aws ,gcp,azure,file,aws,",
		EnvFile:     "/run/nexus/evict",
		EnvPoll:     "750ms",
		EnvTaints:   "a/b, c",
		EnvNodeName: "node-7",
	}), kube)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range srcs {
		names = append(names, s.Name())
	}
	if got := strings.Join(names, ","); got != "kubernetes,aws,gcp,azure,file" {
		t.Fatalf("sources = %s", got)
	}
	k := srcs[0].(*KubernetesNodeSource)
	if k.NodeName != "node-7" || strings.Join(k.Taints, "|") != "a/b|c" || k.Poll != 750*time.Millisecond {
		t.Fatalf("kubernetes source = %+v", k)
	}
	if f := srcs[4].(*FileSource); f.Path != "/run/nexus/evict" || f.Poll != 750*time.Millisecond {
		t.Fatalf("file source = %+v", f)
	}
	if a := srcs[1].(*AWSSource); a.Poll != 750*time.Millisecond {
		t.Fatalf("aws poll = %v", a.Poll)
	}

	srcs, err = FromEnv(env(map[string]string{EnvSources: "kubernetes", EnvNodeName: "n"}), kube)
	if err != nil {
		t.Fatal(err)
	}
	if k := srcs[0].(*KubernetesNodeSource); k.Taints != nil || k.Poll != DefaultPoll {
		t.Fatalf("defaults not applied: %+v", k)
	}

	for _, tc := range []struct {
		name string
		env  map[string]string
		kube bool
		want string
	}{
		{"unknown", map[string]string{EnvSources: "aws,oracle"}, false, `unknown eviction source "oracle"`},
		{"file without path", map[string]string{EnvSources: "file"}, false, EnvFile},
		{"kube without client", map[string]string{EnvSources: "kubernetes", EnvNodeName: "n"}, false, "no kubernetes client"},
		{"kube without node", map[string]string{EnvSources: "kubernetes"}, true, EnvNodeName},
		{"bad poll", map[string]string{EnvSources: "aws", EnvPoll: "soon"}, false, EnvPoll},
		{"zero poll", map[string]string{EnvSources: "aws", EnvPoll: "0s"}, false, "positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var kc *fake.Clientset
			if tc.kube {
				kc = kube
			}
			var err error
			if kc != nil {
				_, err = FromEnv(env(tc.env), kc)
			} else {
				_, err = FromEnv(env(tc.env), nil)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestCapacityTypeFromNodeLabels(t *testing.T) {
	for _, tc := range []struct {
		labels map[string]string
		want   string
	}{
		{nil, ""},
		{map[string]string{"kubernetes.io/os": "linux"}, ""},
		{map[string]string{"karpenter.sh/capacity-type": "spot"}, "spot"},
		{map[string]string{"karpenter.sh/capacity-type": "on-demand"}, "on-demand"},
		{map[string]string{"eks.amazonaws.com/capacityType": "SPOT"}, "spot"},
		{map[string]string{"eks.amazonaws.com/capacityType": "ON_DEMAND"}, "on-demand"},
		{map[string]string{"cloud.google.com/gke-spot": "true"}, "spot"},
		{map[string]string{"cloud.google.com/gke-preemptible": "true"}, "spot"},
		{map[string]string{"cloud.google.com/gke-spot": "false"}, ""},
		{map[string]string{"kubernetes.azure.com/scalesetpriority": "spot"}, "spot"},
		// Karpenter wins over the managed node group label.
		{map[string]string{"karpenter.sh/capacity-type": "on-demand", "eks.amazonaws.com/capacityType": "SPOT"}, "on-demand"},
	} {
		if got := CapacityTypeFromNodeLabels(tc.labels); got != tc.want {
			t.Errorf("CapacityTypeFromNodeLabels(%v) = %q, want %q", tc.labels, got, tc.want)
		}
	}
}

func TestBoundReasonKeepsUTF8(t *testing.T) {
	s := strings.Repeat("a", MaxReasonLen-1) + "é" // é is two bytes, straddling the bound
	got := boundReason(s)
	if len(got) != MaxReasonLen-1 {
		t.Fatalf("len = %d, want %d", len(got), MaxReasonLen-1)
	}
}
