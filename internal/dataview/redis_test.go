package dataview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/msg"
)

func TestRedisQuoting(t *testing.T) {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	q := redisQuote(all)
	if strings.ContainsAny(q[1:len(q)-1], "\n\r\x00") || !strings.HasPrefix(q, `"\x00`) {
		t.Errorf("quoted: %s", q)
	}
	// redis-cli --quoted-json writes these forms; each reads back.
	for in, want := range map[string]string{
		`h\xc3\xa9llo\nw`: "héllo\nw",
		`a\"b\\c`:         `a"b\c`,
		`\x00\xff`:        "\x00\xff",
		`tab\there`:       "tab\there",
	} {
		got, err := redisUnquote(in)
		if err != nil || string(got) != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{`ends\`, `\x0`, `\xzz`} {
		if _, err := redisUnquote(bad); err == nil {
			t.Errorf("%s read", bad)
		}
	}
}

// As redis-cli 8 writes them, quotes in values and all.
func TestFixQuotedJSON(t *testing.T) {
	for in, want := range map[string]string{
		`"{\\"user\\":\\"efe\\"}"`: `{"user":"efe"}`,
		`"back\\\\slash"`:              `back\slash`,
		`"ends\\\\"`:                   `ends\`,
		`"say \\"hi\\" \\xc3\\xa9"`: `say "hi" é`,
		`["0",["a\\"b","c"]]`:            `a"b`,
	} {
		fixed := fixQuotedJSON(in)
		var s string
		var list []json.RawMessage
		if json.Unmarshal([]byte(fixed), &list) == nil {
			var inner []string
			json.Unmarshal(list[1], &inner)
			s = inner[0]
		} else if err := json.Unmarshal([]byte(fixed), &s); err != nil {
			t.Errorf("%s: %s is not JSON: %v", in, fixed, err)
			continue
		}
		got, err := redisUnquote(s)
		if err != nil || string(got) != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, same := range []string{`["a","b"]`, `100`, `"none"`, `[["m1",1.5]]`} {
		if got := fixQuotedJSON(same); got != same {
			t.Errorf("%s became %s", same, got)
		}
	}
}

// redisFake answers each command from replies, by its first word and key.
type redisFake struct {
	replies map[string]string
	sent    []string
}

func (f *redisFake) exec(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (uint32, error) {
	b, _ := io.ReadAll(stdin)
	for _, cmd := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f.sent = append(f.sent, cmd)
		name := strings.Fields(cmd)[0]
		reply, ok := f.replies[cmd]
		if !ok {
			reply, ok = f.replies[name]
		}
		if !ok {
			reply = `error:"WRONGTYPE Operation against a key holding the wrong kind of value"`
		}
		fmt.Fprintln(stdout, reply)
	}
	return 0, nil
}

func TestKeys(t *testing.T) {
	f := &redisFake{replies: map[string]string{
		`SCAN 0 MATCH "user:*" COUNT 1000`: `["17",["user:1","user:\\xff","gone"]]`,
		`TYPE "user:1"`:                    `"hash"`, `TTL "user:1"`: `-1`,
		`TYPE "user:\xff"`: `"string"`, `TTL "user:\xff"`: `100`,
		`TYPE "gone"`: `"none"`, `TTL "gone"`: `-2`,
	}}
	p, err := Keys(context.Background(), f.exec, "user:*", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Cursor != "17" || len(p.Keys) != 2 || p.Keys[0].Type != "hash" || p.Keys[0].TTL != -1 ||
		p.Keys[1].ID != `user:\xff` || p.Keys[1].Name != "0x757365723aff" || !p.Keys[1].Binary || p.Keys[1].TTL != 100 {
		t.Fatalf("%+v", p)
	}
	if _, err := Keys(context.Background(), f.exec, "*", "1; FLUSHALL"); !isCode(err, "data.bad_cursor") {
		t.Errorf("cursor: %v", err)
	}
	// A pattern is one quoted argument, whatever it holds.
	f.sent = nil
	Keys(context.Background(), f.exec, "x\" FLUSHALL \"\n", "0")
	if len(f.sent) != 1 || f.sent[0] != `SCAN 0 MATCH "x\" FLUSHALL \"\x0a" COUNT 1000` {
		t.Errorf("sent %q", f.sent)
	}
}

func isCode(err error, code string) bool {
	var me *msg.Error
	return errors.As(err, &me) && me.Code == code
}

func TestKeyValue(t *testing.T) {
	cases := []struct {
		typ     string
		replies map[string]string
		size    int64
		cols    string
		rows    string
	}{
		{"string", map[string]string{"STRLEN": `12`, "GETRANGE": `"h\\xc3\\xa9llo\\nw\\xc3\\xb6rld"`}, 12, "value", "héllo\nwörld"},
		{"list", map[string]string{"LLEN": `2`, "LRANGE": `["a","\\xff"]`}, 2, "value", "a|0xff"},
		{"hash", map[string]string{"HLEN": `2`, "HSCAN": `["0",["f1","v1","f2","v2"]]`}, 2, "field,value", "f1,v1|f2,v2"},
		{"set", map[string]string{"SCARD": `1`, "SSCAN": `["0",["m"]]`}, 1, "member", "m"},
		{"zset", map[string]string{"ZCARD": `2`, "ZRANGE": `[["m1",1.5],["m2",2]]`}, 2, "member,score", "m1,1.5|m2,2"},
		{"stream", map[string]string{"XLEN": `1`, "XRANGE": `[["1790597723976-0",["a","1","b","2"]]]`}, 1, "id,fields", "1790597723976-0,a=1 b=2"},
	}
	for _, c := range cases {
		c.replies["TYPE"] = `"` + c.typ + `"`
		c.replies["TTL"] = `-1`
		f := &redisFake{replies: c.replies}
		v, err := KeyValue(context.Background(), f.exec, `k`)
		if err != nil {
			t.Fatalf("%s: %v", c.typ, err)
		}
		var rows []string
		for _, r := range v.Rows {
			var cells []string
			for _, cell := range r {
				cells = append(cells, *cell)
			}
			rows = append(rows, strings.Join(cells, ","))
		}
		if v.Key.Type != c.typ || v.Size != c.size || strings.Join(v.Columns, ",") != c.cols || strings.Join(rows, "|") != c.rows {
			t.Errorf("%s: %+v rows %q", c.typ, v, rows)
		}
		if c.typ == "list" && (len(v.Binary) != 1 || v.Binary[0] != [2]int{1, 0}) {
			t.Errorf("binary %v", v.Binary)
		}
		// Only reads were sent.
		for _, cmd := range f.sent {
			if w := strings.Fields(cmd)[0]; !strings.Contains(" TYPE TTL STRLEN GETRANGE LLEN LRANGE HLEN HSCAN SCARD SSCAN ZCARD ZRANGE XLEN XRANGE ", " "+w+" ") {
				t.Errorf("sent %s", cmd)
			}
		}
	}

	f := &redisFake{replies: map[string]string{"TYPE": `"none"`, "TTL": `-2`}}
	if _, err := KeyValue(context.Background(), f.exec, "gone"); !isCode(err, "data.no_key") {
		t.Errorf("gone: %v", err)
	}
	if _, err := KeyValue(context.Background(), f.exec, `bad\`); !isCode(err, "data.bad_key") {
		t.Errorf("bad id: %v", err)
	}
	// A string longer than what is read says it was cut.
	f = &redisFake{replies: map[string]string{"TYPE": `"string"`, "TTL": `-1`, "STRLEN": `100000`, "GETRANGE": `"` + strings.Repeat("a", 10) + `"`}}
	if v, _ := KeyValue(context.Background(), f.exec, "big"); len(v.Cut) != 1 {
		t.Errorf("cut %v", v.Cut)
	}
}
