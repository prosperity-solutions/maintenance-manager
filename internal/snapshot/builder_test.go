package snapshot

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
)

func TestDebounceWait(t *testing.T) {
	first := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		since time.Duration // now - first
		want  time.Duration
	}{
		{0, 100 * time.Millisecond},                      // trailing quiet period
		{500 * time.Millisecond, 100 * time.Millisecond}, // still within the max
		{950 * time.Millisecond, 50 * time.Millisecond},  // capped at first+1s
		{time.Second, 0},                                 // at the cap: now
		{3 * time.Second, 0},                             // past the cap: now, never negative
	}
	for _, tc := range cases {
		if got := debounceWait(first, first.Add(tc.since)); got != tc.want {
			t.Errorf("debounceWait after %s = %s, want %s", tc.since, got, tc.want)
		}
	}
}

func TestRelevantUpdate(t *testing.T) {
	obj := func(gen int64, labels map[string]string, rv string) *metav1.PartialObjectMetadata {
		return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Generation: gen, Labels: labels, ResourceVersion: rv}}
	}
	l := map[string]string{"app": "shop"}
	if relevantUpdate(obj(1, l, "1"), obj(1, l, "2")) {
		t.Error("a status-only update (same generation and labels) must not trigger a rebuild")
	}
	if !relevantUpdate(obj(1, l, "1"), obj(2, l, "2")) {
		t.Error("a generation change must trigger a rebuild")
	}
	if !relevantUpdate(obj(1, l, "1"), obj(1, map[string]string{"app": "other"}, "2")) {
		t.Error("a label change must trigger a rebuild: labels do not bump generation")
	}
	if !relevantUpdate("not an object", obj(1, l, "1")) {
		t.Error("unknown objects trigger a rebuild rather than being missed")
	}
	w := &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Generation: 3}}
	if !relevantUpdate(w, &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Generation: 4}}) {
		t.Error("a window spec change must trigger a rebuild")
	}
}

func TestStripRoute(t *testing.T) {
	m := NewRouteMetadata()
	m.Name, m.Namespace, m.Generation = "web", "shop", 7
	m.Labels = map[string]string{"app": "shop"}
	m.Annotations = map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{…}"}
	m.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl"}}
	out, err := StripRoute(m)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(*metav1.PartialObjectMetadata)
	if got.Annotations != nil || got.ManagedFields != nil {
		t.Fatal("annotations and managedFields must be dropped")
	}
	if got.Name != "web" || got.Namespace != "shop" || got.Generation != 7 || got.Labels["app"] != "shop" {
		t.Fatalf("name, namespace, generation and labels must stay: %+v", got.ObjectMeta)
	}
	if v, _ := StripRoute("other"); v != "other" {
		t.Fatal("other objects pass through")
	}
}
