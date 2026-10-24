package controller

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"

	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

// The ladder of §5.3: our SecurityPolicy when the route's slot is free, else
// our EnvoyExtensionPolicy when that slot is free, else a conflict.
func TestDesiredLadder(t *testing.T) {
	ref := func(kind, name string) TargetRef { return TargetRef{Group: gatewayGroup, Kind: kind, Name: name} }
	routeSP := Policy{Namespace: "shop", Name: "ip-allow", Refs: []TargetRef{ref("HTTPRoute", "web")}}
	cases := []struct {
		name          string
		policies, ext []Policy
		sp, eep       []string // armed routes in "shop"
		conflicts     []string // routes not armed
	}{
		{"free routes use the SecurityPolicy", nil, nil, []string{"api", "web"}, nil, nil},
		{"a foreign route SecurityPolicy moves that route to the EnvoyExtensionPolicy", []Policy{routeSP}, nil, []string{"api"}, []string{"web"}, nil},
		{"a foreign extAuth on the Gateway moves every route", []Policy{{Namespace: "gw", Name: "oidc", ExtAuth: true, Refs: []TargetRef{ref("Gateway", "eg")}}}, nil,
			nil, []string{"api", "web"}, nil},
		{"a foreign route EnvoyExtensionPolicy is no obstacle while the SecurityPolicy slot is free", nil,
			[]Policy{{Namespace: "shop", Name: "lua", Refs: []TargetRef{ref("HTTPRoute", "web")}}}, []string{"api", "web"}, nil, nil},
		{"both slots taken: conflict", []Policy{routeSP}, []Policy{{Namespace: "shop", Name: "lua", Refs: []TargetRef{ref("HTTPRoute", "web")}}},
			[]string{"api"}, nil, []string{"web"}},
		{"a foreign EnvoyExtensionPolicy on the Gateway would be replaced: conflict", []Policy{routeSP},
			[]Policy{{Namespace: "gw", Name: "wasm", Refs: []TargetRef{ref("Gateway", "eg")}}}, []string{"api"}, nil, []string{"web"}},
		{"a foreign EnvoyExtensionPolicy by selector", []Policy{routeSP},
			[]Policy{{Namespace: "shop", Name: "s", Selectors: []TargetSelector{{Kind: "HTTPRoute", Labels: labels.SelectorFromSet(map[string]string{"app": "shop"})}}}},
			[]string{"api"}, nil, []string{"web"}},
		{"our own EnvoyExtensionPolicy is never a conflict", []Policy{routeSP},
			[]Policy{{Namespace: "shop", Name: SPName("mm"), Ours: true, Refs: []TargetRef{ref("HTTPRoute", "web")}}}, []string{"api"}, []string{"web"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			in.Policies, in.Extensions = tc.policies, tc.ext
			d := ComputeDesired(in)
			if !reflect.DeepEqual(d.Routes["shop"], tc.sp) || !reflect.DeepEqual(d.ExtRoutes["shop"], tc.eep) {
				t.Fatalf("SecurityPolicy %v, EnvoyExtensionPolicy %v; want %v, %v (conflicts %v)", d.Routes["shop"], d.ExtRoutes["shop"], tc.sp, tc.eep, d.Conflicts)
			}
			var got []string
			for k := range d.Conflicts {
				got = append(got, k.Name)
			}
			if len(got) != len(tc.conflicts) || (len(got) == 1 && got[0] != tc.conflicts[0]) {
				t.Fatalf("conflicts %v, want %v", d.Conflicts, tc.conflicts)
			}
			for _, name := range tc.eep {
				if via := d.Via[snapshot.RouteKey{Namespace: "shop", Name: name}]; !strings.Contains(via, "EnvoyExtensionPolicy") {
					t.Fatalf("%s: no explanation: %q", name, via)
				}
			}
		})
	}
}

// A route on our EnvoyExtensionPolicy stays there while it can, even once
// its SecurityPolicy slot is free again: every move adds or removes a filter
// and drains the listener (§5.4).
func TestDesiredStickyExtension(t *testing.T) {
	in := baseInput()
	in.ExtArmed = map[snapshot.RouteKey]bool{{Namespace: "shop", Name: "web"}: true}
	d := ComputeDesired(in)
	if !reflect.DeepEqual(d.Routes["shop"], []string{"api"}) || !reflect.DeepEqual(d.ExtRoutes["shop"], []string{"web"}) {
		t.Fatalf("SecurityPolicy %v, EnvoyExtensionPolicy %v", d.Routes, d.ExtRoutes)
	}
	// Once the extension slot is taken, it moves (or conflicts) after all.
	in.Extensions = []Policy{{Namespace: "shop", Name: "lua", Refs: []TargetRef{{Group: gatewayGroup, Kind: "HTTPRoute", Name: "web"}}}}
	d = ComputeDesired(in)
	if !reflect.DeepEqual(d.Routes["shop"], []string{"api", "web"}) || len(d.ExtRoutes) != 0 {
		t.Fatalf("SecurityPolicy %v, EnvoyExtensionPolicy %v", d.Routes, d.ExtRoutes)
	}
}

func TestPlanWithExtensions(t *testing.T) {
	cases := []struct {
		name   string
		d      Desired
		actual Actual
		want   string
	}{
		{"both kinds in one namespace: one grant first", Desired{Routes: map[string][]string{"a": {"api"}}, ExtRoutes: map[string][]string{"a": {"web"}}}, Actual{},
			"ensure-grant:a ensure-securitypolicy:a[api] ensure-envoyextensionpolicy:a[web]"},
		{"only extensions", Desired{ExtRoutes: map[string][]string{"b": {"web"}}}, Actual{},
			"ensure-grant:b ensure-envoyextensionpolicy:b[web]"},
		{"move from SecurityPolicy to EnvoyExtensionPolicy keeps the grant", Desired{ExtRoutes: map[string][]string{"a": {"web"}}},
			Actual{Policies: map[string]bool{"a": true}, Grants: map[string]bool{"a": true}},
			"ensure-grant:a ensure-envoyextensionpolicy:a[web] delete-securitypolicy:a"},
		{"disarm both: policies before the grant", Desired{},
			Actual{Policies: map[string]bool{"a": true}, Extensions: map[string]bool{"a": true}, Grants: map[string]bool{"a": true}},
			"delete-securitypolicy:a delete-envoyextensionpolicy:a delete-referencegrant:a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := kinds(Plan(tc.d, tc.actual)); got != tc.want {
				t.Fatalf("plan\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestFailureIsolationByKind(t *testing.T) {
	failed := map[string]bool{}
	failed[failureKey(Action{Kind: EnsureSP, Namespace: "a"})] = true
	if skipAfterFailure(failed, Action{Kind: EnsureEEP, Namespace: "a"}) {
		t.Fatal("a failed SecurityPolicy must not stop the EnvoyExtensionPolicy")
	}
	if !skipAfterFailure(failed, Action{Kind: DeleteGrant, Namespace: "a"}) {
		t.Fatal("a failed policy step must keep the grant")
	}
	if skipAfterFailure(failed, Action{Kind: EnsureSP, Namespace: "b"}) {
		t.Fatal("other namespaces carry on")
	}
	failed[failureKey(Action{Kind: EnsureGrant, Namespace: "c"})] = true
	for _, k := range []ActionKind{EnsureSP, EnsureEEP} {
		if !skipAfterFailure(failed, Action{Kind: k, Namespace: "c"}) {
			t.Fatalf("no %s without its grant", k)
		}
	}
}

func TestDesiredExtensionObject(t *testing.T) {
	eep := DesiredEEP("mm", "shop", []string{"api", "web"})
	if eep.GetKind() != "EnvoyExtensionPolicy" || eep.GetName() != SPName("mm") || eep.GetLabels()[LabelOwner] != "mm" {
		t.Fatalf("EEP meta %+v", eep.Object["metadata"])
	}
	procs, _, _ := unstructured.NestedSlice(eep.Object, "spec", "extProc")
	if len(procs) != 1 {
		t.Fatalf("one ext_proc filter, got %d", len(procs))
	}
	p := procs[0].(map[string]any)
	if p["failOpen"] != true || p["messageTimeout"] != "100ms" {
		t.Fatal("failOpen and the 100ms timeout, as the SecurityPolicy")
	}
	if attrs, _, _ := unstructured.NestedSlice(p, "processingMode", "request", "attributes"); len(attrs) != 1 || attrs[0] != RouteNameAttribute {
		t.Fatalf("attributes %v", attrs)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(eep.Object, "spec", "mergeType"); found {
		t.Fatal("no mergeType: Envoy Gateway 1.8 has none for EnvoyExtensionPolicies")
	}
	from, _, _ := unstructured.NestedSlice(DesiredGrant("mm", "shop").Object, "spec", "from")
	if len(from) != 2 || from[1].(map[string]any)["kind"] != "EnvoyExtensionPolicy" {
		t.Fatalf("the grant must admit both policies: %v", from)
	}
}

func TestNeedsUpdateExtension(t *testing.T) {
	want := DesiredEEP("mm", "shop", []string{"web"})
	if needsUpdate(want.DeepCopy(), want, true) {
		t.Fatal("identical objects need no update")
	}
	for name, mutate := range map[string]func(*unstructured.Unstructured){
		"a request body mode": func(u *unstructured.Unstructured) {
			procs, _, _ := unstructured.NestedSlice(u.Object, "spec", "extProc")
			_ = unstructured.SetNestedField(procs[0].(map[string]any), "Buffered", "processingMode", "request", "body")
			_ = unstructured.SetNestedSlice(u.Object, procs, "spec", "extProc")
		},
		"a response body mode": func(u *unstructured.Unstructured) {
			procs, _, _ := unstructured.NestedSlice(u.Object, "spec", "extProc")
			_ = unstructured.SetNestedField(procs[0].(map[string]any), "Streamed", "processingMode", "response", "body")
			_ = unstructured.SetNestedSlice(u.Object, procs, "spec", "extProc")
		},
		"a metadata block": func(u *unstructured.Unstructured) {
			procs, _, _ := unstructured.NestedSlice(u.Object, "spec", "extProc")
			procs[0].(map[string]any)["metadata"] = map[string]any{"writableNamespaces": []any{"x"}}
			_ = unstructured.SetNestedSlice(u.Object, procs, "spec", "extProc")
		},
		"a second ext_proc": func(u *unstructured.Unstructured) {
			procs, _, _ := unstructured.NestedSlice(u.Object, "spec", "extProc")
			_ = unstructured.SetNestedSlice(u.Object, append(procs, procs[0]), "spec", "extProc")
		},
		"a Lua filter": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Inline"}}, "spec", "lua")
		},
		"a mergeType": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "StrategicMerge", "spec", "mergeType")
		},
	} {
		drift := want.DeepCopy()
		mutate(drift)
		if !needsUpdate(drift, want, true) {
			t.Errorf("%s added by someone else must be removed (§5.1 drift)", name)
		}
	}
}

func TestStripForeignExtension(t *testing.T) {
	strip := stripForeignExtension("mm")
	foreign := &egv1a1.EnvoyExtensionPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "lua",
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "x"}}}}
	foreign.Spec.TargetRefs = []gwapiv1.LocalPolicyTargetReferenceWithSectionName{{LocalPolicyTargetReference: gwapiv1.LocalPolicyTargetReference{Kind: "HTTPRoute", Name: "web"}}}
	foreign.Spec.Lua = []egv1a1.Lua{{}}
	out, err := strip(foreign)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(*egv1a1.EnvoyExtensionPolicy)
	if len(got.Spec.TargetRefs) != 1 || got.Spec.Lua != nil || got.ManagedFields != nil {
		t.Fatalf("foreign: keep the targets only: %+v", got.Spec)
	}
	ours := &egv1a1.EnvoyExtensionPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: SPName("mm"), Labels: map[string]string{LabelOwner: "mm"}}}
	ours.Spec.ExtProc = []egv1a1.ExtProc{{}}
	out, _ = strip(ours)
	if len(out.(*egv1a1.EnvoyExtensionPolicy).Spec.ExtProc) != 1 {
		t.Fatal("ours stays whole")
	}
	if _, err := strip("not a policy"); err != nil {
		t.Fatal(errors.New("other objects pass through"))
	}
}

// A foreign EnvoyExtensionPolicy on the route's ListenerSet (or its
// listener) would be replaced by ours on the route: a conflict.
func TestDesiredExtensionParentListenerSet(t *testing.T) {
	in := baseInput()
	lsKind := gwapiv1.Kind("ListenerSet")
	ns := gwapiv1.Namespace("shop")
	in.Routes = []snapshot.RouteInfo{route("shop", "web", map[string]string{"app": "shop"}, gwapiv1.ParentReference{Kind: &lsKind, Name: "ls", Namespace: &ns})}
	in.Policies = []Policy{{Namespace: "shop", Name: "ip", Refs: []TargetRef{{Group: gatewayGroup, Kind: "HTTPRoute", Name: "web"}}}}
	in.Extensions = []Policy{{Namespace: "shop", Name: "wasm", Refs: []TargetRef{{Group: gatewayGroup, Kind: "ListenerSet", Name: "ls", SectionName: "https"}}}}
	d := ComputeDesired(in)
	if len(d.ExtRoutes) != 0 || len(d.Conflicts) != 1 {
		t.Fatalf("a foreign EnvoyExtensionPolicy on the ListenerSet must conflict: %v %v", d.ExtRoutes, d.Conflicts)
	}
}

// Sticky with a squatted SecurityPolicy name: the route stays on the
// EnvoyExtensionPolicy and says why.
func TestDesiredStickyWithSquatter(t *testing.T) {
	in := baseInput()
	in.Blocked = map[string]string{"shop": "squatted"}
	in.ExtArmed = map[snapshot.RouteKey]bool{{Namespace: "shop", Name: "web"}: true}
	d := ComputeDesired(in)
	if len(d.ExtRoutes["shop"]) != 2 || !strings.Contains(d.Via[snapshot.RouteKey{Namespace: "shop", Name: "web"}], "squatted") {
		t.Fatalf("EnvoyExtensionPolicy %v, via %v", d.ExtRoutes, d.Via)
	}
}

func TestArmedCount(t *testing.T) {
	d := Desired{Routes: map[string][]string{"a": {"x", "y"}}, ExtRoutes: map[string][]string{"a": {"z"}, "b": {"w"}}}
	if armedCount(d) != 4 || armedNamespaces(d) != 2 {
		t.Fatalf("armed %d in %d namespaces, want 4 in 2", armedCount(d), armedNamespaces(d))
	}
}
