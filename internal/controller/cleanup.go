package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Cleanup is `maintenance-manager cleanup`, run by the chart's pre-delete
// hook (§5.1): scale our Deployment to 0 and wait until its pods are gone
// and the Lease is free, so no leader can write again; then delete our
// SecurityPolicies (cluster-wide, by owner label), then our ReferenceGrants.
// It is idempotent: re-running after a partial failure finishes the job.
func Cleanup(ctx context.Context, c client.Client, releaseNS string, log *slog.Logger) error {
	var d appsv1.Deployment
	err := c.Get(ctx, client.ObjectKey{Namespace: releaseNS, Name: serviceName}, &d)
	switch {
	case apierrors.IsNotFound(err):
		log.Info("no Deployment to scale down")
	case err != nil:
		return fmt.Errorf("get Deployment: %w", err)
	default:
		patch := client.MergeFrom(d.DeepCopy())
		zero := int32(0)
		d.Spec.Replicas = &zero
		if err := c.Patch(ctx, &d, patch); err != nil {
			return fmt.Errorf("scale Deployment to 0: %w", err)
		}
		log.Info("scaled the Deployment to 0")
	}

	if err := waitFor(ctx, 90*time.Second, "maintenance-manager pods to stop", func() (bool, error) {
		var pods corev1.PodList
		if err := c.List(ctx, &pods, client.InNamespace(releaseNS), client.MatchingLabels{"app.kubernetes.io/name": serviceName}); err != nil {
			return false, err
		}
		return len(pods.Items) == 0, nil
	}); err != nil {
		return err
	}
	if err := waitFor(ctx, 30*time.Second, "the leader Lease to be free", func() (bool, error) {
		var l coordinationv1.Lease
		err := c.Get(ctx, client.ObjectKey{Namespace: releaseNS, Name: leaseName}, &l)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "" || l.Spec.RenewTime == nil {
			return true, nil
		}
		dur := leaseDuration
		if l.Spec.LeaseDurationSeconds != nil {
			dur = time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second
		}
		return time.Since(l.Spec.RenewTime.Time) > dur, nil
	}); err != nil {
		return err
	}

	sel := client.MatchingLabels{LabelOwner: releaseNS, LabelManagedBy: managedBy}
	for _, step := range []struct {
		gvkList string
		opts    []client.ListOption
	}{
		{"SecurityPolicyList", []client.ListOption{sel}},
		{"ReferenceGrantList", []client.ListOption{sel, client.InNamespace(releaseNS)}},
	} {
		list := &unstructured.UnstructuredList{}
		gv := SecurityPolicyGVK.GroupVersion()
		if step.gvkList == "ReferenceGrantList" {
			gv = ReferenceGrantGVK.GroupVersion()
		}
		list.SetGroupVersionKind(gv.WithKind(step.gvkList))
		if err := c.List(ctx, list, step.opts...); err != nil {
			return fmt.Errorf("list %s: %w", step.gvkList, err)
		}
		for i := range list.Items {
			obj := &list.Items[i]
			// Only our names: the controller never adopts other objects that
			// carry our labels, so cleanup must not delete them either.
			if ns, ok := grantNamespace(obj); (step.gvkList == "SecurityPolicyList" && obj.GetName() != SPName(releaseNS)) ||
				(step.gvkList == "ReferenceGrantList" && (!ok || obj.GetName() != GrantName(ns))) {
				continue
			}
			if err := client.IgnoreNotFound(c.Delete(ctx, obj)); err != nil {
				return fmt.Errorf("delete %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
			}
			log.Info("deleted", "kind", obj.GetKind(), "namespace", obj.GetNamespace(), "name", obj.GetName())
		}
	}
	return nil
}

func waitFor(ctx context.Context, timeout time.Duration, what string, f func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := f()
		if err == nil && ok {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("waiting for %s: %w", what, err)
			}
			return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// grantNamespace returns the single "from" namespace of one of our grants.
func grantNamespace(g *unstructured.Unstructured) (string, bool) {
	from, _, _ := unstructured.NestedSlice(g.Object, "spec", "from")
	if len(from) != 1 {
		return "", false
	}
	f0, ok := from[0].(map[string]any)
	if !ok {
		return "", false
	}
	ns, _, _ := unstructured.NestedString(f0, "namespace")
	return ns, ns != ""
}
