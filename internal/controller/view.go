package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

// View is a read-only picture of what the controller does, for the editor's
// dashboard and services screen. It never writes.
type View struct {
	Desired Desired
	// Conflicts holds the §5.3 reasons for every route a service selects,
	// armed or not, that neither of our policies can arm, so the editor can
	// show red before a window arms.
	Conflicts map[snapshot.RouteKey][]string
	// Via says, for every selected route that would be armed through our
	// EnvoyExtensionPolicy, why.
	Via map[snapshot.RouteKey]string
	// Accepted is, per app namespace and policy kind (SecurityPolicy,
	// EnvoyExtensionPolicy) we have there, whether Envoy Gateway accepted it
	// (§10 "Envoy Accepted"), and why not.
	Accepted map[PolicyRef]Acceptance
	// Targets is, per app namespace, the HTTPRoutes our policies target
	// right now, with the kind of the policy: what is armed, as opposed to
	// what Desired says the leader will arm.
	Targets map[string]map[string]string
}

// PolicyRef names one of our policies: its app namespace and kind.
type PolicyRef struct {
	Namespace, Kind string
}

// Acceptance is Envoy Gateway's verdict on one of our SecurityPolicies.
type Acceptance struct {
	OK     bool
	Reason string
}

// View reads like a reconcile and computes the desired state, without
// planning or writing. Safe for concurrent use; Writer may be nil.
func (r *Reconciler) View(ctx context.Context) (View, error) {
	in, _, ex, err := r.read(ctx)
	if err != nil {
		return View{}, fmt.Errorf("read: %w", err)
	}
	v := View{Desired: ComputeDesired(in), Conflicts: map[snapshot.RouteKey][]string{}, Via: map[snapshot.RouteKey]string{},
		Accepted: map[PolicyRef]Acceptance{}, Targets: map[string]map[string]string{}}
	selectors := snapshot.Selectors(in.Services)
	for _, rt := range in.Routes {
		matched := false
		for _, sel := range selectors {
			if sel.Matches(labels.Set(rt.Labels)) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		switch mode, reasons, via := attach(rt, in); mode {
		case ModeNone:
			v.Conflicts[rt.Key] = reasons
		case ModeExtensionPolicy:
			v.Via[rt.Key] = via
		}
	}
	for _, objs := range []map[string]*unstructured.Unstructured{ex.policies, ex.extensions} {
		for ns, obj := range objs {
			ok, why := EnvoyAccepted(obj)
			v.Accepted[PolicyRef{Namespace: ns, Kind: obj.GetKind()}] = Acceptance{OK: ok, Reason: why}
			refs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "targetRefs")
			if v.Targets[ns] == nil {
				v.Targets[ns] = map[string]string{}
			}
			for _, ref := range refs {
				if m, ok := ref.(map[string]any); ok && m["kind"] == "HTTPRoute" {
					if name, ok := m["name"].(string); ok {
						v.Targets[ns][name] = obj.GetKind()
					}
				}
			}
		}
	}
	return v, nil
}

// ListenerSetKinds returns one empty list per ListenerSet kind the API
// server serves, for Reconciler.ListenerSets. Only a "no match" means a kind
// is absent; any other discovery error is returned.
func ListenerSetKinds(mapper meta.RESTMapper) ([]*unstructured.UnstructuredList, error) {
	var out []*unstructured.UnstructuredList
	for _, gvk := range listenerSetGVKs {
		_, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		switch {
		case err == nil:
			l := &unstructured.UnstructuredList{}
			l.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
			out = append(out, l)
		case meta.IsNoMatchError(err):
		default:
			return nil, fmt.Errorf("discover %s: %w", gvk.Kind, err)
		}
	}
	return out, nil
}
