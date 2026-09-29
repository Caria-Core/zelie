// Package steam reads what Valve's tools print: the text format its
// SteamCMD and game clients call KeyValues, or VDF, and the build numbers
// inside it.
package steam

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Node is one key of a VDF file. A key holds either a string or more keys,
// in file order.
type Node struct {
	Key      string
	Value    string
	Children []*Node
	object   bool
}

// IsObject reports whether the node holds keys rather than a string.
func (n *Node) IsObject() bool { return n.object }

// Get follows keys down from n. Steam is not consistent about case (an
// app manifest says "AppState" and "buildid"), so keys match without it.
func (n *Node) Get(path ...string) *Node {
	for _, key := range path {
		if n == nil {
			return nil
		}
		var next *Node
		for _, c := range n.Children {
			if strings.EqualFold(c.Key, key) {
				next = c
				break
			}
		}
		n = next
	}
	return n
}

// Text is the string at path, if there is one there.
func (n *Node) Text(path ...string) (string, bool) {
	found := n.Get(path...)
	if found == nil || found.object {
		return "", false
	}
	return found.Value, true
}

const maxDepth = 32

// Parse reads a VDF document. The result is a node with no key whose
// children are the file's top-level keys.
func Parse(data []byte) (*Node, error) {
	p := &parser{data: data}
	root := &Node{object: true}
	for {
		p.skip()
		if p.pos >= len(p.data) {
			return root, nil
		}
		n, err := p.pair(0)
		if err != nil {
			return nil, err
		}
		root.Children = append(root.Children, n)
	}
}

type parser struct {
	data []byte
	pos  int
}

// skip moves over white space and // comments.
func (p *parser) skip() {
	for p.pos < len(p.data) {
		switch c := p.data[p.pos]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			p.pos++
		case c == '/' && p.pos+1 < len(p.data) && p.data[p.pos+1] == '/':
			for p.pos < len(p.data) && p.data[p.pos] != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

var errEnd = errors.New("the text ends inside a key")

// token reads a quoted string, or a bare word up to white space or a brace.
func (p *parser) token() (string, error) {
	if p.pos >= len(p.data) {
		return "", errEnd
	}
	if p.data[p.pos] != '"' {
		start := p.pos
		for p.pos < len(p.data) && !strings.ContainsRune(" \t\r\n{}\"", rune(p.data[p.pos])) {
			p.pos++
		}
		if p.pos == start {
			return "", fmt.Errorf("unexpected %q at byte %d", p.data[p.pos], p.pos)
		}
		return string(p.data[start:p.pos]), nil
	}
	p.pos++
	var b strings.Builder
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		p.pos++
		switch c {
		case '"':
			return b.String(), nil
		case '\\':
			if p.pos >= len(p.data) {
				return "", errEnd
			}
			e := p.data[p.pos]
			p.pos++
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '\\', '"':
				b.WriteByte(e)
			default:
				// Paths in manifests hold single backslashes.
				b.WriteByte('\\')
				b.WriteByte(e)
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", errEnd
}

// pair reads a key and what follows it.
func (p *parser) pair(depth int) (*Node, error) {
	if depth > maxDepth {
		return nil, errors.New("keys are nested too deeply")
	}
	key, err := p.token()
	if err != nil {
		return nil, err
	}
	n := &Node{Key: key}
	p.skip()
	if p.pos >= len(p.data) {
		return nil, errEnd
	}
	if p.data[p.pos] != '{' {
		if n.Value, err = p.token(); err != nil {
			return nil, err
		}
		return n, nil
	}
	p.pos++
	n.object = true
	for {
		p.skip()
		if p.pos >= len(p.data) {
			return nil, errEnd
		}
		if p.data[p.pos] == '}' {
			p.pos++
			return n, nil
		}
		child, err := p.pair(depth + 1)
		if err != nil {
			return nil, err
		}
		n.Children = append(n.Children, child)
	}
}

// appStart is the line before an app's block in what SteamCMD prints for
// app_info_print: the app id in quotes, with the block on the next line.
var appStart = regexp.MustCompile(`(?m)^[ \t]*"(\d+)"[ \t]*\r?\n[ \t]*\{`)

// ParseAppInfo finds the apps in the output of SteamCMD's app_info_print,
// which is VDF blocks among ordinary log lines, and returns each by id.
func ParseAppInfo(out []byte) (map[int64]*Node, error) {
	apps := map[int64]*Node{}
	for at := 0; ; {
		m := appStart.FindSubmatchIndex(out[at:])
		if m == nil {
			break
		}
		start := at + m[0]
		p := &parser{data: out, pos: start}
		n, err := p.pair(0)
		if err != nil {
			// A line that only looked like the start of a block.
			at = start + 1
			continue
		}
		if id, err := strconv.ParseInt(n.Key, 10, 64); err == nil {
			apps[id] = n
		}
		at = p.pos
	}
	if len(apps) == 0 {
		return nil, errors.New("SteamCMD printed no app information")
	}
	return apps, nil
}

// LatestBuilds returns the build id of each app's public branch, from the
// output of app_info_print. An app with no public branch is left out.
func LatestBuilds(out []byte) (map[int64]string, error) {
	apps, err := ParseAppInfo(out)
	if err != nil {
		return nil, err
	}
	builds := make(map[int64]string, len(apps))
	for id, n := range apps {
		if b, ok := n.Text("depots", "branches", "public", "buildid"); ok && validBuild(b) {
			builds[id] = b
		}
	}
	return builds, nil
}

// InstalledBuild reads the build id from an app manifest,
// steamapps/appmanifest_<id>.acf, which SteamCMD writes when it installs or
// updates a game.
func InstalledBuild(manifest []byte) (string, error) {
	doc, err := Parse(bytes.TrimPrefix(manifest, []byte("\xef\xbb\xbf")))
	if err != nil {
		return "", err
	}
	b, ok := doc.Text("AppState", "buildid")
	if !ok || !validBuild(b) {
		return "", errors.New("the manifest has no build id")
	}
	return b, nil
}

// validBuild accepts what Steam uses for builds: a number.
func validBuild(s string) bool {
	if s == "" || len(s) > 18 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ManifestPath is where a game's manifest is, relative to the folder
// SteamCMD installs into.
func ManifestPath(appID int64) string {
	return "steamapps/appmanifest_" + strconv.FormatInt(appID, 10) + ".acf"
}
