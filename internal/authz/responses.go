package authz

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	procmodev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
)

// answer is one complete response that ends the request, in both protocols
// Envoy may ask in: ext_authz Check() (SecurityPolicy) and ext_proc
// (EnvoyExtensionPolicy, §6.1). Both are immutable and shared: gRPC only
// reads them.
type answer struct {
	authz *authv3.CheckResponse
	proc  *extprocv3.ProcessingResponse
}

// header builds one response header that replaces any existing value, as
// ext_authz takes it (value).
func header(k, v string) *corev3.HeaderValueOption {
	return &corev3.HeaderValueOption{
		Header:       &corev3.HeaderValue{Key: k, Value: v},
		AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
	}
}

// procHeaders converts headers for ext_proc, whose header mutations carry
// the value in raw_value.
func procHeaders(hs []*corev3.HeaderValueOption) []*corev3.HeaderValueOption {
	out := make([]*corev3.HeaderValueOption, 0, len(hs))
	for _, h := range hs {
		out = append(out, &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: h.GetHeader().GetKey(), RawValue: []byte(h.GetHeader().GetValue())},
			AppendAction: h.GetAppendAction(),
		})
	}
	return out
}

// denied builds a complete deny answer: Envoy sends it instead of asking the
// app.
func denied(code typev3.StatusCode, contentType, body string, extra ...*corev3.HeaderValueOption) answer {
	headers := append([]*corev3.HeaderValueOption{
		header("content-type", contentType),
		header("cache-control", "no-store"),
	}, extra...)
	return answer{
		authz: &authv3.CheckResponse{
			Status: &status.Status{Code: int32(codes.PermissionDenied)},
			HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: &authv3.DeniedHttpResponse{
				Status:  &typev3.HttpStatus{Code: code},
				Headers: headers,
				Body:    body,
			}},
		},
		proc: &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extprocv3.ImmediateResponse{
				Status:  &typev3.HttpStatus{Code: code},
				Headers: &extprocv3.HeaderMutation{SetHeaders: procHeaders(headers)},
				Body:    []byte(body),
			},
		}},
	}
}

// skipResponseHeaders ends an ext_proc stream after the request headers:
// Envoy does not send the response headers when there is nothing to add.
var skipResponseHeaders = &procmodev3.ProcessingMode{
	RequestHeaderMode:   procmodev3.ProcessingMode_SEND,
	ResponseHeaderMode:  procmodev3.ProcessingMode_SKIP,
	RequestBodyMode:     procmodev3.ProcessingMode_NONE,
	ResponseBodyMode:    procmodev3.ProcessingMode_NONE,
	RequestTrailerMode:  procmodev3.ProcessingMode_SKIP,
	ResponseTrailerMode: procmodev3.ProcessingMode_SKIP,
}

// procContinue lets the request through, removing the bypass header when
// strip is set. With skip, the stream ends here; without, Envoy sends the
// response headers next (for X-Maintenance-Manager).
func procContinue(strip, skip bool) *extprocv3.ProcessingResponse {
	hr := &extprocv3.HeadersResponse{}
	if strip {
		hr.Response = &extprocv3.CommonResponse{HeaderMutation: &extprocv3.HeaderMutation{RemoveHeaders: []string{bypassHeader}}}
	}
	r := &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_RequestHeaders{RequestHeaders: hr}}
	if skip {
		r.ModeOverride = skipResponseHeaders
	}
	return r
}

// procResponseHeaders adds headers to the app's response.
func procResponseHeaders(add ...*corev3.HeaderValueOption) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ResponseHeaders{
		ResponseHeaders: &extprocv3.HeadersResponse{Response: &extprocv3.CommonResponse{
			HeaderMutation: &extprocv3.HeaderMutation{SetHeaders: procHeaders(add)},
		}},
	}}
}
