package egg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// YAML is edited as a node tree, which keeps comments and key order. JSON
// goes through the egg package's ordered tree, so keys stay where they are.
// Both understand the same paths: dotted keys, a number for a list entry
// and * for every entry at that level.

// detectIndent finds how a file is indented from its first indented line.
func detectIndent(content []byte) string {
	for _, l := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(l, "\t") {
			return "\t"
		}
		if n := len(l) - len(strings.TrimLeft(l, " ")); n > 0 && strings.TrimSpace(l) != "" {
			return strings.Repeat(" ", n)
		}
	}
	return "  "
}

func applyYAML(content []byte, reps []Replace) ([]byte, []string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(content))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read YAML: %w", err)
		}
		docs = append(docs, &doc)
	}
	if len(docs) == 0 {
		docs = []*yaml.Node{{Kind: yaml.DocumentNode}}
	}
	doc := docs[0]
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.ShortTag() == "!!null" {
		*root = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", HeadComment: root.HeadComment}
	}

	var skipped []string
	for _, r := range reps {
		segs, err := splitPath(r.Key)
		if err == nil {
			err = yamlSet(root, segs, r)
		}
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", r.Key, err))
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(len(detectIndent(content)))
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			return nil, nil, fmt.Errorf("write YAML: %w", err)
		}
	}
	if err := enc.Close(); err != nil {
		return nil, nil, fmt.Errorf("write YAML: %w", err)
	}
	return buf.Bytes(), skipped, nil
}

func yamlScalar(old *yaml.Node, value string) *yaml.Node {
	kind := inferKind(value)
	if old != nil && old.Kind == yaml.ScalarNode {
		kind = kindLike(old.ShortTag(), value)
	}
	if kind == kindFloat && wholeNumber.MatchString(value) {
		// Written as a whole number, it is an int to YAML; forcing the tag
		// would print it.
		kind = kindInt
	}
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: kind, Value: value}
	if old != nil && old.Kind == yaml.ScalarNode && kind == kindString {
		// Keep quotes the file already had.
		n.Style = old.Style & (yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle)
	}
	if old != nil {
		n.HeadComment, n.LineComment, n.FootComment = old.HeadComment, old.LineComment, old.FootComment
	}
	return n
}

// yamlLeaf sets the value of the node at the end of a path.
func yamlLeaf(n *yaml.Node, r Replace) {
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if r.IfValue != "" && n.Value != r.IfValue {
			return
		}
	default:
		if r.IfValue != "" {
			return
		}
	}
	*n = *yamlScalar(n, r.Value)
}

// yamlSet follows segs down from n and sets the value at the end.
func yamlSet(n *yaml.Node, segs []string, r Replace) error {
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	seg, rest := segs[0], segs[1:]
	step := func(child *yaml.Node) error {
		if len(rest) == 0 {
			yamlLeaf(child, r)
			return nil
		}
		return yamlSet(child, rest, r)
	}
	switch n.Kind {
	case yaml.MappingNode:
		if seg == "*" {
			for i := 1; i < len(n.Content); i += 2 {
				if err := step(n.Content[i]); err != nil {
					return err
				}
			}
			return nil
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value != seg {
				continue
			}
			child := n.Content[i+1]
			if len(rest) > 0 && child.Kind == yaml.ScalarNode {
				if child.ShortTag() != "!!null" {
					return fmt.Errorf("%s holds a value, not a section", seg)
				}
				*child = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			}
			return step(child)
		}
		if r.IfValue != "" || hasWildcard(rest) {
			return nil
		}
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: kindString, Value: seg}
		if len(rest) == 0 {
			n.Content = append(n.Content, key, yamlScalar(nil, r.Value))
			return nil
		}
		child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		n.Content = append(n.Content, key, child)
		return yamlSet(child, rest, r)
	case yaml.SequenceNode:
		if seg == "*" {
			for _, c := range n.Content {
				if err := step(c); err != nil {
					return err
				}
			}
			return nil
		}
		i, err := strconv.Atoi(seg)
		if err != nil || i < 0 || i >= len(n.Content) {
			return fmt.Errorf("%s is not an entry of a list", seg)
		}
		return step(n.Content[i])
	}
	return fmt.Errorf("cannot look for %s inside a value", seg)
}

func applyJSON(content []byte, reps []Replace) ([]byte, []string, error) {
	var root any = newObject()
	if len(bytes.TrimSpace(content)) > 0 {
		var err error
		if root, err = decodeJSON(content); err != nil {
			return nil, nil, fmt.Errorf("read JSON: %w", err)
		}
	}
	var skipped []string
	for _, r := range reps {
		segs, err := splitPath(r.Key)
		if err == nil {
			root, err = jsonSet(root, segs, r)
		}
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", r.Key, err))
		}
	}
	var buf bytes.Buffer
	writeJSON(&buf, root, detectIndent(content), 0)
	if len(content) == 0 || bytes.HasSuffix(content, []byte("\n")) {
		buf.WriteByte('\n')
	}
	return buf.Bytes(), skipped, nil
}

func jsonScalar(old any, value string) any {
	kind := inferKind(value)
	switch old.(type) {
	case string:
		kind = kindString
	case bool:
		kind = kindLike(kindBool, value)
	case json.Number:
		kind = kindLike(kindInt, value)
	}
	switch kind {
	case kindBool:
		return value == "true"
	case kindInt, kindFloat:
		return json.Number(value)
	}
	return value
}

// jsonLeaf returns what replaces the value at the end of a path.
func jsonLeaf(cur any, r Replace) any {
	if r.IfValue != "" {
		switch cur.(type) {
		case *object, []any:
			return cur
		}
		if text(cur) != r.IfValue {
			return cur
		}
	}
	return jsonScalar(cur, r.Value)
}

// jsonSet follows segs down from v and returns v with the value set. A
// scalar that has to change is replaced, so callers store the result.
func jsonSet(v any, segs []string, r Replace) (any, error) {
	seg, rest := segs[0], segs[1:]
	step := func(child any) (any, error) {
		if len(rest) == 0 {
			return jsonLeaf(child, r), nil
		}
		if child == nil && r.IfValue == "" && !hasWildcard(rest) {
			child = newObject()
		}
		return jsonSet(child, rest, r)
	}
	switch t := v.(type) {
	case *object:
		if seg == "*" {
			for _, k := range t.keys {
				nv, err := step(t.vals[k])
				if err != nil {
					return v, err
				}
				t.vals[k] = nv
			}
			return t, nil
		}
		cur, ok := t.vals[seg]
		if !ok {
			if r.IfValue != "" || hasWildcard(rest) {
				return t, nil
			}
			if len(rest) == 0 {
				t.set(seg, jsonScalar(nil, r.Value))
				return t, nil
			}
			cur = newObject()
		}
		nv, err := step(cur)
		if err != nil {
			return v, err
		}
		t.set(seg, nv)
		return t, nil
	case []any:
		if seg == "*" {
			for i := range t {
				nv, err := step(t[i])
				if err != nil {
					return v, err
				}
				t[i] = nv
			}
			return t, nil
		}
		i, err := strconv.Atoi(seg)
		if err != nil || i < 0 || i >= len(t) {
			return v, fmt.Errorf("%s is not an entry of a list", seg)
		}
		nv, err := step(t[i])
		if err != nil {
			return v, err
		}
		t[i] = nv
		return t, nil
	}
	return v, fmt.Errorf("%s holds a value, not a section", strings.Join(segs, "."))
}

func writeJSON(buf *bytes.Buffer, v any, indent string, level int) {
	pad := func(n int) {
		buf.WriteByte('\n')
		buf.WriteString(strings.Repeat(indent, n))
	}
	switch t := v.(type) {
	case *object:
		if len(t.keys) == 0 {
			buf.WriteString("{}")
			return
		}
		buf.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			pad(level + 1)
			writeJSONString(buf, k)
			buf.WriteString(": ")
			writeJSON(buf, t.vals[k], indent, level+1)
		}
		pad(level)
		buf.WriteByte('}')
	case []any:
		if len(t) == 0 {
			buf.WriteString("[]")
			return
		}
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			pad(level + 1)
			writeJSON(buf, e, indent, level+1)
		}
		pad(level)
		buf.WriteByte(']')
	case string:
		writeJSONString(buf, t)
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case json.Number:
		buf.WriteString(t.String())
	default:
		buf.WriteString("null")
	}
}

func writeJSONString(buf *bytes.Buffer, s string) {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	// Encode ends with a newline.
	buf.Truncate(buf.Len() - 1)
}
