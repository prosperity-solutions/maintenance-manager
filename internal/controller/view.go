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
	// armed or not, so the editor can show red before a window arms.
	Conflicts map[snapshot.RouteKey][]string
	// Accepted is, per app namespace that has our SecurityPolicy, whether
	// Envoy Gateway accepted it (§10 "Envoy Accepted"), and why not.
	Accepted map[string]Acceptance
	// Targets is, per app namespace, the HTTPRoutes our SecurityPolicy
	// targets right now: what is armed, as opposed to what Desired says
	// the leader will arm.
	Targets map[string]map[string]bool
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
	v := View{Desired: ComputeDesired(in), Conflicts: map[snapshot.RouteKey][]string{}, Accepted: map[string]Acceptance{}, Targets: map[string]map[string]bool{}}
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
		if why, blocked := in.Blocked[rt.Key.Namespace]; blocked {
			v.Conflicts[rt.Key] = []string{why}
		} else if reasons := conflicts(rt, in); len(reasons) > 0 {
			v.Conflicts[rt.Key] = reasons
		}
	}
	for ns, sp := range ex.policies {
		ok, why := EnvoyAccepted(sp)
		v.Accepted[ns] = Acceptance{OK: ok, Reason: why}
		refs, _, _ := unstructured.NestedSlice(sp.Object, "spec", "targetRefs")
		v.Targets[ns] = map[string]bool{}
		for _, ref := range refs {
			if m, ok := ref.(map[string]any); ok && m["kind"] == "HTTPRoute" {
				if name, ok := m["name"].(string); ok {
					v.Targets[ns][name] = true
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
