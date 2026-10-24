package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

// Reconciler computes the desired state and applies the plan. Its readers
// must be synced caches: it never acts on partial data.
type Reconciler struct {
	Namespace string        // release namespace
	Lead      time.Duration // arming lead time
	Shared    client.Reader // our resources (release ns) and HTTPRoutes (cluster-wide)
	Leader    client.Reader // SecurityPolicies, Gateways, ListenerSets (cluster-wide), ReferenceGrants (release ns)
	Writer    client.Writer
	Log       *slog.Logger
	Now       func() time.Time

	// ListenerSets, when non-nil, is the ListenerSet list to read (only if
	// the kind is served).
	ListenerSets client.ObjectList
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Result is what one reconcile did.
type Result struct {
	Desired Desired
	Actions []Action
	Writes  int
}

// Reconcile reads everything, plans and applies. Any read error aborts the
// whole reconcile before a single write: no disarming or GC from partial
// data. A write error stops the remaining steps, so the grant-before-policy
// and policy-before-grant order always holds.
func (r *Reconciler) Reconcile(ctx context.Context) (Result, error) {
	in, actual, existing, err := r.read(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("read: %w", err)
	}
	d := ComputeDesired(in)
	actions := Plan(d, actual)
	res := Result{Desired: d, Actions: actions}
	for _, a := range actions {
		wrote, err := r.apply(ctx, a, existing)
		if wrote {
			res.Writes++
		}
		if err != nil {
			return res, fmt.Errorf("%s %s: %w", a.Kind, a.Namespace, err)
		}
	}
	return res, nil
}

// existingObjects holds our current SecurityPolicies (by app ns) and grants
// (by app ns) as unstructured objects.
type existingObjects struct {
	policies map[string]*unstructured.Unstructured
	grants   map[string]*unstructured.Unstructured
}

func (r *Reconciler) read(ctx context.Context) (Input, Actual, existingObjects, error) {
	var (
		windows  mmv1.MaintenanceWindowList
		services mmv1.MaintenanceServiceList
		routes   gwapiv1.HTTPRouteList
		sps      egv1a1.SecurityPolicyList
		gateways gwapiv1.GatewayList
		grants   unstructured.UnstructuredList
	)
	grants.SetGroupVersionKind(ReferenceGrantGVK.GroupVersion().WithKind("ReferenceGrantList"))
	inNS := client.InNamespace(r.Namespace)
	ex := existingObjects{policies: map[string]*unstructured.Unstructured{}, grants: map[string]*unstructured.Unstructured{}}
	for _, l := range []struct {
		reader client.Reader
		list   client.ObjectList
		opts   []client.ListOption
	}{
		{r.Shared, &windows, []client.ListOption{inNS}},
		{r.Shared, &services, []client.ListOption{inNS}},
		{r.Shared, &routes, nil},
		{r.Leader, &sps, nil},
		{r.Leader, &gateways, nil},
		{r.Leader, &grants, []client.ListOption{inNS, client.MatchingLabels{LabelOwner: r.Namespace}}},
	} {
		if err := l.reader.List(ctx, l.list, l.opts...); err != nil {
			return Input{}, Actual{}, ex, err
		}
	}

	in := Input{
		Windows: windows.Items, Services: services.Items, Now: r.now(), Lead: r.Lead,
		ListenerSets:      map[types.NamespacedName]types.NamespacedName{},
		GatewayLabels:     map[types.NamespacedName]map[string]string{},
		ListenerSetLabels: map[types.NamespacedName]map[string]string{},
	}
	for _, rt := range routes.Items {
		in.Routes = append(in.Routes, snapshot.RouteInfo{
			Key: snapshot.RouteKey{Namespace: rt.Namespace, Name: rt.Name}, Labels: rt.Labels, Parents: rt.Spec.ParentRefs,
		})
	}
	for _, g := range gateways.Items {
		in.GatewayLabels[types.NamespacedName{Namespace: g.Namespace, Name: g.Name}] = g.Labels
	}
	if r.ListenerSets != nil {
		if err := r.Leader.List(ctx, r.ListenerSets); err != nil {
			return Input{}, Actual{}, ex, err
		}
		if err := r.readListenerSets(in); err != nil {
			return Input{}, Actual{}, ex, err
		}
	}

	actual := Actual{Policies: map[string]bool{}, Grants: map[string]bool{}}
	for i := range sps.Items {
		sp := &sps.Items[i]
		ours := sp.Labels[LabelOwner] == r.Namespace && sp.Labels[LabelManagedBy] == managedBy
		if ours {
			if sp.Name != SPName(r.Namespace) {
				continue // ours by label but not by name: leave it, never adopt
			}
			u, err := toUnstructured(sp, SecurityPolicyGVK.Kind)
			if err != nil {
				return Input{}, Actual{}, ex, err
			}
			actual.Policies[sp.Namespace] = true
			ex.policies[sp.Namespace] = u
		}
		in.Policies = append(in.Policies, parsePolicy(sp, ours))
	}
	for i := range grants.Items {
		g := &grants.Items[i]
		from, _, _ := unstructured.NestedSlice(g.Object, "spec", "from")
		if len(from) != 1 {
			continue
		}
		ns, _, _ := unstructured.NestedString(from[0].(map[string]any), "namespace")
		if g.GetName() != GrantName(ns) {
			continue
		}
		actual.Grants[ns] = true
		ex.grants[ns] = g
	}
	return in, actual, ex, nil
}

func (r *Reconciler) readListenerSets(in Input) error {
	u, ok := r.ListenerSets.(*unstructured.UnstructuredList)
	if !ok {
		return errors.New("ListenerSets must be an *unstructured.UnstructuredList")
	}
	for _, ls := range u.Items {
		key := types.NamespacedName{Namespace: ls.GetNamespace(), Name: ls.GetName()}
		in.ListenerSetLabels[key] = ls.GetLabels()
		name, _, _ := unstructured.NestedString(ls.Object, "spec", "parentRef", "name")
		ns, found, _ := unstructured.NestedString(ls.Object, "spec", "parentRef", "namespace")
		if !found || ns == "" {
			ns = ls.GetNamespace()
		}
		if name != "" {
			in.ListenerSets[key] = types.NamespacedName{Namespace: ns, Name: name}
		}
	}
	return nil
}

// toUnstructured goes through JSON, not the unstructured converter: the
// converter turns unsigned fields (consecutiveGatewayErrors) into uint64,
// which unstructured DeepCopy panics on.
func toUnstructured(obj runtime.Object, kind string) (*unstructured.Unstructured, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetGroupVersionKind(SecurityPolicyGVK.GroupVersion().WithKind(kind))
	return u, nil
}

// parsePolicy extracts targets, selectors and extAuth from a SecurityPolicy.
// GetTargetRefs covers the deprecated singular targetRef too. A selector
// that does not parse matches nothing, as in Envoy Gateway; an empty one
// matches everything.
func parsePolicy(sp *egv1a1.SecurityPolicy, ours bool) Policy {
	p := Policy{Namespace: sp.Namespace, Name: sp.Name, Ours: ours, ExtAuth: sp.Spec.ExtAuth != nil}
	for _, ref := range sp.Spec.GetTargetRefs() {
		p.Refs = append(p.Refs, TargetRef{Group: string(ref.Group), Kind: string(ref.Kind), Name: string(ref.Name)})
	}
	for _, s := range sp.Spec.TargetSelectors {
		sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: s.MatchLabels, MatchExpressions: s.MatchExpressions})
		if err != nil {
			continue
		}
		group := ""
		if s.Group != nil {
			group = string(*s.Group)
		}
		// from All, or from Selector (we have no RBAC on namespace labels):
		// treated as every namespace. Over-matching means not armed, which
		// fails open.
		anyNS := s.Namespaces != nil && s.Namespaces.From != "" && s.Namespaces.From != egv1a1.TargetNamespaceFromSame
		p.Selectors = append(p.Selectors, TargetSelector{Group: group, Kind: string(s.Kind), Labels: sel, AnyNamespace: anyNS})
	}
	return p
}
