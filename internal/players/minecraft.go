package players

import (
	"regexp"
	"strings"
	"sync"
)

var (
	// The part of a log line after its time and thread. Vanilla and Forge
	// put the level in a second bracket, Paper puts it next to the time.
	mcPrefix = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\[[^\]]*\](?: ?\[[^\]]*\])*: (.*)$`) })
	mcJoin   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(\S+)(?: \(formerly \S+\))? joined the game$`) })
	mcLeave  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(\S+) left the game$`) })
	mcChat   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(?:\[Not Secure\] )?<([^>\s]+)> (.*)$`) })
	mcUUID   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^UUID of player (\S+) is ([0-9a-fA-F-]{32,36})$`) })
	mcLogin  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(\S+)\[/([^\]]+)\] logged in with entity id `) })
)

// What the parser remembers about players that logged in, so the join that
// follows has an id and an address. Names are bounded: a long run with many
// visitors starts over.
const mcRemember = 1024

// Minecraft reads the console of a Java edition server.
type Minecraft struct {
	uuids map[string]string
	ips   map[string]string
}

func NewMinecraft() *Minecraft {
	return &Minecraft{uuids: map[string]string{}, ips: map[string]string{}}
}

func (m *Minecraft) Feed(line string) []Event {
	sub := mcPrefix().FindStringSubmatch(strings.TrimRight(line, " \t\r"))
	if sub == nil {
		return nil
	}
	msg := sub[1]
	if c := mcChat().FindStringSubmatch(msg); c != nil {
		return []Event{{Kind: Chat, PlayerID: m.id(c[1]), Name: c[1], Channel: "global", Text: c[2]}}
	}
	if u := mcUUID().FindStringSubmatch(msg); u != nil {
		if len(m.uuids) >= mcRemember {
			clear(m.uuids)
		}
		m.uuids[u[1]] = strings.ToLower(u[2])
		return nil
	}
	if l := mcLogin().FindStringSubmatch(msg); l != nil {
		if len(m.ips) >= mcRemember {
			clear(m.ips)
		}
		m.ips[l[1]] = stripPort(l[2])
		return nil
	}
	if j := mcJoin().FindStringSubmatch(msg); j != nil {
		return []Event{{Kind: Join, PlayerID: m.id(j[1]), Name: j[1], IP: m.ips[j[1]]}}
	}
	if l := mcLeave().FindStringSubmatch(msg); l != nil {
		return []Event{{Kind: Leave, PlayerID: m.id(l[1]), Name: l[1]}}
	}
	return nil
}

func (m *Minecraft) id(name string) string {
	if u, ok := m.uuids[name]; ok {
		return u
	}
	return "name:" + name
}
