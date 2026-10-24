//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/controller"
)

// securityPolicy builds a foreign SecurityPolicy with CORS and the given
// targetRefs, creates it and deletes it when the test ends.
func securityPolicy(t *testing.T, ns, name string, refs ...map[string]any) *unstructured.Unstructured {
	t.Helper()
	sp := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"targetRefs": toAny(refs),
		"cors": map[string]any{
			"allowOrigins": []any{"https://app.example"},
			"allowMethods": []any{"GET"},
		},
	}}}
	sp.SetGroupVersionKind(gvk("gateway.envoyproxy.io", "v1alpha1", "SecurityPolicy"))
	sp.SetNamespace(ns)
	sp.SetName(name)
	if err := kc.Create(t.Context(), sp); err != nil {
		t.Fatalf("create SecurityPolicy %s/%s: %v", ns, name, err)
	}
	t.Cleanup(func() { _ = client.IgnoreNotFound(kc.Delete(context.Background(), sp)) })
	return sp
}

func toAny(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

func waitPolicyAccepted(t *testing.T, ns, name string) *unstructured.Unstructured {
	t.Helper()
	sp := &unstructured.Unstructured{}
	sp.SetGroupVersionKind(gvk("gateway.envoyproxy.io", "v1alpha1", "SecurityPolicy"))
	waitFor(t, 3*time.Minute, "SecurityPolicy "+ns+"/"+name+" Accepted", func() (bool, string) {
		if err := kc.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, sp); err != nil {
			return false, err.Error()
		}
		return accepted(sp)
	})
	return sp
}

// conditions lists "Type=Status" of every ancestor of a policy.
func conditions(sp *unstructured.Unstructured) []string {
	var out []string
	ancestors, _, _ := unstructured.NestedSlice(sp.Object, "status", "ancestors")
	for _, a := range ancestors {
		conds, _, _ := unstructured.NestedSlice(a.(map[string]any), "conditions")
		for _, c := range conds {
			m := c.(map[string]any)
			out = append(out, fmt.Sprintf("%v=%v", m["type"], m["status"]))
		}
	}
	return out
}

// E6: a Gateway-level SecurityPolicy without extAuth (CORS) is no conflict:
// ours is Accepted and merged, and CORS still works.
func TestE6GatewayPolicyMerges(t *testing.T) {
	ns := "e6"
	securityPolicy(t, "envoy-gateway-system", "e6-cors", map[string]any{
		"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": "eg", "sectionName": "e6",
	})
	waitPolicyAccepted(t, "envoy-gateway-system", "e6-cors")
	maintenance(t, ns, nil, mmv1.Phase{Name: "warn", Action: mmv1.ActionBanner})

	ours := waitPolicyAccepted(t, ns, controller.SPName(mmNamespace))
	t.Logf("our SecurityPolicy conditions with a Gateway CORS policy: %v", conditions(ours))
	merged := false
	for _, c := range conditions(ours) {
		if c == "Merged=True" {
			merged = true
		}
	}
	if !merged {
		t.Errorf("our SecurityPolicy does not report Merged=True: %v", conditions(ours))
	}
	waitStable(t, 2*time.Minute, 3, "banner and CORS on the same response", func() (bool, string) {
		r := get(ns, "/", browser, map[string]string{"Origin": "https://app.example"})
		return fromEcho(r, ns) && r.header.Get("X-Maintenance-Manager") != "" &&
				r.header.Get("Access-Control-Allow-Origin") == "https://app.example",
			fmt.Sprintf("%s mm=%q acao=%q", r, r.header.Get("X-Maintenance-Manager"), r.header.Get("Access-Control-Allow-Origin"))
	})
}

// E7: a foreign route-level SecurityPolicy takes the route out of ours; the
// foreign policy works, and our other route stays gated.
func TestE7ForeignRoutePolicy(t *testing.T) {
	ns := "e7"
	newApp(t, ns)
	api := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"parentRefs": []any{map[string]any{"name": "eg", "namespace": "envoy-gateway-system"}},
		"hostnames":  []any{"e7-api.example.test"},
		"rules":      []any{map[string]any{"backendRefs": []any{map[string]any{"name": "echo", "port": int64(80)}}}},
	}}}
	api.SetGroupVersionKind(gvk("gateway.networking.k8s.io", "v1", "HTTPRoute"))
	api.SetNamespace(ns)
	api.SetName("api")
	api.SetLabels(map[string]string{"e2e.maintenance-manager.oss.life.li/app": ns})
	if err := kc.Create(t.Context(), api); err != nil {
		t.Fatal(err)
	}
	svc, pg := service(t, ns), page(t, ns)
	create(t, &mmv1.MaintenanceWindow{ObjectMeta: objMeta(ns), Spec: mmv1.MaintenanceWindowSpec{Enabled: true,
		Phases: []mmv1.Phase{{Name: "down", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{svc}, Page: pg}}}}}})
	waitArmed(t, ns, "api", "web")
	waitStable(t, 2*time.Minute, 3, "both routes blocked", func() (bool, string) {
		a, w := get("e7-api", "/", browser, nil), get(ns, "/", browser, nil)
		return a.status == 503 && w.status == 503, a.String() + " | " + w.String()
	})

	t0 := time.Now()
	securityPolicy(t, ns, "foreign", map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "web"})
	waitArmed(t, ns, "api") // web leaves our targets: an update, so no drain
	waitPolicyAccepted(t, ns, "foreign")
	t.Logf("route removed and the foreign policy accepted %s after it appeared", time.Since(t0).Round(100*time.Millisecond))
	waitStable(t, 2*time.Minute, 3, "web runs the foreign policy, api stays blocked", func() (bool, string) {
		w := get(ns, "/", browser, map[string]string{"Origin": "https://app.example"})
		a := get("e7-api", "/", browser, nil)
		return fromEcho(w, ns) && w.header.Get("Access-Control-Allow-Origin") == "https://app.example" && a.status == 503,
			w.String() + " | " + a.String()
	})
}

// E10: when our SecurityPolicy is created, Envoy drains the listener: an open
// WebSocket ends with FIN/EOF, not RST, within the drain window. A close code
// 1006 (no close frame) is fine. Timings go to the log (D5).
func TestE10WebSocketDrain(t *testing.T) {
	ns := "e10"
	ctx := t.Context()
	n := &corev1.Namespace{}
	n.Name = ns
	if err := kc.Create(ctx, n); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kc.Delete(context.Background(), n) })
	if err := applyFile(ctx, "ws.yaml", ns); err != nil {
		t.Fatal(err)
	}
	if err := waitDeployment(ctx, ns, "ws", 2*time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	// No other SecurityPolicy of ours may be created or deleted while the
	// socket is open: each one drains the shared listener too.
	waitFor(t, 3*time.Minute, "no SecurityPolicies of ours anywhere", func() (bool, string) {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk("gateway.envoyproxy.io", "v1alpha1", "SecurityPolicyList"))
		if err := kc.List(ctx, list, client.MatchingLabels{controller.LabelOwner: mmNamespace}); err != nil {
			return false, err.Error()
		}
		return len(list.Items) == 0, fmt.Sprintf("%d left", len(list.Items))
	})
	time.Sleep(5 * time.Second) // let any earlier listener update settle

	var conn net.Conn
	waitFor(t, 2*time.Minute, "a WebSocket through Envoy", func() (bool, string) {
		c, err := openWebSocket(ns)
		if err != nil {
			return false, err.Error()
		}
		conn = c
		return true, ""
	})
	defer func() { _ = conn.Close() }()
	closed := make(chan error, 1)
	var closedAt time.Time
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := conn.Read(buf); err != nil {
				closedAt = time.Now()
				closed <- err
				return
			}
		}
	}()

	svc, pg := service(t, ns), page(t, ns)
	create(t, &mmv1.MaintenanceWindow{ObjectMeta: objMeta(ns), Spec: mmv1.MaintenanceWindowSpec{Enabled: true,
		Phases: []mmv1.Phase{{Name: "warn", Action: mmv1.ActionBanner, Routings: []mmv1.Routing{{Services: []string{svc}, Page: pg}}}}}})
	armedAt := time.Now()
	waitArmed(t, ns)
	t.Logf("our SecurityPolicy Accepted %s after the window was created", time.Since(armedAt).Round(100*time.Millisecond))

	select {
	case err := <-closed:
		t.Logf("drain: the WebSocket closed %s after the window was created (%v)", closedAt.Sub(armedAt).Round(100*time.Millisecond), err)
		if errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("the WebSocket was reset (RST), want a graceful close (FIN): %v", err)
		}
		if !errors.Is(err, io.EOF) {
			t.Fatalf("the WebSocket ended with %v, want EOF (FIN)", err)
		}
	case <-time.After(150 * time.Second):
		t.Fatal("the WebSocket stayed open for 150 s after our SecurityPolicy appeared; expected a drain within about 60 s")
	}
}

// openWebSocket upgrades a raw TCP connection through Envoy and returns it
// after the 101.
func openWebSocket(ns string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", envoyAddr)
	if err != nil {
		return nil, err
	}
	req := "GET /.ws HTTP/1.1\r\nHost: " + ns + ".example.test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, req); err != nil {
		_ = conn.Close()
		return nil, err
	}
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("upgrade: %w", err)
	}
	if !strings.Contains(status, " 101 ") {
		_ = conn.Close()
		return nil, fmt.Errorf("upgrade: got %q, want 101", strings.TrimSpace(status))
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
