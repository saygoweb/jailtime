package action

import (
	"bytes"
	"text/template"
)

// Context holds values available in action command templates.
type Context struct {
	IP        string
	Jail      string
	File      string
	Line      string
	JailTime  int64  // seconds
	FindTime  int64  // seconds
	HitCount  int
	Timestamp string // RFC3339
}

// Render renders a Go text/template string with the given Context.
// Returns the rendered string or an error.
func Render(tmpl string, ctx Context) (string, error) {
	t, err := template.New("action").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// CompileTemplate parses a template string once for reuse.
// The name parameter is used for error messages.
func CompileTemplate(name, tmpl string) (*template.Template, error) {
	return template.New(name).Parse(tmpl)
}

// RenderCompiled executes a pre-compiled template with the given context.
func RenderCompiled(t *template.Template, ctx Context) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return "", err
	}
	return buf.String(), nil
}
