package authz

import (
	"strings"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
)

// RouteKey identifies the HTTPRoute Envoy matched. It is the key into the
// snapshot's route map (§5.2).
type RouteKey struct {
	Namespace, Name string
}

// envoyGatewayMetadata is the filter-metadata namespace Envoy Gateway fills
// when the SecurityPolicy sets includeRouteMetadata (EG ≥ 1.8).
const envoyGatewayMetadata = "envoy-gateway"

// routeKey reads the matched route from
// attributes.route_metadata_context.filter_metadata["envoy-gateway"].resources[0].
// It allocates nothing: the strings point into the decoded request.
func routeKey(req *authv3.CheckRequest) (RouteKey, bool) {
	md := req.GetAttributes().GetRouteMetadataContext().GetFilterMetadata()[envoyGatewayMetadata]
	resources := md.GetFields()["resources"].GetListValue().GetValues()
	if len(resources) == 0 {
		return RouteKey{}, false
	}
	f := resources[0].GetStructValue().GetFields()
	if f["kind"].GetStringValue() != "HTTPRoute" {
		return RouteKey{}, false
	}
	k := RouteKey{Namespace: f["namespace"].GetStringValue(), Name: f["name"].GetStringValue()}
	if k.Namespace == "" || k.Name == "" {
		return RouteKey{}, false
	}
	return k, true
}

// ParseRouteKey parses "namespace/name".
func ParseRouteKey(s string) (RouteKey, bool) {
	ns, name, ok := strings.Cut(s, "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		return RouteKey{}, false
	}
	return RouteKey{Namespace: ns, Name: name}, true
}

// wantsHTML reports whether the request is a browser page load: GET or HEAD
// with an Accept header that names text/html (§4.5).
func wantsHTML(http *authv3.AttributeContext_HttpRequest) bool {
	m := http.GetMethod()
	if m != "GET" && m != "HEAD" {
		return false
	}
	return strings.Contains(http.GetHeaders()["accept"], "text/html")
}
