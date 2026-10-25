package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MaintenanceService selects the HTTPRoutes one app consists of. Its name is
// its display name and cannot change.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories=maintenance-manager,shortName=mms
// +kubebuilder:metadata:annotations="helm.sh/resource-policy=keep"
// +kubebuilder:metadata:labels="app.kubernetes.io/part-of=maintenance-manager"
type MaintenanceService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MaintenanceServiceSpec `json:"spec"`
}

// MaintenanceServiceSpec says which routes belong to the service.
type MaintenanceServiceSpec struct {
	// RouteSelector selects HTTPRoutes by label, cluster-wide. It must not be
	// empty: an empty selector would match every route in the cluster.
	// +required
	// +kubebuilder:validation:XValidation:rule="(has(self.matchLabels) && size(self.matchLabels) > 0) || (has(self.matchExpressions) && size(self.matchExpressions) > 0)",message="routeSelector must not be empty: it would select every HTTPRoute in the cluster"
	RouteSelector metav1.LabelSelector `json:"routeSelector"`
}

// +kubebuilder:object:root=true

// MaintenanceServiceList is a list of MaintenanceService.
type MaintenanceServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaintenanceService `json:"items"`
}

// MaintenancePage is what users see: an HTML template plus localized texts.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories=maintenance-manager,shortName=mmp
// +kubebuilder:metadata:annotations="helm.sh/resource-policy=keep"
// +kubebuilder:metadata:labels="app.kubernetes.io/part-of=maintenance-manager"
type MaintenancePage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MaintenancePageSpec `json:"spec"`
}

// MaintenancePageSpec is a page template and its texts per locale.
type MaintenancePageSpec struct {
	// FallbackLocale is used when no requested locale matches. It must be a key
	// of Locales.
	FallbackLocale string `json:"fallbackLocale"`

	// HTML is a Go html/template with .Title, .Message, .Locale, .Until,
	// .Continue and .Interstitial. Empty means the built-in default page. Only a safe subset
	// is allowed: text, {{.Field}}, if/else, with, and the functions eq, ne,
	// lt, le, gt, ge, and, or, not and len; no range, template, define, block,
	// variables or other functions. A page outside it is invalid and visitors
	// get the built-in page.
	// +optional
	// +kubebuilder:validation:MaxLength=65536
	HTML string `json:"html,omitempty"`

	// Locales maps a BCP 47 tag (e.g. "de-CH") to its texts.
	// +kubebuilder:validation:MaxProperties=32
	Locales map[string]PageText `json:"locales"`
}

// PageText is the localized text of a page.
type PageText struct {
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Title string `json:"title,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Message string `json:"message,omitempty"`
	// ContinueLabel is the text of the interstitial's button. Empty means a
	// built-in translation for the locale's language ("Weiter" for de), or
	// "Continue".
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[^\r\n]*$`
	ContinueLabel string `json:"continueLabel,omitempty"`
}

// +kubebuilder:object:root=true

// MaintenancePageList is a list of MaintenancePage.
type MaintenancePageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaintenancePage `json:"items"`
}

// MaintenanceWindow says when maintenance happens and what each phase does to
// which services.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories=maintenance-manager,shortName=mmw
// +kubebuilder:metadata:annotations="helm.sh/resource-policy=keep"
// +kubebuilder:metadata:labels="app.kubernetes.io/part-of=maintenance-manager"
// +kubebuilder:printcolumn:name="Enabled",type=boolean,JSONPath=`.spec.enabled`
// +kubebuilder:printcolumn:name="Ends",type=string,JSONPath=`.spec.endsAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type MaintenanceWindow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MaintenanceWindowSpec `json:"spec"`
}

// MaintenanceWindowSpec is a sequence of phases. See docs/HANDOVER.md §4.4
// for the phase rules.
type MaintenanceWindowSpec struct {
	// Enabled starts the window (and a phase 0 without startsAt); false stops
	// it immediately.
	// +optional
	Enabled bool `json:"enabled"`

	// EndsAt ends the window. Without it the window runs until disabled.
	// +optional
	EndsAt *metav1.Time `json:"endsAt,omitempty"`

	// +kubebuilder:validation:MaxItems=16
	Phases []Phase `json:"phases"`
}

// Phase is one step of a window.
type Phase struct {
	// Name is a DNS label (lowercase letters, digits, dashes); it appears in
	// the X-Maintenance-Manager header.
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// StartsAt is when the phase begins. Empty on phase 0 means "when the
	// window is enabled"; empty on a later phase means pending.
	// +optional
	StartsAt *metav1.Time `json:"startsAt,omitempty"`

	// Action is banner, interstitial or block.
	Action Action `json:"action"`

	// +kubebuilder:validation:MaxItems=32
	Routings []Routing `json:"routings"`
}

// Action is what a phase does to requests.
type Action string

// The three actions, in increasing severity.
const (
	ActionBanner       Action = "banner"
	ActionInterstitial Action = "interstitial"
	ActionBlock        Action = "block"
)

// Routing applies one page to a set of services.
type Routing struct {
	// Services are MaintenanceService names.
	// +kubebuilder:validation:MaxItems=64
	Services []string `json:"services"`

	// Page is a MaintenancePage name. Missing or empty means the default page.
	// +optional
	Page string `json:"page,omitempty"`
}

// +kubebuilder:object:root=true

// MaintenanceWindowList is a list of MaintenanceWindow.
type MaintenanceWindowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaintenanceWindow `json:"items"`
}
