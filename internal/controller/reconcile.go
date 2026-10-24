package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
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

	// ListenerSets name the ListenerSet / XListenerSet list kinds to read, one
	// per served kind (only their GroupVersionKind is used).
	ListenerSets []*unstructured.UnstructuredList
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
	// A failed write affects only its namespace, and within it only its
	// policy kind: a failed grant skips both policies (never one without its
	// grant), a failed policy step skips the grant deletion (never a grant
	// deleted while a policy may remain), and the other kind carries on.
	// Every other namespace carries on: one tenant's broken object cannot
	// stall the cluster.
	failed := map[string]bool{}
	var errs []error
	for _, a := range actions {
		if skipAfterFailure(failed, a) {
			continue
		}
		wrote, err := r.apply(ctx, a, existing)
		if wrote {
			res.Writes++
		}
		if err != nil {
			failed[failureKey(a)] = true
			errs = append(errs, fmt.Errorf("%s %s: %w", a.Kind, a.Namespace, err))
		}
	}
	return res, errors.Join(errs...)
}

// failureKey is the scope a failed step blocks: the namespace for a grant,
// the namespace and kind for a policy.
func failureKey(a Action) string {
	switch a.Kind {
	case EnsureSP, DeleteSP:
		return a.Namespace + "/sp"
	case EnsureEEP, DeleteEEP:
		return a.Namespace + "/eep"
	}
	return a.Namespace
}

func skipAfterFailure(failed map[string]bool, a Action) bool {
	if failed[a.Namespace] {
		return true
	}
	if a.Kind == DeleteGrant {
		return failed[a.Namespace+"/sp"] || failed[a.Namespace+"/eep"]
	}
	return failed[failureKey(a)]
}

// existingObjects holds our current SecurityPolicies, EnvoyExtensionPolicies
// and grants, by app namespace, as unstructured objects.
type existingObjects struct {
	policies   map[string]*unstructured.Unstructured
	extensions map[string]*unstructured.Unstructured
	grants     map[string]*unstructured.Unstructured
}

func (r *Reconciler) read(ctx context.Context) (Input, Actual, existingObjects, error) {
	var (
		windows    mmv1.MaintenanceWindowList
		services   mmv1.MaintenanceServiceList
		routes     gwapiv1.HTTPRouteList
		sps        egv1a1.SecurityPolicyList
		eeps       egv1a1.EnvoyExtensionPolicyList
		gateways   gwapiv1.GatewayList
		grants     unstructured.UnstructuredList
		namespaces corev1.NamespaceList
	)
	grants.SetGroupVersionKind(ReferenceGrantGVK.GroupVersion().WithKind("ReferenceGrantList"))
	inNS := client.InNamespace(r.Namespace)
	ex := existingObjects{policies: map[string]*unstructured.Unstructured{}, extensions: map[string]*unstructured.Unstructured{}, grants: map[string]*unstructured.Unstructured{}}
	for _, l := range []struct {
		reader client.Reader
		list   client.ObjectList
		opts   []client.ListOption
	}{
		{r.Shared, &windows, []client.ListOption{inNS}},
		{r.Shared, &services, []client.ListOption{inNS}},
		{r.Shared, &routes, nil},
		{r.Leader, &sps, nil},
		{r.Leader, &eeps, nil},
		{r.Leader, &gateways, nil},
		{r.Leader, &namespaces, nil},
		{r.Leader, &grants, []client.ListOption{inNS}},
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
		NamespaceLabels:   map[string]map[string]string{},
		Blocked:           map[string]string{},
		BlockedExt:        map[string]string{},
		ExtArmed:          map[snapshot.RouteKey]bool{},
	}
	for _, n := range namespaces.Items {
		in.NamespaceLabels[n.Name] = n.Labels
	}
	for _, rt := range routes.Items {
		in.Routes = append(in.Routes, snapshot.RouteInfo{
			Key: snapshot.RouteKey{Namespace: rt.Namespace, Name: rt.Name}, Labels: rt.Labels, Parents: rt.Spec.ParentRefs,
		})
	}
	for _, g := range gateways.Items {
		in.GatewayLabels[types.NamespacedName{Namespace: g.Namespace, Name: g.Name}] = g.Labels
	}
	for _, kind := range r.ListenerSets {
		// A fresh list per read, so View is safe for concurrent use.
		l := &unstructured.UnstructuredList{}
		l.SetGroupVersionKind(kind.GroupVersionKind())
		if err := r.Leader.List(ctx, l); err != nil {
			return Input{}, Actual{}, ex, err
		}
		readListenerSets(in, l)
	}

	actual := Actual{Policies: map[string]bool{}, Extensions: map[string]bool{}, Grants: map[string]bool{}}
	for i := range sps.Items {
		sp := &sps.Items[i]
		// Ours means our name AND our owner label (a lost managed-by label is
		// drift, repaired by the update). Anything else, labelled or not, is
		// foreign and goes through the conflict check.
		ours := sp.Name == SPName(r.Namespace) && sp.Labels[LabelOwner] == r.Namespace
		if !ours && sp.Name == SPName(r.Namespace) {
			in.Blocked[sp.Namespace] = fmt.Sprintf("a SecurityPolicy %s/%s that is not ours has our name; maintenance-manager cannot arm this namespace until it is renamed or removed", sp.Namespace, sp.Name)
		}
		if ours {
			u, err := toUnstructured(sp, SecurityPolicyGVK.Kind)
			if err != nil {
				return Input{}, Actual{}, ex, err
			}
			actual.Policies[sp.Namespace] = true
			ex.policies[sp.Namespace] = u
		}
		in.Policies = append(in.Policies, parsePolicy(sp, ours))
	}
	for i := range eeps.Items {
		eep := &eeps.Items[i]
		ours := eep.Name == SPName(r.Namespace) && eep.Labels[LabelOwner] == r.Namespace
		if !ours && eep.Name == SPName(r.Namespace) {
			in.BlockedExt[eep.Namespace] = fmt.Sprintf("an EnvoyExtensionPolicy %s/%s that is not ours has our name; maintenance-manager cannot arm this namespace through one until it is renamed or removed", eep.Namespace, eep.Name)
		}
		p := parseTargets(eep.Namespace, eep.Name, ours, eep.Spec.PolicyTargetReferences)
		if ours {
			u, err := toUnstructured(eep, ExtensionPolicyGVK.Kind)
			if err != nil {
				return Input{}, Actual{}, ex, err
			}
			actual.Extensions[eep.Namespace] = true
			ex.extensions[eep.Namespace] = u
			for _, ref := range p.Refs {
				if ref.Kind == "HTTPRoute" {
					in.ExtArmed[snapshot.RouteKey{Namespace: eep.Namespace, Name: ref.Name}] = true
				}
			}
		}
		in.Extensions = append(in.Extensions, p)
	}
	// The release namespace is ours, and so is every mm-from-<ns> grant in it,
	// whatever its labels or spec say: keying by name lets the update repair
	// drift instead of a create that fails with AlreadyExists forever.
	for i := range grants.Items {
		g := &grants.Items[i]
		ns, ok := strings.CutPrefix(g.GetName(), grantPrefix)
		if !ok || ns == "" {
			continue
		}
		actual.Grants[ns] = true
		ex.grants[ns] = g
	}
	return in, actual, ex, nil
}

func readListenerSets(in Input, u *unstructured.UnstructuredList) {
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
func parsePolicy(sp *egv1a1.SecurityPolicy, ours bool) Policy {
	p := parseTargets(sp.Namespace, sp.Name, ours, sp.Spec.PolicyTargetReferences)
	p.ExtAuth = sp.Spec.ExtAuth != nil
	return p
}

// parseTargets extracts the targets and selectors of a policy.
// GetTargetRefs covers the deprecated singular targetRef too. A selector
// that does not parse matches nothing, as in Envoy Gateway; an empty one
// matches everything.
func parseTargets(ns, name string, ours bool, refs egv1a1.PolicyTargetReferences) Policy {
	p := Policy{Namespace: ns, Name: name, Ours: ours}
	for _, ref := range refs.GetTargetRefs() {
		p.Refs = append(p.Refs, TargetRef{Group: string(ref.Group), Kind: string(ref.Kind), Name: string(ref.Name)})
	}
	for _, s := range refs.TargetSelectors {
		sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: s.MatchLabels, MatchExpressions: s.MatchExpressions})
		if err != nil {
			continue
		}
		group := ""
		if s.Group != nil {
			group = string(*s.Group)
		}
		ts := TargetSelector{Group: group, Kind: string(s.Kind), Labels: sel}
		if s.Namespaces != nil {
			switch s.Namespaces.From {
			case egv1a1.TargetNamespaceFromAll:
				ts.AnyNamespace = true
			case egv1a1.TargetNamespaceFromSelector:
				// Evaluated against namespace labels, like Envoy Gateway does; a
				// selector that does not parse matches nothing.
				ts.NamespaceSelector = labels.Nothing()
				if s.Namespaces.Selector != nil {
					if nsSel, err := metav1.LabelSelectorAsSelector(s.Namespaces.Selector); err == nil {
						ts.NamespaceSelector = nsSel
					}
				}
			}
		}
		p.Selectors = append(p.Selectors, ts)
	}
	return p
}
