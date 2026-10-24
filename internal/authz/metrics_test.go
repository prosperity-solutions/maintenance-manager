package authz

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/metrics"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

// Every Check() answer counts exactly one result (§6, M7).
func TestCheckCountsEachResultOnce(t *testing.T) {
	svc := mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "shop"}}}}
	w := mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Name: "w", UID: "u"}, Spec: mmv1.MaintenanceWindowSpec{Enabled: true,
		Phases: []mmv1.Phase{{Name: "down", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"shop"}}}}}}}
	snap := snapshot.Build(snapshot.Input{Windows: []mmv1.MaintenanceWindow{w}, Services: []mmv1.MaintenanceService{svc}, Now: tDown, Bypass: testToken,
		Routes: []snapshot.RouteInfo{{Key: snapshot.RouteKey{Namespace: "shop", Name: "web"}, Labels: map[string]string{"app": "shop"}}}})
	s := &Server{Now: func() time.Time { return tDown }}
	s.Update(snap)

	all := []prometheus.Counter{metrics.CheckPass, metrics.CheckNotReady, metrics.CheckBypass, metrics.CheckBanner,
		metrics.CheckInterstitialPage, metrics.CheckInterstitialPass, metrics.CheckBlockHTML, metrics.CheckBlockProblem, metrics.CheckStatus}
	for name, c := range map[string]struct {
		srv  *Server
		r    req
		want prometheus.Counter
	}{
		"not ready":     {&Server{}, req{}, metrics.CheckNotReady},
		"unknown route": {s, req{route: "other/x"}, metrics.CheckPass},
		"status":        {s, req{path: statusPath}, metrics.CheckStatus},
		"bypass":        {s, req{headers: map[string]string{bypassHeader: testToken}}, metrics.CheckBypass},
		"block html":    {s, req{headers: map[string]string{"accept": browser}}, metrics.CheckBlockHTML},
		"block problem": {s, req{headers: map[string]string{"accept": "application/json"}}, metrics.CheckBlockProblem},
	} {
		before := make([]float64, len(all))
		for i, m := range all {
			before[i] = testutil.ToFloat64(m)
		}
		if _, err := c.srv.Check(t.Context(), c.r.build()); err != nil {
			t.Fatal(err)
		}
		for i, m := range all {
			want := 0.0
			if m == c.want {
				want = 1
			}
			if d := testutil.ToFloat64(m) - before[i]; d != want {
				t.Errorf("%s: counter %d moved by %v, want %v", name, i, d, want)
			}
		}
	}
}
