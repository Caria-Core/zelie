package egg

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

var (
	numberRe   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`) })
	alphaDash  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[\p{L}\p{M}\p{N}_-]+$`) })
	regexClose = map[byte]byte{'(': ')', '{': '}', '[': ']', '<': '>'}
)

// Check tests a value against the variable's rules. The rules are the
// Laravel ones eggs use; a rule Check does not know is ignored, so an egg
// with an exotic rule stays usable. Like Laravel, an empty value passes
// everything except "required".
func (v Variable) Check(value string) error {
	if strings.TrimSpace(value) == "" {
		for _, r := range v.Rules {
			if r == "required" {
				return fmt.Errorf("%s is required", v.label())
			}
		}
		return nil
	}
	numeric := false
	for _, r := range v.Rules {
		if r == "integer" || r == "numeric" {
			numeric = true
		}
	}
	for _, r := range v.Rules {
		name, arg, _ := strings.Cut(r, ":")
		if err := v.checkRule(name, arg, value, numeric); err != nil {
			return err
		}
	}
	return nil
}

func (v Variable) label() string {
	if v.Name != "" {
		return v.Name
	}
	return v.Env
}

func (v Variable) checkRule(name, arg, value string, numeric bool) error {
	switch name {
	case "integer":
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			return fmt.Errorf("%s must be an integer", v.label())
		}
	case "numeric":
		if !numberRe().MatchString(value) {
			return fmt.Errorf("%s must be a number", v.label())
		}
	case "boolean":
		switch value {
		case "0", "1", "true", "false":
		default:
			return fmt.Errorf("%s must be true or false", v.label())
		}
	case "alpha_dash":
		if !alphaDash().MatchString(value) {
			return fmt.Errorf("%s may only contain letters, numbers, dashes and underscores", v.label())
		}
	case "url":
		u, err := url.Parse(value)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("%s must be a valid URL", v.label())
		}
	case "in":
		for _, opt := range strings.Split(arg, ",") {
			if strings.Trim(opt, `"`) == value {
				return nil
			}
		}
		return fmt.Errorf("%s must be one of: %s", v.label(), arg)
	case "regex":
		// PHP patterns can use features RE2 lacks, such as lookahead. A
		// pattern that does not compile is skipped rather than failing
		// every value.
		re, err := phpRegex(arg)
		if err != nil {
			return nil
		}
		if !re.MatchString(value) {
			return fmt.Errorf("%s has an invalid format", v.label())
		}
	case "max", "min", "between":
		return v.checkSize(name, arg, value, numeric)
	}
	return nil
}

// checkSize applies max, min and between. Laravel measures a number by its
// value when the variable is also declared integer or numeric, and any
// other value by its length in characters.
func (v Variable) checkSize(name, arg, value string, numeric bool) error {
	var lo, hi float64
	var haveLo, haveHi bool
	bounds := strings.Split(arg, ",")
	parse := func(s string) (float64, bool) {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		return f, err == nil
	}
	switch {
	case name == "max" && len(bounds) == 1:
		hi, haveHi = parse(bounds[0])
	case name == "min" && len(bounds) == 1:
		lo, haveLo = parse(bounds[0])
	case name == "between" && len(bounds) == 2:
		var ok1, ok2 bool
		lo, ok1 = parse(bounds[0])
		hi, ok2 = parse(bounds[1])
		haveLo, haveHi = ok1, ok2
	}
	if !haveLo && !haveHi {
		return nil
	}

	var size float64
	unit := ""
	if numeric {
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return errors.New(v.label() + " must be a number")
		}
		size = f
	} else {
		size = float64(utf8.RuneCountInString(value))
		unit = " characters"
	}
	switch {
	case haveLo && haveHi && (size < lo || size > hi):
		return fmt.Errorf("%s must be between %s and %s%s", v.label(), num(lo), num(hi), unit)
	case haveHi && !haveLo && size > hi:
		return fmt.Errorf("%s must be at most %s%s", v.label(), num(hi), unit)
	case haveLo && !haveHi && size < lo:
		return fmt.Errorf("%s must be at least %s%s", v.label(), num(lo), unit)
	}
	return nil
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// phpRegex turns a PHP pattern such as /^[a-z]+$/i into a Go regexp.
func phpRegex(s string) (*regexp.Regexp, error) {
	if len(s) < 2 {
		return nil, errors.New("no delimiters")
	}
	open := s[0]
	closer := open
	if c, ok := regexClose[open]; ok {
		closer = c
	}
	end := strings.LastIndexByte(s, closer)
	if end < 1 {
		return nil, errors.New("no closing delimiter")
	}
	body := s[1:end]
	if open == closer {
		body = strings.ReplaceAll(body, `\`+string(open), string(open))
	}
	flags := ""
	for _, f := range s[end+1:] {
		switch f {
		case 'i', 'm', 's':
			flags += string(f)
		case 'u':
		default:
			return nil, errors.New("unsupported flag")
		}
	}
	if flags != "" {
		body = "(?" + flags + ")" + body
	}
	return regexp.Compile(body)
}
