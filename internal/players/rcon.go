package players

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// RCONTimeout is how long connecting, and each command, may take.
const RCONTimeout = 5 * time.Second

// ErrRCON is what every failure to use the remote console wraps. The text
// never holds the password, which travels in the address.
var ErrRCON = errors.New("remote console")

// RCON is a Rust WebRCON connection. It is meant to be short: open it for a
// request, run what is needed and close it.
type RCON struct {
	conn *websocket.Conn
	next int
}

// DialRCON connects to a Rust server's WebRCON at host:port.
func DialRCON(ctx context.Context, hostport, password string) (*RCON, error) {
	ctx, cancel := context.WithTimeout(ctx, RCONTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+hostport+"/"+url.PathEscape(password), nil)
	if err != nil {
		return nil, rconError("connect", err, password)
	}
	// A player list of a full server is far over the default limit.
	conn.SetReadLimit(4 << 20)
	return &RCON{conn: conn, next: 1000}, nil
}

// rconError drops the address from a library error: it holds the password.
func rconError(what string, err error, password string) error {
	text := err.Error()
	for _, secret := range []string{url.PathEscape(password), password} {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "•••")
		}
	}
	if len(text) > 200 {
		text = text[:200]
	}
	return fmt.Errorf("%w: %s: %s", ErrRCON, what, text)
}

type rconMessage struct {
	Identifier int    `json:"Identifier"`
	Message    string `json:"Message"`
}

// Run sends a command and returns the server's answer to it. Other
// messages the server prints meanwhile are skipped.
func (r *RCON) Run(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, RCONTimeout)
	defer cancel()
	r.next++
	id := r.next
	req, _ := json.Marshal(map[string]any{"Identifier": id, "Message": command, "Name": "WebRcon"})
	if err := r.conn.Write(ctx, websocket.MessageText, req); err != nil {
		return "", rconError("send", err, "")
	}
	for {
		_, data, err := r.conn.Read(ctx)
		if err != nil {
			return "", rconError("read", err, "")
		}
		var m rconMessage
		if json.Unmarshal(data, &m) != nil || m.Identifier != id {
			continue
		}
		return m.Message, nil
	}
}

func (r *RCON) Close() { r.conn.Close(websocket.StatusNormalClosure, "") }

// RCONPlayer is one row of Rust's playerlist.
type RCONPlayer struct {
	ID               string
	Name             string
	Ping             int
	Address          string
	ConnectedSeconds int
}

// flexString reads a JSON string or number as text: servers have printed
// SteamIDs both ways.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

// ParseRustPlayerList reads the answer to playerlist.
func ParseRustPlayerList(text string) ([]RCONPlayer, error) {
	var rows []struct {
		SteamID          flexString `json:"SteamID"`
		DisplayName      string     `json:"DisplayName"`
		Ping             int        `json:"Ping"`
		Address          string     `json:"Address"`
		ConnectedSeconds float64    `json:"ConnectedSeconds"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &rows); err != nil {
		return nil, fmt.Errorf("%w: the player list is not JSON", ErrRCON)
	}
	out := make([]RCONPlayer, 0, len(rows))
	for _, r := range rows {
		out = append(out, RCONPlayer{ID: string(r.SteamID), Name: r.DisplayName, Ping: r.Ping, Address: r.Address, ConnectedSeconds: int(r.ConnectedSeconds)})
	}
	return out, nil
}

// CleanArg makes text safe to put between quotes in a console command:
// control characters go, quotes, backslashes and command separators become
// harmless ones, and the length is capped in characters.
func CleanArg(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			continue
		case r == '"':
			r = '\''
		case r == '\\':
			r = '/'
		case r == ';':
			r = ','
		}
		if n++; n > max {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
