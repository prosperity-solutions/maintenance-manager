// Package controller arms and disarms routes: per app namespace with armed
// routes, one ReferenceGrant and one SecurityPolicy, plus one
// EnvoyExtensionPolicy for routes whose SecurityPolicy slot another policy
// holds (docs/HANDOVER.md §5).
// Desired and Plan are pure; Reconciler applies the plan; Controller runs it
// on the leader only.
package controller

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

const gatewayGroup = "gateway.networking.k8s.io"

// TargetRef is one local target of a SecurityPolicy (in the policy's
// namespace).
type TargetRef struct {
	Group, Kind, Name, SectionName string
}

// TargetSelector is one label selector target of a SecurityPolicy.
type TargetSelector struct {
	Group, Kind  string
	Labels       labels.Selector
	AnyNamespace bool // namespaces.from: All
	// NamespaceSelector is set for namespaces.from: Selector, evaluated
	// against namespace labels; nil means the policy's own namespace (Same).
	NamespaceSelector labels.Selector
}

// Policy is what the conflict check needs from a SecurityPolicy or an
// EnvoyExtensionPolicy (ExtAuth is false for the latter).
type Policy struct {
	Namespace, Name string
	Ours            bool // carries our owner label value
	Refs            []TargetRef
	Selectors       []TargetSelector
	ExtAuth         bool
}

// Parent is a Gateway or ListenerSet a route attaches to.
type Parent struct {
	Kind        string // Gateway or ListenerSet
	Namespace   string
	Name        string
	SectionName string
}

// Input is everything Desired reads.
type Input struct {
	Windows  []mmv1.MaintenanceWindow
	Services []mmv1.MaintenanceService
	Routes   []snapshot.RouteInfo
	Policies []Policy
	// Extensions are the EnvoyExtensionPolicies, ours included.
	Extensions []Policy
	// ExtArmed holds the routes our EnvoyExtensionPolicies target now: they
	// stay on it while it is possible, so a route does not flip between the
	// two policies (each flip drains the listener, §5.4).
	ExtArmed map[snapshot.RouteKey]bool
	// ListenerSets maps a ListenerSet to the Gateway it belongs to.
	ListenerSets map[types.NamespacedName]types.NamespacedName
	// GatewayLabels and ListenerSetLabels serve targetSelectors on parents.
	GatewayLabels     map[types.NamespacedName]map[string]string
	ListenerSetLabels map[types.NamespacedName]map[string]string
	// NamespaceLabels serve namespaces.from: Selector.
	NamespaceLabels map[string]map[string]string
	// Blocked and BlockedExt hold app namespaces where a foreign
	// SecurityPolicy or EnvoyExtensionPolicy squats on our name, with the
	// reason.
	Blocked    map[string]string
	BlockedExt map[string]string
	Now        time.Time
	Lead       time.Duration
}

// Desired is the arm set: per app namespace, the sorted names of the routes
// to arm through our SecurityPolicy (Routes) and through our
// EnvoyExtensionPolicy (ExtRoutes).
type Desired struct {
	Routes    map[string][]string
	ExtRoutes map[string][]string
	Conflicts map[snapshot.RouteKey][]string // why a route is not armed
	Via       map[snapshot.RouteKey]string   // why a route is armed through the EnvoyExtensionPolicy
	NextWake  time.Time                      // next arm boundary, zero if none
}

// Mode is how a route is armed.
type Mode int

const (
	ModeNone            Mode = iota // not armed: Conflicts says why
	ModeSecurityPolicy              // our SecurityPolicy (ext_authz)
	ModeExtensionPolicy             // our EnvoyExtensionPolicy (ext_proc)
)

// attach is the ladder of §5.3: our SecurityPolicy when the route's slot is
// free; else our EnvoyExtensionPolicy when that slot is free; else nothing.
// A route our EnvoyExtensionPolicy already targets stays on it while it can.
// It returns the mode and, for ModeNone, the reasons, or for
// ModeExtensionPolicy, why.
func attach(r snapshot.RouteInfo, in Input) (Mode, []string, string) {
	var sp []string
	if why, blocked := in.Blocked[r.Key.Namespace]; blocked {
		sp = []string{why}
	} else {
		sp = conflicts(r, in)
	}
	var ext []string
	if why, blocked := in.BlockedExt[r.Key.Namespace]; blocked {
		ext = []string{why}
	} else {
		ext = extConflicts(r, in)
	}
	switch {
	case len(ext) == 0 && in.ExtArmed[r.Key]:
		why := "kept on maintenance-manager's EnvoyExtensionPolicy while the window stays armed"
		if len(sp) > 0 {
			why = sp[0]
		}
		return ModeExtensionPolicy, nil, extVia(why)
	case len(sp) == 0:
		return ModeSecurityPolicy, nil, ""
	case len(ext) == 0:
		return ModeExtensionPolicy, nil, extVia(strings.Join(sp, "; "))
	}
	return ModeNone, append(sp, ext...), ""
}

func extVia(why string) string {
	return "armed through an EnvoyExtensionPolicy (" + why + "); the platform's authentication on this route answers first"
}

// ComputeDesired applies §4.4 rule 7 (armed windows) and §5.3 (conflicts).
func ComputeDesired(in Input) Desired {
	d := Desired{Routes: map[string][]string{}, ExtRoutes: map[string][]string{}, Conflicts: map[snapshot.RouteKey][]string{}, Via: map[snapshot.RouteKey]string{}}
	selectors := snapshot.Selectors(in.Services)
	armed := map[snapshot.RouteKey]snapshot.RouteInfo{}
	for i := range in.Windows {
		w := &in.Windows[i]
		if len(snapshot.ValidateWindow(&w.Spec)) > 0 {
			continue
		}
		if next := snapshot.NextBoundary(&w.Spec, in.Now, in.Lead); !next.IsZero() && (d.NextWake.IsZero() || next.Before(d.NextWake)) {
			d.NextWake = next
		}
		if !snapshot.Armed(&w.Spec, in.Now, in.Lead) {
			continue
		}
		for _, r := range snapshot.WindowRoutes(&w.Spec, selectors, in.Routes) {
			armed[r.Key] = r
		}
	}
	for key, r := range armed {
		switch mode, reasons, via := attach(r, in); mode {
		case ModeSecurityPolicy:
			d.Routes[key.Namespace] = append(d.Routes[key.Namespace], key.Name)
		case ModeExtensionPolicy:
			d.ExtRoutes[key.Namespace] = append(d.ExtRoutes[key.Namespace], key.Name)
			d.Via[key] = via
		default:
			d.Conflicts[key] = reasons
		}
	}
	for _, m := range []map[string][]string{d.Routes, d.ExtRoutes} {
		for ns := range m {
			slices.Sort(m[ns])
		}
	}
	return d
}

// extConflicts lists the foreign EnvoyExtensionPolicies that make the
// EnvoyExtensionPolicy rung unsafe (§5.3): one that targets the route (the
// oldest wins), or one that targets its parent Gateway, listener or
// ListenerSet (ours on the route would replace it: Envoy Gateway 1.8 has no
// mergeType for EnvoyExtensionPolicies).
func extConflicts(r snapshot.RouteInfo, in Input) []string {
	var out []string
	parents := parentsOf(r, in.ListenerSets)
	for _, p := range in.Extensions {
		if p.Ours {
			continue
		}
		name := p.Namespace + "/" + p.Name
		if p.targets(r.Key.Namespace, "HTTPRoute", r.Key.Name, r.Labels, in.NamespaceLabels) {
			out = append(out, fmt.Sprintf("EnvoyExtensionPolicy %s targets this route", name))
			continue
		}
		for _, pa := range parents {
			if p.targets(pa.Namespace, pa.Kind, pa.Name, parentLabels(pa, in), in.NamespaceLabels) {
				out = append(out, fmt.Sprintf("EnvoyExtensionPolicy %s targets the parent %s %s/%s", name, pa.Kind, pa.Namespace, pa.Name))
				break
			}
		}
	}
	return out
}

func parentLabels(pa Parent, in Input) map[string]string {
	if pa.Kind == "ListenerSet" {
		return in.ListenerSetLabels[types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}]
	}
	return in.GatewayLabels[types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}]
}

// conflicts lists the foreign SecurityPolicies that make a route unsafe to
// arm (§5.3): one that targets the route, or one with extAuth that targets
// its parent Gateway, listener or ListenerSet.
func conflicts(r snapshot.RouteInfo, in Input) []string {
	var out []string
	parents := parentsOf(r, in.ListenerSets)
	for _, p := range in.Policies {
		if p.Ours {
			continue
		}
		name := p.Namespace + "/" + p.Name
		if p.targets(r.Key.Namespace, "HTTPRoute", r.Key.Name, r.Labels, in.NamespaceLabels) {
			out = append(out, fmt.Sprintf("SecurityPolicy %s targets this route", name))
			continue
		}
		if !p.ExtAuth {
			continue // a parent policy without extAuth merges (mergeType)
		}
		for _, pa := range parents {
			if p.targets(pa.Namespace, pa.Kind, pa.Name, parentLabels(pa, in), in.NamespaceLabels) {
				out = append(out, fmt.Sprintf("SecurityPolicy %s has extAuth on the parent %s %s/%s", name, pa.Kind, pa.Namespace, pa.Name))
				break
			}
		}
	}
	return out
}

// targets reports whether the policy targets the object, by name (any
// sectionName) or by label selector.
func (p Policy) targets(ns, kind, name string, lbls map[string]string, nsLabels map[string]map[string]string) bool {
	if ns == p.Namespace {
		for _, ref := range p.Refs {
			if ref.Kind == kind && ref.Name == name && (ref.Group == "" || ref.Group == gatewayGroup) {
				return true
			}
		}
	}
	for _, s := range p.Selectors {
		if s.Kind != kind || (s.Group != "" && s.Group != gatewayGroup) {
			continue
		}
		inScope := ns == p.Namespace
		switch {
		case s.AnyNamespace:
			inScope = true
		case s.NamespaceSelector != nil:
			inScope = s.NamespaceSelector.Matches(labels.Set(nsLabels[ns]))
		}
		if inScope && s.Labels.Matches(labels.Set(lbls)) {
			return true
		}
	}
	return false
}

// parentsOf resolves a route's parentRefs to Gateways and ListenerSets, and a
// ListenerSet to its Gateway as well (a route under a ListenerSet inherits
// both policies).
func parentsOf(r snapshot.RouteInfo, ls map[types.NamespacedName]types.NamespacedName) []Parent {
	var out []Parent
	for _, ref := range r.Parents {
		// Gateway and ListenerSet live in gateway.networking.k8s.io; the
		// experimental XListenerSet in gateway.networking.x-k8s.io.
		if ref.Group != nil && string(*ref.Group) != gatewayGroup && string(*ref.Group) != "gateway.networking.x-k8s.io" {
			continue
		}
		kind := "Gateway"
		if ref.Kind != nil {
			kind = string(*ref.Kind)
		}
		ns := r.Key.Namespace
		if ref.Namespace != nil {
			ns = string(*ref.Namespace)
		}
		section := ""
		if ref.SectionName != nil {
			section = string(*ref.SectionName)
		}
		switch kind {
		case "Gateway":
			out = append(out, Parent{Kind: kind, Namespace: ns, Name: string(ref.Name), SectionName: section})
		case "ListenerSet", "XListenerSet":
			out = append(out, Parent{Kind: "ListenerSet", Namespace: ns, Name: string(ref.Name), SectionName: section})
			if gw, ok := ls[types.NamespacedName{Namespace: ns, Name: string(ref.Name)}]; ok {
				out = append(out, Parent{Kind: "Gateway", Namespace: gw.Namespace, Name: gw.Name})
			}
		}
	}
	return out
}
