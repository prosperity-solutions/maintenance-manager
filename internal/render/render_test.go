package render

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var fixed = time.Date(2026, 10, 30, 22, 30, 0, 0, time.UTC)

func TestParse(t *testing.T) {
	good := `<html lang="{{.Locale}}"><h1>{{.Title}}</h1>{{if .Until}}{{.Until}}{{end}}</html>`
	if _, err := Parse(good); err != nil {
		t.Fatalf("valid template: %v", err)
	}
	if p, err := Parse(""); err != nil || p == nil {
		t.Fatalf("empty template: %v", err)
	}
	cases := map[string]string{
		"parse error":    `<p>{{.Title</p>`,
		"execute error":  `<p>{{.Nope}}</p>`,
		"undefined call": `<p>{{.Title.Missing}}</p>`,
		"too large":      strings.Repeat("x", MaxBody+1),
	}
	for name, src := range cases {
		if _, err := Parse(src); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRenderEscaping(t *testing.T) {
	p, err := Parse(`{{.Title}}|{{.Message}}|{{.Until}}`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Render(Input{Title: "<b>t</b>", Message: `a&"b"`, Until: fixed})
	if err != nil {
		t.Fatal(err)
	}
	want := `&lt;b&gt;t&lt;/b&gt;|a&amp;&#34;b&#34;|<time datetime="2026-10-30T22:30:00Z">22:30 UTC</time>`
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	got, _ = p.Render(Input{})
	if got != "||" {
		t.Errorf("zero input: %q", got)
	}
}

func TestRenderTooLarge(t *testing.T) {
	p, err := Parse(`{{.Message}}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Render(Input{Message: strings.Repeat("x", MaxBody+1)}); err == nil {
		t.Error("expected size error")
	}
}

func TestFormats(t *testing.T) {
	in := fixed.In(time.FixedZone("X", 5*3600))
	if got := string(FormatUntil(in)); got != `<time datetime="2026-10-30T22:30:00Z">22:30 UTC</time>` {
		t.Errorf("FormatUntil: %q", got)
	}
	if FormatUntil(time.Time{}) != "" {
		t.Error("zero FormatUntil")
	}
	if got := RetryAfter(in); got != "Fri, 30 Oct 2026 22:30:00 GMT" {
		t.Errorf("RetryAfter: %q", got)
	}
	if RetryAfter(time.Time{}) != "" {
		t.Error("zero RetryAfter")
	}
}

func TestProblemJSON(t *testing.T) {
	got := ProblemJSON("Back <soon>", "Planned.", "w1", fixed)
	bs := string(rune(92)) // json escapes <, > as backslash-u003c / backslash-u003e
	want := `{"type":"https://maintenance-manager.oss.life.li/problems/maintenance","title":"Back ` + bs + `u003csoon` + bs + `u003e","status":503,"detail":"Planned.","window":"w1","until":"2026-10-30T22:30:00Z"}`
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	got = ProblemJSON("t", "d", "w", time.Time{})
	if !strings.HasSuffix(got, `"until":null}`) || !json.Valid([]byte(got)) {
		t.Errorf("null until: %s", got)
	}
}

func TestStatusJSON(t *testing.T) {
	got := StatusJSON("block", "w", "down", "T", "M", fixed, "de")
	want := `{"action":"block","window":"w","phase":"down","title":"T","message":"M","until":"2026-10-30T22:30:00Z","locale":"de"}`
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	got = StatusJSON("none", "", "", "", "", time.Time{}, "en")
	want = `{"action":"none","window":"","phase":"","title":"","message":"","until":null,"locale":"en"}`
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func TestDefaultPage(t *testing.T) {
	if len(defaultHTML) >= 8<<10 {
		t.Fatalf("default.html is %d bytes", len(defaultHTML))
	}
	in := Input{Locale: "de", Title: "Gleich wieder da", Message: "Geplante Wartung.", Until: fixed}
	out, err := Default.Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) >= 8<<10 {
		t.Errorf("rendered %d bytes", len(out))
	}
	for _, s := range []string{`lang="de"`, `<time datetime="2026-10-30T22:30:00Z">`, "<h1>Gleich wieder da</h1>"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q", s)
		}
	}
	if strings.Contains(out, "Continue") {
		t.Error("Continue shown without interstitial")
	}
	in.Interstitial = true
	in.Until = time.Time{}
	out, _ = Default.Render(in)
	if !strings.Contains(out, `href="">Continue</a>`) {
		t.Error("Continue link missing")
	}
	if strings.Contains(out, "<time datetime") {
		t.Error("time element with unknown until")
	}
}
