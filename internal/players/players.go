// Package players reads player activity out of a game server's console
// output. It does no I/O and keeps no clock: a line that carries no time
// gives an event with a zero At, and the caller stamps it.
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
