// Package render builds what visitors see: page templates, the built-in
// default page and the JSON bodies (docs/HANDOVER.md §4.2, §6, §7).
package render

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"text/template/parse"
	"time"
)

// MaxBody is the largest rendered page body (64 KiB).
const MaxBody = 64 << 10

const problemType = "https://maintenance-manager.oss.life.li/problems/maintenance"

//go:embed default.html
var defaultHTML string

// DefaultSource is the built-in page's template, for people to start their
// own from.
func DefaultSource() string { return defaultHTML }

// Input is what a page template sees: .Title .Message .Locale .Until
// .Continue .Interstitial
type Input struct {
	Locale, Title, Message string
	Until                  time.Time // zero = unknown, .Until renders empty
	// Continue is the interstitial button's text; empty means ContinueFor(Locale).
	Continue     string
	Interstitial bool
	// Zone is the time zone of .Until's text before the visitor's browser
	// converts it; nil means UTC. Visitors get UTC; the editor's preview,
	// which runs no scripts, gets the editor's zone.
	Zone *time.Location
}

// view is the value the template executes against. Its fields must stay
// method-free types (string, template.HTML, bool): templates look up methods
// before fields, so a field with methods (time.Time, say) would widen what a
// page can call past checkTree's allow-list. TestViewHasNoMethods guards it.
type view struct {
	Locale, Title, Message string
	Until                  template.HTML
	Continue               string
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
	if err := checkTree(t); err != nil {
		return nil, err
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
// if the result exceeds MaxBody. When the page shows .Until as an element,
// the local-time script is added once at the very end (browsers still run a
// script after </html>; anywhere earlier it could land inside the author's
// own <script> or <textarea>), outside the MaxBody count, so a page that
// fitted before still fits.
func (p *Page) Render(in Input) (string, error) {
	b := cappedBuffer{max: MaxBody}
	if in.Continue == "" {
		in.Continue = ContinueFor(in.Locale)
	}
	v := view{
		Locale:       in.Locale,
		Title:        in.Title,
		Message:      in.Message,
		Until:        FormatUntil(in.Until, in.Zone),
		Continue:     in.Continue,
		Interstitial: in.Interstitial,
	}
	if err := p.t.Execute(&b, v); err != nil {
		if errors.Is(err, errTooLarge) {
			return "", fmt.Errorf("rendered page is larger than the limit of %d bytes", MaxBody)
		}
		return "", fmt.Errorf("execute page template: %w", err)
	}
	return withLocalTime(b.String()), nil
}

// checkTree allows a small, safe subset of html/template, checked on the
// parse tree before anything runs: text, {{.Field}}, if/else, with, and the
// comparison and logic functions. It rejects:
//   - range: since Go 1.24 it iterates over an integer, so
//     {{range 9223372036854775807}}{{end}} spins for ever without output;
//   - template, define and block: self-calls recurse, chained calls double;
//   - variables and every other function (printf, js, html, urlquery, index,
//     slice, call, print…): nested js or printf widths, or a variable doubled
//     step by step, build gigabytes in memory without writing a byte.
//
// A static check is needed because Parse runs the template once, on sample
// data, so a branch taken only for another locale escapes any runtime limit.
// The page data has no lists and needs none of it. Every replica parses every
// page when it rebuilds its snapshot: a hang there froze every change,
// including disabling a window.
func checkTree(t *template.Template) error {
	if n := len(t.Templates()); n > 1 {
		return errors.New("page template: {{define}} and {{block}} are not supported")
	}
	if t.Tree == nil {
		return nil
	}
	return walk(t.Tree.Root)
}

// allowedFuncs are the functions a page may call: none of them allocates
// more than its arguments.
var allowedFuncs = map[string]bool{
	"eq": true, "ne": true, "lt": true, "le": true, "gt": true, "ge": true,
	"and": true, "or": true, "not": true, "len": true,
}

func walk(n parse.Node) error {
	switch n := n.(type) {
	case nil:
		return nil
	case *parse.ListNode:
		if n == nil {
			return nil
		}
		for _, c := range n.Nodes {
			if err := walk(c); err != nil {
				return err
			}
		}
	case *parse.ActionNode:
		return walkPipe(n.Pipe)
	case *parse.IfNode:
		return walkBranch(&n.BranchNode)
	case *parse.WithNode:
		return walkBranch(&n.BranchNode)
	case *parse.RangeNode:
		return errors.New("page template: {{range}} is not supported (the page data has no lists)")
	case *parse.TemplateNode:
		return errors.New("page template: {{template}} is not supported")
	case *parse.TextNode, *parse.CommentNode:
	default:
		return fmt.Errorf("page template: %s is not supported", n)
	}
	return nil
}

func walkBranch(b *parse.BranchNode) error {
	if err := walkPipe(b.Pipe); err != nil {
		return err
	}
	if err := walk(b.List); err != nil {
		return err
	}
	return walk(b.ElseList)
}

func walkPipe(p *parse.PipeNode) error {
	if p == nil {
		return nil
	}
	if len(p.Decl) > 0 {
		return errors.New("page template: variables ({{$x := …}}) are not supported")
	}
	for _, c := range p.Cmds {
		for _, a := range c.Args {
			if err := walkArg(a); err != nil {
				return err
			}
		}
	}
	return nil
}

func walkArg(n parse.Node) error {
	switch n := n.(type) {
	case *parse.IdentifierNode:
		if !allowedFuncs[n.Ident] {
			return fmt.Errorf("page template: the function %q is not supported (allowed: eq ne lt le gt ge and or not len)", n.Ident)
		}
	case *parse.VariableNode:
		return errors.New("page template: variables ($) are not supported")
	case *parse.PipeNode:
		return walkPipe(n)
	case *parse.ChainNode:
		return walkArg(n.Node)
	case *parse.FieldNode, *parse.DotNode, *parse.StringNode, *parse.NumberNode, *parse.BoolNode, *parse.NilNode:
	default:
		return fmt.Errorf("page template: %s is not supported", n)
	}
	return nil
}

var errTooLarge = errors.New("rendered page too large")

// cappedBuffer stops a template as soon as its output passes max (a second
// line of defence; checkTree keeps intermediate values small).
type cappedBuffer struct {
	bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.Len()+len(p) > c.max {
		return 0, errTooLarge
	}
	return c.Buffer.Write(p)
}

// localTime turns every <time data-mm-local> into the visitor's own time zone
// and language, with the date when it is not today. Without scripts the text
// stays as rendered. A tag like de_CH is read as de-CH; one Intl rejects
// falls back to the browser's language.
const localTime = `<script>(function(){try{var l=(document.documentElement.lang||"").replace(/_/g,"-")||undefined,n=new Date();` +
	`try{new Intl.DateTimeFormat(l)}catch(e){l=undefined}` +
	`document.querySelectorAll("time[data-mm-local]").forEach(function(t){t.removeAttribute("data-mm-local");` +
	`var d=new Date(t.getAttribute("datetime"));if(isNaN(d))return;` +
	`var o={hour:"2-digit",minute:"2-digit",timeZoneName:"short"};if(d.toDateString()!==n.toDateString()){o.month="short";o.day="numeric"}` +
	`t.textContent=new Intl.DateTimeFormat(l,o).format(d)})}catch(e){}})();</script>`

// timeMark is how a rendered page shows that .Until sits in its text: inside
// an attribute or <title> the quote is escaped, so this does not match there.
const timeMark = `" data-mm-local>`

// withLocalTime adds the local-time script once to a page that shows .Until.
func withLocalTime(body string) string {
	if !strings.Contains(body, timeMark) {
		return body
	}
	return body + localTime
}

// FormatUntil renders .Until: <time datetime="RFC3339 UTC">15:04 UTC</time>
// (in zone instead of UTC when zone is set). Render adds the script that shows
// it in the visitor's own time zone. Zero gives "".
func FormatUntil(t time.Time, zone *time.Location) template.HTML {
	if t.IsZero() {
		return ""
	}
	if zone == nil {
		zone = time.UTC
	}
	z := t.In(zone)
	return template.HTML(`<time datetime="` + t.UTC().Format(time.RFC3339) + timeMark + z.Format("15:04 MST") + `</time>`) //nolint:gosec // built from time formats only
}

// continueLabels are the built-in texts of the interstitial's button, by
// language.
var continueLabels = map[string]string{
	"cs": "Pokračovat", "da": "Fortsæt", "de": "Weiter", "en": "Continue", "es": "Continuar",
	"fi": "Jatka", "fr": "Continuer", "hu": "Tovább", "it": "Continua", "ja": "続ける",
	"nb": "Fortsett", "nl": "Doorgaan", "nn": "Hald fram", "no": "Fortsett", "pl": "Dalej", "pt": "Continuar",
	"ro": "Continuă", "sv": "Fortsätt", "tr": "Devam", "zh": "继续",
}

// ContinueFor is the built-in button text for a locale tag: its language's,
// or "Continue".
func ContinueFor(tag string) string {
	tag = strings.ToLower(strings.ReplaceAll(tag, "_", "-"))
	if strings.HasPrefix(tag, "zh-tw") || strings.HasPrefix(tag, "zh-hk") || strings.HasPrefix(tag, "zh-hant") {
		return "繼續"
	}
	lang, _, _ := strings.Cut(tag, "-")
	if s, ok := continueLabels[strings.ToLower(lang)]; ok {
		return s
	}
	return "Continue"
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
