package workflows

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
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
