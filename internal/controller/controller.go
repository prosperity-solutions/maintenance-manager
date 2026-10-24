package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

// onlyRaces reports whether every joined error is an AlreadyExists or a
// Conflict: cache lag after our own writes, not a real failure.
func onlyRaces(err error) bool {
	var joined interface{ Unwrap() []error }
	errs := []error{err}
	if errors.As(err, &joined) {
		errs = joined.Unwrap()
	}
	for _, e := range errs {
		if !apierrors.IsAlreadyExists(e) && !apierrors.IsConflict(e) {
			return false
		}
	}
	return len(errs) > 0
}

// Leader election timing (client-go defaults used by controller-runtime).
const (
	leaseDuration = 15 * time.Second
	renewDeadline = 10 * time.Second
	retryPeriod   = 2 * time.Second
	leaseName     = "maintenance-manager"

	debounceQuiet   = 100 * time.Millisecond
	debounceMax     = time.Second
	resync          = 10 * time.Minute // full reconcile even without events
	maxTermFailures = 3
	syncRounds      = 4 // 30 s each before a term counts as failed
	retryAfterErr   = 5 * time.Second
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

	conflicts map[snapshot.RouteKey][]string // last logged, per route
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
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
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
				// lead returned while still leading: it gave up. Stop renewing,
				// so another replica can take over.
				cancelRun()
			},
			OnStoppedLeading: func() { c.Log.Info("not leading any more") },
		},
	})
	if err != nil {
		c.Log.Error("controller: leader election setup failed", "err", err)
		return
	}
	elector.Run(runCtx) // returns when leadership is lost, lead gives up, or ctx is done
}

func (c *Controller) recover(where string) {
	if p := recover(); p != nil {
		c.Log.Error("controller panicked; it restarts, authz is unaffected", "where", where, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
	}
}

// lead runs terms (discovery, a dedicated cache for the cluster-wide kinds
// only the leader needs, the reconcile loop) until ctx is cancelled. After
// maxTermFailures failed terms in a row it returns, and campaign releases the
// Lease, so a healthy replica can take over. client-go releases the Lease
// without waiting for this goroutine; optimistic concurrency (Create, Update
// with resourceVersion, Delete with UID and resourceVersion preconditions)
// keeps a late write from an ex-leader harmless.
func (c *Controller) lead(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		err := c.safeTerm(ctx)
		if err == nil {
			return // ctx done
		}
		failures++
		c.Log.Error("controller term failed", "err", err, "failures", failures)
		if failures >= maxTermFailures {
			c.Log.Error("giving up the Lease so another replica can try")
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(retryAfterErr):
		}
	}
}

// safeTerm runs one term and turns a panic into an error.
func (c *Controller) safeTerm(ctx context.Context) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	return c.term(ctx)
}

var (
	listenerSetGVKs = []schema.GroupVersionKind{
		{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "ListenerSet"},
		{Group: "gateway.networking.x-k8s.io", Version: "v1alpha1", Kind: "XListenerSet"},
	}
)

// served reports whether the API server serves a kind. Only a "no match"
// means absent; any other discovery error fails the term rather than drop
// a watch the conflict check needs.
func (c *Controller) served(gvk schema.GroupVersionKind) (bool, error) {
	_, err := c.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	switch {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err):
		return false, nil
	default:
		return false, fmt.Errorf("discover %s: %w", gvk.Kind, err)
	}
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
	objs := []client.Object{&egv1a1.SecurityPolicy{}, &gwapiv1.Gateway{}, &corev1.Namespace{}, grant}
	var lsLists []*unstructured.UnstructuredList
	for _, gvk := range listenerSetGVKs {
		ok, err := c.served(gvk)
		if err != nil {
			return err
		}
		if ok {
			ls := &unstructured.Unstructured{}
			ls.SetGroupVersionKind(gvk)
			objs = append(objs, ls)
			l := &unstructured.UnstructuredList{}
			l.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
			lsLists = append(lsLists, l)
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
		AddFunc: func(any) { trigger() },
		UpdateFunc: func(o, n any) {
			if relevantUpdate(o, n) {
				trigger()
			}
		},
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
	for round := 1; ; round++ {
		wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
		synced := leaderCache.WaitForCacheSync(wctx) && c.Shared.WaitForCacheSync(wctx)
		wcancel()
		if synced {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		c.Log.Warn("controller caches have not synced after 30 s; nothing is armed meanwhile. Check RBAC on securitypolicies, gateways, listenersets, namespaces (cluster-wide) and referencegrants (release namespace)")
		if round >= syncRounds {
			return fmt.Errorf("caches did not sync in %d rounds", round)
		}
	}

	rec := &Reconciler{
		Namespace: c.Namespace, Lead: c.Lead, Shared: c.Shared, Leader: leaderCache, Writer: c.Writer,
		Log: c.Log, Now: c.Now, ListenerSets: lsLists,
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
			case err != nil && onlyRaces(err):
				// Our own write not yet in the cache, or EG wrote status: retry soon.
				c.Log.Info("reconcile raced with a cache update; retrying", "err", err)
				next = time.Second
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
	c.logConflicts(res.Desired.Conflicts)
	if err == nil && res.Writes > 0 {
		armed := 0
		for _, rs := range res.Desired.Routes {
			armed += len(rs)
		}
		c.Log.Info("reconciled", "namespaces", len(res.Desired.Routes), "armedRoutes", armed, "conflicts", len(res.Desired.Conflicts), "writes", res.Writes)
	}
	return res, err
}

// logConflicts logs a route's conflict reasons when they change, so an
// operator can see why a route is not armed (the editor shows them too).
func (c *Controller) logConflicts(now map[snapshot.RouteKey][]string) {
	for k, reasons := range now {
		if !slices.Equal(c.conflicts[k], reasons) {
			c.Log.Warn("route not armed: conflict", "route", k.Namespace+"/"+k.Name, "reasons", reasons)
		}
	}
	for k := range c.conflicts {
		if _, still := now[k]; !still {
			c.Log.Info("route conflict resolved", "route", k.Namespace+"/"+k.Name)
		}
	}
	c.conflicts = now
}

// relevantUpdate: generation or labels changed (status churn is ignored).
func relevantUpdate(o, n any) bool {
	om, ok1 := o.(client.Object)
	nm, ok2 := n.(client.Object)
	if !ok1 || !ok2 {
		return true
	}
	return om.GetGeneration() != nm.GetGeneration() || !maps.Equal(om.GetLabels(), nm.GetLabels())
}

func reset(t *time.Timer, d time.Duration) {
	t.Stop()
	select {
	case <-t.C:
	default:
	}
	t.Reset(d)
}
