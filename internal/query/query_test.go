package query

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// infoReply is an A2S_INFO answer as a Rust server sends it.
func infoReply(players, max, bots byte) []byte {
	b := []byte(a2sHeader + "I")
	b = append(b, 17) // protocol
	for _, s := range []string{"My Rust", "Procedural Map", "rust", "Rust"} {
		b = append(b, s...)
		b = append(b, 0)
	}
	b = binary.LittleEndian.AppendUint16(b, 4200)
	b = append(b, players, max, bots)
	// Server type, environment, visibility, VAC: read by nobody here.
	return append(b, 'd', 'l', 0, 1)
}

// udpServer answers every datagram with what answer returns for it, and
// keeps what it received.
type udpServer struct {
	addr string
	mu   sync.Mutex
	got  [][]byte
}

func serveUDP(t *testing.T, answer func(n int, req []byte) []byte) *udpServer {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	s := &udpServer{addr: pc.LocalAddr().String()}
	go func() {
		buf := make([]byte, 1400)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			req := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			s.got = append(s.got, req)
			count := len(s.got)
			s.mu.Unlock()
			if reply := answer(count, req); reply != nil {
				pc.WriteTo(reply, from)
			}
		}
	}()
	return s
}

func (s *udpServer) asked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func TestA2SWithChallenge(t *testing.T) {
	challenge := []byte{1, 2, 3, 4}
	s := serveUDP(t, func(n int, req []byte) []byte {
		if !strings.HasSuffix(string(req), "\x00"+string(challenge)) {
			return append([]byte(a2sHeader+"A"), challenge...)
		}
		return infoReply(3, 100, 1)
	})
	info, err := A2S(context.Background(), s.addr)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "My Rust" || info.Map != "Procedural Map" || info.Game != "Rust" || info.Players != 3 || info.MaxPlayers != 100 || info.Bots != 1 {
		t.Errorf("info %+v", info)
	}
	if n := s.asked(); n != 2 {
		t.Errorf("the server was asked %d times, want 2", n)
	}
}

func TestA2SWithoutChallenge(t *testing.T) {
	s := serveUDP(t, func(int, []byte) []byte { return infoReply(0, 8, 0) })
	info, err := A2S(context.Background(), s.addr)
	if err != nil || info.Players != 0 || info.MaxPlayers != 8 {
		t.Errorf("info %+v, %v", info, err)
	}
}

func TestA2SBadAnswers(t *testing.T) {
	cases := map[string]func(int, []byte) []byte{
		"not a query answer": func(int, []byte) []byte { return []byte("hello there") },
		"cut short":          func(int, []byte) []byte { return infoReply(1, 2, 0)[:12] },
		"endless challenge":  func(int, []byte) []byte { return []byte(a2sHeader + "A\x01\x02\x03\x04") },
		"other type":         func(int, []byte) []byte { return []byte(a2sHeader + "E\x00\x00") },
	}
	for name, answer := range cases {
		s := serveUDP(t, answer)
		if _, err := A2S(context.Background(), s.addr); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestA2SNoAnswer(t *testing.T) {
	s := serveUDP(t, func(int, []byte) []byte { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := A2S(ctx, s.addr); err == nil {
		t.Fatal("a silent server gave an answer")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the query outlived its context by %v", time.Since(start))
	}
}

// fakeMinecraft answers a status request the way a server does and returns
// what the handshake said.
func fakeMinecraft(t *testing.T, status string) (addr string, handshake chan []byte) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	handshake = make(chan []byte, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 256)
		n, _ := c.Read(buf)
		handshake <- append([]byte(nil), buf[:n]...)
		body := appendVarInt(nil, 0)
		body = appendVarInt(body, len(status))
		body = append(body, status...)
		c.Write(append(appendVarInt(nil, len(body)), body...))
	}()
	return l.Addr().String(), handshake
}

func TestMinecraft(t *testing.T) {
	const status = `{"version":{"name":"Paper 1.21.4","protocol":769},"players":{"max":20,"online":2,"sample":[]},"description":"hi"}`
	addr, handshake := fakeMinecraft(t, status)
	got, err := Minecraft(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	if got.Online != 2 || got.Max != 20 || got.Version != "Paper 1.21.4" || got.Raw != status {
		t.Errorf("status %+v", got)
	}
	// The handshake and the status request go out together: the handshake's
	// length, id 0, protocol 767 as a VarInt, the host, the port, next state
	// 1, then the two bytes of the status request.
	hs := <-handshake
	host, port, _ := net.SplitHostPort(addr)
	want := append(appendVarInt(nil, 0), appendVarInt(nil, 767)...)
	want = append(want, appendVarInt(nil, len(host))...)
	want = append(want, host...)
	var p uint16
	for _, c := range port {
		p = p*10 + uint16(c-'0')
	}
	want = binary.BigEndian.AppendUint16(want, p)
	want = append(want, 1)
	want = append(appendVarInt(nil, len(want)), want...)
	want = append(want, 1, 0)
	if string(hs) != string(want) {
		t.Errorf("handshake % x, want % x", hs, want)
	}
}

func TestMinecraftBadStatus(t *testing.T) {
	addr, _ := fakeMinecraft(t, "not json")
	if _, err := Minecraft(context.Background(), addr); err == nil {
		t.Error("a status that is not JSON was accepted")
	}
	if _, err := Minecraft(context.Background(), "127.0.0.1"); err == nil {
		t.Error("an address with no port was accepted")
	}
}

func TestVarInt(t *testing.T) {
	for _, n := range []int{0, 1, 127, 128, 767, 25565, 1 << 20} {
		b := appendVarInt(nil, n)
		got, err := readVarInt(strings.NewReader(string(b)))
		if err != nil || got != n {
			t.Errorf("%d became %d, %v", n, got, err)
		}
	}
	if _, err := readVarInt(strings.NewReader("\xff\xff\xff\xff\xff\xff")); err == nil {
		t.Error("a VarInt of six bytes was accepted")
	}
}
