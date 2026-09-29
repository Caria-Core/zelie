package egg

import (
	"regexp"
	"strings"
)

// Done reports whether a console line means the server has started. A done
// string matches when the line contains it; one that starts with "regex:"
// is a regular expression the line is searched with. Lines that cannot be
// compiled never match.
type Done struct {
	texts []string
	exprs []*regexp.Regexp
}

// DoneMatcher prepares the egg's done lines.
func (e *Egg) DoneMatcher() *Done {
	d := &Done{}
	for _, s := range e.Done {
		if expr, ok := strings.CutPrefix(s, "regex:"); ok {
			if re, err := regexp.Compile(expr); err == nil {
				d.exprs = append(d.exprs, re)
			}
			continue
		}
		if s != "" {
			d.texts = append(d.texts, s)
		}
	}
	return d
}

// Empty reports whether the egg names no line, so nothing tells when the
// server is up.
func (d *Done) Empty() bool { return len(d.texts) == 0 && len(d.exprs) == 0 }

// Match reports whether the line means the server is up.
func (d *Done) Match(line string) bool {
	for _, t := range d.texts {
		if strings.Contains(line, t) {
			return true
		}
	}
	for _, re := range d.exprs {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}
