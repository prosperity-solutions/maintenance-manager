// Package workflows asserts the rules .github/workflows/*.yml must keep
// (docs/HANDOVER.md §15, §17.2), and runs the agent guard's fixtures. zizmor
// and actionlint cover general hygiene; this covers what is specific to us.
package workflows

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type workflow struct {
	Name        string            `yaml:"name"`
	On          yaml.Node         `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Concurrency map[string]string `yaml:"concurrency"`
	Jobs        map[string]job    `yaml:"jobs"`
}

type job struct {
	Name           string            `yaml:"name"`
	Needs          yaml.Node         `yaml:"needs"`
	If             string            `yaml:"if"`
	TimeoutMinutes int               `yaml:"timeout-minutes"`
	Permissions    yaml.Node         `yaml:"permissions"`
	Concurrency    yaml.Node         `yaml:"concurrency"`
	Env            map[string]string `yaml:"env"`
	Steps          []step            `yaml:"steps"`
}

type step struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	If   string            `yaml:"if"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

func (j job) needs() []string {
	switch j.Needs.Kind {
	case yaml.ScalarNode:
		return []string{j.Needs.Value}
	case yaml.SequenceNode:
		var out []string
		for _, n := range j.Needs.Content {
			out = append(out, n.Value)
		}
		return out
	}
	return nil
}

var releaseJobs = []string{"publish", "release", "verify"}

func loadWorkflows(t *testing.T) map[string]struct {
	raw string
	wf  workflow
	doc yaml.Node
} {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRoot(t), ".github/workflows/*.y*ml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	out := map[string]struct {
		raw string
		wf  workflow
		doc yaml.Node
	}{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var wf workflow
		if err := yaml.Unmarshal(raw, &wf); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[filepath.Base(f)] = struct {
			raw string
			wf  workflow
			doc yaml.Node
		}{string(raw), wf, doc}
	}
	return out
}

func TestOnlyCIWorkflow(t *testing.T) {
	wfs := loadWorkflows(t)
	if len(wfs) != 1 {
		t.Fatalf("ci.yml must be the only workflow (§15), found %d", len(wfs))
	}
	if _, ok := wfs["ci.yml"]; !ok {
		t.Fatal("the workflow must be .github/workflows/ci.yml: verification pins --signer-workflow to it")
	}
}

func TestTriggers(t *testing.T) {
	for name, w := range loadWorkflows(t) {
		var on map[string]struct {
			Types    []string `yaml:"types"`
			Branches []string `yaml:"branches"`
		}
		if err := w.wf.On.Decode(&on); err != nil {
			t.Fatalf("%s: on: %v", name, err)
		}
		keys := make([]string, 0, len(on))
		for k := range on {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"pull_request", "push"}) {
			t.Errorf("%s: triggers must be exactly pull_request and push, got %v", name, keys)
		}
		if got := on["pull_request"].Types; !slices.Equal(got, []string{"opened", "synchronize", "reopened", "ready_for_review"}) {
			t.Errorf("%s: pull_request types = %v", name, got)
		}
		if got := on["push"].Branches; !slices.Equal(got, []string{"main"}) {
			t.Errorf("%s: push branches = %v, want [main]", name, got)
		}
	}
}

func TestPermissionsAndConcurrency(t *testing.T) {
	for name, w := range loadWorkflows(t) {
		if len(w.wf.Permissions) != 1 || w.wf.Permissions["contents"] != "read" {
			t.Errorf("%s: top-level permissions must be exactly contents: read, got %v", name, w.wf.Permissions)
		}
		if w.wf.Concurrency["cancel-in-progress"] != "${{ github.event_name == 'pull_request' }}" {
			t.Errorf("%s: workflow concurrency must cancel only PR runs", name)
		}
		for id, j := range w.wf.Jobs {
			if !j.Concurrency.IsZero() {
				t.Errorf("%s/%s: no job-level concurrency groups (§15)", name, id)
			}
			// Every widened permission carries a comment saying why.
			for i := 0; i+1 < len(j.Permissions.Content); i += 2 {
				k, v := j.Permissions.Content[i], j.Permissions.Content[i+1]
				if v.LineComment == "" {
					t.Errorf("%s/%s: permission %s: %s needs a comment saying why", name, id, k.Value, v.Value)
				}
			}
		}
	}
}

var pinned = regexp.MustCompile(`^uses: [^@\s]+@[0-9a-f]{40} # v\d+\.\d+\.\d+\s*$`)

func TestActionsPinnedBySHA(t *testing.T) {
	for name, w := range loadWorkflows(t) {
		for i, line := range strings.Split(w.raw, "\n") {
			l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
			if !strings.HasPrefix(l, "uses:") {
				continue
			}
			if !pinned.MatchString(l) {
				t.Errorf("%s:%d: pin by 40-character SHA with a `# vX.Y.Z` comment: %s", name, i+1, l)
			}
		}
	}
}

func TestNoSecretsNoCacheNoArtifacts(t *testing.T) {
	for name, w := range loadWorkflows(t) {
		if strings.Contains(w.raw, "secrets.") {
			t.Errorf("%s: no secrets. expressions; the repo has none (§18)", name)
		}
		for _, forbidden := range []string{"actions/cache", "actions/upload-artifact", "actions/download-artifact", "pull_request_target", "workflow_run"} {
			if strings.Contains(w.raw, forbidden) {
				t.Errorf("%s: %s is not allowed (§15, A2, A3)", name, forbidden)
			}
		}
	}
}

func TestJobs(t *testing.T) {
	for name, w := range loadWorkflows(t) {
		jobs := w.wf.Jobs
		gate, ok := jobs["gate"]
		if !ok {
			t.Fatalf("%s: no gate job", name)
		}
		if gate.If != "github.event_name != 'pull_request' || github.event.pull_request.draft == false" {
			t.Errorf("%s: gate must carry the draft guard, got %q", name, gate.If)
		}
		for id, j := range jobs {
			if j.TimeoutMinutes <= 0 {
				t.Errorf("%s/%s: every job needs timeout-minutes", name, id)
			}
			if id != "gate" && !reachesGate(jobs, id, map[string]bool{}) {
				t.Errorf("%s/%s: must need gate, directly or transitively", name, id)
			}
			for i, s := range j.Steps {
				if strings.Contains(s.Run, "${{") {
					t.Errorf("%s/%s step %d: never interpolate ${{ }} into run:, pass it through env: (A1)", name, id, i)
				}
				if strings.HasPrefix(s.Uses, "actions/checkout@") && s.With["persist-credentials"] != "false" {
					t.Errorf("%s/%s step %d: checkout needs persist-credentials: false", name, id, i)
				}
				if ghCall.MatchString(s.Run) && s.Env["GH_TOKEN"] != "${{ github.token }}" && j.Env["GH_TOKEN"] != "${{ github.token }}" {
					t.Errorf("%s/%s step %d: runs gh without GH_TOKEN: ${{ github.token }}", name, id, i)
				}
				for _, sink := range []string{"GITHUB_ENV", "GITHUB_PATH"} {
					if strings.Contains(s.Run, sink) {
						t.Errorf("%s/%s step %d: never write to %s (A1)", name, id, i, sink)
					}
				}
			}
		}
		ag, ok := jobs["all-green"]
		if !ok || ag.If != "always()" || ag.Name != "all-green" {
			t.Errorf("%s: all-green must exist, be named all-green and run if: always()", name)
		}
	}
}

// ghCall matches gh in command position, not inside an echoed hint.
var ghCall = regexp.MustCompile(`(?m)(^\s*|\$\(|[|&;]\s*)gh\s`)

func reachesGate(jobs map[string]job, id string, seen map[string]bool) bool {
	if seen[id] {
		return false
	}
	seen[id] = true
	for _, n := range jobs[id].needs() {
		if n == "gate" || reachesGate(jobs, n, seen) {
			return true
		}
	}
	return false
}

func TestReleaseJobs(t *testing.T) {
	w := loadWorkflows(t)["ci.yml"]
	jobs := w.wf.Jobs
	conds := map[string]string{
		"publish": "${{ !cancelled() && needs.all-green.result == 'success' && github.event_name == 'push' && github.ref == 'refs/heads/main' }}",
		"attest":  "${{ !cancelled() && needs.publish.result == 'success' && needs.publish.outputs.released == 'true' }}",
		"release": "${{ !cancelled() && needs.attest.result == 'success' && needs.publish.outputs.released == 'true' }}",
		"verify":  "${{ !cancelled() && needs.release.result == 'success' && needs.publish.outputs.released == 'true' }}",
	}
	for id, want := range conds {
		j, ok := jobs[id]
		if !ok {
			t.Errorf("release job %s is missing", id)
			continue
		}
		if j.If != want {
			t.Errorf("%s: if: %q, want %q", id, j.If, want)
		}
	}

	for _, id := range releaseJobs {
		j := jobs[id]
		if len(j.Steps) == 0 || !strings.HasPrefix(j.Steps[0].Uses, "step-security/harden-runner@") {
			t.Errorf("%s: first step must be step-security/harden-runner", id)
			continue
		}
		hr := j.Steps[0].With
		if hr["egress-policy"] != "block" {
			t.Errorf("%s: harden-runner egress-policy must be block", id)
		}
		for _, ep := range strings.Fields(hr["allowed-endpoints"]) {
			host := strings.Split(ep, ":")[0]
			if host == "dns.google" || host == "*.githubusercontent.com" || host == "*" {
				t.Errorf("%s: %s must never be allow-listed (§17.2)", id, ep)
			}
		}
		for i, s := range j.Steps {
			if strings.HasPrefix(s.Uses, "actions/setup-") && s.With["cache"] != "false" {
				t.Errorf("%s step %d: release jobs use no cache (A2): set cache: false", id, i)
			}
		}
	}

	attest := jobs["attest"]
	for i, s := range attest.Steps {
		if s.Run != "" || strings.HasPrefix(s.Uses, "actions/checkout@") || !strings.HasPrefix(s.Uses, "actions/") {
			t.Errorf("attest step %d: no checkout, no run:, first-party actions only (A4)", i)
		}
	}
	for id, j := range jobs {
		if id == "attest" {
			continue
		}
		var perms map[string]string
		_ = j.Permissions.Decode(&perms)
		if _, ok := perms["id-token"]; ok {
			t.Errorf("%s: only attest may hold id-token (A4)", id)
		}
	}
}

// A5: the only registry push targets are our image and our chart.
func TestPushTargets(t *testing.T) {
	allowed := map[string]bool{
		"ghcr.io/prosperity-solutions/maintenance-manager":        true,
		"ghcr.io/prosperity-solutions/charts":                     true,
		"ghcr.io/prosperity-solutions/charts/maintenance-manager": true,
	}
	ref := regexp.MustCompile(`ghcr\.io/[A-Za-z0-9._/-]*[A-Za-z0-9]`)
	w := loadWorkflows(t)["ci.yml"]
	for _, m := range ref.FindAllString(w.raw, -1) {
		if !allowed[m] {
			t.Errorf("unexpected registry reference %s (A5)", m)
		}
	}
	if strings.Contains(w.raw, "docker push") || strings.Contains(w.raw, "crane push") || strings.Contains(w.raw, "crane copy") {
		t.Error("pushes happen only through ko build and helm push (A5)")
	}
}
