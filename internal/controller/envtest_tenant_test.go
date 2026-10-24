//go:build envtest

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1b1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

func removeOurs(t *testing.T, appNS string) {
	t.Cleanup(func() {
		_ = k8s.DeleteAllOf(context.Background(), &egv1a1.SecurityPolicy{}, client.InNamespace(appNS), client.MatchingLabels{LabelOwner: releaseNS})
		_ = k8s.DeleteAllOf(context.Background(), &egv1a1.EnvoyExtensionPolicy{}, client.InNamespace(appNS), client.MatchingLabels{LabelOwner: releaseNS})
		_ = k8s.DeleteAllOf(context.Background(), &gwapiv1b1.ReferenceGrant{}, client.InNamespace(releaseNS), client.MatchingLabels{LabelOwner: releaseNS})
	})
}

func TestEnvtestSquatterBlocksOnlyItsNamespace(t *testing.T) {
	// A foreign SecurityPolicy with our name in "aaa" (sorts first) must not
	// stop "shop" from being armed: errors and conflicts stay per namespace.
	// aaa's route goes to our EnvoyExtensionPolicy instead; a squatter on that
	// name too makes it a conflict.
	now := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	_ = k8s.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "aaa"}})
	create(t, &mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Namespace: releaseNS, Name: "all"},
		Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"sq": "yes"}}}})
	create(t, httpRoute("aaa", "web", map[string]string{"sq": "yes"}))
	create(t, httpRoute("shop", "sq", map[string]string{"sq": "yes"}))
	squatter := &egv1a1.SecurityPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "aaa", Name: SPName(releaseNS)}}
	squatter.Spec.TargetRefs = []gwapiv1.LocalPolicyTargetReferenceWithSectionName{{LocalPolicyTargetReference: gwapiv1.LocalPolicyTargetReference{Group: gatewayGroup, Kind: "HTTPRoute", Name: "other"}}}
	squatter.Spec.CORS = &egv1a1.CORS{}
	create(t, squatter)
	create(t, &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: releaseNS, Name: "sq"}, Spec: mmv1.MaintenanceWindowSpec{
		Enabled: true, Phases: []mmv1.Phase{{Name: "down", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"all"}}}}}}})
	removeOurs(t, "shop")
	removeOurs(t, "aaa")

	r, rec := newReconciler(t, now)
	res := reconcile(t, r)
	if sp := getSP(t, "shop"); sp == nil || targets(sp) != "sq" {
		t.Fatalf("shop must be armed despite the squatter in aaa (ops %v)", rec.take())
	}
	if eep := getEEP(t, "aaa"); eep == nil || extTargets(eep) != "web" {
		t.Fatalf("aaa/web must be armed through our EnvoyExtensionPolicy: %v", res.Desired)
	}
	extSquatter := &egv1a1.EnvoyExtensionPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "aaa", Name: SPName(releaseNS)}}
	extSquatter.Spec.TargetRefs = squatter.Spec.TargetRefs
	if err := k8s.Delete(t.Context(), getEEP(t, "aaa")); err != nil {
		t.Fatal(err)
	}
	create(t, extSquatter)
	res = reconcile(t, r)
	if _, conflict := res.Desired.Conflicts[snapshot.RouteKey{Namespace: "aaa", Name: "web"}]; !conflict {
		t.Fatalf("both names squatted: aaa/web must be reported as a conflict: %v", res.Desired.Conflicts)
	}
	var sq egv1a1.SecurityPolicy
	if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(squatter), &sq); err != nil || sq.Spec.CORS == nil {
		t.Fatal("the squatter must be left untouched")
	}
}

func TestEnvtestNamespaceSelector(t *testing.T) {
	// namespaces.from: Selector is evaluated against namespace labels: a
	// selector that matches no namespace is no conflict.
	now := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	create(t, &mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Namespace: releaseNS, Name: "ns-sel"},
		Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"nssel": "yes"}}}})
	create(t, httpRoute("shop", "nssel", map[string]string{"nssel": "yes"}))
	create(t, &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: releaseNS, Name: "nssel"}, Spec: mmv1.MaintenanceWindowSpec{
		Enabled: true, Phases: []mmv1.Phase{{Name: "down", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"ns-sel"}}}}}}})
	sel := &egv1a1.SecurityPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "sel"}}
	sel.Spec.TargetSelectors = []egv1a1.TargetSelector{{Kind: "HTTPRoute", Namespaces: &egv1a1.TargetSelectorNamespaces{
		From: egv1a1.TargetNamespaceFromSelector, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"nobody": "here"}}}}}
	sel.Spec.CORS = &egv1a1.CORS{}
	create(t, sel)
	removeOurs(t, "shop")

	r, _ := newReconciler(t, now)
	if res := reconcile(t, r); len(res.Desired.Conflicts) != 0 || getSP(t, "shop") == nil {
		t.Fatalf("a namespace selector matching nothing must not disarm: %v", res.Desired.Conflicts)
	}
	var shop corev1.Namespace
	if err := k8s.Get(t.Context(), client.ObjectKey{Name: "shop"}, &shop); err != nil {
		t.Fatal(err)
	}
	shop.Labels = map[string]string{"nobody": "here"}
	if err := k8s.Update(t.Context(), &shop); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var n corev1.Namespace
		if k8s.Get(context.Background(), client.ObjectKey{Name: "shop"}, &n) == nil {
			delete(n.Labels, "nobody")
			_ = k8s.Update(context.Background(), &n)
		}
	})
	if res := reconcile(t, r); len(res.Desired.ExtRoutes["shop"]) != 1 || len(res.Desired.Routes["shop"]) != 0 {
		t.Fatalf("a matching namespace selector takes the SecurityPolicy slot: the route moves to the EnvoyExtensionPolicy: %v", res.Desired)
	}
}

func TestEnvtestWriteFailureStaysInItsNamespace(t *testing.T) {
	// A write that fails in "aaa" skips aaa's later steps (no policy without
	// its grant) and leaves "shop" armed.
	now := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	_ = k8s.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "aaa"}})
	create(t, &mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Namespace: releaseNS, Name: "wf"},
		Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"wf": "yes"}}}})
	create(t, httpRoute("aaa", "wf", map[string]string{"wf": "yes"}))
	create(t, httpRoute("shop", "wf", map[string]string{"wf": "yes"}))
	create(t, &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: releaseNS, Name: "wf"}, Spec: mmv1.MaintenanceWindowSpec{
		Enabled: true, Phases: []mmv1.Phase{{Name: "down", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"wf"}}}}}}})
	removeOurs(t, "aaa")
	removeOurs(t, "shop")

	r, _ := newReconciler(t, now)
	r.Writer = interceptor.NewClient(k8s, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == GrantName("aaa") {
				return errors.New("injected")
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	if _, err := r.Reconcile(t.Context()); err == nil {
		t.Fatal("the injected failure must be reported")
	}
	if sp := getSP(t, "shop"); sp == nil || targets(sp) != "wf" {
		t.Fatal("shop must be armed despite the failure in aaa")
	}
	if getSP(t, "aaa") != nil {
		t.Fatal("aaa must get no policy without its grant")
	}
}

func TestEnvtestGrantDriftRepaired(t *testing.T) {
	// A grant with our name but no labels and an edited spec is ours: it is
	// updated, not re-created (AlreadyExists forever) and not ignored.
	now := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	create(t, &mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Namespace: releaseNS, Name: "gd"},
		Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"gd": "yes"}}}})
	create(t, httpRoute("shop", "gd", map[string]string{"gd": "yes"}))
	create(t, &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: releaseNS, Name: "gd"}, Spec: mmv1.MaintenanceWindowSpec{
		Enabled: true, Phases: []mmv1.Phase{{Name: "down", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"gd"}}}}}}})
	stale := &gwapiv1b1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{Namespace: releaseNS, Name: GrantName("shop")}, Spec: gwapiv1b1.ReferenceGrantSpec{
		From: []gwapiv1b1.ReferenceGrantFrom{{Group: "gateway.envoyproxy.io", Kind: "SecurityPolicy", Namespace: "elsewhere"}},
		To:   []gwapiv1b1.ReferenceGrantTo{{Group: "", Kind: "Service"}}}}
	create(t, stale)
	removeOurs(t, "shop")

	r, rec := newReconciler(t, now)
	reconcile(t, r)
	var g gwapiv1b1.ReferenceGrant
	if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(stale), &g); err != nil {
		t.Fatal(err)
	}
	if g.Labels[LabelOwner] != releaseNS || len(g.Spec.From) != 2 || g.Spec.From[0].Namespace != "shop" || g.Spec.From[1].Kind != "EnvoyExtensionPolicy" {
		t.Fatalf("the grant must be repaired, got labels %v from %v (ops %v)", g.Labels, g.Spec.From, rec.take())
	}
	if getSP(t, "shop") == nil {
		t.Fatal("shop must be armed")
	}
}
