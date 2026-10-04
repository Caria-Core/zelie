package players

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"time"
)

// QueryPlayer is one entry of a Source query player list.
type QueryPlayer struct {
	Name     string
	Score    int
	Duration time.Duration
}

const (
	a2sHeader     = "\xff\xff\xff\xff"
	a2sPlayerReq  = 'U'
	a2sChallenge  = 'A'
	a2sPlayerResp = 'D'
	a2sTimeout    = 2 * time.Second
)

// A2SPlayers asks the server at addr, a host and port over UDP, who is on
// it. Servers answer the first request with a challenge number that must
// come back with the second.
func A2SPlayers(ctx context.Context, addr string) ([]QueryPlayer, error) {
	ctx, cancel := context.WithTimeout(ctx, a2sTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	// Closing the socket is what ends a read when the context is cancelled.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	request := []byte(a2sHeader + "U\xff\xff\xff\xff")
	buf := make([]byte, 1400)
	for range 3 {
		if _, err := conn.Write(request); err != nil {
			return nil, err
		}
		n, err := conn.Read(buf)
		if err != nil {
			return nil, fmt.Errorf("query %s: %w", addr, err)
		}
		reply := buf[:n]
		if len(reply) < 5 || string(reply[:4]) != a2sHeader {
			return nil, errors.New("the answer is not a Source query answer")
		}
		switch reply[4] {
		case a2sChallenge:
			if len(reply) < 9 {
				return nil, errors.New("the challenge is cut short")
			}
			request = append([]byte(a2sHeader+"U"), reply[5:9]...)
		case a2sPlayerResp:
			return parseA2SPlayers(reply[5:])
		default:
			return nil, fmt.Errorf("the server answered with type %#x", reply[4])
		}
	}
	return nil, errors.New("the server kept asking for a challenge")
}

func parseA2SPlayers(b []byte) ([]QueryPlayer, error) {
	if len(b) < 1 {
		return nil, errors.New("the player list is cut short")
	}
	count := int(b[0])
	b = b[1:]
	out := make([]QueryPlayer, 0, count)
	for range count {
		if len(b) < 1 {
			break
		}
		b = b[1:] // index, which servers leave at 0
		end := -1
		for i, c := range b {
			if c == 0 {
				end = i
				break
			}
		}
		if end < 0 || len(b) < end+1+8 {
			return out, errors.New("the player list is cut short")
		}
		p := QueryPlayer{Name: string(b[:end])}
		b = b[end+1:]
		p.Score = int(int32(binary.LittleEndian.Uint32(b)))
		secs := math.Float32frombits(binary.LittleEndian.Uint32(b[4:]))
		if secs > 0 && secs < 1e9 {
			p.Duration = time.Duration(float64(secs) * float64(time.Second))
		}
		b = b[8:]
		out = append(out, p)
	}
	return out, nil
}
