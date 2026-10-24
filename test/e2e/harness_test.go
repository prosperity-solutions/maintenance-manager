//go:build e2e

// Package e2e runs maintenance-manager against Envoy Gateway on kind
// (docs/HANDOVER.md §13.2). Run: go test -tags e2e ./test/e2e/...
//
// Environment:
//
//	MM_E2E_EG       Envoy Gateway version: v1.8.3 or v1.9.1 (default v1.9.1)
//	MM_E2E_CLUSTER  kind cluster name (default mm-e2e)
//	MM_E2E_KEEP=1   keep the cluster afterwards; an existing one is reused
//
// Needs Docker (or a compatible runtime). Every tool runs through
// scripts/dev/t, pinned in tools/go.mod.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
)

// egCharts pins the Envoy Gateway chart per version by digest (A7).
var egCharts = map[string]string{
	"v1.8.3": "sha256:cfb34ff4266c87a394cd6be5c13607a2dd47083aef771368302eaeaa99c4a0a9",
	"v1.9.1": "sha256:91bae9aedb91ab34731e987afe01a3ccf454393015abeca705eea8ee15553e86",
}

// kindNode is the newest node both EG lines support (1.8: k8s 1.32-1.35,
// 1.9: 1.33-1.36, https://gateway.envoyproxy.io/news/releases/matrix/),
// pinned by digest (A7).
const kindNode = "kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0"

const (
	bypassToken = "e2e-bypass-0123456789" // the chart wants at least 16 characters
	mmNamespace = "maintenance-manager"
	envoyAddr   = "127.0.0.1:30080" // kind extraPortMappings → Envoy's NodePort (D9)
)

var (
	root     string        // repository root
	kc       client.Client // cluster client
	cluster  string
	kubeFile string // kubeconfig of the kind cluster, for helm
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	var err error
	if root, err = filepath.Abs("../.."); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	eg := envOr("MM_E2E_EG", "v1.9.1")
	digest, ok := egCharts[eg]
	if !ok {
		fmt.Fprintf(os.Stderr, "MM_E2E_EG=%s is not pinned; known: v1.8.3, v1.9.1\n", eg)
		return 1
	}
	cluster = envOr("MM_E2E_CLUSTER", "mm-e2e")
	keep := os.Getenv("MM_E2E_KEEP") == "1"
	kubeFile = filepath.Join(os.TempDir(), "mm-e2e-"+cluster+".kubeconfig")
	defer os.Remove(kubeFile)

	// Compile the tools first, outside the setup deadline: on a cold machine
	// the first `go tool` build of kind, ko and helm takes a while.
	t0 := time.Now()
	for _, name := range []string{"kind", "ko", "helm"} {
		if err := tool(context.Background(), name, "version"); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: building %s failed: %v\n", name, err)
			return 1
		}
	}
	fmt.Printf("e2e: %-28s %s\n", "tools", time.Since(t0).Round(time.Second))

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := setup(ctx, eg, digest, kubeFile); err != nil {
		fmt.Fprintf(os.Stderr, "e2e setup failed: %v\n", err)
		diagnose()
		if !keep {
			_ = tool(context.Background(), "kind", "delete", "cluster", "--name", cluster)
		}
		return 1
	}
	fmt.Printf("e2e: setup done in %s (EG %s)\n", time.Since(start).Round(time.Second), eg)

	code := m.Run()
	if code != 0 {
		diagnose()
	}

	fmt.Printf("e2e: total %s (EG %s)\n", time.Since(start).Round(time.Second), eg)
	if !keep {
		_ = tool(context.Background(), "kind", "delete", "cluster", "--name", cluster)
	}
	return code
}

func setup(ctx context.Context, eg, digest, kubeFile string) error {
	step := func(name string, f func() error) error {
		t0 := time.Now()
		if err := f(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Printf("e2e: %-28s %s\n", name, time.Since(t0).Round(time.Second))
		return nil
	}
	if err := step("kind create", func() error {
		out, _ := toolOutput(ctx, "kind", "get", "clusters")
		if strings.Contains("\n"+out+"\n", "\n"+cluster+"\n") {
			return nil // MM_E2E_KEEP reuse
		}
		// Retried: a registry or Docker blip should not turn the leg red.
		return attempts(ctx, 3, func() error {
			err := tool(ctx, "kind", "create", "cluster", "--name", cluster, "--image", kindNode,
				"--config", filepath.Join(root, "test/e2e/testdata/kind.yaml"), "--wait", "120s")
			if err != nil {
				_ = tool(ctx, "kind", "delete", "cluster", "--name", cluster)
			}
			return err
		})
	}); err != nil {
		return err
	}
	kubeconfig, err := toolOutput(ctx, "kind", "get", "kubeconfig", "--name", cluster)
	if err != nil {
		return err
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfig))
	if err != nil {
		return err
	}
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		return err
	}
	if err := mmv1.AddToScheme(sch); err != nil {
		return err
	}
	if kc, err = client.New(cfg, client.Options{Scheme: sch}); err != nil {
		return err
	}
	if err := os.WriteFile(kubeFile, []byte(kubeconfig), 0o600); err != nil {
		return err
	}

	if err := step("envoy gateway "+eg, func() error {
		if err := attempts(ctx, 3, func() error {
			return tool(ctx, "helm", "upgrade", "--install", "eg", "oci://docker.io/envoyproxy/gateway-helm@"+digest,
				"--kubeconfig", kubeFile, "-n", "envoy-gateway-system", "--create-namespace", "--wait", "--timeout", "4m")
		}); err != nil {
			return err
		}
		if err := applyFile(ctx, "gateway.yaml", ""); err != nil {
			return err
		}
		return waitDeployment(ctx, "envoy-gateway-system", "", 3*time.Minute, func(d *appsv1.Deployment) bool {
			return d.Labels["gateway.envoyproxy.io/owning-gateway-name"] == "eg"
		})
	}); err != nil {
		return err
	}

	return step("ko build + chart install", func() error {
		// The node's architecture, not Go's: an amd64 Go under Rosetta on an
		// arm64 kind node would otherwise build the wrong image.
		var nodes corev1.NodeList
		if err := kc.List(ctx, &nodes); err != nil {
			return fmt.Errorf("list nodes: %w", err)
		}
		if len(nodes.Items) == 0 {
			return errors.New("the cluster has no nodes")
		}
		arch := nodes.Items[0].Status.NodeInfo.Architecture
		// A tag per run: a reused cluster must run this build, and a
		// kind-loaded image cannot be pulled by registry digest.
		tag := fmt.Sprintf("e2e-%d", time.Now().Unix())
		if err := toolEnv(ctx, []string{"KO_DOCKER_REPO=kind.local", "KIND_CLUSTER_NAME=" + cluster, "VERSION=0.0.0-e2e"},
			"ko", "build", "-B", "--tags", tag, "--platform", "linux/"+arch, "./cmd/maintenance-manager"); err != nil {
			return err
		}
		return tool(ctx, "helm", "upgrade", "--install", "mm", filepath.Join(root, "chart"),
			"--kubeconfig", kubeFile, "-n", mmNamespace, "--create-namespace",
			"--set", "editor.password=e2e", "--set", "bypass.token="+bypassToken, "--set", "image.repository=kind.local/maintenance-manager",
			"--set", "image.tag="+tag, "--set", "image.digest=", "--set", "image.pullPolicy=IfNotPresent",
			// A reused cluster (MM_E2E_KEEP) may carry fields earlier tests patched.
			"--force-conflicts",
			"--wait", "--timeout", "3m")
	})
}

// diagnose prints what a red CI leg needs to be debugged from its log alone.
func diagnose() {
	if kc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fmt.Println("e2e: --- diagnostics ---")
	var pods corev1.PodList
	if err := kc.List(ctx, &pods); err == nil {
		for _, p := range pods.Items {
			fmt.Printf("pod %s/%s phase=%s deleting=%v\n", p.Namespace, p.Name, p.Status.Phase, p.DeletionTimestamp != nil)
		}
	}
	for _, kind := range []schema.GroupVersionKind{
		gvk("gateway.envoyproxy.io", "v1alpha1", "SecurityPolicyList"),
		gvk("gateway.networking.k8s.io", "v1", "HTTPRouteList"),
	} {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(kind)
		if err := kc.List(ctx, list); err != nil {
			continue
		}
		for _, o := range list.Items {
			status, _, _ := unstructured.NestedFieldNoCopy(o.Object, "status")
			fmt.Printf("%s %s/%s status=%v\n", strings.TrimSuffix(kind.Kind, "List"), o.GetNamespace(), o.GetName(), status)
		}
	}
	var mmPods corev1.PodList
	if err := kc.List(ctx, &mmPods, client.InNamespace(mmNamespace)); err == nil && len(mmPods.Items) > 0 {
		fmt.Println("maintenance-manager pods:", len(mmPods.Items))
	}
}

// ---- tools ----

func tool(ctx context.Context, args ...string) error { return toolEnv(ctx, nil, args...) }

func toolEnv(ctx context.Context, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, filepath.Join(root, "scripts/dev/t"), args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w\n%s", strings.Join(args, " "), err, tail(out.String(), 40))
	}
	return nil
}

func toolOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, filepath.Join(root, "scripts/dev/t"), args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w\n%s", strings.Join(args, " "), err, tail(stderr.String(), 20))
	}
	return string(out), nil
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// attempts runs f up to n times with a short pause.
func attempts(ctx context.Context, n int, f func() error) error {
	var err error
	for i := 1; i <= n; i++ {
		if err = f(); err == nil || ctx.Err() != nil {
			return err
		}
		fmt.Printf("e2e: attempt %d/%d failed, retrying: %v\n", i, n, firstLine(err))
		time.Sleep(time.Duration(i) * 5 * time.Second)
	}
	return err
}

func firstLine(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	return s
}

// ---- cluster objects ----

// manifests reads a testdata file, replacing NAMESPACE.
func manifests(name, namespace string) ([]*unstructured.Unstructured, error) {
	raw, err := os.ReadFile(filepath.Join(root, "test/e2e/testdata", name))
	if err != nil {
		return nil, err
	}
	if namespace != "" {
		raw = bytes.ReplaceAll(raw, []byte("NAMESPACE"), []byte(namespace))
	}
	var objs []*unstructured.Unstructured
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return objs, nil
			}
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if len(obj.Object) > 0 {
			objs = append(objs, obj)
		}
	}
}

// applyFile server-side-applies a testdata manifest.
func applyFile(ctx context.Context, name, namespace string) error {
	objs, err := manifests(name, namespace)
	if err != nil {
		return err
	}
	for _, obj := range objs {
		if err := retry(ctx, 90*time.Second, func() error {
			return kc.Patch(ctx, obj, client.Apply, client.FieldOwner("mm-e2e"), client.ForceOwnership) //nolint:staticcheck // SSA of unstructured
		}); err != nil {
			return fmt.Errorf("%s: apply %s %s: %w", name, obj.GetKind(), obj.GetName(), err)
		}
	}
	return nil
}

// newApp creates a fresh namespace with the echo app and its HTTPRoute, and
// deletes the namespace when the test ends.
func newApp(t *testing.T, ns string) {
	t.Helper()
	ctx := t.Context()
	n := &corev1.Namespace{}
	n.Name = ns
	if err := kc.Create(ctx, n); err != nil {
		t.Fatalf("namespace %s: %v (left over from a kept cluster? delete it or the cluster)", ns, err)
	}
	t.Cleanup(func() {
		// t.Context() is already cancelled when cleanups run.
		if err := client.IgnoreNotFound(kc.Delete(context.Background(), n)); err != nil {
			t.Errorf("delete namespace %s: %v", ns, err)
		}
	})
	if err := applyFile(ctx, "app.yaml", ns); err != nil {
		t.Fatal(err)
	}
	if err := waitDeployment(ctx, ns, "echo", 2*time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	// Reachable through Envoy before anything is attached.
	waitFor(t, 2*time.Minute, "the app answers 200 through Envoy", func() (bool, string) {
		r := get(ns, "/", "text/html", nil)
		return r.status == 200, r.String()
	})
}

func waitDeployment(ctx context.Context, ns, name string, timeout time.Duration, match func(*appsv1.Deployment) bool) error {
	return retry(ctx, timeout, func() error {
		var list appsv1.DeploymentList
		if err := kc.List(ctx, &list, client.InNamespace(ns)); err != nil {
			return err
		}
		found := false
		for i := range list.Items {
			d := &list.Items[i]
			if (name != "" && d.Name != name) || (match != nil && !match(d)) {
				continue
			}
			found = true
			want := int32(1)
			if d.Spec.Replicas != nil {
				want = *d.Spec.Replicas
			}
			if d.Status.ReadyReplicas < want || d.Status.ObservedGeneration < d.Generation || d.Status.UpdatedReplicas < want {
				return fmt.Errorf("deployment %s/%s: %d/%d ready", ns, d.Name, d.Status.ReadyReplicas, want)
			}
		}
		if !found {
			return fmt.Errorf("no matching deployment in %s yet", ns)
		}
		return nil
	})
}

// mmReplicas is the maintenance-manager Deployment's desired replica count.
func mmReplicas(t *testing.T) int {
	t.Helper()
	var d appsv1.Deployment
	if err := kc.Get(context.Background(), client.ObjectKey{Namespace: mmNamespace, Name: "maintenance-manager"}, &d); err != nil {
		t.Fatal(err)
	}
	if d.Spec.Replicas == nil {
		return 1
	}
	return int(*d.Spec.Replicas)
}

// waitPods waits until exactly want maintenance-manager pods exist and none is
// terminating. A terminating pod keeps answering Check() for its 10 s drain,
// so "new pods ready" is not enough after a rollout or a scale-down.
func waitPods(t *testing.T, want int) {
	t.Helper()
	waitFor(t, 3*time.Minute, fmt.Sprintf("%d maintenance-manager pods, none terminating", want), func() (bool, string) {
		var pods corev1.PodList
		if err := kc.List(context.Background(), &pods, client.InNamespace(mmNamespace),
			client.MatchingLabels{"app.kubernetes.io/name": "maintenance-manager"}); err != nil {
			return false, err.Error()
		}
		ready := 0
		for _, p := range pods.Items {
			if p.DeletionTimestamp != nil {
				return false, "pod " + p.Name + " is terminating"
			}
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					ready++
				}
			}
		}
		return len(pods.Items) == want && ready == want, fmt.Sprintf("%d pods, %d ready", len(pods.Items), ready)
	})
}

func retry(ctx context.Context, timeout time.Duration, f func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := f()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		time.Sleep(time.Second)
	}
}

// waitStable is waitFor that needs n consecutive successes, so one lucky
// answer during a transition does not count.
func waitStable(t *testing.T, timeout time.Duration, n int, what string, f func() (bool, string)) {
	t.Helper()
	streak := 0
	waitFor(t, timeout, what, func() (bool, string) {
		ok, detail := f()
		if !ok {
			streak = 0
			return false, detail
		}
		streak++
		return streak >= n, detail
	})
}

// waitFor polls until ok or the timeout, then fails with the last detail.
func waitFor(t *testing.T, timeout time.Duration, what string, f func() (bool, string)) {
	t.Helper()
	t0 := time.Now()
	deadline := t0.Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		var ok bool
		if ok, last = f(); ok {
			t.Logf("%s after %s", what, time.Since(t0).Round(100*time.Millisecond))
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s; last: %s", timeout, what, last)
}

// gvk is a small helper for unstructured lookups.
func gvk(group, version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

// ---- HTTP through Envoy ----

// No keep-alives: every SecurityPolicy create or delete drains the listener,
// and reused connections would flake (§13.2).
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext:       (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type response struct {
	status int
	header http.Header
	body   string
	err    error
}

func (r response) String() string {
	if r.err != nil {
		return "error: " + r.err.Error()
	}
	b := r.body
	if len(b) > 200 {
		b = b[:200] + "…"
	}
	return fmt.Sprintf("%d %s %q", r.status, r.header.Get("Content-Type"), b)
}

// get sends a request for host <ns>.example.test through Envoy.
func get(ns, path, accept string, headers map[string]string) response {
	return do(http.MethodGet, ns, path, accept, headers)
}

func do(method, ns, path, accept string, headers map[string]string) response {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+envoyAddr+path, nil)
	if err != nil {
		return response{err: err}
	}
	req.Host = ns + ".example.test"
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return response{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return response{status: resp.StatusCode, header: resp.Header, body: string(body), err: err}
}
