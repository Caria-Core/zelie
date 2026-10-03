package egg

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// MaxConfigFile is the largest config file ApplyFile is meant for. The
// caller enforces it when it reads the file.
const MaxConfigFile = 4 << 20

// KnownParser reports whether ApplyFile can edit files with the parser.
func KnownParser(parser string) bool {
	switch parser {
	case "properties", "file", "ini", "yaml", "json", "xml":
		return true
	}
	return false
}

// ApplyFile makes the changes an egg asks for in one config file and
// returns the new content. The parser is one of properties, file, yaml,
// json, ini and xml. Values go through expand first, so they may hold
// placeholders; expand may be nil.
//
// A file that cannot be read as its format is an error. A key that cannot
// be set, such as a path that runs through a plain value, is left alone and
// named in skipped, so the rest of the file still gets its changes.
func ApplyFile(parser string, content []byte, replaces []Replace, expand func(string) string) (out []byte, skipped []string, err error) {
	reps := make([]Replace, 0, len(replaces))
	for _, r := range replaces {
		if r.Key == "" {
			continue
		}
		if expand != nil {
			r.Value = expand(r.Value)
		}
		reps = append(reps, r)
	}
	switch parser {
	case "properties":
		return []byte(applyProperties(string(content), reps)), nil, nil
	case "file":
		return []byte(applyLines(string(content), reps)), nil, nil
	case "ini":
		return []byte(applyINI(string(content), reps)), nil, nil
	case "yaml":
		return applyYAML(content, reps)
	case "json":
		return applyJSON(content, reps)
	case "xml":
		return applyXML(content, reps)
	}
	return nil, nil, fmt.Errorf("unknown config file parser %q", parser)
}

// Text files are edited line by line, so what the user wrote stays: order,
// comments, spacing and the kind of line ending.

// splitLines cuts text into lines without their endings. eol is the ending
// to write back and trailing says whether the text ended with one.
func splitLines(text string) (lines []string, eol string, trailing bool) {
	eol = "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	if text == "" {
		return nil, eol, true
	}
	lines = strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
		trailing = true
	}
	return lines, eol, trailing
}

func joinLines(lines []string, eol string, trailing bool) string {
	if len(lines) == 0 {
		return ""
	}
	out := strings.Join(lines, eol)
	if trailing {
		out += eol
	}
	return out
}

// keyValue splits a "key = value" line. head is everything up to where the
// value starts, so a replaced value keeps the line's own spacing. Comments
// and lines without an equals sign are not key lines.
func keyValue(line string) (key, head, value string, ok bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || strings.ContainsAny(trimmed[:1], "#!;[") {
		return "", "", "", false
	}
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return "", "", "", false
	}
	start := eq + 1
	for start < len(line) && (line[start] == ' ' || line[start] == '\t') {
		start++
	}
	return strings.TrimSpace(line[:eq]), line[:start], strings.TrimSpace(line[start:]), true
}

// applyProperties sets keys in a Java-style properties file, and adds the
// ones that are missing.
func applyProperties(text string, reps []Replace) string {
	lines, eol, trailing := splitLines(text)
	for _, r := range reps {
		found := false
		for i, l := range lines {
			key, head, value, ok := keyValue(l)
			if !ok || key != r.Key {
				continue
			}
			found = true
			if r.IfValue == "" || value == r.IfValue {
				lines[i] = head + r.Value
			}
		}
		if !found && r.IfValue == "" {
			lines, trailing = append(lines, r.Key+"="+r.Value), true
		}
	}
	return joinLines(lines, eol, trailing)
}

// applyLines is the "file" parser: a line that starts with the key is
// replaced by the value. A line that is not there is added at the end only
// when the replacement says Add, because most eggs list lines for the
// game's default file and a user's own file may never have had them.
func applyLines(text string, reps []Replace) string {
	lines, eol, trailing := splitLines(text)
	present := make([]bool, len(reps))
	for i, l := range lines {
		for j, r := range reps {
			if strings.HasPrefix(l, r.Key) {
				present[j] = true
			}
		}
		for _, r := range reps {
			if !strings.HasPrefix(l, r.Key) {
				continue
			}
			if r.IfValue != "" && strings.TrimSpace(l[len(r.Key):]) != r.IfValue {
				continue
			}
			lines[i] = r.Value
			break
		}
	}
	for j, r := range reps {
		if r.Add && r.IfValue == "" && !present[j] {
			lines, trailing = append(lines, r.Value), true
		}
	}
	return joinLines(lines, eol, trailing)
}

// iniSection is a run of lines under one [header]. The part before the
// first header is the section with no name.
type iniSection struct {
	name       string
	start, end int // lines start..end-1, not counting the header
}

func iniSections(lines []string) []iniSection {
	out := []iniSection{{}}
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "[") || !strings.Contains(t, "]") {
			continue
		}
		out[len(out)-1].end = i
		out = append(out, iniSection{name: t[1:strings.LastIndex(t, "]")], start: i + 1})
	}
	out[len(out)-1].end = len(lines)
	return out
}

// applyINI sets "section.key" in an ini file. Section names may hold dots
// themselves, as Unreal's do, so every split of the key at a dot is tried
// against the sections the file has.
func applyINI(text string, reps []Replace) string {
	lines, eol, trailing := splitLines(text)
	for _, r := range reps {
		lines, trailing = setINI(lines, r, trailing)
	}
	return joinLines(lines, eol, trailing)
}

func setINI(lines []string, r Replace, trailing bool) ([]string, bool) {
	type candidate struct{ section, key string }
	cands := []candidate{{"", r.Key}}
	for i, c := range r.Key {
		if c == '.' {
			cands = append(cands, candidate{r.Key[:i], r.Key[i+1:]})
		}
	}
	sections := iniSections(lines)
	for _, c := range cands {
		found := false
		for _, s := range sections {
			if s.name != c.section {
				continue
			}
			for i := s.start; i < s.end; i++ {
				key, head, value, ok := keyValue(lines[i])
				if !ok || key != c.key {
					continue
				}
				found = true
				if r.IfValue == "" || value == r.IfValue {
					lines[i] = head + r.Value
				}
			}
		}
		if found {
			return lines, trailing
		}
	}
	if r.IfValue != "" {
		return lines, trailing
	}
	// Not there: add it to the first section the key can belong to.
	for _, c := range cands[1:] {
		for _, s := range sections {
			if s.name == c.section {
				at := s.end
				for at > s.start && strings.TrimSpace(lines[at-1]) == "" {
					at--
				}
				return slicesInsert(lines, at, c.key+"="+r.Value), true
			}
		}
	}
	if !strings.Contains(r.Key, ".") {
		// Into the part before the first header.
		at := sections[0].end
		for at > 0 && strings.TrimSpace(lines[at-1]) == "" {
			at--
		}
		return slicesInsert(lines, at, r.Key+"="+r.Value), true
	}
	section, key, _ := strings.Cut(r.Key, ".")
	if n := len(lines); n > 0 && strings.TrimSpace(lines[n-1]) != "" {
		lines = append(lines, "")
	}
	return append(lines, "["+section+"]", key+"="+r.Value), true
}

func slicesInsert(lines []string, at int, line string) []string {
	lines = append(lines, "")
	copy(lines[at+1:], lines[at:])
	lines[at] = line
	return lines
}

// A value that an egg writes as text is stored in the type the file
// already has for that key. A new key gets a type from how the value looks:
// booleans and whole numbers are written as such, everything else as text.
var wholeNumber = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^-?(0|[1-9][0-9]{0,17})$`) })

const (
	kindString = "!!str"
	kindBool   = "!!bool"
	kindInt    = "!!int"
	kindFloat  = "!!float"
)

func inferKind(value string) string {
	switch {
	case value == "true" || value == "false":
		return kindBool
	case wholeNumber().MatchString(value):
		return kindInt
	}
	return kindString
}

// kindLike picks the kind for value given the kind the key had before.
func kindLike(old, value string) string {
	switch old {
	case kindBool:
		if value == "true" || value == "false" {
			return kindBool
		}
	case kindInt:
		if wholeNumber().MatchString(value) {
			return kindInt
		}
		if _, err := strconv.ParseFloat(value, 64); err == nil {
			return kindFloat
		}
	case kindFloat:
		if _, err := strconv.ParseFloat(value, 64); err == nil {
			return kindFloat
		}
	case kindString:
		return kindString
	default:
		return inferKind(value)
	}
	return kindString
}

// hasWildcard reports whether a path has a * in it.
func hasWildcard(segs []string) bool {
	for _, s := range segs {
		if s == "*" {
			return true
		}
	}
	return false
}

// splitPath cuts a dotted key into its steps.
func splitPath(key string) ([]string, error) {
	segs := strings.Split(key, ".")
	for _, s := range segs {
		if s == "" {
			return nil, fmt.Errorf("%q is not a valid path", key)
		}
	}
	return segs, nil
}
