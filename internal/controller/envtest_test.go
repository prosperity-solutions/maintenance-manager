//go:build envtest

package controller

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1b1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
)

// Run: KUBEBUILDER_ASSETS=$(scripts/dev/t setup-envtest use 1.36.x -p path) \
//   go test -tags envtest ./internal/controller/...

const releaseNS = "mm"

var (
	k8s    client.WithWatch
	scheme = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, mmv1.AddToScheme, gwapiv1.Install, gwapiv1b1.Install, egv1a1.AddToScheme} {
		if err := add(scheme); err != nil {
			panic(err)
		}
	}
	eg, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/envoyproxy/gateway").Output()
	if err != nil {
		panic("locate the envoyproxy/gateway module (its CRDs feed envtest): " + err.Error())
	}
	crds := filepath.Join(strings.TrimSpace(string(eg)), "charts/gateway-helm/charts/crds/crds")
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../chart/templates/crds", crds, filepath.Join(crds, "generated")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		panic("envtest: " + err.Error() + " (set KUBEBUILDER_ASSETS, see the comment above)")
	}
	k8s, err = client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	for _, ns := range []string{releaseNS, "shop", "other", "gw"} {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		if err := k8s.Create(ctx, n); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

// recorder logs every write, in order.
type recorder struct {
	mu  sync.Mutex
	ops []string
}

func (r *recorder) add(op string, obj client.Object) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, op+" "+obj.GetObjectKind().GroupVersionKind().Kind+" "+obj.GetNamespace()+"/"+obj.GetName())
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.ops
	r.ops = nil
	return out
}

func newReconciler(t *testing.T, now time.Time) (*Reconciler, *recorder) {
	t.Helper()
	rec := &recorder{}
	w := interceptor.NewClient(k8s, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			rec.add("create", obj)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			rec.add("update", obj)
			return c.Update(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			rec.add("delete", obj)
			return c.Delete(ctx, obj, opts...)
		},
	})
	return &Reconciler{
		Namespace: releaseNS, Lead: 15 * time.Minute, Shared: k8s, Leader: k8s, Writer: w,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now },
	}, rec
}

func create(t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8s.Create(t.Context(), obj); err != nil {
		t.Fatalf("create %s: %v", obj.GetName(), err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), obj) })
}

func httpRoute(ns, name string, lbls map[string]string) *gwapiv1.HTTPRoute {
	gwNS := gwapiv1.Namespace("gw")
	return &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbls},
		Spec:       gwapiv1.HTTPRouteSpec{CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: []gwapiv1.ParentReference{{Name: "eg", Namespace: &gwNS}}}},
	}
}

func getSP(t *testing.T, ns string) *egv1a1.SecurityPolicy {
	t.Helper()
	var sp egv1a1.SecurityPolicy
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: SPName(releaseNS)}, &sp); err != nil {
		return nil
	}
	return &sp
}

func targets(sp *egv1a1.SecurityPolicy) string {
	var out []string
	for _, r := range sp.Spec.TargetRefs {
		out = append(out, string(r.Name))
	}
	return strings.Join(out, ",")
}

func reconcile(t *testing.T, r *Reconciler) Result {
	t.Helper()
	res, err := r.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func TestEnvtestArmConflictDriftDisarm(t *testing.T) {
	now := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	svc := &mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Namespace: releaseNS, Name: "shop"},
		Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "shop"}}}}
	create(t, svc)
	create(t, httpRoute("shop", "web", map[string]string{"app": "shop"}))
	create(t, httpRoute("shop", "api", map[string]string{"app": "shop"}))
	w := &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: releaseNS, Name: "db"}, Spec: mmv1.MaintenanceWindowSpec{
		Enabled: true, Phases: []mmv1.Phase{{Name: "down", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"shop"}}}}}}}
	create(t, w)

	r, rec := newReconciler(t, now)

	// Arm: grant first, then the policy.
	reconcile(t, r)
	if got := strings.Join(rec.take(), "; "); got != "create ReferenceGrant mm/mm-from-shop; create SecurityPolicy shop/"+SPName(releaseNS) {
		t.Fatalf("arm order: %s", got)
	}
	sp := getSP(t, "shop")
	if sp == nil || targets(sp) != "api,web" || sp.Spec.MergeType == nil || *sp.Spec.MergeType != egv1a1.StrategicMerge {
		t.Fatalf("policy %+v", sp)
	}
	rv := sp.ResourceVersion

	// Idempotent: nothing written, resourceVersion unchanged.
	reconcile(t, r)
	if ops := rec.take(); len(ops) != 0 {
		t.Fatalf("a no-op reconcile wrote: %v", ops)
	}
	if getSP(t, "shop").ResourceVersion != rv {
		t.Fatal("a no-op reconcile changed the policy")
	}

	// Conflict: a foreign policy on "web" removes it from our targets with an
	// update (RDS-only), never a delete.
	foreign := &egv1a1.SecurityPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "jwt"}}
	foreign.Spec.TargetRefs = []gwapiv1.LocalPolicyTargetReferenceWithSectionName{{LocalPolicyTargetReference: gwapiv1.LocalPolicyTargetReference{Group: gatewayGroup, Kind: "HTTPRoute", Name: "web"}}}
	foreign.Spec.CORS = &egv1a1.CORS{}
	create(t, foreign)
	res := reconcile(t, r)
	if got := strings.Join(rec.take(), "; "); got != "update SecurityPolicy shop/"+SPName(releaseNS) {
		t.Fatalf("conflict: %s", got)
	}
	if targets(getSP(t, "shop")) != "api" || len(res.Desired.Conflicts) != 1 {
		t.Fatalf("after conflict: targets %s, conflicts %v", targets(getSP(t, "shop")), res.Desired.Conflicts)
	}

	// Drift: someone adds bodyToExtAuth; the next reconcile removes it.
	sp = getSP(t, "shop")
	sp.Spec.ExtAuth.BodyToExtAuth = &egv1a1.BodyToExtAuth{MaxRequestBytes: 1024}
	if err := k8s.Update(t.Context(), sp); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	if getSP(t, "shop").Spec.ExtAuth.BodyToExtAuth != nil {
		t.Fatal("bodyToExtAuth was not removed")
	}
	rec.take()

	// Disarm: policy first, then the grant.
	w.Spec.Enabled = false
	if err := k8s.Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	if got := strings.Join(rec.take(), "; "); got != "delete SecurityPolicy shop/"+SPName(releaseNS)+"; delete ReferenceGrant mm/mm-from-shop" {
		t.Fatalf("disarm order: %s", got)
	}
}

func TestEnvtestGCLeavesOthersAlone(t *testing.T) {
	now := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	ours := DesiredSP(releaseNS, "other", []string{"x"})
	theirs := DesiredSP("someone-else", "other", []string{"x"}) // another install's owner value
	unlabelled := &unstructured.Unstructured{}
	unlabelled.SetGroupVersionKind(SecurityPolicyGVK)
	unlabelled.SetNamespace("other")
	unlabelled.SetName("plain")
	_ = unstructured.SetNestedField(unlabelled.Object, map[string]any{"cors": map[string]any{}}, "spec")
	_ = unstructured.SetNestedSlice(unlabelled.Object, []any{map[string]any{"group": gatewayGroup, "kind": "HTTPRoute", "name": "x"}}, "spec", "targetRefs")
	for _, o := range []*unstructured.Unstructured{ours, theirs, unlabelled} {
		if err := k8s.Create(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, o := range []*unstructured.Unstructured{theirs, unlabelled} {
			_ = k8s.Delete(context.Background(), o)
		}
	})
	r, rec := newReconciler(t, now)
	reconcile(t, r)
	if got := strings.Join(rec.take(), "; "); got != "delete SecurityPolicy other/"+SPName(releaseNS) {
		t.Fatalf("GC: %s", got)
	}
}

func TestEnvtestCleanup(t *testing.T) {
	sp := DesiredSP(releaseNS, "shop", []string{"web"})
	grant := DesiredGrant(releaseNS, "shop")
	for _, o := range []*unstructured.Unstructured{sp, grant} {
		if err := k8s.Create(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}
	two := int32(2)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: releaseNS, Name: serviceName}, Spec: appsv1.DeploymentSpec{
		Replicas: &two,
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": serviceName}},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": serviceName}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "x", Image: "x"}}},
		},
	}}
	create(t, d)
	if err := Cleanup(t.Context(), k8s, releaseNS, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(d), d); err != nil || *d.Spec.Replicas != 0 {
		t.Fatalf("Deployment not scaled to 0: %v", err)
	}
	if getSP(t, "shop") != nil {
		t.Fatal("our SecurityPolicy survived cleanup")
	}
	var g gwapiv1b1.ReferenceGrant
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: releaseNS, Name: "mm-from-shop"}, &g); err == nil {
		t.Fatal("our ReferenceGrant survived cleanup")
	}
	// Idempotent.
	if err := Cleanup(t.Context(), k8s, releaseNS, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
}
