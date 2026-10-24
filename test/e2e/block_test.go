//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// setMMArgs replaces maintenance-manager's container args and waits until only
// pods with the new args run.
func setMMArgs(t *testing.T, args []string) {
	t.Helper()
	ctx := context.Background()
	var d appsv1.Deployment
	if err := kc.Get(ctx, types.NamespacedName{Namespace: mmNamespace, Name: "maintenance-manager"}, &d); err != nil {
		t.Fatal(err)
	}
	patch, _ := json.Marshal([]map[string]any{{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": args}})
	if err := kc.Patch(ctx, &d, client.RawPatch(types.JSONPatchType, patch)); err != nil {
		t.Fatal(err)
	}
	if err := waitDeployment(ctx, mmNamespace, "maintenance-manager", 3*time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	waitPods(t, mmReplicas(t))
}

// blockRoute points maintenance-manager's hidden --e2e-block-route flag (M2)
// at ns/web, and restores the original args when the test ends.
func blockRoute(t *testing.T, ns string) {
	t.Helper()
	var d appsv1.Deployment
	if err := kc.Get(context.Background(), types.NamespacedName{Namespace: mmNamespace, Name: "maintenance-manager"}, &d); err != nil {
		t.Fatal(err)
	}
	orig := d.Spec.Template.Spec.Containers[0].Args
	args := []string{}
	for _, a := range orig {
		if !strings.HasPrefix(a, "--e2e-block-route=") {
			args = append(args, a)
		}
	}
	setMMArgs(t, append(args, "--e2e-block-route="+ns+"/web"))
	t.Cleanup(func() { setMMArgs(t, args) })
}

// attachFixture applies the fixture SecurityPolicy and ReferenceGrant, waits
// until Envoy Gateway accepts the policy, and removes both when the test ends.
func attachFixture(t *testing.T, ns string) {
	t.Helper()
	ctx := t.Context()
	if err := applyFile(ctx, "fixture-securitypolicy.yaml", ns); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// t.Context() is already cancelled when cleanups run.
		if err := deleteFile(context.Background(), "fixture-securitypolicy.yaml", ns); err != nil {
			t.Errorf("remove fixture: %v", err)
		}
	})
	waitFor(t, 2*time.Minute, "fixture SecurityPolicy Accepted", func() (bool, string) {
		sp := &unstructured.Unstructured{}
		sp.SetGroupVersionKind(gvk("gateway.envoyproxy.io", "v1alpha1", "SecurityPolicy"))
		if err := kc.Get(ctx, types.NamespacedName{Namespace: ns, Name: "mm-fixture"}, sp); err != nil {
			return false, err.Error()
		}
		return accepted(sp)
	})
}

// accepted reports whether every ancestor reports Accepted=True for the
// current generation.
func accepted(obj *unstructured.Unstructured) (bool, string) {
	ancestors, _, _ := unstructured.NestedSlice(obj.Object, "status", "ancestors")
	if len(ancestors) == 0 {
		return false, "no ancestors in status yet"
	}
	for _, a := range ancestors {
		conds, _, _ := unstructured.NestedSlice(a.(map[string]any), "conditions")
		ok := false
		for _, c := range conds {
			m := c.(map[string]any)
			if m["type"] == "Accepted" {
				gen, _ := m["observedGeneration"].(int64)
				if m["status"] != "True" || gen != obj.GetGeneration() {
					return false, fmt.Sprintf("Accepted=%v reason=%v message=%v", m["status"], m["reason"], m["message"])
				}
				ok = true
			}
		}
		if !ok {
			return false, "no Accepted condition yet"
		}
	}
	return true, ""
}

// fromEcho reports whether a response came from the echo app for ns, not from
// some other 200.
func fromEcho(r response, ns string) bool {
	return r.status == 200 && strings.Contains(r.body, `"hostname":"`+ns+`.example.test"`)
}

// E1: block → 503 HTML for a browser, problem+json for an API client,
// Cache-Control: no-store. (Retry-After arrives with windows in M3: the static
// M2 block has no end time.)
func TestE1Block(t *testing.T) {
	ns := "e1"
	newApp(t, ns)
	blockRoute(t, ns)
	attachFixture(t, ns)

	waitStable(t, 2*time.Minute, 5, "browser and API requests are blocked", func() (bool, string) {
		r := get(ns, "/", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8", nil)
		if r.status != 503 {
			return false, "browser: " + r.String()
		}
		r = get(ns, "/api/orders", "application/json", nil)
		return r.status == 503, "API: " + r.String()
	})

	r := get(ns, "/", "text/html", nil)
	if r.status != 503 || !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") {
		t.Fatalf("browser: want 503 text/html, got %s", r)
	}
	if !strings.Contains(r.body, "<html") {
		t.Fatalf("browser: want an HTML page, got %s", r)
	}
	if got := r.header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("browser: Cache-Control %q, want no-store", got)
	}

	r = get(ns, "/api/orders", "application/json", nil)
	if r.status != 503 || r.header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("API: want 503 application/problem+json, got %s", r)
	}
	var problem struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal([]byte(r.body), &problem); err != nil || problem.Status != 503 ||
		problem.Type != "https://maintenance-manager.oss.life.li/problems/maintenance" {
		t.Fatalf("API: bad problem+json %s (%v)", r.body, err)
	}
	if got := r.header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("API: Cache-Control %q, want no-store", got)
	}

	// A POST is not a page load: problem+json too.
	if r := do("POST", ns, "/submit", "text/html", nil); r.status != 503 || r.header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("POST: want 503 problem+json, got %s", r)
	}
}

// E5: maintenance-manager scaled to 0 → the route serves the app (fail-open).
func TestE5FailOpen(t *testing.T) {
	ns := "e5"
	newApp(t, ns)
	blockRoute(t, ns)
	attachFixture(t, ns)
	waitStable(t, 2*time.Minute, 5, "blocked while maintenance-manager runs", func() (bool, string) {
		r := get(ns, "/", "text/html", nil)
		return r.status == 503, r.String()
	})

	ctx := context.Background()      // also used in Cleanup, after t.Context() is cancelled
	replicas := int32(mmReplicas(t)) //nolint:gosec // a replica count, far below 2^31
	scale := func(n int32) {
		var d appsv1.Deployment
		if err := kc.Get(ctx, types.NamespacedName{Namespace: mmNamespace, Name: "maintenance-manager"}, &d); err != nil {
			t.Fatal(err)
		}
		patch := client.MergeFrom(d.DeepCopy())
		d.Spec.Replicas = &n
		if err := kc.Patch(ctx, &d, patch); err != nil {
			t.Fatal(err)
		}
	}
	scale(0)
	t.Cleanup(func() {
		scale(replicas)
		waitPods(t, int(replicas))
	})
	waitPods(t, 0)

	waitStable(t, 2*time.Minute, 5, "the echo app answers with maintenance-manager down", func() (bool, string) {
		r := get(ns, "/", "text/html", nil)
		return fromEcho(r, ns), r.String()
	})
}
