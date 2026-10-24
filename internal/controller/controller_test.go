package controller

import (
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

func kinds(as []Action) string {
	var out []string
	for _, a := range as {
		s := string(a.Kind) + ":" + a.Namespace
		if a.Kind == EnsureSP {
			s += "[" + strings.Join(a.Targets, ",") + "]"
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func TestPlanOrder(t *testing.T) {
	cases := []struct {
		name    string
		desired map[string][]string
		actual  Actual
		want    string
	}{
		{"arm two namespaces: grants before policies", map[string][]string{"b": {"web"}, "a": {"api", "web"}}, Actual{},
			"ensure-grant:a ensure-grant:b ensure-securitypolicy:a[api,web] ensure-securitypolicy:b[web]"},
		{"disarm: policy before grant", nil, Actual{Policies: map[string]bool{"a": true}, Grants: map[string]bool{"a": true}},
			"delete-securitypolicy:a delete-referencegrant:a"},
		{"mixed: arm b, keep a, drop c", map[string][]string{"a": {"web"}, "b": {"web"}},
			Actual{Policies: map[string]bool{"a": true, "c": true}, Grants: map[string]bool{"a": true, "c": true}},
			"ensure-grant:a ensure-grant:b ensure-securitypolicy:a[web] ensure-securitypolicy:b[web] delete-securitypolicy:c delete-referencegrant:c"},
		{"stale grant without a policy is collected", nil, Actual{Grants: map[string]bool{"x": true}}, "delete-referencegrant:x"},
		{"nothing to do", nil, Actual{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := kinds(Plan(Desired{Routes: tc.desired}, tc.actual)); got != tc.want {
				t.Fatalf("plan\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestPlanTargetChangeNeverDeletes(t *testing.T) {
	// Changing targetRefs is RDS-only (§5.4): the plan must ensure, not
	// delete and re-create, or the listener drains.
	got := Plan(Desired{Routes: map[string][]string{"a": {"web"}}}, Actual{Policies: map[string]bool{"a": true}, Grants: map[string]bool{"a": true}})
	for _, a := range got {
		if a.Kind == DeleteSP || a.Kind == DeleteGrant {
			t.Fatalf("a target change must not delete: %s", kinds(got))
		}
	}
}

var now = time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)

func mt(t time.Time) *metav1.Time { m := metav1.NewTime(t); return &m }

func window(name string, start *metav1.Time, services ...string) mmv1.MaintenanceWindow {
	return mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: mmv1.MaintenanceWindowSpec{
		Enabled: true,
		Phases:  []mmv1.Phase{{Name: "down", StartsAt: start, Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: services}}}},
	}}
}

func service(name string, lbls map[string]string) mmv1.MaintenanceService {
	return mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: lbls}}}
}

func route(ns, name string, lbls map[string]string, parents ...gwapiv1.ParentReference) snapshot.RouteInfo {
	return snapshot.RouteInfo{Key: snapshot.RouteKey{Namespace: ns, Name: name}, Labels: lbls, Parents: parents}
}

func gw(name string) gwapiv1.ParentReference {
	ns := gwapiv1.Namespace("gw")
	return gwapiv1.ParentReference{Name: gwapiv1.ObjectName(name), Namespace: &ns}
}

func baseInput() Input {
	return Input{
		Windows:  []mmv1.MaintenanceWindow{window("w", nil, "shop")},
		Services: []mmv1.MaintenanceService{service("shop", map[string]string{"app": "shop"})},
		Routes: []snapshot.RouteInfo{
			route("shop", "web", map[string]string{"app": "shop"}, gw("eg")),
			route("shop", "api", map[string]string{"app": "shop"}, gw("eg")),
			route("other", "x", map[string]string{"app": "other"}, gw("eg")),
		},
		Now: now, Lead: 15 * time.Minute,
	}
}

func TestDesiredArming(t *testing.T) {
	in := baseInput()
	d := ComputeDesired(in)
	if !reflect.DeepEqual(d.Routes, map[string][]string{"shop": {"api", "web"}}) {
		t.Fatalf("armed %v", d.Routes)
	}
	// Outside the lead time: not armed yet, but the controller wakes then.
	in.Windows = []mmv1.MaintenanceWindow{window("w", mt(now.Add(time.Hour)), "shop")}
	d = ComputeDesired(in)
	if len(d.Routes) != 0 || !d.NextWake.Equal(now.Add(45*time.Minute)) {
		t.Fatalf("armed %v, next wake %s", d.Routes, d.NextWake)
	}
	// Within the lead time: armed.
	in.Windows = []mmv1.MaintenanceWindow{window("w", mt(now.Add(10*time.Minute)), "shop")}
	if d := ComputeDesired(in); len(d.Routes["shop"]) != 2 {
		t.Fatalf("within the lead time: %v", d.Routes)
	}
	// Disabled and invalid windows never arm.
	w := window("w", nil, "shop")
	w.Spec.Enabled = false
	in.Windows = []mmv1.MaintenanceWindow{w}
	if d := ComputeDesired(in); len(d.Routes) != 0 {
		t.Fatal("a disabled window armed routes")
	}
}

func TestDesiredConflicts(t *testing.T) {
	ref := func(kind, name string) TargetRef { return TargetRef{Group: gatewayGroup, Kind: kind, Name: name} }
	sel := func(kind string, m map[string]string, any bool) TargetSelector {
		return TargetSelector{Kind: kind, Labels: labels.SelectorFromSet(m), AnyNamespace: any}
	}
	cases := []struct {
		name   string
		policy Policy
		armed  []string // armed routes in "shop"
	}{
		{"foreign route policy by name", Policy{Namespace: "shop", Name: "jwt", Refs: []TargetRef{ref("HTTPRoute", "web")}}, []string{"api"}},
		{"same name in another namespace is no conflict", Policy{Namespace: "other", Name: "jwt", Refs: []TargetRef{ref("HTTPRoute", "web")}}, []string{"api", "web"}},
		{"targetSelector in the same namespace", Policy{Namespace: "shop", Name: "s", Selectors: []TargetSelector{sel("HTTPRoute", map[string]string{"app": "shop"}, false)}}, nil},
		{"targetSelector from another namespace, Same", Policy{Namespace: "other", Name: "s", Selectors: []TargetSelector{sel("HTTPRoute", map[string]string{"app": "shop"}, false)}}, []string{"api", "web"}},
		{"targetSelector from another namespace, All", Policy{Namespace: "other", Name: "s", Selectors: []TargetSelector{sel("HTTPRoute", map[string]string{"app": "shop"}, true)}}, nil},
		{"gateway policy without extAuth merges", Policy{Namespace: "gw", Name: "cors", Refs: []TargetRef{ref("Gateway", "eg")}}, []string{"api", "web"}},
		{"gateway policy with extAuth conflicts", Policy{Namespace: "gw", Name: "oidc", ExtAuth: true, Refs: []TargetRef{ref("Gateway", "eg")}}, nil},
		{"extAuth on another gateway is fine", Policy{Namespace: "gw", Name: "oidc", ExtAuth: true, Refs: []TargetRef{ref("Gateway", "other")}}, []string{"api", "web"}},
		{"our own policy is never a conflict", Policy{Namespace: "shop", Name: "mm-x", Ours: true, ExtAuth: true, Refs: []TargetRef{ref("HTTPRoute", "web")}}, []string{"api", "web"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			in.Policies = []Policy{tc.policy}
			d := ComputeDesired(in)
			if !reflect.DeepEqual(d.Routes["shop"], tc.armed) {
				t.Fatalf("armed %v, want %v (conflicts %v)", d.Routes["shop"], tc.armed, d.Conflicts)
			}
		})
	}
}

func TestDesiredListenerSetParent(t *testing.T) {
	in := baseInput()
	lsKind := gwapiv1.Kind("ListenerSet")
	ns := gwapiv1.Namespace("shop")
	in.Routes = []snapshot.RouteInfo{route("shop", "web", map[string]string{"app": "shop"}, gwapiv1.ParentReference{Kind: &lsKind, Name: "ls", Namespace: &ns})}
	in.ListenerSets = map[types.NamespacedName]types.NamespacedName{{Namespace: "shop", Name: "ls"}: {Namespace: "gw", Name: "eg"}}
	in.Policies = []Policy{{Namespace: "gw", Name: "oidc", ExtAuth: true, Refs: []TargetRef{{Group: gatewayGroup, Kind: "Gateway", Name: "eg"}}}}
	if d := ComputeDesired(in); len(d.Routes) != 0 {
		t.Fatalf("extAuth on the ListenerSet's Gateway must conflict: %v", d.Routes)
	}
}

func TestParsePolicy(t *testing.T) {
	sp := &egv1a1.SecurityPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "p"}}
	single := gwapiv1.LocalPolicyTargetReferenceWithSectionName{LocalPolicyTargetReference: gwapiv1.LocalPolicyTargetReference{Group: gatewayGroup, Kind: "HTTPRoute", Name: "web"}}
	sp.Spec.TargetRef = &single //nolint:staticcheck // the deprecated singular form must be honoured
	sp.Spec.TargetSelectors = []egv1a1.TargetSelector{{Kind: "HTTPRoute", MatchLabels: map[string]string{"a": "b"},
		Namespaces: &egv1a1.TargetSelectorNamespaces{From: egv1a1.TargetNamespaceFromAll}}}
	sp.Spec.ExtAuth = &egv1a1.ExtAuth{}
	p := parsePolicy(sp, false)
	if len(p.Refs) != 1 || p.Refs[0].Name != "web" || len(p.Selectors) != 1 || !p.Selectors[0].AnyNamespace || !p.ExtAuth {
		t.Fatalf("parsed %+v", p)
	}
}

func TestDesiredObjects(t *testing.T) {
	sp := DesiredSP("mm", "shop", []string{"api", "web"})
	if sp.GetName() != SPName("mm") || sp.GetNamespace() != "shop" || sp.GetLabels()[LabelOwner] != "mm" {
		t.Fatalf("SP meta %+v", sp.Object["metadata"])
	}
	if v, _, _ := unstructured.NestedString(sp.Object, "spec", "mergeType"); v != "StrategicMerge" {
		t.Fatal("mergeType must be StrategicMerge (§5.1)")
	}
	if v, _, _ := unstructured.NestedBool(sp.Object, "spec", "extAuth", "failOpen"); !v {
		t.Fatal("failOpen must be true")
	}
	if v, _, _ := unstructured.NestedString(sp.Object, "spec", "extAuth", "timeout"); v != "100ms" {
		t.Fatal("timeout must be 100ms")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(sp.Object, "spec", "extAuth", "bodyToExtAuth"); found {
		t.Fatal("bodyToExtAuth must stay unset")
	}
	g := DesiredGrant("mm", "shop")
	if g.GetName() != "mm-from-shop" || g.GetNamespace() != "mm" {
		t.Fatalf("grant meta %s/%s", g.GetNamespace(), g.GetName())
	}
}

func TestNeedsUpdate(t *testing.T) {
	want := DesiredSP("mm", "shop", []string{"web"})
	if needsUpdate(want.DeepCopy(), want, true) {
		t.Fatal("identical objects need no update")
	}
	// The API server adds defaults inside our structures: tolerated.
	same := want.DeepCopy()
	if err := unstructured.SetNestedField(same.Object, "Default", "spec", "extAuth", "grpc", "defaultedField"); err != nil {
		t.Fatal(err)
	}
	if needsUpdate(same, want, true) {
		t.Fatal("server-side defaults inside our structures must not cause updates")
	}
	drift := want.DeepCopy()
	_ = unstructured.SetNestedField(drift.Object, map[string]any{"maxRequestBytes": int64(1024)}, "spec", "extAuth", "bodyToExtAuth")
	if !needsUpdate(drift, want, true) {
		t.Fatal("bodyToExtAuth added by someone else must be removed (§5.1 drift)")
	}
	other := want.DeepCopy()
	_ = unstructured.SetNestedSlice(other.Object, []any{map[string]any{"group": gatewayGroup, "kind": "HTTPRoute", "name": "api"}}, "spec", "targetRefs")
	if !needsUpdate(other, want, true) {
		t.Fatal("different targets need an update")
	}
	label := want.DeepCopy()
	label.SetLabels(map[string]string{LabelManagedBy: managedBy})
	if !needsUpdate(label, want, true) {
		t.Fatal("a missing owner label needs an update")
	}
	float := want.DeepCopy()
	_ = unstructured.SetNestedField(float.Object, float64(9001), "spec", "extAuth", "grpc", "backendRefs")
	if !needsUpdate(float, want, true) {
		t.Fatal("a mangled backendRefs needs an update")
	}
}

func TestEnvoyAccepted(t *testing.T) {
	sp := func(gen int64, conds ...map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{}}
		u.SetGeneration(gen)
		var cs []any
		for _, c := range conds {
			cs = append(cs, c)
		}
		_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"ancestorRef": map[string]any{"name": "eg"}, "conditions": cs}}, "status", "ancestors")
		return u
	}
	cond := func(typ, status string, gen int64) map[string]any {
		return map[string]any{"type": typ, "status": status, "observedGeneration": gen}
	}
	cases := []struct {
		name string
		sp   *unstructured.Unstructured
		want bool
	}{
		{"accepted", sp(2, cond("Accepted", "True", 2)), true},
		{"merged only (EG 1.9 with a Gateway policy)", sp(2, cond("Merged", "True", 2)), true},
		{"stale generation", sp(3, cond("Accepted", "True", 2)), false},
		{"conflicted", sp(2, cond("Accepted", "False", 2)), false},
		{"no conditions yet", sp(1), false},
		{"no ancestors", &unstructured.Unstructured{Object: map[string]any{}}, false},
	}
	for _, tc := range cases {
		if got, why := EnvoyAccepted(tc.sp); got != tc.want {
			t.Errorf("%s: %v (%s), want %v", tc.name, got, why, tc.want)
		}
	}
}
