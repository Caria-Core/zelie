package query

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// MinecraftStatus is what a Java Edition server answers to a server list
// ping.
type MinecraftStatus struct {
	// Raw is the JSON the server sent.
	Raw     string
	Version string
	Online  int
	Max     int
}

const slpTimeout = 10 * time.Second

// slpProtocol is the protocol version the handshake claims. Servers answer
// a status request whatever it is.
const slpProtocol = 767

// Minecraft asks the server at addr, a host and port, for its status
// with the Server List Ping: a handshake with next state 1, then a status
// request. Every packet is its length and its body, and numbers in them are
// VarInts.
func Minecraft(ctx context.Context, addr string) (MinecraftStatus, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return MinecraftStatus{}, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return MinecraftStatus{}, fmt.Errorf("port %q: %w", portText, err)
	}
	ctx, cancel := context.WithTimeout(ctx, slpTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return MinecraftStatus{}, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	hs := appendVarInt(nil, 0)
	hs = appendVarInt(hs, slpProtocol)
	hs = appendVarInt(hs, len(host))
	hs = append(hs, host...)
	hs = binary.BigEndian.AppendUint16(hs, uint16(port))
	hs = appendVarInt(hs, 1)
	out := append(appendVarInt(nil, len(hs)), hs...)
	out = append(out, 1, 0)
	if _, err := conn.Write(out); err != nil {
		return MinecraftStatus{}, err
	}

	r := bufio.NewReader(conn)
	if _, err := readVarInt(r); err != nil { // the packet's length
		return MinecraftStatus{}, err
	}
	if id, err := readVarInt(r); err != nil || id != 0 {
		return MinecraftStatus{}, fmt.Errorf("the status answer has id %d: %v", id, err)
	}
	n, err := readVarInt(r)
	if err != nil || n <= 0 || n > 1<<20 {
		return MinecraftStatus{}, fmt.Errorf("the status answer has length %d: %v", n, err)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return MinecraftStatus{}, err
	}
	var parsed struct {
		Version struct {
			Name string `json:"name"`
		} `json:"version"`
		Players struct {
			Max    int `json:"max"`
			Online int `json:"online"`
		} `json:"players"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return MinecraftStatus{}, fmt.Errorf("the status is not JSON: %w", err)
	}
	return MinecraftStatus{Raw: string(b), Version: parsed.Version.Name, Online: parsed.Players.Online, Max: parsed.Players.Max}, nil
}

func appendVarInt(b []byte, n int) []byte {
	for u := uint32(n); ; u >>= 7 {
		if u < 0x80 {
			return append(b, byte(u))
		}
		b = append(b, byte(u)|0x80)
	}
}

func readVarInt(r io.ByteReader) (int, error) {
	var n uint32
	for shift := 0; shift < 35; shift += 7 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		n |= uint32(b&0x7f) << shift
		if b&0x80 == 0 {
			return int(int32(n)), nil
		}
	}
	return 0, errors.New("VarInt is too long")
}
