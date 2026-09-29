package egg

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// applyXML sets the text of elements named by a dotted path, such as
// Settings.Server.Port. The path may start with the root element or below
// it. Only elements that exist and have no child elements are changed, and
// the file is cut and pasted rather than written again, so the rest of it
// stays as it was. Attributes are not supported.
func applyXML(content []byte, reps []Replace) ([]byte, []string, error) {
	var skipped []string
	for _, r := range reps {
		segs, err := splitPath(r.Key)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", r.Key, err))
			continue
		}
		if content, err = xmlSet(content, segs, r); err != nil {
			return nil, nil, fmt.Errorf("read XML: %w", err)
		}
	}
	return content, skipped, nil
}

type xmlEdit struct {
	from, to int
	with     string
}

func xmlSet(content []byte, segs []string, r Replace) ([]byte, error) {
	type frame struct {
		name       string
		match      bool
		begin, end int // the start tag
		children   bool
	}
	var stack []frame
	var edits []xmlEdit
	d := xml.NewDecoder(bytes.NewReader(content))
	// Raw tokens keep names as written, which is what the path uses.
	for {
		before := int(d.InputOffset())
		tok, err := d.RawToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		after := int(d.InputOffset())
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) > 0 {
				stack[len(stack)-1].children = true
			}
			names := make([]string, 0, len(stack)+1)
			for _, f := range stack {
				names = append(names, f.name)
			}
			names = append(names, t.Name.Local)
			stack = append(stack, frame{
				name: t.Name.Local, begin: before, end: after,
				match: slices.Equal(names, segs) || (len(names) > 1 && slices.Equal(names[1:], segs)),
			})
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, errors.New("unexpected closing tag")
			}
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if !f.match || f.children {
				continue
			}
			var escaped bytes.Buffer
			xml.EscapeText(&escaped, []byte(r.Value))
			if bytes.HasSuffix(content[f.begin:f.end], []byte("/>")) {
				if r.IfValue != "" {
					continue
				}
				open := bytes.TrimRight(content[f.begin:f.end-2], " \t\r\n")
				edits = append(edits, xmlEdit{f.begin, f.end, string(open) + ">" + escaped.String() + "</" + f.name + ">"})
				continue
			}
			if r.IfValue != "" && strings.TrimSpace(string(content[f.end:before])) != r.IfValue {
				continue
			}
			edits = append(edits, xmlEdit{f.end, before, escaped.String()})
		}
	}
	if len(stack) > 0 {
		return nil, errors.New("an element is not closed")
	}
	out := slices.Clone(content)
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		out = append(out[:e.from], append([]byte(e.with), out[e.to:]...)...)
	}
	return out, nil
}
