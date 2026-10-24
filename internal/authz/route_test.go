package authz

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/protobuf/types/known/structpb"
)

// request builds a CheckRequest the way Envoy Gateway sends it with
// includeRouteMetadata: true.
func request(method, accept string, resource map[string]any, mutate ...func(*authv3.CheckRequest)) *authv3.CheckRequest {
	req := &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
			Method: method, Path: "/", Headers: map[string]string{"accept": accept},
		}},
	}}
	if resource != nil {
		md, err := structpb.NewStruct(map[string]any{"resources": []any{resource}})
		if err != nil {
			panic(err)
		}
		req.Attributes.RouteMetadataContext = &corev3.Metadata{
			FilterMetadata: map[string]*structpb.Struct{envoyGatewayMetadata: md},
		}
	}
	for _, m := range mutate {
		m(req)
	}
	return req
}

func route(ns, name string) map[string]any {
	return map[string]any{"kind": "HTTPRoute", "namespace": ns, "name": name}
}

func TestRouteKey(t *testing.T) {
	cases := []struct {
		name string
		res  map[string]any
		want RouteKey
		ok   bool
	}{
		{"httproute", route("shop", "web"), RouteKey{Namespace: "shop", Name: "web"}, true},
		{"with annotations and sectionName", map[string]any{"kind": "HTTPRoute", "namespace": "a", "name": "b", "sectionName": "rule-0", "annotations": map[string]any{"x": "y"}}, RouteKey{Namespace: "a", Name: "b"}, true},
		{"no metadata", nil, RouteKey{}, false},
		{"grpcroute", map[string]any{"kind": "GRPCRoute", "namespace": "a", "name": "b"}, RouteKey{}, false},
		{"missing name", map[string]any{"kind": "HTTPRoute", "namespace": "a"}, RouteKey{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := routeKey(request("GET", "*/*", tc.res))
			if got != tc.want || ok != tc.ok {
				t.Fatalf("routeKey = %v, %v; want %v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestWantsHTML(t *testing.T) {
	cases := []struct {
		method, accept string
		want           bool
	}{
		{"GET", "text/html,application/xhtml+xml,*/*;q=0.8", true},
		{"HEAD", "text/html", true},
		{"POST", "text/html", false},
		{"GET", "application/json", false},
		{"GET", "", false},
		{"GET", "*/*", false},
		{"GET", "TEXT/HTML", true},
		{"get", "text/html", false}, // methods are case-sensitive
	}
	for _, tc := range cases {
		got := wantsHTML(request(tc.method, tc.accept, nil).GetAttributes().GetRequest().GetHttp())
		if got != tc.want {
			t.Errorf("wantsHTML(%s, %q) = %v, want %v", tc.method, tc.accept, got, tc.want)
		}
	}
}
