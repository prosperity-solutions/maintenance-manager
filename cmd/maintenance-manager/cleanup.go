package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/prosperity-solutions/maintenance-manager/internal/controller"
)

// cleanup is the uninstall hook (§5.1). Exit 1 on failure makes Helm report
// it; re-running it is safe.
func cleanup(log *slog.Logger) int {
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		log.Error("cleanup: POD_NAMESPACE is not set; the chart's hook Job sets it")
		return 2
	}
	scheme, err := newScheme()
	if err != nil {
		log.Error("cleanup: scheme", "err", err)
		return 1
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error("cleanup: no Kubernetes config", "err", err)
		return 1
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error("cleanup: client", "err", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := controller.Cleanup(ctx, c, ns, log); err != nil {
		log.Error("cleanup failed; run it again, it is safe to repeat", "err", err)
		return 1
	}
	log.Info("cleanup done: no SecurityPolicies or ReferenceGrants of this install remain")
	return 0
}
