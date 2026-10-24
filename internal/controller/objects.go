package controller

import (
	"strings"

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
	// ExtensionPolicyGVK is the fallback for routes whose SecurityPolicy slot
	// another policy holds (§5.3).
	ExtensionPolicyGVK = schema.GroupVersionKind{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Kind: "EnvoyExtensionPolicy"}
	ReferenceGrantGVK  = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1beta1", Kind: "ReferenceGrant"}
)

// SPName is our SecurityPolicy's name in every app namespace:
// mm-<hash8(release namespace)>. Our EnvoyExtensionPolicy has the same name.
func SPName(releaseNS string) string { return "mm-" + snapshot.Hash8(releaseNS) }

// GrantName is the ReferenceGrant for an app namespace, in the release
// namespace: mm-from-<ns> (always fits: namespaces are ≤ 63 characters).
func GrantName(appNS string) string { return grantPrefix + appNS }

const grantPrefix = "mm-from-"

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
				"backendRefs":     []any{map[string]any{"name": serviceName, "namespace": releaseNS, "port": int64(authzPort)}},
				"backendSettings": backendSettings(),
			},
		},
	}
}

// RouteNameAttribute is the Envoy attribute that names the matched route in
// an ext_proc request; authz parses the HTTPRoute out of it (§6.1).
const RouteNameAttribute = "xds.route_name"

// eepSpec is the EnvoyExtensionPolicy spec of §5.1: one ext_proc filter that
// asks authz about the request headers, the same backend and fail-open as
// the SecurityPolicy, and the response headers only when authz asks for them
// (allowModeOverride). No mergeType: Envoy Gateway 1.8 has none for
// EnvoyExtensionPolicies, so a parent one is a conflict instead (§5.3).
func eepSpec(releaseNS string, targets []string) map[string]any {
	refs := make([]any, 0, len(targets))
	for _, t := range targets {
		refs = append(refs, map[string]any{"group": gatewayGroup, "kind": "HTTPRoute", "name": t})
	}
	return map[string]any{
		"targetRefs": refs,
		"extProc": []any{map[string]any{
			"backendRefs":     []any{map[string]any{"name": serviceName, "namespace": releaseNS, "port": int64(authzPort)}},
			"backendSettings": extBackendSettings(),
			"messageTimeout":  "100ms",
			"failOpen":        true,
			"processingMode": map[string]any{
				"request":           map[string]any{"attributes": []any{RouteNameAttribute}},
				"response":          map[string]any{},
				"allowModeOverride": true,
			},
		}},
	}
}

// backendSettings is the passive health check both policies use.
func backendSettings() map[string]any {
	return map[string]any{
		"healthCheck": map[string]any{
			"passive": map[string]any{
				"consecutiveGatewayErrors": int64(3),
				"interval":                 "2s",
				"baseEjectionTime":         "10s",
			},
		},
	}
}

// extBackendSettings adds a high circuit-breaker limit to the health check:
// a banner request holds its ext_proc stream until the app answers, and
// Envoy's default of 1024 parallel requests per cluster would let everything
// beyond that pass unchecked (fail-open), blocked requests included (§6.1).
func extBackendSettings() map[string]any {
	s := backendSettings()
	s["circuitBreaker"] = map[string]any{"maxParallelRequests": int64(65536), "maxPendingRequests": int64(65536)}
	return s
}

// DesiredEEP is our EnvoyExtensionPolicy for an app namespace.
func DesiredEEP(releaseNS, appNS string, targets []string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": eepSpec(releaseNS, targets)}}
	u.SetGroupVersionKind(ExtensionPolicyGVK)
	u.SetNamespace(appNS)
	u.SetName(SPName(releaseNS))
	u.SetLabels(ownerLabels(releaseNS))
	return u
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

// DesiredGrant lets our SecurityPolicy and EnvoyExtensionPolicy in appNS
// reference our Service.
func DesiredGrant(releaseNS, appNS string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"from": []any{
			map[string]any{"group": "gateway.envoyproxy.io", "kind": "SecurityPolicy", "namespace": appNS},
			map[string]any{"group": "gateway.envoyproxy.io", "kind": "EnvoyExtensionPolicy", "namespace": appNS},
		},
		"to": []any{map[string]any{"group": "", "kind": "Service", "name": serviceName}},
	}}}
	u.SetGroupVersionKind(ReferenceGrantGVK)
	u.SetNamespace(releaseNS)
	u.SetName(GrantName(appNS))
	u.SetLabels(ownerLabels(releaseNS))
	return u
}

// specAllowed lists, per kind and path, the keys our policies may carry.
// Anything else under spec was added by someone else and is removed on the
// next reconcile (§5.1 drift), e.g. extAuth.bodyToExtAuth or a request body
// processing mode.
var specAllowed = map[string]map[string]map[string]bool{
	"SecurityPolicy": {
		"":        {"mergeType": true, "targetRefs": true, "extAuth": true},
		"extAuth": {"failOpen": true, "timeout": true, "includeRouteMetadata": true, "headersToExtAuth": true, "grpc": true},
	},
	"EnvoyExtensionPolicy": {
		"":        {"targetRefs": true, "extProc": true},
		"extProc": {"backendRefs": true, "backendSettings": true, "messageTimeout": true, "failOpen": true, "processingMode": true},
		// Inside processingMode only the request attributes, an empty
		// response (send the headers) and the override are ours.
		"extProc.processingMode": {"request": true, "response": true, "allowModeOverride": true},
	},
}

// needsUpdate reports whether an existing object differs from the desired
// one: a desired field is missing or different, an owner label is off, or
// a policy spec carries a key we do not set. Fields the API server added
// (defaults) inside other structures are tolerated.
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
		allowed := specAllowed[desired.GetKind()]
		for path, keys := range allowed {
			for _, m := range mapsAt(es, path) {
				for k := range m {
					if !keys[k] {
						return true
					}
				}
			}
		}
		// processingMode.request/response: anything but attributes is a body
		// mode someone else added.
		for _, pm := range mapsAt(es, "extProc.processingMode") {
			for _, dir := range []string{"request", "response"} {
				if m, ok := pm[dir].(map[string]any); ok {
					for k := range m {
						if k != "attributes" || dir == "response" {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// mapsAt returns the maps at a dotted path under spec, descending into every
// element of a list on the way ("" is spec itself).
func mapsAt(spec map[string]any, path string) []map[string]any {
	cur := []map[string]any{spec}
	if path == "" {
		return cur
	}
	for _, key := range strings.Split(path, ".") {
		var next []map[string]any
		for _, m := range cur {
			switch v := m[key].(type) {
			case map[string]any:
				next = append(next, v)
			case []any:
				for _, e := range v {
					if em, ok := e.(map[string]any); ok {
						next = append(next, em)
					}
				}
			}
		}
		cur = next
	}
	return cur
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
