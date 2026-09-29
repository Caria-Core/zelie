package egg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Eggs come as JSON or YAML, and key order carries meaning: the first image
// is the default one and the first startup command is the one that runs. A
// plain map would lose it, so both formats are read into this small tree.
// Values are string, bool, json.Number, nil, []any or *object.

const maxDepth = 64

type object struct {
	keys []string
	vals map[string]any
}

func newObject() *object { return &object{vals: map[string]any{}} }

func (o *object) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) get(k string) any {
	if o == nil {
		return nil
	}
	return o.vals[k]
}

func decode(data []byte) (*object, error) {
	var v any
	var err error
	if b := bytes.TrimSpace(data); len(b) > 0 && b[0] == '{' {
		v, err = decodeJSON(data)
	} else {
		v, err = decodeYAML(data)
	}
	if err != nil {
		return nil, err
	}
	root, ok := v.(*object)
	if !ok {
		return nil, errors.New("egg is not an object")
	}
	return root, nil
}

func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := readJSON(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after JSON value")
	}
	return v, nil
}

func readJSON(dec *json.Decoder, depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	if d == '[' {
		list := []any{}
		for dec.More() {
			v, err := readJSON(dec, depth+1)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		_, err := dec.Token()
		return list, err
	}
	o := newObject()
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		v, err := readJSON(dec, depth+1)
		if err != nil {
			return nil, err
		}
		o.set(k.(string), v)
	}
	_, err = dec.Token()
	return o, err
}

func decodeYAML(data []byte) (any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, errors.New("empty document")
	}
	return fromNode(doc.Content[0], 0)
}

func fromNode(n *yaml.Node, depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("nested too deeply")
	}
	switch n.Kind {
	case yaml.MappingNode:
		o := newObject()
		for i := 0; i+1 < len(n.Content); i += 2 {
			v, err := fromNode(n.Content[i+1], depth+1)
			if err != nil {
				return nil, err
			}
			o.set(n.Content[i].Value, v)
		}
		return o, nil
	case yaml.SequenceNode:
		list := []any{}
		for _, c := range n.Content {
			v, err := fromNode(c, depth+1)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		return list, nil
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!null":
			return nil, nil
		case "!!bool":
			var b bool
			if err := n.Decode(&b); err != nil {
				return nil, err
			}
			return b, nil
		case "!!int", "!!float":
			return json.Number(n.Value), nil
		}
		return n.Value, nil
	}
	return nil, fmt.Errorf("line %d: YAML aliases are not supported", n.Line)
}

// text turns a scalar into the string a panel would store for it. The
// YAML eggs write numbers and booleans unquoted where JSON eggs use strings.
func text(v any) string {
	switch t := v.(type) {
	case string:
		return strings.ReplaceAll(t, "\r\n", "\n")
	case bool:
		if t {
			return "true"
		}
		return "false"
	case json.Number:
		return t.String()
	}
	return ""
}

func boolean(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case json.Number:
		return t.String() != "0"
	case string:
		return t == "1" || t == "true"
	}
	return false
}

func stringList(v any) []string {
	switch t := v.(type) {
	case []any:
		var out []string
		for _, e := range t {
			if s := text(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if t != "" {
			return []string{t}
		}
	}
	return nil
}

// embedded reads a config section that older eggs store as a JSON string
// and newer ones as a real object. An empty string means an empty section.
func embedded(v any) (*object, error) {
	switch t := v.(type) {
	case *object:
		return t, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		v, err := decodeJSON([]byte(t))
		if err != nil {
			return nil, err
		}
		o, _ := v.(*object)
		return o, nil
	}
	return nil, nil
}
