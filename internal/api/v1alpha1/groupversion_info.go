// Package v1alpha1 holds the three custom resources of maintenance-manager
// (docs/HANDOVER.md §4): MaintenanceService (which routes), MaintenancePage
// (what users see) and MaintenanceWindow (when, and what happens).
//
// Schema markers carry structural limits only; every other rule is validated
// in Go, by both the editor and the snapshot builder.
//
// +kubebuilder:object:generate=true
// +groupName=maintenance-manager.oss.life.li
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

//go:generate ../../../scripts/dev/t controller-gen object paths=.
//go:generate ../../../scripts/dev/t controller-gen crd:crdVersions=v1 paths=. output:crd:dir=../../../chart/templates/crds

// GroupVersion is the API group and version of the three resources.
var GroupVersion = schema.GroupVersion{Group: "maintenance-manager.oss.life.li", Version: "v1alpha1"}

var schemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&MaintenanceService{}, &MaintenanceServiceList{},
		&MaintenancePage{}, &MaintenancePageList{},
		&MaintenanceWindow{}, &MaintenanceWindowList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
})

// AddToScheme adds the types in this group-version to a scheme.
var AddToScheme = schemeBuilder.AddToScheme
