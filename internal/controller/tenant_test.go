package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/types"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

func TestDesiredXListenerSetParent(t *testing.T) {
	in := baseInput()
	kind := gwapiv1.Kind("XListenerSet")
	group := gwapiv1.Group("gateway.networking.x-k8s.io")
	ns := gwapiv1.Namespace("shop")
	in.Routes = []snapshot.RouteInfo{route("shop", "web", map[string]string{"app": "shop"}, gwapiv1.ParentReference{Group: &group, Kind: &kind, Name: "xls", Namespace: &ns})}
	in.ListenerSets = map[types.NamespacedName]types.NamespacedName{{Namespace: "shop", Name: "xls"}: {Namespace: "gw", Name: "eg"}}
	in.Policies = []Policy{{Namespace: "gw", Name: "oidc", ExtAuth: true, Refs: []TargetRef{{Group: gatewayGroup, Kind: "Gateway", Name: "eg"}}}}
	if d := ComputeDesired(in); len(d.Routes) != 0 {
		t.Fatalf("extAuth on an XListenerSet's Gateway must conflict: %v", d.Routes)
	}
}

func TestDesiredSquatterBlocksNamespace(t *testing.T) {
	in := baseInput()
	in.Blocked = map[string]string{"shop": "squatted"}
	d := ComputeDesired(in)
	if len(d.Routes) != 0 || len(d.Conflicts) != 2 {
		t.Fatalf("a blocked namespace arms nothing and reports every route: %v %v", d.Routes, d.Conflicts)
	}
}

func TestOnlyRaces(t *testing.T) {
	if onlyRaces(nil) {
		t.Fatal("no error is not a race")
	}
}

func TestRelevantUpdate(t *testing.T) {
	ns := func(l map[string]string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop", Labels: l}}
	}
	if !relevantUpdate(ns(nil), ns(map[string]string{"team": "a"})) {
		t.Fatal("a namespace label change must trigger (namespace selectors)")
	}
	if relevantUpdate(ns(map[string]string{"team": "a"}), ns(map[string]string{"team": "a"})) {
		t.Fatal("a status-only update must not trigger")
	}
	o, n := ns(nil), ns(nil)
	n.Generation = 2
	if !relevantUpdate(o, n) {
		t.Fatal("a spec change (generation) must trigger")
	}
}
