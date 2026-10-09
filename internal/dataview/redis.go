package dataview

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Caria-Core/zelie/internal/msg"
)

// Redis is read through redis-cli with --quoted-json: one reply per line,
// strings in Redis's own quoting (\xHH, \n), so binary keys and values
// come through intact. Only the commands below are ever sent.

var (
	errNoKey     = msg.Define(http.StatusNotFound, "data.no_key", "There is no key named {key}.")
	errBadKey    = msg.Define(http.StatusBadRequest, "data.bad_key", "That is not a key this viewer listed.")
	errBadCursor = msg.Define(http.StatusBadRequest, "data.bad_cursor", "That is not a place in the key list.")
	errTooLarge  = msg.Define(http.StatusUnprocessableEntity, "data.too_large", "Redis answered with more than the viewer can show.")
)

var redisClient = []string{"sh", "-c", `REDISCLI_AUTH="$REDIS_PASSWORD" exec redis-cli --no-auth-warning --quoted-json`}

// maxReply is how much of redis-cli's output one call reads. A key's items
// come back whole and only get cut for display afterwards, so without a
// limit a few big values would fill the core's memory.
const maxReply = 8 << 20

// ItemCount is how many items of a list, hash, set, sorted set or stream
// are shown.
const ItemCount = 100

// Key is one key. ID is its name in Redis's quoting, which the viewer
// sends back to open it; Name is how it reads.
type Key struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Binary bool   `json:"binary,omitempty"`
	Type   string `json:"type"`
	// TTL is in seconds, -1 for a key that never expires.
	TTL int64 `json:"ttl"`
}

// KeyPage is one step through the keys. Cursor is where the next step
// starts, "0" when the whole key space has been gone through.
type KeyPage struct {
	Keys   []Key  `json:"keys"`
	Cursor string `json:"cursor"`
}

// Value is what a key holds: up to ItemCount items, as rows under
// Columns. Size is its full length: characters, items or entries.
type Value struct {
	Key     Key         `json:"key"`
	Size    int64       `json:"size"`
	Columns []string    `json:"columns"`
	Rows    [][]*string `json:"rows"`
	// Binary lists the cells shown as hex, as row and column, and Cut the
	// ones cut short.
	Binary [][2]int `json:"binary,omitempty"`
	Cut    [][2]int `json:"cut,omitempty"`
}

// redisQuote writes a string the way redis-cli reads a quoted argument.
func redisQuote(b []byte) string {
	var s strings.Builder
	s.WriteByte('"')
	for _, c := range b {
		switch {
		case c == '"' || c == '\\':
			s.WriteByte('\\')
			s.WriteByte(c)
		case c >= 0x20 && c < 0x7f:
			s.WriteByte(c)
		default:
			fmt.Fprintf(&s, `\x%02x`, c)
		}
	}
	s.WriteByte('"')
	return s.String()
}

// redisUnquote reads a string as redis-cli --quoted-json writes it.
func redisUnquote(s string) ([]byte, error) {
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			continue
		}
		if i+1 >= len(s) {
			return nil, errors.New("quoting ends in a backslash")
		}
		i++
		switch s[i] {
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'a':
			out = append(out, '\a')
		case 'b':
			out = append(out, '\b')
		case 'x':
			if i+2 >= len(s) {
				return nil, errors.New("short \\x escape")
			}
			b, err := hex.DecodeString(s[i+1 : i+3])
			if err != nil {
				return nil, err
			}
			out = append(out, b[0])
			i += 2
		default:
			out = append(out, s[i])
		}
	}
	return out, nil
}

// fixQuotedJSON mends a line of redis-cli --quoted-json. It writes a
// string in Redis's quoting and then escapes the backslashes for JSON, but
// not the quotes: a value holding a " comes out as \\" and ends the JSON
// string early. Inside a string every backslash arrives doubled, so a quote
// after an odd number of Redis backslashes belongs to the value.
//
// It also quotes the scores inf and -inf, which it prints bare.
func fixQuotedJSON(line string) string {
	var out strings.Builder
	in, escaping := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case !in:
			if tok := nonFinite(line[i:]); tok != "" {
				out.WriteString(`"` + tok + `"`)
				i += len(tok) - 1
				continue
			}
			in = c == '"'
		case c == '\\' && i+1 < len(line):
			out.WriteByte(c)
			i++
			c = line[i]
			if c == '\\' {
				escaping = !escaping
			} else {
				escaping = false
			}
		case c == '"' && escaping:
			out.WriteByte('\\')
			escaping = false
		case c == '"':
			in = false
		default:
			escaping = false
		}
		out.WriteByte(c)
	}
	return out.String()
}

// nonFinite returns the number JSON has no word for that s starts with.
func nonFinite(s string) string {
	for _, t := range [...]string{"-inf", "inf", "nan"} {
		if strings.HasPrefix(s, t) {
			return t
		}
	}
	return ""
}

// readable is how a value is shown: as text when it is text, otherwise as
// hex.
func readable(b []byte) (string, bool) {
	if utf8.Valid(b) {
		return string(b), false
	}
	return "0x" + hex.EncodeToString(b), true
}

// redisRun sends commands, one per line, and returns one reply for each.
// A reply the server refused is an error value in its place.
func redisRun(ctx context.Context, exec Exec, commands []string) ([]json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, PageTimeout+30*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	stdout := &capped{n: maxReply}
	code, err := exec(ctx, redisClient, strings.NewReader(strings.Join(commands, "\n")+"\n"), stdout, &limited{w: &stderr, n: 4096})
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, errQuery.Err("detail", strings.TrimSpace(stderr.String()))
	}
	if stdout.over {
		return nil, errTooLarge.Err()
	}
	var replies []json.RawMessage
	for _, line := range strings.Split(strings.TrimRight(stdout.buf.String(), "\n"), "\n") {
		if rest, ok := strings.CutPrefix(line, "error:"); ok {
			replies = append(replies, json.RawMessage(`{"error":`+rest+`}`))
			continue
		}
		line = fixQuotedJSON(line)
		if !json.Valid([]byte(line)) {
			return nil, fmt.Errorf("redis-cli said %.200q", line)
		}
		replies = append(replies, json.RawMessage(line))
	}
	if len(replies) != len(commands) {
		return nil, fmt.Errorf("sent %d commands to redis-cli, got %d replies", len(commands), len(replies))
	}
	return replies, nil
}

func replyString(r json.RawMessage) ([]byte, bool) {
	var s string
	if json.Unmarshal(r, &s) != nil {
		return nil, false
	}
	b, err := redisUnquote(s)
	return b, err == nil
}

func replyInt(r json.RawMessage) int64 {
	var n json.Number
	d := json.NewDecoder(bytes.NewReader(r))
	d.UseNumber()
	if d.Decode(&n) != nil {
		return 0
	}
	i, _ := n.Int64()
	return i
}

// Keys goes one step through the keys whose names match pattern, a Redis
// glob.
func Keys(ctx context.Context, exec Exec, pattern, cursor string) (KeyPage, error) {
	if pattern == "" {
		pattern = "*"
	}
	if cursor == "" {
		cursor = "0"
	}
	if _, err := strconv.ParseUint(cursor, 10, 64); err != nil {
		return KeyPage{}, errBadCursor.Err()
	}
	if strings.ContainsRune(pattern, 0) {
		return KeyPage{}, errBadValue.Err()
	}
	r, err := redisRun(ctx, exec, []string{"SCAN " + cursor + " MATCH " + redisQuote([]byte(pattern)) + " COUNT 1000"})
	if err != nil {
		return KeyPage{}, err
	}
	var scan []json.RawMessage
	if json.Unmarshal(r[0], &scan) != nil || len(scan) != 2 {
		return KeyPage{}, fmt.Errorf("SCAN replied %s", r[0])
	}
	var next string
	var ids []string
	json.Unmarshal(scan[0], &next)
	json.Unmarshal(scan[1], &ids)
	page := KeyPage{Keys: []Key{}, Cursor: next}
	if len(ids) == 0 {
		return page, nil
	}
	var cmds []string
	for _, id := range ids {
		b, err := redisUnquote(id)
		if err != nil {
			return KeyPage{}, err
		}
		q := redisQuote(b)
		cmds = append(cmds, "TYPE "+q, "TTL "+q)
	}
	r, err = redisRun(ctx, exec, cmds)
	if err != nil {
		return KeyPage{}, err
	}
	for i, id := range ids {
		b, _ := redisUnquote(id)
		k := Key{ID: id, TTL: replyInt(r[2*i+1])}
		k.Name, k.Binary = readable(b)
		json.Unmarshal(r[2*i], &k.Type)
		if k.Type == "none" {
			continue // gone since the scan
		}
		page.Keys = append(page.Keys, k)
	}
	return page, nil
}

// KeyValue reads what a key holds.
func KeyValue(ctx context.Context, exec Exec, id string) (Value, error) {
	b, err := redisUnquote(id)
	if err != nil || len(b) == 0 {
		return Value{}, errBadKey.Err()
	}
	q := redisQuote(b)
	n := strconv.Itoa(ItemCount - 1)
	// Every read at once: the ones for other types answer WRONGTYPE.
	r, err := redisRun(ctx, exec, []string{
		"TYPE " + q, "TTL " + q,
		"STRLEN " + q, "GETRANGE " + q + " 0 " + strconv.Itoa(MaxFull-1),
		"LLEN " + q, "LRANGE " + q + " 0 " + n,
		"HLEN " + q, "HSCAN " + q + " 0 COUNT " + strconv.Itoa(ItemCount),
		"SCARD " + q, "SSCAN " + q + " 0 COUNT " + strconv.Itoa(ItemCount),
		"ZCARD " + q, "ZRANGE " + q + " 0 " + n + " WITHSCORES",
		"XLEN " + q, "XRANGE " + q + " - + COUNT " + strconv.Itoa(ItemCount),
	})
	if err != nil {
		return Value{}, err
	}
	v := Value{Key: Key{ID: id, TTL: replyInt(r[1])}, Rows: [][]*string{}}
	v.Key.Name, v.Key.Binary = readable(b)
	json.Unmarshal(r[0], &v.Key.Type)
	add := func(cells ...[]byte) {
		row := make([]*string, len(cells))
		for i, c := range cells {
			s, bin := readable(c)
			max := MaxCell
			if v.Key.Type == "string" {
				max = MaxFull
			}
			if utf8.RuneCountInString(s) > max {
				s = string([]rune(s)[:max])
				v.Cut = append(v.Cut, [2]int{len(v.Rows), i})
			}
			if bin {
				v.Binary = append(v.Binary, [2]int{len(v.Rows), i})
			}
			row[i] = &s
		}
		v.Rows = append(v.Rows, row)
	}
	list := func(r json.RawMessage) [][]byte {
		var items []string
		json.Unmarshal(r, &items)
		var out [][]byte
		for _, s := range items {
			if b, err := redisUnquote(s); err == nil {
				out = append(out, b)
			}
		}
		return out
	}
	scanned := func(r json.RawMessage) [][]byte {
		var scan []json.RawMessage
		if json.Unmarshal(r, &scan) != nil || len(scan) != 2 {
			return nil
		}
		return list(scan[1])
	}
	switch v.Key.Type {
	case "none":
		return Value{}, errNoKey.Err("key", v.Key.Name)
	case "string":
		v.Size, v.Columns = replyInt(r[2]), []string{"value"}
		if s, ok := replyString(r[3]); ok {
			add(s)
			if int64(len(s)) < v.Size {
				v.Cut = append(v.Cut, [2]int{0, 0})
			}
		}
	case "list":
		v.Size, v.Columns = replyInt(r[4]), []string{"value"}
		for _, item := range list(r[5]) {
			add(item)
		}
	case "hash":
		v.Size, v.Columns = replyInt(r[6]), []string{"field", "value"}
		items := scanned(r[7])
		for i := 0; i+1 < len(items) && len(v.Rows) < ItemCount; i += 2 {
			add(items[i], items[i+1])
		}
	case "set":
		v.Size, v.Columns = replyInt(r[8]), []string{"member"}
		for _, item := range scanned(r[9]) {
			if len(v.Rows) < ItemCount {
				add(item)
			}
		}
	case "zset":
		v.Size, v.Columns = replyInt(r[10]), []string{"member", "score"}
		var pairs [][]json.RawMessage
		json.Unmarshal(r[11], &pairs)
		for _, p := range pairs {
			if len(p) == 2 {
				m, _ := replyString(p[0])
				add(m, []byte(strings.Trim(string(p[1]), `"`)))
			}
		}
	case "stream":
		v.Size, v.Columns = replyInt(r[12]), []string{"id", "fields"}
		var entries [][]json.RawMessage
		json.Unmarshal(r[13], &entries)
		for _, e := range entries {
			if len(e) != 2 {
				continue
			}
			id, _ := replyString(e[0])
			fields := list(e[1])
			var parts []string
			for i := 0; i+1 < len(fields); i += 2 {
				k, _ := readable(fields[i])
				val, _ := readable(fields[i+1])
				parts = append(parts, k+"="+val)
			}
			add(id, []byte(strings.Join(parts, " ")))
		}
	default:
		// A module's type: the viewer can say what it is, not read it.
		v.Columns = []string{}
	}
	return v, nil
}
