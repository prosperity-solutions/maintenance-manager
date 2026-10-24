// Package render builds what visitors see: page templates, the built-in
// default page and the JSON bodies (docs/HANDOVER.md §4.2, §6, §7).
package render

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"time"
)

// MaxBody is the largest rendered page body (64 KiB).
const MaxBody = 64 << 10

const problemType = "https://maintenance-manager.oss.life.li/problems/maintenance"

//go:embed default.html
var defaultHTML string

// Input is what a page template sees: .Title .Message .Locale .Until .Interstitial
type Input struct {
	Locale, Title, Message string
	Until                  time.Time // zero = unknown, .Until renders empty
	Interstitial           bool
}

// view is the value the template executes against.
type view struct {
	Locale, Title, Message string
	Until                  template.HTML
	Interstitial           bool
}

// Page is a parsed, validated page template.
type Page struct{ t *template.Template }

// Default is the built-in page.
var Default = mustParse(defaultHTML)

func mustParse(s string) *Page {
	p, err := Parse(s)
	if err != nil {
		panic("render: built-in default page: " + err.Error())
	}
	return p
}

// Parse parses a MaintenancePage html template ("" gives the built-in Default).
// It executes the template once with a sample Input to catch execution errors
// and checks the sample output is at most MaxBody.
func Parse(html string) (*Page, error) {
	if html == "" {
		html = defaultHTML
	}
	t, err := template.New("page").Parse(html)
	if err != nil {
		return nil, fmt.Errorf("parse page template: %w", err)
	}
	p := &Page{t: t}
	sample := Input{
		Locale:       "en",
		Title:        "Title",
		Message:      "Message",
		Until:        time.Date(2026, 10, 30, 22, 30, 0, 0, time.UTC),
		Interstitial: true,
	}
	if _, err := p.Render(sample); err != nil {
		return nil, fmt.Errorf("page template: %w", err)
	}
	return p, nil
}

// Render executes the page. .Until is the template.HTML from FormatUntil
// (empty when Until is zero) and is not escaped; everything else is. It fails
// if the result exceeds MaxBody.
func (p *Page) Render(in Input) (string, error) {
	var b bytes.Buffer
	v := view{
		Locale:       in.Locale,
		Title:        in.Title,
		Message:      in.Message,
		Until:        FormatUntil(in.Until),
		Interstitial: in.Interstitial,
	}
	if err := p.t.Execute(&b, v); err != nil {
		return "", fmt.Errorf("execute page template: %w", err)
	}
	if b.Len() > MaxBody {
		return "", fmt.Errorf("rendered page is %d bytes, the limit is %d", b.Len(), MaxBody)
	}
	return b.String(), nil
}

// FormatUntil renders <time datetime="RFC3339 UTC">15:04 UTC</time>. Zero gives "".
func FormatUntil(t time.Time) template.HTML {
	if t.IsZero() {
		return ""
	}
	u := t.UTC()
	return template.HTML(`<time datetime="` + u.Format(time.RFC3339) + `">` + u.Format("15:04") + ` UTC</time>`) //nolint:gosec // built from time formats only
}

// RetryAfter formats an HTTP-date (UTC). Zero gives "".
func RetryAfter(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(http.TimeFormat)
}

func untilPtr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

type problem struct {
	Type   string  `json:"type"`
	Title  string  `json:"title"`
	Status int     `json:"status"`
	Detail string  `json:"detail"`
	Window string  `json:"window"`
	Until  *string `json:"until"`
}

// ProblemJSON is the block body for non-browser requests (§6).
func ProblemJSON(title, detail, window string, until time.Time) string {
	return marshal(problem{problemType, title, http.StatusServiceUnavailable, detail, window, untilPtr(until)})
}

type status struct {
	Action  string  `json:"action"`
	Window  string  `json:"window"`
	Phase   string  `json:"phase"`
	Title   string  `json:"title"`
	Message string  `json:"message"`
	Until   *string `json:"until"`
	Locale  string  `json:"locale"`
}

// StatusJSON is the body of GET /__maintenance-manager/status (§7). action is
// "none", "banner", "interstitial" or "block".
func StatusJSON(action, window, phase, title, message string, until time.Time, locale string) string {
	return marshal(status{action, window, phase, title, message, untilPtr(until), locale})
}

func marshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil { // unreachable: only strings, an int and nil pointers
		panic(err)
	}
	return string(b)
}
