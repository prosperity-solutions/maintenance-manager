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
	"text/template/parse"
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

// view is the value the template executes against. Its fields must stay
// method-free types (string, template.HTML, bool): templates look up methods
// before fields, so a field with methods (time.Time, say) would widen what a
// page can call past checkTree's allow-list. TestViewHasNoMethods guards it.
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
// if the result exceeds MaxBody.
func (p *Page) Render(in Input) (string, error) {
	b := cappedBuffer{max: MaxBody}
	v := view{
		Locale:       in.Locale,
		Title:        in.Title,
		Message:      in.Message,
		Until:        FormatUntil(in.Until),
		Interstitial: in.Interstitial,
	}
	if err := p.t.Execute(&b, v); err != nil {
		if errors.Is(err, errTooLarge) {
			return "", fmt.Errorf("rendered page is larger than the limit of %d bytes", MaxBody)
		}
		return "", fmt.Errorf("execute page template: %w", err)
	}
	return b.String(), nil
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
