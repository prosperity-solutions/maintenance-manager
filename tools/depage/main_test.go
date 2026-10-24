package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

// fakeProxy answers .info requests from a path -> publish time map.
func fakeProxy(t *testing.T, times map[string]time.Time) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		at, ok := times[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"Version":"x","Time":"` + at.Format(time.RFC3339) + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const gomod = `module example.com/m

go 1.26.7

toolchain go1.26.9

require (
	example.com/Old v1.0.0
	example.com/young v1.2.0 // indirect
)
`

func TestRun(t *testing.T) {
	old := now.Add(-30 * 24 * time.Hour)
	young := now.Add(-2 * 24 * time.Hour)
	proxy := fakeProxy(t, map[string]time.Time{
		"/golang.org/toolchain/@v/v0.0.1-go1.26.9.linux-amd64.info": old,
		"/example.com/!old/@v/v1.0.0.info":                          old, // case-escaped path
		"/example.com/young/@v/v1.2.0.info":                         young,
	})

	cases := []struct {
		name   string
		allow  string
		mod    string
		strict bool
		want   int
		output string
	}{
		{name: "young version fails", mod: gomod, strict: true, want: 1, output: "example.com/young@v1.2.0"},
		{name: "allow-listed young version passes", mod: gomod, allow: "example.com/young@v1.2.0   # GO-2026-0001\n", strict: true, want: 0, output: "allowed by"},
		{name: "allow line without reason is an error", mod: gomod, allow: "example.com/young@v1.2.0\n", strict: true, want: 1, output: "want `<path>@<version>"},
		{name: "unresolvable fails on PRs", mod: strings.Replace(gomod, "v1.0.0", "v1.9.9", 1), allow: "example.com/young@v1.2.0 # x\n", strict: true, want: 1, output: "cannot resolve"},
		{name: "unresolvable warns on main", mod: strings.Replace(gomod, "v1.0.0", "v1.9.9", 1), allow: "example.com/young@v1.2.0 # x\n", strict: false, want: 0, output: "warning only on main"},
		{name: "young go line fails without a toolchain line", mod: strings.Replace(strings.Replace(gomod, "toolchain go1.26.9\n", "", 1), "go 1.26.7", "go 1.26.10", 1), allow: "example.com/young@v1.2.0 # x\n", strict: true, want: 1, output: "toolchain@go1.26.10"},
		{name: "young toolchain fails", mod: strings.Replace(gomod, "go1.26.9", "go1.26.10", 1), allow: "example.com/young@v1.2.0 # x\n", strict: true, want: 1, output: "toolchain@go1.26.10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := config{
				Mods:     []string{write(t, dir, "go.mod", tc.mod)},
				Allow:    filepath.Join(dir, "fresh-deps.txt"),
				Proxy:    proxy.URL,
				MinAge:   7 * 24 * time.Hour,
				Parallel: 4,
				Now:      now,
				Strict:   tc.strict,
				Client:   proxy.Client(),
			}
			if tc.allow != "" {
				write(t, dir, "fresh-deps.txt", tc.allow)
			}
			var out bytes.Buffer
			got := run(context.Background(), cfg, &out, &out)
			if got != tc.want {
				t.Fatalf("exit %d, want %d\n%s", got, tc.want, out.String())
			}
			if !strings.Contains(out.String(), tc.output) {
				t.Fatalf("output lacks %q:\n%s", tc.output, out.String())
			}
		})
	}
}

func TestLocalReplaceIsSkipped(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "go.mod", "module m\n\ngo 1.26.7\n\nrequire example.com/x v1.0.0\n\nreplace example.com/x => ../x\n")
	pins, err := readPins(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pins {
		if p.Path == "example.com/x" {
			t.Fatalf("local replacement should not be checked: %v", pins)
		}
	}
}
