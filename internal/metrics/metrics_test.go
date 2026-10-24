package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRegisterAndNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg)
	CheckBlockHTML.Inc()
	EditorWrite("create")
	EditorAuth.WithLabelValues("failed").Inc()
	if got := testutil.ToFloat64(CheckBlockHTML); got != 1 {
		t.Fatalf("block_html = %v", got)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if !strings.HasPrefix(mf.GetName(), "maintenance_manager_") {
			t.Errorf("%s: every metric carries the maintenance_manager_ prefix", mf.GetName())
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() != "result" && l.GetName() != "verb" {
					t.Errorf("%s has label %s: labels never carry a host, route or window (§6)", mf.GetName(), l.GetName())
				}
			}
		}
	}
}

// The authz hot path increments pre-resolved handles: no allocation.
func TestCheckCountersDoNotAllocate(t *testing.T) {
	if n := testing.AllocsPerRun(1000, func() { CheckPass.Inc() }); n != 0 {
		t.Fatalf("CheckPass.Inc allocates %v", n)
	}
}
