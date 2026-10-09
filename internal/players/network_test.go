package players

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func playerReply(names ...string) []byte {
	b := []byte(a2sHeader + "D")
	b = append(b, byte(len(names)))
	for i, n := range names {
		b = append(b, byte(i))
		b = append(b, n...)
		b = append(b, 0)
		b = binary.LittleEndian.AppendUint32(b, uint32(i*10))
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(90.5))
	}
	return b
}

func fakeUDP(t *testing.T, answer func(req []byte) []byte) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1400)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if reply := answer(buf[:n]); reply != nil {
				pc.WriteTo(reply, from)
			}
		}
	}()
	return pc.LocalAddr().String()
}

func TestA2SPlayersWithChallenge(t *testing.T) {
	addr := fakeUDP(t, func(req []byte) []byte {
		if string(req[5:]) == "\xff\xff\xff\xff" {
			return []byte(a2sHeader + "A\x01\x02\x03\x04")
		}
		if string(req[5:]) != "\x01\x02\x03\x04" {
			return nil
		}
		return playerReply("Ann", "Bob")
	})
	got, err := A2SPlayers(context.Background(), addr)
	if err != nil || len(got) != 2 || got[1].Name != "Bob" || got[1].Score != 10 || got[0].Duration != 90500*time.Millisecond {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestA2SPlayersFailures(t *testing.T) {
	for name, reply := range map[string][]byte{
		"junk":      []byte("hello"),
		"cut short": append(playerReply("Ann")[:8], 'x'),
		"wrong":     []byte(a2sHeader + "Z"),
	} {
		addr := fakeUDP(t, func([]byte) []byte { return reply })
		if _, err := A2SPlayers(context.Background(), addr); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	silent := fakeUDP(t, func([]byte) []byte { return nil })
	start := time.Now()
	if _, err := A2SPlayers(context.Background(), silent); err == nil || time.Since(start) > 4*time.Second {
		t.Errorf("silent server: %v after %v", err, time.Since(start))
	}
}

func TestMinecraftListShapes(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  []string
		done  bool
	}{
		{"vanilla", []string{"[12:00:00] [Server thread/INFO]: There are 2 of a max of 20 players online: Steve, Alex"}, []string{"Steve", "Alex"}, true},
		{"vanilla uuids", []string{"[12:00:00] [Server thread/INFO]: There are 1 of a max of 20 players online: Steve (069a79f4-44e9-4726-a5be-fca90e38aaf5)"}, []string{"Steve"}, true},
		{"empty", []string{"[12:00:00] [Server thread/INFO]: There are 0 of a max of 20 players online: "}, nil, true},
		{"paper next line", []string{"[12:00:00 INFO]: There are 2/20 players online:", "[12:00:00 INFO]: Steve, Alex"}, []string{"Steve", "Alex"}, true},
		{"paper same line", []string{"[12:00:00 INFO]: There are 1/20 players online: Steve"}, []string{"Steve"}, true},
		{"group prefix", []string{"[12:00:00 INFO]: There are 1/20 players online:", "[12:00:00 INFO]: default: Steve"}, []string{"Steve"}, true},
		{"noise first", []string{"[12:00:00 INFO]: <Steve> hi", "[12:00:00 INFO]: There are 1/20 players online: Steve"}, []string{"Steve"}, true},
		{"header only", []string{"[12:00:00 INFO]: There are 3/20 players online:"}, nil, false},
		{"unrelated", []string{"[12:00:00 INFO]: Done"}, nil, false},
	}
	for _, c := range cases {
		var l MinecraftList
		done := false
		for _, line := range c.lines {
			done = l.Feed(line)
		}
		if done != c.done || strings.Join(l.Names, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: done=%v names=%v", c.name, done, l.Names)
		}
	}
}

func TestCleanArg(t *testing.T) {
	got := CleanArg("he said \"hi\"\r\nkick \\ all; now\x00", 100)
	if got != "he said 'hi'kick / all, now" {
		t.Errorf("%q", got)
	}
	if got := CleanArg(strings.Repeat("é", 50), 10); len([]rune(got)) != 10 {
		t.Errorf("%q", got)
	}
}

// rconServer is a Rust WebRCON that answers commands with what handle
// returns, on the password in the path.
func rconServer(t *testing.T, password string, handle func(cmd string) string) (hostport string, seen *[]string) {
	t.Helper()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+password {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var req struct {
				Identifier int
				Message    string
			}
			json.Unmarshal(data, &req)
			got = append(got, req.Message)
			// A broadcast comes first, as Rust's log lines do.
			b, _ := json.Marshal(map[string]any{"Identifier": 0, "Message": "noise"})
			c.Write(ctx, websocket.MessageText, b)
			reply := handle(req.Message)
			if reply == "\x00silence" {
				continue
			}
			b, _ = json.Marshal(map[string]any{"Identifier": req.Identifier, "Message": reply})
			c.Write(ctx, websocket.MessageText, b)
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), &got
}

func TestRCON(t *testing.T) {
	list := `[{"SteamID":"76561198000000001","DisplayName":"Ann","Ping":30,"Address":"1.2.3.4:5555","ConnectedSeconds":120.5},{"SteamID":76561198000000002,"DisplayName":"Bob","Ping":0,"Address":"","ConnectedSeconds":3}]`
	addr, seen := rconServer(t, "p@ss word", func(cmd string) string {
		if cmd == "playerlist" {
			return list
		}
		return "ok"
	})
	ctx := context.Background()
	c, err := DialRCON(ctx, addr, "p@ss word")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, err := c.Run(ctx, "playerlist")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ParseRustPlayerList(out)
	if err != nil || len(rows) != 2 || rows[1].ID != "76561198000000002" || rows[0].ConnectedSeconds != 120 {
		t.Fatalf("%+v %v", rows, err)
	}
	if out, _ := c.Run(ctx, `kick "1" "x"`); out != "ok" || (*seen)[1] != `kick "1" "x"` {
		t.Errorf("%q %v", out, *seen)
	}
}

func TestRCONErrorsHideThePassword(t *testing.T) {
	addr, _ := rconServer(t, "right", func(string) string { return "" })
	_, err := DialRCON(context.Background(), addr, "wrongpassword")
	if err == nil || strings.Contains(err.Error(), "wrongpassword") || strings.Contains(err.Error(), addr+"/") {
		t.Errorf("error %v", err)
	}
	if _, err := DialRCON(context.Background(), "127.0.0.1:1", "secretpw"); err == nil || strings.Contains(err.Error(), "secretpw") {
		t.Errorf("error %v", err)
	}
}

func TestRCONDoesNotFollowARedirect(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/admin", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	_, err := DialRCON(context.Background(), strings.TrimPrefix(redirector.URL, "http://"), "pw")
	if !errors.Is(err, ErrRCON) {
		t.Errorf("error %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the dial followed a redirect to another server (%d requests)", n)
	}
}

func TestRCONTimeout(t *testing.T) {
	addr, _ := rconServer(t, "p", func(string) string { return "\x00silence" })
	c, err := DialRCON(context.Background(), addr, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := c.Run(ctx, "playerlist"); err == nil {
		t.Error("no error")
	}
}

func steamFake(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "0123456789abcdef0123456789abcdef" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		calls.Add(1)
		ids := strings.Split(r.URL.Query().Get("steamids"), ",")
		if len(ids) > 100 {
			http.Error(w, "too many", http.StatusBadRequest)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "GetPlayerSummaries"):
			var ps []map[string]any
			for _, id := range ids {
				if id == "76561198000000009" {
					continue
				}
				ps = append(ps, map[string]any{"steamid": id, "avatarfull": "https://a/" + id, "profileurl": "https://p/" + id, "timecreated": 1500000000})
			}
			json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"players": ps}})
		case strings.Contains(r.URL.Path, "GetPlayerBans"):
			var ps []map[string]any
			for _, id := range ids {
				ps = append(ps, map[string]any{"SteamId": id, "NumberOfVACBans": 1, "DaysSinceLastBan": 40, "CommunityBanned": true})
			}
			json.NewEncoder(w).Encode(map[string]any{"players": ps})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSteamLookupBatchesAndCaches(t *testing.T) {
	var calls atomic.Int32
	srv := steamFake(t, &calls)
	now := time.Unix(1_800_000_000, 0)
	s := &Steam{BaseURL: srv.URL, Now: func() time.Time { return now }}
	key := "0123456789abcdef0123456789abcdef"

	var ids []string
	for i := range 150 {
		ids = append(ids, fmt.Sprintf("76561198%09d", i+100))
	}
	ids = append(ids, "name:Steve", "76561198000000009")
	got := s.Lookup(context.Background(), key, ids)
	if len(got) != 151 || calls.Load() != 4 { // two batches, two calls each
		t.Fatalf("%d results, %d calls", len(got), calls.Load())
	}
	info := got[ids[0]]
	if info.VACBans != 1 || !info.CommunityBanned || info.DaysSinceLastBan == nil || *info.DaysSinceLastBan != 40 || info.AccountCreated == nil {
		t.Errorf("%+v", info)
	}
	s.Lookup(context.Background(), key, ids)
	if calls.Load() != 4 {
		t.Errorf("cache missed: %d calls", calls.Load())
	}
	now = now.Add(25 * time.Hour)
	s.Lookup(context.Background(), key, ids[:3])
	if calls.Load() != 6 {
		t.Errorf("expired entries not asked again: %d calls", calls.Load())
	}
}

func TestSteamFailureIsCachedBriefly(t *testing.T) {
	var calls atomic.Int32
	srv := steamFake(t, &calls)
	now := time.Unix(1_800_000_000, 0)
	s := &Steam{BaseURL: srv.URL, Now: func() time.Time { return now }}
	id := []string{"76561198000000001"}
	if got := s.Lookup(context.Background(), "ffffffffffffffffffffffffffffffff", id); len(got) != 0 {
		t.Fatal(got)
	}
	// The wrong key was refused before the server counted it; a right one
	// is not asked for until the failure expires.
	if got := s.Lookup(context.Background(), "0123456789abcdef0123456789abcdef", id); len(got) != 0 || calls.Load() != 0 {
		t.Fatalf("failure not cached: %v %d", got, calls.Load())
	}
	now = now.Add(16 * time.Minute)
	if got := s.Lookup(context.Background(), "0123456789abcdef0123456789abcdef", id); len(got) != 1 {
		t.Fatal("not asked after the failure expired")
	}
}

func TestSteamCheck(t *testing.T) {
	var calls atomic.Int32
	srv := steamFake(t, &calls)
	s := &Steam{BaseURL: srv.URL}
	if err := s.Check(context.Background(), "0123456789abcdef0123456789abcdef"); err != nil {
		t.Error(err)
	}
	if err := s.Check(context.Background(), "ffffffffffffffffffffffffffffffff"); err != ErrSteamKeyRejected {
		t.Error(err)
	}
	srv.Close()
	err := s.Check(context.Background(), "0123456789abcdef0123456789abcdef")
	if err == nil || strings.Contains(err.Error(), "0123456789abcdef") {
		t.Errorf("error %v", err)
	}
}

func TestSteamCacheIsBounded(t *testing.T) {
	s := &Steam{cache: map[string]steamEntry{}}
	exp := time.Now().Add(time.Hour)
	for i := range steamCacheSize {
		s.cache[string(rune('a'+i%26))+string(rune(i))] = steamEntry{expires: exp}
	}
	s.trim()
	if len(s.cache) >= steamCacheSize {
		t.Errorf("%d entries", len(s.cache))
	}
}
