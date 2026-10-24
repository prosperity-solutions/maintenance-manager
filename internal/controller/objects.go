package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

// Labels on every object we create (§5.1). The owner value is the release
// namespace; GC only ever touches objects that carry it.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelOwner     = "maintenance-manager.oss.life.li/owner"
	managedBy      = "maintenance-manager"
	serviceName    = "maintenance-manager"
	authzPort      = 9001
)

var (
	SecurityPolicyGVK = schema.GroupVersionKind{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Kind: "SecurityPolicy"}
	ReferenceGrantGVK = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1beta1", Kind: "ReferenceGrant"}
)

// SPName is our SecurityPolicy's name in every app namespace:
// mm-<hash8(release namespace)>.
func SPName(releaseNS string) string { return "mm-" + snapshot.Hash8(releaseNS) }

// GrantName is the ReferenceGrant for an app namespace, in the release
// namespace: mm-from-<ns> (always fits: namespaces are ≤ 63 characters).
func GrantName(appNS string) string { return "mm-from-" + appNS }

func ownerLabels(releaseNS string) map[string]string {
	return map[string]string{LabelManagedBy: managedBy, LabelOwner: releaseNS}
}

// spSpec is the SecurityPolicy spec of §5.1. bodyToExtAuth and
// recomputeRoute stay unset on purpose.
func spSpec(releaseNS string, targets []string) map[string]any {
	refs := make([]any, 0, len(targets))
	for _, t := range targets {
		refs = append(refs, map[string]any{"group": gatewayGroup, "kind": "HTTPRoute", "name": t})
	}
	return map[string]any{
		"mergeType":  "StrategicMerge",
		"targetRefs": refs,
		"extAuth": map[string]any{
			"failOpen":             true,
			"timeout":              "100ms",
			"includeRouteMetadata": true,
			"headersToExtAuth":     []any{"accept", "accept-language", "cookie", "x-maintenance-manager-bypass"},
			"grpc": map[string]any{
				"backendRefs": []any{map[string]any{"name": serviceName, "namespace": releaseNS, "port": int64(authzPort)}},
				"backendSettings": map[string]any{
					"healthCheck": map[string]any{
						"passive": map[string]any{
							"consecutiveGatewayErrors": int64(3),
							"interval":                 "2s",
							"baseEjectionTime":         "10s",
						},
					},
				},
			},
		},
	}
}

// DesiredSP is our SecurityPolicy for an app namespace.
func DesiredSP(releaseNS, appNS string, targets []string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spSpec(releaseNS, targets)}}
	u.SetGroupVersionKind(SecurityPolicyGVK)
	u.SetNamespace(appNS)
	u.SetName(SPName(releaseNS))
	u.SetLabels(ownerLabels(releaseNS))
	return u
}

// DesiredGrant lets SecurityPolicies in appNS reference our Service.
func DesiredGrant(releaseNS, appNS string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"from": []any{map[string]any{"group": "gateway.envoyproxy.io", "kind": "SecurityPolicy", "namespace": appNS}},
		"to":   []any{map[string]any{"group": "", "kind": "Service", "name": serviceName}},
	}}}
	u.SetGroupVersionKind(ReferenceGrantGVK)
	u.SetNamespace(releaseNS)
	u.SetName(GrantName(appNS))
	u.SetLabels(ownerLabels(releaseNS))
	return u
}

// specAllowed lists, per path, the keys our objects may carry. Anything
// else under spec was added by someone else and is removed on the next
// reconcile (§5.1 drift), e.g. extAuth.bodyToExtAuth.
var specAllowed = map[string]map[string]bool{
	"":        {"mergeType": true, "targetRefs": true, "extAuth": true},
	"extAuth": {"failOpen": true, "timeout": true, "includeRouteMetadata": true, "headersToExtAuth": true, "grpc": true},
}

// needsUpdate reports whether an existing object differs from the desired
// one: a desired field is missing or different, an owner label is off, or
// the SecurityPolicy spec carries a key we do not set. Fields the API server
// added (defaults) inside our structures are tolerated.
func needsUpdate(existing, desired *unstructured.Unstructured, strict bool) bool {
	for k, v := range desired.GetLabels() {
		if existing.GetLabels()[k] != v {
			return true
		}
	}
	es, _, _ := unstructured.NestedMap(existing.Object, "spec")
	ds, _, _ := unstructured.NestedMap(desired.Object, "spec")
	if !subset(ds, es) {
		return true
	}
	if strict {
		for k := range es {
			if !specAllowed[""][k] {
				return true
			}
		}
		if ext, ok := es["extAuth"].(map[string]any); ok {
			for k := range ext {
				if !specAllowed["extAuth"][k] {
					return true
				}
			}
		}
	}
	return false
}

// subset reports whether every value in want is present and equal in have.
// Maps are compared key by key (have may carry more keys: defaults); lists
// must match in length and order.
func subset(want, have any) bool {
	switch w := want.(type) {
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok {
			return false
		}
		for k, wv := range w {
			hv, ok := h[k]
			if !ok || !subset(wv, hv) {
				return false
			}
		}
		return true
	case []any:
		h, ok := have.([]any)
		if !ok || len(h) != len(w) {
			return false
		}
		for i := range w {
			if !subset(w[i], h[i]) {
				return false
			}
		}
		return true
	case int64:
		switch h := have.(type) {
		case int64:
			return h == w
		case float64:
			return h == float64(w)
		}
		return false
	default:
		return want == have
	}
}

func metaFor(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }
