package players

import (
	"encoding/json"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A block that is still open after this much is not one of ours, and is
// dropped so a stray "{" cannot swallow the console.
const (
	maxBlockLines = 200
	maxBlockBytes = 16 << 10
)

var (
	// Both start the line: a player's chat can contain anything, but it
	// never starts a line of its own.
	rustJoin       = regexp.MustCompile(`^([^/\s]+)/(\d{17})/(.*) joined \[[^\]/]*/(\d{17})\]\s*$`)
	rustLeaveAddr  = regexp.MustCompile(`^([^/\s]+)/(\d{17})/(.*) disconnecting: (.*)$`)
	rustLeaveID    = regexp.MustCompile(`^(.*)\[(\d{17})\] disconnecting: (.*)$`)
	steamID        = regexp.MustCompile(`\b\d{17}\b`)
	betterChatLine = "[Better Chat]"
)

// Rust reads what a Rust server prints. Chat and reports arrive as JSON
// objects spread over several lines, so it holds a block while one is open.
type Rust struct {
	block []string
	size  int
	open  bool
}

func NewRust() *Rust { return &Rust{} }

func (r *Rust) Feed(line string) []Event {
	line = strings.TrimRight(line, " \t\r")
	if r.open {
		switch {
		case line == "}":
			text := strings.Join(append(r.block, line), "\n")
			r.reset()
			return rustBlock(text)
		case line == "{":
			// The one before never closed.
			r.reset()
			r.start()
			return nil
		}
		r.block = append(r.block, line)
		r.size += len(line) + 1
		if len(r.block) > maxBlockLines || r.size > maxBlockBytes {
			r.reset()
		}
		return nil
	}
	if line == "{" {
		r.start()
		return nil
	}
	if len(line) > 2 && line[0] == '{' && line[len(line)-1] == '}' {
		return rustBlock(line)
	}
	return rustLine(line)
}

func (r *Rust) start() {
	r.open, r.block, r.size = true, append(r.block[:0], "{"), 2
}

func (r *Rust) reset() {
	r.open, r.block, r.size = false, r.block[:0], 0
}

func rustLine(line string) []Event {
	if strings.HasPrefix(line, betterChatLine) {
		// The same chat comes as a JSON block, which has the player's id.
		return nil
	}
	if strings.HasPrefix(line, "[CHAT]") || strings.HasPrefix(line, "[TEAM CHAT]") {
		// What a player typed must not be able to fill the log with near misses.
		return nil
	}
	if m := rustJoin.FindStringSubmatch(line); m != nil {
		return []Event{{Kind: Join, PlayerID: m[2], Name: m[3], IP: stripPort(m[1])}}
	}
	if m := rustLeaveAddr.FindStringSubmatch(line); m != nil {
		return []Event{{Kind: Leave, PlayerID: m[2], Name: m[3], IP: stripPort(m[1]), Reason: m[4]}}
	}
	if m := rustLeaveID.FindStringSubmatch(line); m != nil {
		return []Event{{Kind: Leave, PlayerID: m[2], Name: m[1], Reason: m[3]}}
	}
	switch {
	case strings.Contains(line, " joined ["):
		return nearMiss("join", line)
	case strings.Contains(line, "disconnecting"):
		return nearMiss("leave", line)
	case looksLikeReport(line):
		return nearMiss("report", line)
	}
	return nil
}

func rustBlock(text string) []Event {
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		if looksLikeReport(text) {
			return nearMiss("report", text)
		}
		return nil
	}
	if _, ok := m["Channel"]; ok {
		if id, name, msg := jsonString(m["UserId"]), jsonString(m["Username"]), jsonString(m["Message"]); id != "" && name != "" {
			return []Event{{
				Kind: Chat, PlayerID: id, Name: name,
				Channel: chatChannel(m["Channel"]),
				Text:    strings.TrimPrefix(msg, name+": "),
				At:      unixSeconds(m["Time"]),
			}}
		}
	}
	// Reports (F7) have not been measured on a live server. This is the
	// shape the report plugins we know of use.
	if p, t := jsonString(m["PlayerId"]), jsonString(m["TargetId"]); p != "" && t != "" {
		reason := jsonString(m["Subject"])
		if reason == "" {
			reason = jsonString(m["Type"])
		}
		return []Event{{
			Kind: Report, PlayerID: p, Name: jsonString(m["PlayerName"]),
			Target: t, TargetName: jsonString(m["TargetName"]),
			Reason: reason, Text: jsonString(m["Message"]),
			At: unixSeconds(m["Time"]),
		}}
	}
	if looksLikeReport(text) {
		return nearMiss("report", text)
	}
	return nil
}

func looksLikeReport(s string) bool {
	return strings.Contains(strings.ToLower(s), "report") && len(steamID.FindAllString(s, 2)) >= 2
}

func nearMiss(what, text string) []Event {
	return []Event{{Kind: NearMiss, Reason: what, Text: text}}
}

func chatChannel(v any) string {
	switch s := jsonString(v); s {
	case "0":
		return "global"
	case "1":
		return "team"
	default:
		return s
	}
}

func jsonString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	}
	return ""
}

func unixSeconds(v any) time.Time {
	n, err := strconv.ParseInt(jsonString(v), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// stripPort turns "1.2.3.4:5678" into "1.2.3.4".
func stripPort(addr string) string {
	if ap, err := netip.ParseAddrPort(addr); err == nil {
		return ap.Addr().String()
	}
	if i := strings.LastIndexByte(addr, ':'); i > 0 {
		return addr[:i]
	}
	return addr
}
