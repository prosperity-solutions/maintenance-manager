package controller

import (
	"slices"
)

// ActionKind is one step of a plan.
type ActionKind string

// The steps. Ensure creates the object, or updates it when it differs from
// the desired one; the executor decides which by reading it.
const (
	EnsureGrant ActionKind = "ensure-grant"
	EnsureSP    ActionKind = "ensure-securitypolicy"
	EnsureEEP   ActionKind = "ensure-envoyextensionpolicy"
	DeleteSP    ActionKind = "delete-securitypolicy"
	DeleteEEP   ActionKind = "delete-envoyextensionpolicy"
	DeleteGrant ActionKind = "delete-referencegrant"
)

// Action is one step for one app namespace.
type Action struct {
	Kind      ActionKind
	Namespace string   // the app namespace
	Targets   []string // EnsureSP, EnsureEEP: sorted route names
}

// Actual is what exists, owner-label filtered: the app namespaces that have
// our SecurityPolicy, our EnvoyExtensionPolicy, and our ReferenceGrant.
type Actual struct {
	Policies   map[string]bool
	Extensions map[string]bool
	Grants     map[string]bool
}

// Plan orders the steps from actual to desired (§5.1): every grant before
// any policy that needs it, every policy deletion before any grant deletion.
// Ensure steps are emitted for every desired namespace on every reconcile;
// the executor writes only on a difference, which also repairs drift.
// Objects of other owners never appear in Actual, so Plan never touches them
// (GC by owner label).
func Plan(d Desired, a Actual) []Action {
	var out []Action
	need := map[string]bool{}
	for ns := range d.Routes {
		need[ns] = true
	}
	for ns := range d.ExtRoutes {
		need[ns] = true
	}
	for _, ns := range sortedKeys(need) {
		out = append(out, Action{Kind: EnsureGrant, Namespace: ns})
	}
	for _, ns := range sortedKeys(d.Routes) {
		out = append(out, Action{Kind: EnsureSP, Namespace: ns, Targets: d.Routes[ns]})
	}
	for _, ns := range sortedKeys(d.ExtRoutes) {
		out = append(out, Action{Kind: EnsureEEP, Namespace: ns, Targets: d.ExtRoutes[ns]})
	}
	for _, ns := range sortedKeys(a.Policies) {
		if _, want := d.Routes[ns]; !want {
			out = append(out, Action{Kind: DeleteSP, Namespace: ns})
		}
	}
	for _, ns := range sortedKeys(a.Extensions) {
		if _, want := d.ExtRoutes[ns]; !want {
			out = append(out, Action{Kind: DeleteEEP, Namespace: ns})
		}
	}
	for _, ns := range sortedKeys(a.Grants) {
		if !need[ns] {
			out = append(out, Action{Kind: DeleteGrant, Namespace: ns})
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
