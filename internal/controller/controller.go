package controller

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
)

// Leader election timing (client-go defaults used by controller-runtime).
const (
	leaseDuration = 15 * time.Second
	renewDeadline = 10 * time.Second
	retryPeriod   = 2 * time.Second
	leaseName     = "maintenance-manager"

	debounceQuiet = 100 * time.Millisecond
	debounceMax   = time.Second
	resync        = 10 * time.Minute // full reconcile even without events
	retryAfterErr = 5 * time.Second
)

// Controller arms and disarms routes on the leader. Every replica runs it;
// only the Lease holder reconciles. It never returns an error and recovers
// from panics: authz runs in the same process and must never stop because
// of the controller (§12: leader lost, authz unaffected).
type Controller struct {
	Config    *rest.Config
	Scheme    *runtime.Scheme
	Mapper    meta.RESTMapper
	Shared    cache.Cache // our resources and HTTPRoutes
	Writer    client.Client
	Namespace string
	Identity  string // this pod
	Lead      time.Duration
	Log       *slog.Logger
	Now       func() time.Time
}

// NeedLeaderElection reports false: the controller runs its own elector, so
// losing the Lease never stops the manager (and authz with it).
func (c *Controller) NeedLeaderElection() bool { return false }

// Start campaigns for the Lease until ctx is done.
func (c *Controller) Start(ctx context.Context) error {
	kube, err := kubernetes.NewForConfig(c.Config)
	if err != nil {
		c.Log.Error("controller: no Kubernetes client; arming is off, authz keeps serving", "err", err)
		<-ctx.Done()
		return nil
	}
	for ctx.Err() == nil {
		c.campaign(ctx, kube)
		select {
		case <-ctx.Done():
		case <-time.After(retryPeriod):
		}
	}
	return nil
}

func (c *Controller) campaign(ctx context.Context, kube kubernetes.Interface) {
	defer c.recover("campaign")
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metaFor(c.Namespace, leaseName),
		Client:     kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: c.Identity},
	}
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   leaseDuration,
		RenewDeadline:   renewDeadline,
		RetryPeriod:     retryPeriod,
		ReleaseOnCancel: true,
		Name:            leaseName,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				c.Log.Info("leading: the controller arms and disarms routes")
				c.lead(ctx)
			},
			OnStoppedLeading: func() { c.Log.Info("not leading any more") },
		},
	})
	if err != nil {
		c.Log.Error("controller: leader election setup failed", "err", err)
		return
	}
	elector.Run(ctx) // returns when leadership is lost or ctx is done
}

func (c *Controller) recover(where string) {
	if p := recover(); p != nil {
		c.Log.Error("controller panicked; it restarts, authz is unaffected", "where", where, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
	}
}

// lead runs one leadership term: discovery, a dedicated cache for the
// cluster-wide kinds only the leader needs, and the reconcile loop. It
// returns (and everything it started stops) when ctx is cancelled. It
// returns only after its writes are done, so ReleaseOnCancel never hands the
// Lease over mid-write.
func (c *Controller) lead(ctx context.Context) {
	defer c.recover("lead")
	for ctx.Err() == nil {
		if err := c.term(ctx); err != nil {
			c.Log.Error("controller term failed; retrying", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(retryAfterErr):
		}
	}
}

var (
	listenerSetGVKs = []schema.GroupVersionKind{
		{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "ListenerSet"},
		{Group: "gateway.networking.x-k8s.io", Version: "v1alpha1", Kind: "XListenerSet"},
	}
)

// served reports whether the API server serves a kind right now.
func (c *Controller) served(gvk schema.GroupVersionKind) bool {
	_, err := c.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	return err == nil
}

func (c *Controller) term(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if reset, ok := c.Mapper.(meta.ResettableRESTMapper); ok {
		reset.Reset() // discover kinds installed since the last term
	}
	grant := &unstructured.Unstructured{}
	grant.SetGroupVersionKind(ReferenceGrantGVK)
	byObject := map[client.Object]cache.ByObject{
		grant: {Namespaces: map[string]cache.Config{c.Namespace: {}}},
	}
	objs := []client.Object{&egv1a1.SecurityPolicy{}, &gwapiv1.Gateway{}, grant}
	var lsList *unstructured.UnstructuredList
	for _, gvk := range listenerSetGVKs {
		if c.served(gvk) {
			ls := &unstructured.Unstructured{}
			ls.SetGroupVersionKind(gvk)
			objs = append(objs, ls)
			lsList = &unstructured.UnstructuredList{}
			lsList.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
			break
		}
	}
	leaderCache, err := cache.New(c.Config, cache.Options{Scheme: c.Scheme, Mapper: c.Mapper, ByObject: byObject})
	if err != nil {
		return fmt.Errorf("leader cache: %w", err)
	}
	kick := make(chan struct{}, 1)
	trigger := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	handler := toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { trigger() },
		UpdateFunc: func(any, any) { trigger() },
		DeleteFunc: func(any) { trigger() },
	}
	for _, obj := range objs {
		inf, err := leaderCache.GetInformer(ctx, obj, cache.BlockUntilSynced(false))
		if err != nil {
			return fmt.Errorf("informer for %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, err)
		}
		if _, err := inf.AddEventHandler(handler); err != nil {
			return err
		}
	}
	// Changes to our resources and to HTTPRoutes come from the shared cache.
	var regs []func()
	defer func() {
		for _, rm := range regs {
			rm()
		}
	}()
	for _, obj := range []client.Object{&mmv1.MaintenanceWindow{}, &mmv1.MaintenanceService{}, &gwapiv1.HTTPRoute{}} {
		inf, err := c.Shared.GetInformer(ctx, obj, cache.BlockUntilSynced(false))
		if err != nil {
			return err
		}
		reg, err := inf.AddEventHandler(handler)
		if err != nil {
			return err
		}
		regs = append(regs, func() { _ = inf.RemoveEventHandler(reg) })
	}

	go func() { _ = leaderCache.Start(ctx) }()
	for !leaderCache.WaitForCacheSync(ctx) || !c.Shared.WaitForCacheSync(ctx) {
		if ctx.Err() != nil {
			return nil
		}
		c.Log.Warn("controller caches have not synced; check RBAC on securitypolicies, gateways, listenersets (cluster-wide) and referencegrants (release namespace)")
	}

	rec := &Reconciler{
		Namespace: c.Namespace, Lead: c.Lead, Shared: c.Shared, Leader: leaderCache, Writer: c.Writer,
		Log: c.Log, Now: c.Now, ListenerSets: nil,
	}
	if lsList != nil {
		rec.ListenerSets = lsList
	}
	return c.loop(ctx, rec, kick)
}

// loop reconciles on changes (debounced) and at the next arm boundary.
func (c *Controller) loop(ctx context.Context, rec *Reconciler, kick <-chan struct{}) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	var first time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-kick:
			now := time.Now()
			if first.IsZero() {
				first = now
			}
			wait := debounceQuiet
			if left := debounceMax - now.Sub(first); left < wait {
				wait = max(left, 0)
			}
			reset(timer, wait)
		case <-timer.C:
			first = time.Time{}
			res, err := c.reconcileOnce(ctx, rec)
			next := resync
			switch {
			case err != nil:
				c.Log.Error("reconcile failed; retrying", "err", err)
				next = retryAfterErr
			case !res.Desired.NextWake.IsZero():
				if until := res.Desired.NextWake.Sub(rec.now()); until < next {
					next = max(until, 0) + 10*time.Millisecond
				}
			}
			reset(timer, next)
		}
	}
}

func (c *Controller) reconcileOnce(ctx context.Context, rec *Reconciler) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	res, err = rec.Reconcile(ctx)
	if err == nil && res.Writes > 0 {
		armed := 0
		for _, rs := range res.Desired.Routes {
			armed += len(rs)
		}
		c.Log.Info("reconciled", "namespaces", len(res.Desired.Routes), "armedRoutes", armed, "conflicts", len(res.Desired.Conflicts), "writes", res.Writes)
	}
	return res, err
}

func reset(t *time.Timer, d time.Duration) {
	t.Stop()
	select {
	case <-t.C:
	default:
	}
	t.Reset(d)
}
