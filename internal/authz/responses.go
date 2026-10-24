package authz

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
)

// header builds one response header that replaces any existing value.
func header(k, v string) *corev3.HeaderValueOption {
	return &corev3.HeaderValueOption{
		Header:       &corev3.HeaderValue{Key: k, Value: v},
		AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
	}
}

// denied builds a complete, immutable deny response. Check() returns it as a
// shared pointer: gRPC only reads it.
func denied(code typev3.StatusCode, contentType, body string, extra ...*corev3.HeaderValueOption) *authv3.CheckResponse {
	headers := append([]*corev3.HeaderValueOption{
		header("content-type", contentType),
		header("cache-control", "no-store"),
	}, extra...)
	return &authv3.CheckResponse{
		Status: &status.Status{Code: int32(codes.PermissionDenied)},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: &authv3.DeniedHttpResponse{
			Status:  &typev3.HttpStatus{Code: code},
			Headers: headers,
			Body:    body,
		}},
	}
}

// The static block of M2: one route named by --e2e-block-route, the built-in
// texts, no end time (so no Retry-After). M3 replaces it with rendered,
// per-window responses.
const (
	staticBlockHTML = `<!doctype html><html lang="en"><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>We&#39;ll be right back</title>` +
		`<h1>We&#39;ll be right back</h1><p>Scheduled maintenance.</p></html>`

	staticBlockJSON = `{"type":"https://maintenance-manager.oss.life.li/problems/maintenance",` +
		`"title":"We'll be right back","status":503,"detail":"Scheduled maintenance.",` +
		`"window":"e2e-block","until":null}`
)

var (
	staticBlockHTMLResponse = denied(typev3.StatusCode_ServiceUnavailable, "text/html; charset=utf-8", staticBlockHTML)
	staticBlockJSONResponse = denied(typev3.StatusCode_ServiceUnavailable, "application/problem+json", staticBlockJSON)
)
