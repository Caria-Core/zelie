// Package msg holds the text Zelie shows people, in a form the web
// interface can translate. Each message is defined once, with a code and
// its English text. The web interface's English strings for these codes
// are generated from the definitions (see TestLocale), and other languages
// translate the codes. The English text also goes along with every
// message, for logs and for a language that lacks a code.
package msg

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Template is a message as defined: {name} marks a value filled in later.
type Template struct {
	Code    string
	English string
	// Status is the HTTP status an error with this message is sent with,
	// or 0 for a message that is only shown, never sent as an error.
	Status int
}

var (
	mu        sync.Mutex
	templates = map[string]Template{}

	validCode   = regexp.MustCompile(`^[a-z]+(\.[a-z0-9_]+)+$`)
	placeholder = regexp.MustCompile(`\{([a-z_]+)\}`)
)

// Define declares a message. It is called from package variables, so a
// code defined twice or a malformed one stops the program at start.
func Define(status int, code, english string) Template {
	mu.Lock()
	defer mu.Unlock()
	if !validCode.MatchString(code) {
		panic("msg: invalid code " + code)
	}
	if _, dup := templates[code]; dup {
		panic("msg: " + code + " is defined twice")
	}
	t := Template{Code: code, English: english, Status: status}
	templates[code] = t
	return t
}

// All returns every message defined so far, by code.
func All() []Template {
	mu.Lock()
	defer mu.Unlock()
	return slices.SortedFunc(maps.Values(templates), func(a, b Template) int { return strings.Compare(a.Code, b.Code) })
}

// Names returns the placeholders in a message's English text.
func (t Template) Names() []string {
	var out []string
	for _, m := range placeholder.FindAllStringSubmatch(t.English, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// Msg is one message: its code, the values of its placeholders and its
// English text.
type Msg struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
	Text   string         `json:"text"`
}

// With fills the placeholders from name, value pairs. Values stay what
// they are, so a translation can pick the plural form for a number.
func (t Template) With(pairs ...any) Msg {
	m := Msg{Code: t.Code, Text: t.English}
	if len(pairs) > 0 {
		m.Params = make(map[string]any, len(pairs)/2)
		for i := 0; i+1 < len(pairs); i += 2 {
			m.Params[fmt.Sprint(pairs[i])] = pairs[i+1]
		}
	}
	m.Text = placeholder.ReplaceAllStringFunc(t.English, func(p string) string {
		if v, ok := m.Params[p[1:len(p)-1]]; ok {
			return fmt.Sprint(v)
		}
		return p
	})
	return m
}

// Error is a message given as an error, with the HTTP status that goes
// with it.
type Error struct {
	Status int
	Msg
}

func (e *Error) Error() string { return e.Text }

// Err makes the message into an error.
func (t Template) Err(pairs ...any) *Error {
	return &Error{Status: t.Status, Msg: t.With(pairs...)}
}

// WithStatus sends the error with another HTTP status, for a message that fits
// more than one case.
func (e *Error) WithStatus(status int) *Error {
	e.Status = status
	return e
}

// Other carries text Zelie did not write, such as a tool's error output.
// It is shown as it is, in every language.
var Other = Define(0, "other.detail", "{detail}")

// Wrap gives any error a message: its own, if it has one, or Other.
func Wrap(err error) Msg {
	var e *Error
	if errors.As(err, &e) {
		return e.Msg
	}
	return Other.With("detail", err.Error())
}
