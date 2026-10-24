//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// busybox for nc, pinned by digest (A7).
const busybox = "busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

// probe runs a Job that tries a TCP connection to every maintenance-manager
// port and succeeds only if each one's outcome is what the NetworkPolicy
// promises.
func probe(t *testing.T, ns string, labels map[string]string, open, closed []int) {
	t.Helper()
	script := "set -x\n"
	host := "maintenance-manager." + mmNamespace + ".svc.cluster.local"
	for _, p := range open {
		script += fmt.Sprintf("nc -z -w 5 %s %d || exit 1\n", host, p)
	}
	for _, p := range closed {
		script += fmt.Sprintf("! nc -z -w 5 %s %d || exit 1\n", host, p)
	}
	zero := int32(0)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "np-probe", Namespace: ns},
		Spec: batchv1.JobSpec{BackoffLimit: &zero, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{
				Name: "nc", Image: busybox, Command: []string{"sh", "-c", script},
			}}},
		}},
	}
	if err := kc.Create(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	bg := metav1.DeletePropagationBackground
	t.Cleanup(func() { _ = kc.Delete(context.Background(), job, &client.DeleteOptions{PropagationPolicy: &bg}) })
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if err := kc.Get(t.Context(), client.ObjectKeyFromObject(job), job); err == nil {
			if job.Status.Succeeded > 0 {
				return
			}
			if job.Status.Failed > 0 {
				t.Fatalf("probe in %s (labels %v): open %v, closed %v not as promised; see the np-probe pod log", ns, labels, open, closed)
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("probe in %s did not finish", ns)
}

// TestNetworkPolicy: with networkPolicy.enabled (the e2e install), a pod in
// another namespace reaches none of the ports; a pod that looks like an
// Envoy Gateway proxy in the Envoy namespace reaches authz (and, there being
// in editorFrom, the editor), but not metrics.
func TestNetworkPolicy(t *testing.T) {
	newNS(t, "np-other")
	probe(t, "np-other", nil, nil, []int{9001, 8080, 8081})
	proxy := map[string]string{"app.kubernetes.io/component": "proxy", "app.kubernetes.io/managed-by": "envoy-gateway"}
	probe(t, "envoy-gateway-system", proxy, []int{9001, 8080}, []int{8081})
}

func newNS(t *testing.T, ns string) {
	t.Helper()
	n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := kc.Create(t.Context(), n); err != nil {
		t.Fatalf("namespace %s: %v", ns, err)
	}
	t.Cleanup(func() { _ = client.IgnoreNotFound(kc.Delete(context.Background(), n)) })
}
