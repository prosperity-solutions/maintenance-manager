// Package controller arms and disarms routes: one SecurityPolicy and one
// ReferenceGrant per app namespace with armed routes (docs/HANDOVER.md §5).
// Desired and Plan are pure; Reconciler applies the plan; Controller runs it
// on the leader only.
package controller

import (
	"fmt"
	"slices"
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
	AnyNamespace bool // namespaces.from All or Selector: treated as every namespace
}

// Policy is what the conflict check needs from a SecurityPolicy.
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
	// ListenerSets maps a ListenerSet to the Gateway it belongs to.
	ListenerSets map[types.NamespacedName]types.NamespacedName
	// GatewayLabels and ListenerSetLabels serve targetSelectors on parents.
	GatewayLabels     map[types.NamespacedName]map[string]string
	ListenerSetLabels map[types.NamespacedName]map[string]string
	Now               time.Time
	Lead              time.Duration
}

// Desired is the arm set: per app namespace, the sorted names of its armed,
// conflict-free routes.
type Desired struct {
	Routes    map[string][]string
	Conflicts map[snapshot.RouteKey][]string // why a route is not armed
	NextWake  time.Time                      // next arm boundary, zero if none
}

// ComputeDesired applies §4.4 rule 7 (armed windows) and §5.3 (conflicts).
func ComputeDesired(in Input) Desired {
	d := Desired{Routes: map[string][]string{}, Conflicts: map[snapshot.RouteKey][]string{}}
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
		if reasons := conflicts(r, in); len(reasons) > 0 {
			d.Conflicts[key] = reasons
			continue
		}
		d.Routes[key.Namespace] = append(d.Routes[key.Namespace], key.Name)
	}
	for ns := range d.Routes {
		slices.Sort(d.Routes[ns])
	}
	return d
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
		if p.targets(r.Key.Namespace, "HTTPRoute", r.Key.Name, r.Labels) {
			out = append(out, fmt.Sprintf("SecurityPolicy %s targets this route", name))
			continue
		}
		if !p.ExtAuth {
			continue // a parent policy without extAuth merges (mergeType)
		}
		for _, pa := range parents {
			lbls := in.GatewayLabels[types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}]
			if pa.Kind == "ListenerSet" {
				lbls = in.ListenerSetLabels[types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}]
			}
			if p.targets(pa.Namespace, pa.Kind, pa.Name, lbls) {
				out = append(out, fmt.Sprintf("SecurityPolicy %s has extAuth on the parent %s %s/%s", name, pa.Kind, pa.Namespace, pa.Name))
				break
			}
		}
	}
	return out
}

// targets reports whether the policy targets the object, by name (any
// sectionName) or by label selector.
func (p Policy) targets(ns, kind, name string, lbls map[string]string) bool {
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
		if (s.AnyNamespace || ns == p.Namespace) && s.Labels.Matches(labels.Set(lbls)) {
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
		if ref.Group != nil && string(*ref.Group) != gatewayGroup {
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
