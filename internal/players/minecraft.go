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
	mcCmd    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\S+ issued server command: `) })
	mcEmote  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(?:\[Not Secure\] )?\* \S`) })
	// A name tag puts a player's text in the line a mob's death is logged on.
	mcNamed = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^Named entity `) })
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

func (m *Minecraft) Feed(line string) []Event { return bounded(m.parse(line)) }

func (m *Minecraft) parse(line string) []Event {
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

// playerLine says whether the line carries something a player made: an event
// of the parser's, a command, an emote or a named mob.
func (m *Minecraft) playerLine(line string) bool {
	sub := mcPrefix().FindStringSubmatch(strings.TrimRight(line, " \t\r"))
	if sub == nil {
		return false
	}
	for _, re := range []*regexp.Regexp{mcCmd(), mcEmote(), mcNamed(), mcUUID(), mcLogin()} {
		if re.MatchString(sub[1]) {
			return true
		}
	}
	return len(m.parse(line)) > 0
}
