package controller

import (
	"testing"

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
