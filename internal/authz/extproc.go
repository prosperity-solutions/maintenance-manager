package authz

import (
	"errors"
	"io"
	"strings"
	"unsafe"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/prosperity-solutions/maintenance-manager/internal/metrics"
)

// Attributes the EnvoyExtensionPolicy asks Envoy to send with the request
// headers (§6.1). The controller writes the same names.
const (
	attrRouteName = "xds.route_name"
)

// procServer serves envoy.service.ext_proc.v3.ExternalProcessor: the same
// decisions as Check(), for routes armed through an EnvoyExtensionPolicy
// because their SecurityPolicy slot is taken (§5.3, §6.1).
type procServer struct {
	extprocv3.UnimplementedExternalProcessorServer
	s *Server
}

// Process handles one HTTP request. It answers the request headers and, only
// when X-Maintenance-Manager has to go on the app's response, the response
// headers; then it returns, which tells Envoy the processor is done. Every
// answer is precomputed; nothing is logged per request.
func (p procServer) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	req, err := stream.Recv()
	if err != nil {
		// Envoy hanging up is no error; anything else (headers over
		// maxRecvMsgSize) is, and Envoy fails open on it.
		if c := status.Code(err); !errors.Is(err, io.EOF) && c != codes.Canceled && c != codes.DeadlineExceeded {
			metrics.CheckError.Inc()
		}
		return nil
	}
	h := req.GetRequestHeaders()
	if h == nil {
		return nil // only request headers start a stream we asked for
	}
	now, after := p.requestHeaders(req, h)
	if err := stream.Send(now); err != nil || after == nil {
		return nil
	}
	// Banner or interstitial pass: the stream stays open until the app's
	// response headers arrive.
	req, err = stream.Recv()
	if err != nil || req.GetResponseHeaders() == nil {
		return nil
	}
	_ = stream.Send(after)
	return nil
}

// requestHeaders answers the request headers, and returns what to answer the
// response headers with, when Envoy is to send them.
func (p procServer) requestHeaders(req *extprocv3.ProcessingRequest, h *extprocv3.HttpHeaders) (now, after *extprocv3.ProcessingResponse) {
	var r reqInfo
	for _, hv := range h.GetHeaders().GetHeaders() {
		switch hv.GetKey() {
		case ":method":
			r.method = headerValue(hv)
		case ":path":
			r.path = headerValue(hv)
		case "accept":
			r.accept = join(r.accept, ",", headerValue(hv))
		case "accept-language":
			r.acceptLanguage = join(r.acceptLanguage, ",", headerValue(hv))
		case "cookie": // HTTP/2 may send cookies as separate headers
			r.cookie = join(r.cookie, "; ", headerValue(hv))
		case bypassHeader:
			r.bypass, r.hasBypass = join(r.bypass, ",", headerValue(hv)), true
		}
	}
	key, found := procRouteKey(req.GetAttributes())
	v := p.s.decide(key, found, r)
	switch v.kind {
	case verdictPass:
		if v.strip {
			return procPassStrip, nil
		}
		return procPass, nil
	case verdictInfo:
		if v.strip {
			return procHoldStrip, v.d.procInfo
		}
		return procHold, v.d.procInfo
	}
	return v.answer.proc, nil
}

// join appends a repeated header's value the way Envoy joins them for
// ext_authz, so both protocols decide alike. It allocates only on a repeat.
func join(prev, sep, v string) string {
	if prev == "" {
		return v
	}
	return prev + sep + v
}

// headerValue returns a header's value. Envoy sends raw_value; the string
// points into the request, which outlives decide(), so it needs no copy.
func headerValue(hv *corev3.HeaderValue) string {
	if raw := hv.GetRawValue(); len(raw) > 0 {
		// Read-only view of bytes the decoded request owns; nothing writes to
		// them and decide() keeps no reference.
		return unsafe.String(&raw[0], len(raw)) //nolint:gosec // G103: audited above
	}
	return hv.GetValue()
}

// procRouteKey reads the matched HTTPRoute from the xds.route_name attribute.
// Envoy Gateway names an HTTPRoute's Envoy routes
// "httproute/<namespace>/<name>/rule/<i>/match/<j>/<host>". The attributes
// arrive keyed by the filter's name, so every entry is searched.
func procRouteKey(attrs map[string]*structpb.Struct) (RouteKey, bool) {
	for _, st := range attrs {
		name := st.GetFields()[attrRouteName].GetStringValue()
		rest, ok := strings.CutPrefix(name, "httproute/")
		if !ok {
			continue
		}
		ns, rest, ok1 := strings.Cut(rest, "/")
		name, _, ok2 := strings.Cut(rest, "/")
		if !ok1 || !ok2 || ns == "" || name == "" {
			continue
		}
		return RouteKey{Namespace: ns, Name: name}, true
	}
	return RouteKey{}, false
}
