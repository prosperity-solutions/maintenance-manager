// Command depage enforces the dependency age rule (docs/HANDOVER.md §17.3): every
// version pinned in a go.mod — requires and the toolchain line — must be at
// least seven days old on the Go module proxy, unless .github/fresh-deps.txt
// names it with the advisory it fixes.
//
// Run from the tools module: go -C tools run ./depage
//
// A version the proxy cannot resolve fails the run, except on a push to main
// (GITHUB_EVENT_NAME=push), where it only warns: main must not go red because
// the proxy had a bad minute after the PR already passed.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

func main() {
	var (
		mods     = flag.String("mod", "../go.mod,go.mod,actionlint/go.mod", "comma-separated go.mod files to check")
		allow    = flag.String("allow", "../.github/fresh-deps.txt", "allow-list of young versions")
		proxy    = flag.String("proxy", "https://proxy.golang.org", "Go module proxy")
		minAge   = flag.Duration("min-age", 7*24*time.Hour, "minimum age of a pinned version")
		parallel = flag.Int("parallel", 16, "concurrent proxy requests")
	)
	flag.Parse()
	cfg := config{
		Mods:     strings.Split(*mods, ","),
		Allow:    *allow,
		Proxy:    strings.TrimSuffix(*proxy, "/"),
		MinAge:   *minAge,
		Parallel: *parallel,
		Now:      time.Now(),
		Strict:   os.Getenv("GITHUB_EVENT_NAME") != "push",
		Client:   &http.Client{Timeout: 30 * time.Second},
	}
	os.Exit(run(context.Background(), cfg, os.Stdout, os.Stderr))
}

type config struct {
	Mods     []string
	Allow    string
	Proxy    string
	MinAge   time.Duration
	Parallel int
	Now      time.Time
	Strict   bool // unresolvable versions fail (true) or warn (false)
	Client   *http.Client
}

// pin is one version to check. Path "toolchain" stands for the go.mod
// toolchain line; it is looked up as golang.org/toolchain on the proxy.
type pin struct {
	Path, Version, From string
}

func (p pin) String() string { return p.Path + "@" + p.Version }

func run(ctx context.Context, cfg config, stdout, stderr io.Writer) int {
	allowed, err := readAllow(cfg.Allow)
	if err != nil {
		fmt.Fprintf(stderr, "depage: %v\n", err)
		return 1
	}
	var pins []pin
	for _, m := range cfg.Mods {
		ps, err := readPins(m)
		if err != nil {
			fmt.Fprintf(stderr, "depage: %v\n", err)
			return 1
		}
		pins = append(pins, ps...)
	}
	pins = dedupe(pins)

	type result struct {
		pin pin
		age time.Duration
		at  time.Time
		err error
	}
	results := make([]result, len(pins))
	sem := make(chan struct{}, max(cfg.Parallel, 1))
	var wg sync.WaitGroup
	for i, p := range pins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			at, err := published(ctx, cfg, p)
			results[i] = result{pin: p, at: at, age: cfg.Now.Sub(at), err: err}
		}()
	}
	wg.Wait()

	failed := false
	young := 0
	for _, r := range results {
		switch {
		case r.err != nil && cfg.Strict:
			failed = true
			fmt.Fprintf(stderr, "❌ %s (%s): cannot resolve on the proxy: %v\n", r.pin, r.pin.From, r.err)
		case r.err != nil:
			fmt.Fprintf(stderr, "⚠️  %s (%s): cannot resolve on the proxy: %v (warning only on main)\n", r.pin, r.pin.From, r.err)
		case r.age >= cfg.MinAge:
		case allowed[r.pin.String()]:
			young++
			fmt.Fprintf(stdout, "✅ %s is %s old, allowed by %s\n", r.pin, days(r.age), cfg.Allow)
		default:
			failed = true
			fmt.Fprintf(stderr, "❌ %s (%s) was published %s, %s ago; the minimum is %s.\n   Pick an older version, or add `%s   # <advisory it fixes>` to %s.\n",
				r.pin, r.pin.From, r.at.UTC().Format(time.RFC3339), days(r.age), days(cfg.MinAge), r.pin, cfg.Allow)
		}
	}
	if failed {
		return 1
	}
	fmt.Fprintf(stdout, "depage: %d pinned versions checked, all at least %s old (%d allow-listed)\n", len(pins), days(cfg.MinAge), young)
	return 0
}

func days(d time.Duration) string {
	return fmt.Sprintf("%.1f days", d.Hours()/24)
}

func readPins(path string) ([]pin, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := modfile.Parse(path, data, nil)
	if err != nil {
		return nil, err
	}
	replaced := map[string]*modfile.Replace{}
	for _, r := range f.Replace {
		replaced[r.Old.Path] = r
	}
	var pins []pin
	if f.Toolchain != nil {
		pins = append(pins, pin{Path: "toolchain", Version: f.Toolchain.Name, From: path})
	}
	for _, r := range f.Require {
		p := pin{Path: r.Mod.Path, Version: r.Mod.Version, From: path}
		if rep, ok := replaced[r.Mod.Path]; ok && (rep.Old.Version == "" || rep.Old.Version == r.Mod.Version) {
			if rep.New.Version == "" {
				continue // local directory replacement: nothing published
			}
			p = pin{Path: rep.New.Path, Version: rep.New.Version, From: path}
		}
		pins = append(pins, p)
	}
	return pins, nil
}

func dedupe(pins []pin) []pin {
	seen := map[string]bool{}
	out := pins[:0]
	for _, p := range pins {
		if !seen[p.String()] {
			seen[p.String()] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// readAllow parses fresh-deps.txt: `<path>@<version>   # <advisory>` per line.
// A line without its reason is an error, not an entry.
func readAllow(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	allowed := map[string]bool{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entry, reason, _ := strings.Cut(line, "#")
		entry = strings.TrimSpace(entry)
		if strings.TrimSpace(reason) == "" || !strings.Contains(entry, "@") || strings.ContainsAny(entry, " \t") {
			return nil, fmt.Errorf("%s:%d: want `<path>@<version>   # <advisory it fixes>`, got %q", path, n, line)
		}
		allowed[entry] = true
	}
	return allowed, sc.Err()
}

func published(ctx context.Context, cfg config, p pin) (time.Time, error) {
	path, version := p.Path, p.Version
	if path == "toolchain" {
		path, version = "golang.org/toolchain", "v0.0.1-"+p.Version+".linux-amd64"
	}
	ep, err := module.EscapePath(path)
	if err != nil {
		return time.Time{}, err
	}
	ev, err := module.EscapeVersion(version)
	if err != nil {
		return time.Time{}, err
	}
	url := cfg.Proxy + "/" + ep + "/@v/" + ev + ".info"
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		at, retry, err := fetchInfo(ctx, cfg.Client, url)
		if err == nil {
			return at, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return time.Time{}, lastErr
}

func fetchInfo(ctx context.Context, c *http.Client, url string) (time.Time, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return time.Time{}, false, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return time.Time{}, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, resp.StatusCode >= 500, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	var info struct{ Time time.Time }
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&info); err != nil {
		return time.Time{}, false, fmt.Errorf("%s: %w", url, err)
	}
	if info.Time.IsZero() {
		return time.Time{}, false, fmt.Errorf("%s: no Time field", url)
	}
	return info.Time, false, nil
}
