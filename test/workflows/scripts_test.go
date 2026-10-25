package workflows

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every tracked script with a #! line is executable in git, because the
// workflow and the hooks run them directly. Caught after the fact once: the
// first release failed with "prefetch.sh: Permission denied".
func TestScriptsAreExecutable(t *testing.T) {
	root := repoRoot(t)
	cmd := exec.CommandContext(t.Context(), "git", "ls-files", "-s", "--", "scripts")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err) // e.g. a source tarball
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line) // mode sha stage path
		if len(fields) != 4 {
			continue
		}
		mode, path := fields[0], fields[3]
		raw, err := os.ReadFile(root + "/" + path)
		if err != nil || !bytes.HasPrefix(raw, []byte("#!")) {
			continue
		}
		if mode != "100755" {
			t.Errorf("%s has a #! line but git mode %s; run: git update-index --chmod=+x %s", path, mode, path)
		}
	}
}

// TestPrefetchRetriesAHang: a go command that hangs instead of failing must
// hit the deadline and be retried, not stall the job until its timeout.
func TestPrefetchRetriesAHang(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("no timeout command here; GitHub runners have it")
	}
	bin := t.TempDir()
	state := filepath.Join(t.TempDir(), "calls")
	// The first call hangs; every later one succeeds.
	// sleep in the background, not exec'd: the deadline must stop go's own
	// children too, or the orphan keeps the output open for 60 s.
	fake := "#!/bin/sh\nif [ ! -e '" + state + "' ]; then : > '" + state + "'; sleep 60 & wait; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", filepath.Join(repoRoot(t), "scripts/ci/prefetch.sh"), "example.com/tool")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "PREFETCH_DEADLINE=1")
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("prefetch: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "with no result (a hung download)") || !strings.Contains(string(out), "attempt 1 failed") || time.Since(start) > 30*time.Second {
		t.Fatalf("the hang was not cut short and retried (%s):\n%s", time.Since(start), out)
	}
}
