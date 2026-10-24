package authz

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/protobuf/types/known/structpb"
)

// request builds a CheckRequest the way Envoy Gateway sends it with
// includeRouteMetadata: true.
func request(method, accept string, resource map[string]any) *authv3.CheckRequest {
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
		{"httproute", route("shop", "web"), RouteKey{"shop", "web"}, true},
		{"with annotations and sectionName", map[string]any{"kind": "HTTPRoute", "namespace": "a", "name": "b", "sectionName": "rule-0", "annotations": map[string]any{"x": "y"}}, RouteKey{"a", "b"}, true},
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

func TestParseRouteKey(t *testing.T) {
	for in, want := range map[string]bool{"ns/name": true, "ns": false, "/name": false, "ns/": false, "a/b/c": false, "": false} {
		if _, ok := ParseRouteKey(in); ok != want {
			t.Errorf("ParseRouteKey(%q) ok = %v, want %v", in, ok, want)
		}
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
	}
	for _, tc := range cases {
		got := wantsHTML(request(tc.method, tc.accept, nil).GetAttributes().GetRequest().GetHttp())
		if got != tc.want {
			t.Errorf("wantsHTML(%s, %q) = %v, want %v", tc.method, tc.accept, got, tc.want)
		}
	}
}

func TestCheckStaticBlock(t *testing.T) {
	s := &Server{BlockRoute: &RouteKey{"shop", "web"}}
	cases := []struct {
		name string
		req  *authv3.CheckRequest
		want *authv3.CheckResponse
	}{
		{"browser on the blocked route", request("GET", "text/html", route("shop", "web")), staticBlockHTMLResponse},
		{"API call on the blocked route", request("GET", "application/json", route("shop", "web")), staticBlockJSONResponse},
		{"POST form on the blocked route", request("POST", "text/html", route("shop", "web")), staticBlockJSONResponse},
		{"another route", request("GET", "text/html", route("shop", "api")), okResponse},
		{"no route metadata", request("GET", "text/html", nil), okResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Check(t.Context(), tc.req)
			if err != nil || got != tc.want {
				t.Fatalf("Check = %p, %v; want %p", got, err, tc.want)
			}
		})
	}
	if code := staticBlockHTMLResponse.GetDeniedResponse().GetStatus().GetCode(); code != 503 {
		t.Fatalf("block status %d, want 503", code)
	}
}

func TestCheckAllocsWithRoute(t *testing.T) {
	s := &Server{BlockRoute: &RouteKey{"shop", "web"}}
	for _, req := range []*authv3.CheckRequest{
		request("GET", "text/html", route("shop", "web")),
		request("GET", "text/html", route("shop", "api")),
	} {
		ctx := t.Context()
		if n := testing.AllocsPerRun(1000, func() { _, _ = s.Check(ctx, req) }); n > 2 {
			t.Fatalf("Check allocates %.0f times per call; the budget is 2", n)
		}
	}
}
