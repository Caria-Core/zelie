// Package query asks running game servers who is on them, the way their
// own server lists do.
package query

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// A2SInfo is the part of Valve's Source query answer that Zelie reads.
type A2SInfo struct {
	Name       string
	Map        string
	Game       string
	Players    int
	MaxPlayers int
	Bots       int
}

// Valve's protocol: every message starts with four 0xFF bytes. The request
// is a T and a fixed string. Since 2020 a server answers it with a
// challenge number first, which must come back at the end of the request.
const (
	a2sHeader      = "\xff\xff\xff\xff"
	a2sInfoRequest = a2sHeader + "TSource Engine Query\x00"
	a2sChallenge   = 'A'
	a2sInfoReply   = 'I'
)

const a2sTimeout = 5 * time.Second

// A2S sends A2S_INFO over UDP to addr, a host and port, and reads
// what the server says about itself.
func A2S(ctx context.Context, addr string) (A2SInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, a2sTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", addr)
	if err != nil {
		return A2SInfo{}, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	// Closing the connection is what makes a cancelled context end the read.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	request := []byte(a2sInfoRequest)
	buf := make([]byte, 1400)
	// One challenge is expected; a second one means the server is not
	// answering the way the protocol says.
	for range 3 {
		if _, err := conn.Write(request); err != nil {
			return A2SInfo{}, err
		}
		n, err := conn.Read(buf)
		if err != nil {
			return A2SInfo{}, fmt.Errorf("query %s: %w", addr, err)
		}
		reply := buf[:n]
		if len(reply) < 5 || string(reply[:4]) != a2sHeader {
			return A2SInfo{}, errors.New("the server's answer is not a Source query answer")
		}
		switch reply[4] {
		case a2sChallenge:
			if len(reply) < 9 {
				return A2SInfo{}, errors.New("the challenge is cut short")
			}
			request = append([]byte(a2sInfoRequest), reply[5:9]...)
		case a2sInfoReply:
			return parseA2SInfo(reply[5:])
		default:
			return A2SInfo{}, fmt.Errorf("the server answered with type %#x", reply[4])
		}
	}
	return A2SInfo{}, errors.New("the server kept asking for a challenge")
}

func parseA2SInfo(b []byte) (A2SInfo, error) {
	r := bytes.NewReader(b)
	var info A2SInfo
	if _, err := r.ReadByte(); err != nil { // protocol version
		return info, errBadInfo
	}
	for _, field := range []*string{&info.Name, &info.Map, new(string), &info.Game} {
		s, err := readCString(r)
		if err != nil {
			return info, errBadInfo
		}
		*field = s
	}
	var tail struct {
		AppID              uint16 // too small for many games; the extended data has the real one
		Players, Max, Bots uint8
	}
	if err := binary.Read(r, binary.LittleEndian, &tail); err != nil {
		return info, errBadInfo
	}
	info.Players, info.MaxPlayers, info.Bots = int(tail.Players), int(tail.Max), int(tail.Bots)
	return info, nil
}

var errBadInfo = errors.New("the server's info answer is cut short")

func readCString(r *bytes.Reader) (string, error) {
	var out []byte
	for {
		c, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if c == 0 {
			return string(out), nil
		}
		out = append(out, c)
	}
}
