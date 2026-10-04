package players

import (
	"regexp"
	"strings"
)

var (
	// "There are 1 of a max of 20 players online: Steve" is vanilla's,
	// "There are 1/20 players online:" is Spigot's and Paper's.
	mcListHead = regexp.MustCompile(`^There are (\d+)(?: of a max of |/)(\d+) players online:\s*(.*)$`)
	mcUUIDTail = regexp.MustCompile(` \([0-9a-fA-F-]{32,36}\)$`)
)

// MinecraftList reads the server's answer to the list command from its
// console. Vanilla puts the names on the same line; Paper may put them on
// the next.
type MinecraftList struct {
	head    bool
	waiting bool
	// Names are who the server said is online, once Feed has reported done.
	Names []string
}

// Feed takes one console line and reports whether the answer is complete.
func (l *MinecraftList) Feed(line string) (done bool) {
	sub := mcPrefix.FindStringSubmatch(strings.TrimRight(line, " \t\r"))
	if sub == nil {
		return false
	}
	text := sub[1]
	if l.waiting {
		l.Names = mcNames(text)
		return true
	}
	h := mcListHead.FindStringSubmatch(text)
	if h == nil {
		return false
	}
	l.head = true
	if names := mcNames(h[3]); len(names) > 0 {
		l.Names = names
		return true
	}
	if h[1] == "0" {
		return true
	}
	l.waiting = true
	return false
}

// Pending says the header came but the names have not.
func (l *MinecraftList) Pending() bool { return l.head && l.waiting }

func mcNames(s string) []string {
	var out []string
	for _, n := range strings.Split(s, ",") {
		n = mcUUIDTail.ReplaceAllString(strings.TrimSpace(n), "")
		// Paper can prefix a group, "default: Steve".
		if i := strings.LastIndex(n, ": "); i >= 0 {
			n = n[i+2:]
		}
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}
