package render

import (
	"encoding/json"
	"errors"
	"reflect"
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
	want := `&lt;b&gt;t&lt;/b&gt;|a&amp;&#34;b&#34;|<time datetime="2026-10-30T22:30:00Z" data-mm-local>22:30 UTC</time>` + localTime
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
	if got := string(FormatUntil(in, nil)); got != `<time datetime="2026-10-30T22:30:00Z" data-mm-local>22:30 UTC</time>` {
		t.Errorf("FormatUntil: %q", got)
	}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	if got := string(FormatUntil(in, berlin)); got != `<time datetime="2026-10-30T22:30:00Z" data-mm-local>23:30 CET</time>` {
		t.Errorf("FormatUntil in Berlin: %q", got)
	}
	if FormatUntil(time.Time{}, nil) != "" {
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
	for _, s := range []string{`lang="de"`, `<time datetime="2026-10-30T22:30:00Z" data-mm-local>`, "<h1>Gleich wieder da</h1>", "<script>"} {
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
	if !strings.Contains(out, `href="">Weiter</a>`) {
		t.Error("the German built-in button text is missing")
	}
	in.Continue = "Zur Seite"
	out, _ = Default.Render(in)
	if !strings.Contains(out, `href="">Zur Seite</a>`) {
		t.Error("the page's own button text is missing")
	}
	if strings.Contains(out, "<time datetime") {
		t.Error("time element with unknown until")
	}
}

func TestParseRejectsLoops(t *testing.T) {
	// Each of these would spin or recurse with no output (or never return).
	for _, src := range []string{
		`{{range 9223372036854775807}}{{end}}`,
		`{{if .Title}}{{range 3}}x{{end}}{{end}}`,
		`{{with .Title}}{{else}}{{range 3}}{{end}}{{end}}`,
		`{{define "a"}}{{template "a"}}{{end}}{{template "a"}}`,
		`{{block "b" .}}x{{end}}`,
		`{{template "page"}}`,
		`{{$a := .Title}}{{$a}}`,
		`{{.Title | printf "%900000d"}}`,
		`{{if js (js "\\")}}x{{end}}`,
		`{{len (printf "%900000d" 1)}}`,
		`{{if .Title}}{{else if .Message}}{{range 3}}{{end}}{{end}}`,
		`{{with .Title}}{{else with .Message}}{{index . 0}}{{end}}`,
		`{{html .Title}}`,
		`{{urlquery .Title}}`,
		`{{$}}`,
		`{{(printf "x").Y}}`,
	} {
		done := make(chan error, 1)
		go func() { _, err := Parse(src); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("Parse(%q) must fail", src)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("Parse(%q) hung", src)
		}
	}
}

func TestParseAllowsTheSafeSubset(t *testing.T) {
	src := `<html lang="{{.Locale}}">{{if and .Title (not .Interstitial)}}<h1>{{.Title}}</h1>{{else if eq .Locale "de"}}x{{end}}` +
		`{{with .Message}}<p>{{.}}</p>{{end}}{{if gt (len .Message) 3}}long{{end}}{{.Until}}</html>`
	if _, err := Parse(src); err != nil {
		t.Fatalf("the safe subset must parse: %v", err)
	}
}

func TestRenderStopsAtTheCap(t *testing.T) {
	// Many small writes: the capped writer stops before the whole page exists.
	p, err := Parse(`{{.Message}}`)
	if err != nil {
		t.Fatal(err)
	}
	var w cappedBuffer
	w.max = 10
	if _, err := w.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, errTooLarge) || w.Len() != 10 {
		t.Fatalf("the 11th byte must be refused, got %v with %d bytes", err, w.Len())
	}
	if _, err := p.Render(Input{Message: strings.Repeat("x", MaxBody+1)}); err == nil || !strings.Contains(err.Error(), "larger than the limit") {
		t.Fatalf("want the size error, got %v", err)
	}
}

func TestViewHasNoMethods(t *testing.T) {
	v := reflect.TypeOf(view{})
	for _, typ := range []reflect.Type{v, reflect.PointerTo(v)} {
		if typ.NumMethod() != 0 {
			t.Fatalf("%s has methods: pages could call them", typ)
		}
	}
	for i := range v.NumField() {
		f := v.Field(i).Type
		if f.NumMethod() != 0 || reflect.PointerTo(f).NumMethod() != 0 {
			t.Fatalf("view.%s (%s) has methods: pages could call them", v.Field(i).Name, f)
		}
	}
}

func TestContinueFor(t *testing.T) {
	for tag, want := range map[string]string{"de": "Weiter", "de-CH": "Weiter", "DE_at": "Weiter", "fr-CA": "Continuer", "en": "Continue", "xx": "Continue", "": "Continue"} {
		if got := ContinueFor(tag); got != want {
			t.Errorf("ContinueFor(%q) = %q, want %q", tag, got, want)
		}
	}
}

// TestUntilInEveryContext: .Until is an element with the script (added once)
// in text, and plain text without a script elsewhere.
func TestUntilInEveryContext(t *testing.T) {
	el := `<time datetime="2026-10-30T22:30:00Z" data-mm-local>22:30 UTC</time>`
	for _, c := range []struct{ tpl, want string }{
		{`<p>{{.Until}}</p>`, `<p>` + el + `</p>` + localTime},
		{`<p>{{.Until}}</p><p>{{.Until}}</p></body>`, `<p>` + el + `</p><p>` + el + `</p></body>` + localTime},
		{`<a title="Back at {{.Until}}">x</a>`, `<a title="Back at 22:30 UTC">x</a>`},
		{`<title>{{.Until}}</title>`, `<title>&lt;time datetime=&#34;2026-10-30T22:30:00Z&#34; data-mm-local&gt;22:30 UTC&lt;/time&gt;</title>`},
		{`<script>var u = {{.Until}};</script>`, ""},
	} {
		p, err := Parse(c.tpl)
		if err != nil {
			t.Fatalf("%s: %v", c.tpl, err)
		}
		got, err := p.Render(Input{Until: fixed})
		if err != nil {
			t.Fatalf("%s: %v", c.tpl, err)
		}
		if c.want == "" {
			if strings.Count(got, "<script>") != 1 {
				t.Errorf("%s: the script was added inside a script: %q", c.tpl, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.tpl, got, c.want)
		}
	}
}

// TestUntilScriptOutsideTheLimit: a page that used .Until many times and fit
// under MaxBody before the script existed still renders.
func TestUntilScriptOutsideTheLimit(t *testing.T) {
	el := len(FormatUntil(fixed, nil))
	n := 100
	tpl := strings.Repeat("{{.Until}}", n) + strings.Repeat("x", MaxBody-n*el)
	p, err := Parse(tpl)
	if err != nil {
		t.Fatalf("a page of exactly %d bytes is refused: %v", MaxBody, err)
	}
	got, err := p.Render(Input{Until: fixed})
	if err != nil || len(got) != MaxBody+len(localTime) {
		t.Fatalf("len %d, err %v", len(got), err)
	}
}
