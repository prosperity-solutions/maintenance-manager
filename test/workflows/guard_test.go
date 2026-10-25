package workflows

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// guardFixture is one file in scripts/dev/testdata/guard. The commands live in
// files, not in this test's source, because the guard blocks command text that
// merely mentions what it forbids. "@UUID@" in a payload or file stands for a
// real-looking ID, built here so that none sits in the repo
// (scripts/dev/public-ids.sh).
type guardFixture struct {
	Expect        string          `json:"expect"` // allow | block
	BootstrapDone bool            `json:"bootstrap_done"`
	Branch        string          `json:"branch"`
	Payload       json.RawMessage `json:"payload"`
	// Files are written into the work tree before the guard runs, e.g. a
	// --body-file.
	Files map[string]string `json:"files"`
	GH    *struct {
		Login   string            `json:"login"`
		Authors map[string]string `json:"authors"`
	} `json:"gh"`
}

// fakeID looks like a real UUID but is not written down anywhere in the repo.
var fakeID = strings.Repeat("1", 8) + "-2222-3333-4444-" + strings.Repeat("5", 12)

func TestAgentGuard(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("jq is required by scripts/dev/agent-guard.sh (docs/HANDOVER.md §21.1)")
	}
	root := repoRoot(t)
	guard := filepath.Join(root, "scripts/dev/agent-guard.sh")
	// Read it so the go test cache notices when the script changes.
	if _, err := os.ReadFile(guard); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(root, "scripts/dev/testdata/guard/*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no guard fixtures: %v", err)
	}
	for _, f := range files {
		t.Run(strings.TrimSuffix(filepath.Base(f), ".json"), func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var fx guardFixture
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatal(err)
			}
			project := t.TempDir()
			if fx.BootstrapDone {
				mustWrite(t, filepath.Join(project, "notes/bootstrap.done"), "")
			}
			work := gitRepo(t, fx.Branch)
			for name, body := range fx.Files {
				mustWrite(t, filepath.Join(work, name), strings.ReplaceAll(body, "@UUID@", fakeID))
			}

			var payload map[string]any
			if err := json.Unmarshal(bytes.ReplaceAll(fx.Payload, []byte("@UUID@"), []byte(fakeID)), &payload); err != nil {
				t.Fatal(err)
			}
			payload["cwd"] = work
			in, _ := json.Marshal(payload)

			cmd := exec.CommandContext(t.Context(), "sh", guard)
			cmd.Dir = work
			cmd.Stdin = bytes.NewReader(in)
			cmd.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+project, "MM_GUARD_GH="+fakeGH(t, fx))
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err = cmd.Run()
			got := "allow"
			if err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) || ee.ExitCode() != 2 {
					t.Fatalf("guard crashed (%v): %s", err, stderr.String())
				}
				got = "block"
			}
			if got != fx.Expect {
				t.Fatalf("want %s, got %s; stderr: %s", fx.Expect, got, stderr.String())
			}
		})
	}
}

// fakeGH writes a stand-in for gh that answers the two calls the guard makes.
// Without a gh block in the fixture any call fails, which the guard must treat
// as a block.
func fakeGH(t *testing.T, fx guardFixture) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	if fx.GH != nil {
		fmt.Fprintf(&b, "if [ \"$1 $2\" = \"api user\" ]; then echo %q; exit 0; fi\n", fx.GH.Login)
		prs := make([]string, 0, len(fx.GH.Authors))
		for pr := range fx.GH.Authors {
			prs = append(prs, pr)
		}
		sort.Strings(prs)
		for _, pr := range prs {
			fmt.Fprintf(&b, "if [ \"$1 $2 $3\" = \"pr view %s\" ]; then echo %q; exit 0; fi\n", pr, fx.GH.Authors[pr])
		}
	}
	b.WriteString("echo 'fake gh: unexpected call' >&2; exit 1\n")
	p := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(p, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func gitRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", branch},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "chore: init"},
	} {
		c := exec.CommandContext(t.Context(), "git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "../.."))
}
