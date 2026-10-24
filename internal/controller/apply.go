package controller

import (
	"context"
	"fmt"
	"maps"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// apply runs one plan step. It writes only when the object is missing or
// differs; wrote reports whether it did.
func (r *Reconciler) apply(ctx context.Context, a Action, ex existingObjects) (wrote bool, err error) {
	switch a.Kind {
	case EnsureGrant:
		return r.ensure(ctx, ex.grants[a.Namespace], DesiredGrant(r.Namespace, a.Namespace), false)
	case EnsureSP:
		return r.ensure(ctx, ex.policies[a.Namespace], DesiredSP(r.Namespace, a.Namespace, a.Targets), true)
	case DeleteSP:
		return r.remove(ctx, ex.policies[a.Namespace])
	case DeleteGrant:
		return r.remove(ctx, ex.grants[a.Namespace])
	}
	return false, fmt.Errorf("unknown action %q", a.Kind)
}

// ensure creates the object, or replaces its spec and owner labels when they
// differ. A full Update (not server-side apply) is what removes fields other
// managers added to our SecurityPolicy, such as bodyToExtAuth: SSA never
// drops fields it does not own.
func (r *Reconciler) ensure(ctx context.Context, existing, desired *unstructured.Unstructured, strict bool) (bool, error) {
	if existing == nil {
		if err := r.Writer.Create(ctx, desired, client.FieldOwner(managedBy)); err != nil {
			return false, err
		}
		r.Log.Info("created", "kind", desired.GetKind(), "namespace", desired.GetNamespace(), "name", desired.GetName())
		return true, nil
	}
	if !needsUpdate(existing, desired, strict) {
		return false, nil
	}
	obj := existing.DeepCopy()
	obj.SetGroupVersionKind(desired.GroupVersionKind())
	obj.Object["spec"] = desired.Object["spec"]
	lbls := obj.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	maps.Copy(lbls, desired.GetLabels())
	obj.SetLabels(lbls)
	delete(obj.Object, "status") // the spec update ignores status anyway
	if err := r.Writer.Update(ctx, obj, client.FieldOwner(managedBy)); err != nil {
		return false, err
	}
	r.Log.Info("updated", "kind", desired.GetKind(), "namespace", desired.GetNamespace(), "name", desired.GetName())
	return true, nil
}

// remove deletes an object we own, with UID and resourceVersion
// preconditions: a stale ex-leader cannot delete what the new leader wrote.
func (r *Reconciler) remove(ctx context.Context, obj *unstructured.Unstructured) (bool, error) {
	if obj == nil {
		return false, nil
	}
	uid, rv := obj.GetUID(), obj.GetResourceVersion()
	err := r.Writer.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &rv})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	r.Log.Info("deleted", "kind", obj.GetKind(), "namespace", obj.GetNamespace(), "name", obj.GetName())
	return true, nil
}
