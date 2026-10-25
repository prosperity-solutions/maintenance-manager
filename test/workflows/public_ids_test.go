package workflows

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPublicIDPatternInSync: the commits job has no checkout, so ci.yml holds
// a copy of public-ids.sh's pattern. The two must stay equal.
func TestPublicIDPatternInSync(t *testing.T) {
	root := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(root, "scripts/dev/public-ids.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^PATTERN='([^']+)'$`).FindSubmatch(script)
	if m == nil {
		t.Fatal("public-ids.sh: no PATTERN='…' line")
	}
	ci, err := os.ReadFile(filepath.Join(root, ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ci), "grep -Eo '"+string(m[1])+"'") {
		t.Fatalf("ci.yml's commits job does not use public-ids.sh's pattern %s", m[1])
	}
}

func TestPublicIDs(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts/dev/public-ids.sh")
	for _, c := range []struct {
		name, in string
		want     int
	}{
		{"empty", "", 0},
		{"clean", "no ids here\nShip-Stamp: v1 8dd0657d6ac71ec1\n", 0},
		{"all-zero example", "id: 00000000-0000-0000-0000-000000000000\n", 0},
		{"real id", "service " + fakeID + "\n", 1},
		{"upper case", strings.ToUpper("x " + fakeID), 1},
		{"attachment link", "![s](https://github.com/user-attachments/assets/" + fakeID + ")\n", 0},
		{"link and a real id", "![s](https://github.com/user-attachments/assets/" + fakeID + ") " + fakeID + "\n", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "sh", script)
			cmd.Stdin = strings.NewReader(c.in)
			out, err := cmd.Output()
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != c.want {
				t.Fatalf("exit %d, want %d", code, c.want)
			}
			if strings.Contains(strings.ToLower(string(out)), strings.ToLower(fakeID)) {
				t.Fatalf("the output repeats the ID: %q", out)
			}
		})
	}
	t.Run("unreadable file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "x")
		mustWrite(t, f, "x\n")
		if err := os.Chmod(f, 0); err != nil {
			t.Fatal(err)
		}
		if os.Getuid() == 0 {
			t.Skip("root reads everything")
		}
		err := exec.CommandContext(t.Context(), "sh", script, f).Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Fatalf("want exit 2, got %v", err)
		}
	})
}
