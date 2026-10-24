//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/controller"
)

// E9: helm uninstall leaves zero SecurityPolicies and zero ReferenceGrants.
// It removes maintenance-manager, so it runs last (Go runs test files in name
// order; this file sorts last).
func TestE9Uninstall(t *testing.T) {
	ns := "e9"
	maintenance(t, ns, nil, mmv1.Phase{Name: "down", Action: mmv1.ActionBlock})

	ctx := t.Context()
	if err := tool(ctx, "helm", "uninstall", "mm", "--kubeconfig", kubeFile, "-n", mmNamespace, "--wait", "--timeout", "5m"); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"SecurityPolicyList", "ReferenceGrantList"} {
		group, version := "gateway.envoyproxy.io", "v1alpha1"
		if kind == "ReferenceGrantList" {
			group, version = "gateway.networking.k8s.io", "v1beta1"
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk(group, version, kind))
		if err := kc.List(context.Background(), list, client.MatchingLabels{controller.LabelManagedBy: "maintenance-manager"}); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 0 {
			t.Fatalf("%d %s left after uninstall", len(list.Items), kind)
		}
	}
	// The app is reachable again without us.
	waitStable(t, 2*time.Minute, 3, "the app answers after uninstall", func() (bool, string) {
		r := get(ns, "/", browser, nil)
		return fromEcho(r, ns), r.String()
	})
}
