// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package k8stest is test support for the kubernetes executor's integration
// tests. It exists as a package (not a _test.go file) so the executor's and the
// worker's integration tests share ONE guard against touching a real cluster.
//
// The guard is deliberately paranoid. These tests create and delete a
// namespace, Jobs, Pods and Secrets; pointed at a shared or production cluster
// by an unlucky kubeconfig they would be destructive. So they run only when
// explicitly opted in AND the current context is recognisably a local
// single-node cluster AND its API server is on a loopback / Docker-Desktop
// address. Any doubt skips.
package k8stest

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Namespace is the ONLY namespace the integration tests use. It is created on
// first use and deleted by Teardown.
const Namespace = "nexus-k8s-exec-test"

// OptInEnv must be "1" for any cluster-touching test to run.
const OptInEnv = "NEXUS_K8S_CONFORMANCE"

var localContextPrefixes = []string{"docker-desktop", "kind-", "minikube", "rancher-desktop", "orbstack", "colima"}

var localHosts = map[string]bool{
	"localhost": true, "127.0.0.1": true, "::1": true,
	"kubernetes.docker.internal": true, "host.docker.internal": true,
}

var (
	once      sync.Once
	client    kubernetes.Interface
	skipWhy   string
	createdNS bool
	ctxName   string
)

// Client returns a clientset for the local cluster, or a non-empty reason to
// skip. Safe for concurrent use; the checks run once per test binary.
func Client() (kubernetes.Interface, string) {
	once.Do(setup)
	return client, skipWhy
}

// ContextName reports the verified local context, for logging.
func ContextName() string { return ctxName }

func setup() {
	if os.Getenv(OptInEnv) != "1" {
		skipWhy = OptInEnv + "!=1: kubernetes integration tests are opt-in (they create namespace " + Namespace + ")"
		return
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	raw, err := rules.Load()
	if err != nil {
		skipWhy = fmt.Sprintf("load kubeconfig: %v", err)
		return
	}
	ctxName = raw.CurrentContext
	if !isLocalContextName(ctxName) {
		skipWhy = fmt.Sprintf("REFUSING: current kubectl context %q is not a recognised local cluster", ctxName)
		return
	}
	kctx := raw.Contexts[ctxName]
	if kctx == nil || raw.Clusters[kctx.Cluster] == nil {
		skipWhy = fmt.Sprintf("context %q has no cluster entry", ctxName)
		return
	}
	server := raw.Clusters[kctx.Cluster].Server
	if !isLocalServer(server) {
		skipWhy = fmt.Sprintf("REFUSING: context %q points at API server %q, which is not a loopback address", ctxName, server)
		return
	}
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: ctxName}).ClientConfig()
	if err != nil {
		skipWhy = fmt.Sprintf("build rest config: %v", err)
		return
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		skipWhy = fmt.Sprintf("build client: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cs.Discovery().ServerVersion(); err != nil {
		skipWhy = fmt.Sprintf("local cluster %q unreachable: %v", ctxName, err)
		return
	}
	if err := ensureNamespace(ctx, cs); err != nil {
		skipWhy = fmt.Sprintf("ensure namespace %s: %v", Namespace, err)
		return
	}
	client = cs
}

func isLocalContextName(name string) bool {
	for _, p := range localContextPrefixes {
		if name == p || strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func isLocalServer(server string) bool {
	u, err := url.Parse(server)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if localHosts[host] {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ensureNamespace creates the test namespace enforcing the RESTRICTED Pod
// Security profile, so every test also proves the executor's pods are
// admissible under the strictest built-in policy rather than merely claiming it.
func ensureNamespace(ctx context.Context, cs kubernetes.Interface) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: Namespace,
		Labels: map[string]string{
			"pod-security.kubernetes.io/enforce": "restricted",
			"pod-security.kubernetes.io/warn":    "restricted",
			"nexus.dev/purpose":                  "kubernetes-executor-integration-tests",
		},
	}}
	_, err := cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	switch {
	case err == nil:
		createdNS = true
	case apierrors.IsAlreadyExists(err):
		existing, gerr := cs.CoreV1().Namespaces().Get(ctx, Namespace, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		if existing.DeletionTimestamp != nil {
			// A previous run's teardown is still in flight; wait it out rather
			// than racing objects into a terminating namespace.
			for i := 0; i < 60; i++ {
				time.Sleep(time.Second)
				if _, gerr := cs.CoreV1().Namespaces().Get(ctx, Namespace, metav1.GetOptions{}); apierrors.IsNotFound(gerr) {
					return ensureNamespace(ctx, cs)
				}
			}
			return fmt.Errorf("namespace %s stuck terminating", Namespace)
		}
		createdNS = true // ours by name; a leftover from an interrupted run
	default:
		return err
	}
	// The default service account appears asynchronously; pods referencing
	// it are rejected until it exists.
	for i := 0; i < 30; i++ {
		if _, err := cs.CoreV1().ServiceAccounts(Namespace).Get(ctx, "default", metav1.GetOptions{}); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("default service account never appeared in %s", Namespace)
}

// Teardown deletes the test namespace if this process created (or adopted) it.
// NEXUS_K8S_KEEP_NAMESPACE=1 keeps it for post-mortem debugging.
func Teardown() {
	if client == nil || !createdNS || os.Getenv("NEXUS_K8S_KEEP_NAMESPACE") == "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = client.CoreV1().Namespaces().Delete(ctx, Namespace, metav1.DeleteOptions{})
}
