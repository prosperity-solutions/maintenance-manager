package controller

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// EnvoyAccepted reports whether Envoy Gateway has taken a SecurityPolicy into
// account on every ancestor for its current generation (§10 dashboard,
// §13.2). An ancestor counts when it reports Accepted=True, or Merged=True:
// measured on EG v1.9.1 (D6), an ancestor where our policy merges into a
// Gateway-level policy carries only "Merged=True" and no Accepted condition.
// The string explains a false.
func EnvoyAccepted(sp *unstructured.Unstructured) (bool, string) {
	ancestors, _, _ := unstructured.NestedSlice(sp.Object, "status", "ancestors")
	if len(ancestors) == 0 {
		return false, "no ancestors in status yet"
	}
	for _, a := range ancestors {
		am, _ := a.(map[string]any)
		conds, _, _ := unstructured.NestedSlice(am, "conditions")
		good := false
		for _, c := range conds {
			m, _ := c.(map[string]any)
			typ, status := m["type"], m["status"]
			if typ != "Accepted" && typ != "Merged" {
				continue
			}
			gen, _ := m["observedGeneration"].(int64)
			if gen != sp.GetGeneration() {
				return false, fmt.Sprintf("%v is for generation %d, the policy is at %d", typ, gen, sp.GetGeneration())
			}
			if status != "True" {
				return false, fmt.Sprintf("%v=%v reason=%v: %v", typ, status, m["reason"], m["message"])
			}
			good = true
		}
		if !good {
			return false, fmt.Sprintf("no Accepted or Merged condition yet on ancestor %v", am["ancestorRef"])
		}
	}
	return true, ""
}
