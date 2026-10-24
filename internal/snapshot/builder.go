package snapshot

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
)

// Debounce timing (§3): rebuild 100 ms after the last change, but at most
// 1 s after the first one.
const (
	debounceQuiet = 100 * time.Millisecond
	debounceMax   = time.Second
)

// HTTPRouteGVK is the Gateway API HTTPRoute.
var HTTPRouteGVK = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}

// StripRoute is the cache transform for HTTPRoutes (§3): keep name,
// namespace, labels, generation and spec.parentRefs; drop managedFields,
// annotations, the rest of the spec and the status. One typed watch serves
// the snapshot (labels) and the controller (parentRefs): controller-runtime
// keeps one cache per kind.
func StripRoute(obj any) (any, error) {
	r, ok := obj.(*gwapiv1.HTTPRoute)
	if !ok {
		return obj, nil
	}
	r.ManagedFields = nil
	r.Annotations = nil
	r.OwnerReferences = nil
	r.Spec = gwapiv1.HTTPRouteSpec{CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: r.Spec.ParentRefs}}
	r.Status = gwapiv1.HTTPRouteStatus{}
	return r, nil
}

// Builder watches the resources, rebuilds a snapshot on every relevant
// change (debounced), and hands it to OnSnapshot. Every replica runs one.
type Builder struct {
	Cache      cache.Cache
	Namespace  string // release namespace: our resources live here
	Bypass     string
	Log        *slog.Logger
	Now        func() time.Time
	OnSnapshot func(*Snapshot)

	kick    chan struct{}
	once    sync.Once
	covered int // routes covered by the last snapshot, for quieter logs
}

// NeedLeaderElection reports false: every replica builds its own snapshot.
func (b *Builder) NeedLeaderElection() bool { return false }

func (b *Builder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Builder) init() {
	b.once.Do(func() { b.kick = make(chan struct{}, 1) })
}

// trigger asks for a rebuild without blocking.
func (b *Builder) trigger() {
	b.init()
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// Start registers the event handlers, waits for the caches, builds the first
// snapshot and then rebuilds on changes until ctx is done.
func (b *Builder) Start(ctx context.Context) error {
	b.init()
	objs := []client.Object{&mmv1.MaintenanceWindow{}, &mmv1.MaintenancePage{}, &mmv1.MaintenanceService{}, &gwapiv1.HTTPRoute{}}
	for _, obj := range objs {
		inf, err := b.Cache.GetInformer(ctx, obj, cache.BlockUntilSynced(false))
		if err != nil {
			return fmt.Errorf("snapshot: informer for %T: %w", obj, err)
		}
		if _, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc:    func(any) { b.trigger() },
			DeleteFunc: func(any) { b.trigger() },
			UpdateFunc: func(oldObj, newObj any) {
				if relevantUpdate(oldObj, newObj) {
					b.trigger()
				}
			},
		}); err != nil {
			return fmt.Errorf("snapshot: event handler for %T: %w", obj, err)
		}
	}
	// Wait for the caches, and say so while waiting: missing RBAC on
	// httproutes or an API outage at boot would otherwise hang silently.
	for {
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		synced := b.Cache.WaitForCacheSync(wctx)
		cancel()
		if synced {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		b.Log.Warn("caches have not synced after 30 s; not ready yet. Check RBAC on httproutes (cluster-wide list/watch) and on our resources, and the API server")
	}

	timer := time.NewTimer(0) // the first build, right away
	defer timer.Stop()
	var first time.Time // first change since the last build, zero if none
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-b.kick:
			now := time.Now()
			if first.IsZero() {
				first = now
			}
			resetTimer(timer, debounceWait(first, now))
		case <-timer.C:
			first = time.Time{}
			next, err := b.rebuild(ctx)
			if err != nil {
				b.Log.Error("snapshot rebuild failed; keeping the previous one", "err", err)
				resetTimer(timer, time.Second)
				continue
			}
			// Wake when a window ends, so ended windows leave the snapshot.
			if !next.IsZero() {
				resetTimer(timer, next.Sub(b.now())+10*time.Millisecond)
			}
		}
	}
}

// debounceWait is how long to wait before rebuilding, given the first change
// since the last build: debounceQuiet after the latest change, but never past
// first+debounceMax (§3).
func debounceWait(first, now time.Time) time.Duration {
	if left := debounceMax - now.Sub(first); left < debounceQuiet {
		return max(left, 0)
	}
	return debounceQuiet
}

func resetTimer(t *time.Timer, d time.Duration) {
	t.Stop()
	select {
	case <-t.C:
	default:
	}
	t.Reset(d)
}

// relevantUpdate filters status-only updates: generation or labels changed
// (labels do not bump generation, and a relabelled route must be picked up).
func relevantUpdate(oldObj, newObj any) bool {
	o, ok1 := oldObj.(metav1.Object)
	n, ok2 := newObj.(metav1.Object)
	if !ok1 || !ok2 {
		return true
	}
	return o.GetGeneration() != n.GetGeneration() || !maps.Equal(o.GetLabels(), n.GetLabels())
}

// rebuild lists everything from the cache, builds and publishes a snapshot,
// and returns the next window end after now (zero if none).
func (b *Builder) rebuild(ctx context.Context) (time.Time, error) {
	var (
		windows  mmv1.MaintenanceWindowList
		pages    mmv1.MaintenancePageList
		services mmv1.MaintenanceServiceList
		routes   gwapiv1.HTTPRouteList
	)
	inNS := client.InNamespace(b.Namespace)
	for _, l := range []struct {
		list client.ObjectList
		opts []client.ListOption
	}{{&windows, []client.ListOption{inNS}}, {&pages, []client.ListOption{inNS}}, {&services, []client.ListOption{inNS}}, {&routes, nil}} {
		if err := b.Cache.List(ctx, l.list, l.opts...); err != nil {
			return time.Time{}, err
		}
	}
	now := b.now()
	in := Input{Windows: windows.Items, Pages: pages.Items, Services: services.Items, Bypass: b.Bypass, Now: now}
	in.Routes = make([]RouteInfo, 0, len(routes.Items))
	for _, r := range routes.Items {
		in.Routes = append(in.Routes, RouteInfo{Key: RouteKey{Namespace: r.Namespace, Name: r.Name}, Labels: r.Labels, Parents: r.Spec.ParentRefs})
	}
	t0 := time.Now()
	snap := Build(in)
	b.OnSnapshot(snap)
	// Any HTTPRoute change in the cluster rebuilds; say so at Info only when
	// the set of covered routes changes size.
	level := slog.LevelDebug
	if len(snap.Routes) != b.covered {
		level = slog.LevelInfo
		b.covered = len(snap.Routes)
	}
	b.Log.Log(ctx, level, "snapshot built", "windows", len(windows.Items), "routes", len(routes.Items), "covered", len(snap.Routes), "took", time.Since(t0).String())

	var next time.Time
	for i := range windows.Items {
		if e := windows.Items[i].Spec.EndsAt; e != nil && e.After(now) && (next.IsZero() || e.Time.Before(next)) {
			next = e.UTC()
		}
	}
	return next, nil
}
