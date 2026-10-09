// Package players reads player activity out of a game server's console
// output, and asks running servers and Steam about who is on them. The
// parsers do no I/O and keep no clock: a line that carries no time gives an
// event with a zero At, and the caller stamps it. a2s.go, rcon.go and
// steam.go are the parts that use the network.
package players

import "time"

// Kind is what an event says happened.
type Kind uint8

const (
	Join Kind = iota + 1
	Leave
	Chat
	Report
	// NearMiss is not something a player did. It marks a line or block that
	// looks like one of the above but did not parse, so a format change in
	// a game or plugin shows up in the log instead of as silence. Reason
	// says which kind it resembled and Text holds the line.
	NearMiss
)

func (k Kind) String() string {
	switch k {
	case Join:
		return "join"
	case Leave:
		return "leave"
	case Chat:
		return "chat"
	case Report:
		return "report"
	case NearMiss:
		return "near miss"
	}
	return "unknown"
}

// Event is one thing a player did.
type Event struct {
	Kind Kind
	// PlayerID is a SteamID64 for Rust, a UUID for Minecraft when the log
	// has shown it, and "name:<name>" when it has not.
	PlayerID string
	Name     string
	IP       string
	// Channel is "global" or "team" for chat.
	Channel string
	Text    string
	// Target is who a report is about.
	Target     string
	TargetName string
	Reason     string
	At         time.Time
}

// Parser turns console lines into events. A parser belongs to one running
// server: it may remember things between lines.
type Parser interface {
	// Feed takes one console line, without colour codes or line ending.
	Feed(line string) []Event
}

// The longest player id and address a parser passes on. A SteamID64 has 17
// digits and a UUID 36 characters. A console that prints longer ones is
// making them up, and each would be stored and indexed.
const (
	maxIDLen = 64
	maxIPLen = 64
)

// bounded turns the events whose ids are too long into near misses, so the
// log says so and the database does not take them.
func bounded(events []Event) []Event {
	for i, ev := range events {
		if len(ev.PlayerID) > maxIDLen || len(ev.Target) > maxIDLen {
			text := ev.PlayerID
			if len(text) <= maxIDLen {
				text = ev.Target
			}
			events[i] = Event{Kind: NearMiss, Reason: "id too long", Text: text}
		}
	}
	return events
}

// ParserFor returns the parser for a catalog egg id, or nil for games that
// have none. Bedrock and the Velocity proxy log in other formats.
func ParserFor(catalogID string) Parser {
	switch catalogID {
	case "rust":
		return NewRust()
	case "minecraft-vanilla", "minecraft-paper", "minecraft-purpur",
		"minecraft-fabric", "minecraft-forge", "minecraft-neoforge":
		return NewMinecraft()
	}
	return nil
}

// PlayerLines marks the lines of a game's console that carry something a
// player made: chat, commands, emotes, and the lines the parser reads
// events from, which hold the player's name. Whoever reads the console for
// what the server itself said must leave these out, since a player can make
// them say anything. lines are in the order they were printed. Games
// without a parser have none marked.
func PlayerLines(catalogID string, lines []string) []bool {
	marks := make([]bool, len(lines))
	switch Family(catalogID) {
	case GameMinecraft:
		m := NewMinecraft()
		for i, l := range lines {
			marks[i] = m.playerLine(l)
		}
	case GameRust:
		r := NewRust()
		for i, l := range lines {
			for j := r.playerLines(l); j > 0; j-- {
				marks[i-j+1] = true
			}
		}
	}
	return marks
}

// Game families with a parser.
const (
	GameRust      = "rust"
	GameMinecraft = "minecraft"
)

// Family returns "rust" or "minecraft" for a catalog egg id that has a
// parser, and "" for any other.
func Family(catalogID string) string {
	switch catalogID {
	case "rust":
		return GameRust
	case "minecraft-vanilla", "minecraft-paper", "minecraft-purpur",
		"minecraft-fabric", "minecraft-forge", "minecraft-neoforge":
		return GameMinecraft
	}
	return ""
}

// queryGames answer Valve's query protocol, which lists who is on them.
// Only games whose eggs are known to are here; the rest stay out rather
// than show a tab that always fails.
var queryGames = map[string]bool{
	"counter-strike-2": true, "garrys-mod": true, "valheim": true,
	"squad": true, "enshrouded": true, "v-rising": true, "sons-of-the-forest": true,
}

// Capability says what the panel can show of a game's players: "full" for
// history, chat and actions, "list" for who is on now, and "" for nothing.
func Capability(catalogID string) string {
	switch {
	case ParserFor(catalogID) != nil:
		return "full"
	case queryGames[catalogID]:
		return "list"
	}
	return ""
}

// QueryPortOffset is how far a game's query port is from its game port when
// the egg has no QUERY_PORT variable to say.
func QueryPortOffset(catalogID string) int {
	if catalogID == "valheim" {
		return 1
	}
	return 0
}
