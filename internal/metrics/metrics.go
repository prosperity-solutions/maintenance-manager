// Package metrics defines maintenance-manager's Prometheus metrics, served
// on :8081/metrics with controller-runtime's (HANDOVER §3, §6, D13). Labels
// never carry a host, route or window name: cardinality stays fixed, and the
// authz hot path uses pre-resolved handles, which allocate nothing.
package metrics

import "github.com/prometheus/client_golang/prometheus"

const ns = "maintenance_manager"

var (
	checks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "check_total",
		Help: "Answers to Envoy (ext_authz Check() and ext_proc), by result.",
	}, []string{"result"})

	// Check() results, resolved once.
	CheckPass             = checks.WithLabelValues("pass")              // no decision for the route now
	CheckNotReady         = checks.WithLabelValues("not_ready")         // no snapshot yet: fail-open
	CheckBypass           = checks.WithLabelValues("bypass")            // valid bypass token
	CheckBanner           = checks.WithLabelValues("banner")            // passed with the header
	CheckInterstitialPage = checks.WithLabelValues("interstitial_page") // the interstitial page shown
	CheckInterstitialPass = checks.WithLabelValues("interstitial_pass") // seen already, or not HTML
	CheckBlockHTML        = checks.WithLabelValues("block_html")        // 503 page
	CheckBlockProblem     = checks.WithLabelValues("block_problem")     // 503 problem+json
	CheckStatus           = checks.WithLabelValues("status")            // /__maintenance-manager/status
	CheckError            = checks.WithLabelValues("error")             // an ext_proc request that could not be read: Envoy fails open

	SnapshotBuilds = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ns, Name: "snapshot_builds_total",
		Help: "Snapshots built (each replica builds its own).",
	})
	SnapshotBuildSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: ns, Name: "snapshot_build_seconds",
		Help:    "Time to build a snapshot and hand it to authz (compiling decisions and rendering pages), after listing the caches.",
		Buckets: []float64{.001, .005, .01, .05, .1, .5, 1, 5},
	})
	SnapshotRoutes = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "snapshot_routes",
		Help: "Routes covered by an enabled, valid, not-ended window in the current snapshot.",
	})
	SnapshotTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "snapshot_timestamp_seconds",
		Help: "Unix time of the current snapshot; alert when it stops moving while changes happen.",
	})

	Leader = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "controller_leader",
		Help: "1 while this replica leads the controller (arms and disarms routes). controller-runtime's leader_election_master_status reports the same Lease.",
	})
	reconciles = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "controller_reconciles_total",
		Help: "Controller reconciles, by result.",
	}, []string{"result"})
	ReconcileOK    = reconciles.WithLabelValues("ok")
	ReconcileError = reconciles.WithLabelValues("error")
	Writes         = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ns, Name: "controller_writes_total",
		Help: "SecurityPolicy and ReferenceGrant writes (create, update, delete).",
	})
	ArmedRoutes = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "controller_armed_routes",
		Help: "Routes the last reconcile wanted armed (leader only).",
	})
	Conflicts = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "controller_conflicts",
		Help: "Routes of armed windows left unarmed because of a conflict (§5.3, leader only).",
	})

	editorWrites = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "editor_writes_total",
		Help: "Editor writes, by verb.",
	}, []string{"verb"})
	EditorAuth = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "editor_auth_total",
		Help: "Editor requests carrying credentials (basic auth sends them on every request), by result: ok, failed, throttled.",
	}, []string{"result"})
)

// EditorWrite counts one editor write.
func EditorWrite(verb string) { editorWrites.WithLabelValues(verb).Inc() }

// Register adds every metric to reg (controller-runtime's metrics.Registry).
func Register(reg prometheus.Registerer) {
	reg.MustRegister(checks, SnapshotBuilds, SnapshotBuildSeconds, SnapshotRoutes, SnapshotTimestamp,
		Leader, reconciles, Writes, ArmedRoutes, Conflicts, editorWrites, EditorAuth)
}
