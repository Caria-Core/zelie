package players

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func feed(p Parser, lines ...string) []Event {
	var out []Event
	for _, l := range lines {
		out = append(out, p.Feed(l)...)
	}
	return out
}

func TestParserFor(t *testing.T) {
	for _, id := range []string{"rust", "minecraft-vanilla", "minecraft-paper", "minecraft-purpur", "minecraft-fabric", "minecraft-forge", "minecraft-neoforge"} {
		if ParserFor(id) == nil {
			t.Errorf("no parser for %s", id)
		}
	}
	for _, id := range []string{"minecraft-bedrock", "minecraft-velocity", "valheim", "", "nodejs"} {
		if ParserFor(id) != nil {
			t.Errorf("unexpected parser for %q", id)
		}
	}
}

// The lines are what a live server printed on 2026-10-04.
const measuredRust = `85.29.33.106:59621/76561198315139556/vetmon has auth level 2
85.29.33.106:59621/76561198315139556/vetmon joined [windows/76561198315139556]
vetmon[76561198315139556] has spawned
[Better Chat] [Global] vetmon: a
{
  "Channel": 0,
  "Message": "vetmon: a",
  "UserId": "76561198315139556",
  "Username": "vetmon",
  "Color": "#55aaff",
  "Time": 1791111707
}
[Better Chat] [Team] vetmon: team
{
  "Channel": 1,
  "Message": "vetmon: team",
  "UserId": "76561198315139556",
  "Username": "vetmon",
  "Color": "#55aaff",
  "Time": 1791111712
}
{
  "id": "76561198315139556",
  "ad": "vetmon",
  "takim": 0,
  "ip": "85.29.33.106",
  "caria": 1,
  "tur": "giris",
  "z": 1791111634870
}`

func TestRustMeasured(t *testing.T) {
	got := feed(NewRust(), strings.Split(measuredRust, "\n")...)
	want := []Event{
		{Kind: Join, PlayerID: "76561198315139556", Name: "vetmon", IP: "85.29.33.106"},
		{Kind: Chat, PlayerID: "76561198315139556", Name: "vetmon", Channel: "global", Text: "a", At: time.Unix(1791111707, 0)},
		{Kind: Chat, PlayerID: "76561198315139556", Name: "vetmon", Channel: "team", Text: "team", At: time.Unix(1791111712, 0)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestRustLines(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []Event
	}{
		{"join with spaces and slashes", "1.2.3.4:5000/76561198000000001/Big Bob/the 2nd joined [linux/76561198000000001]",
			[]Event{{Kind: Join, PlayerID: "76561198000000001", Name: "Big Bob/the 2nd", IP: "1.2.3.4"}}},
		{"join ipv6", "[2001:db8::1]:5000/76561198000000001/x joined [windows/76561198000000001]",
			[]Event{{Kind: Join, PlayerID: "76561198000000001", Name: "x", IP: "2001:db8::1"}}},
		{"leave with address", "1.2.3.4:5000/76561198000000001/Bob disconnecting: disconnect",
			[]Event{{Kind: Leave, PlayerID: "76561198000000001", Name: "Bob", IP: "1.2.3.4", Reason: "disconnect"}}},
		{"leave with id", "Bob[76561198000000001] disconnecting: Kicked: cheating",
			[]Event{{Kind: Leave, PlayerID: "76561198000000001", Name: "Bob", Reason: "Kicked: cheating"}}},
		{"auth level is ignored", "1.2.3.4:5000/76561198000000001/Bob has auth level 1", nil},
		{"chat prefix cannot forge a leave", "[CHAT] 1.2.3.4:5/76561198000000001/x disconnecting: y", nil},
		{"better chat is ignored", "[Better Chat] [Global] Bob: hi", nil},
		{"broken join is a near miss", "someone joined [windows]",
			[]Event{{Kind: NearMiss, Reason: "join", Text: "someone joined [windows]"}}},
		{"broken leave is a near miss", "Bob disconnecting",
			[]Event{{Kind: NearMiss, Reason: "leave", Text: "Bob disconnecting"}}},
		{"report line is a near miss", "Report: 76561198000000001 reported 76561198000000002",
			[]Event{{Kind: NearMiss, Reason: "report", Text: "Report: 76561198000000001 reported 76561198000000002"}}},
		{"plain line", "Saving complete", nil},
	}
	for _, tc := range tests {
		got := NewRust().Feed(tc.line)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v want %+v", tc.name, got, tc.want)
		}
	}
}

func TestRustChatStripsNameOnce(t *testing.T) {
	block := []string{`{`, `  "Channel": 0,`, `  "Message": "bob: bob: hi",`, `  "UserId": "76561198000000001",`, `  "Username": "bob"`, `}`}
	got := feed(NewRust(), block...)
	if len(got) != 1 || got[0].Text != "bob: hi" || !got[0].At.IsZero() {
		t.Fatalf("got %+v", got)
	}
}

func TestRustOtherChannel(t *testing.T) {
	got := NewRust().Feed(`{"Channel": 2, "Message": "x", "UserId": "1", "Username": "a"}`)
	if len(got) != 1 || got[0].Channel != "2" || got[0].Text != "x" {
		t.Fatalf("got %+v", got)
	}
}

func TestRustReport(t *testing.T) {
	block := `{
  "PlayerId": "76561198000000001",
  "PlayerName": "Alice",
  "TargetId": "76561198000000002",
  "TargetName": "Bob",
  "Subject": "cheat",
  "Message": "aimbot",
  "Type": "report",
  "Time": 1791111800
}`
	got := feed(NewRust(), strings.Split(block, "\n")...)
	want := []Event{{Kind: Report, PlayerID: "76561198000000001", Name: "Alice", Target: "76561198000000002",
		TargetName: "Bob", Reason: "cheat", Text: "aimbot", At: time.Unix(1791111800, 0)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestRustReportShapeNearMiss(t *testing.T) {
	block := `{
  "kind": "report",
  "from": "76561198000000001",
  "about": "76561198000000002"
}`
	got := feed(NewRust(), strings.Split(block, "\n")...)
	if len(got) != 1 || got[0].Kind != NearMiss || got[0].Reason != "report" {
		t.Fatalf("got %+v", got)
	}
}

func TestRustMalformedBlock(t *testing.T) {
	r := NewRust()
	got := feed(r, "{", "  not json", "}", "1.2.3.4:1/76561198000000001/a joined [linux/76561198000000001]")
	if len(got) != 1 || got[0].Kind != Join {
		t.Fatalf("got %+v", got)
	}
}

func TestRustUnclosedBlockIsDropped(t *testing.T) {
	r := NewRust()
	r.Feed("{")
	for i := 0; i < maxBlockLines+1; i++ {
		r.Feed(fmt.Sprintf(`  "k%d": 1,`, i))
	}
	if r.open {
		t.Fatal("block still open after the line cap")
	}
	if got := r.Feed("1.2.3.4:1/76561198000000001/a joined [linux/76561198000000001]"); len(got) != 1 {
		t.Fatalf("line after the dropped block was lost: %+v", got)
	}
}

func TestRustOversizedBlock(t *testing.T) {
	r := NewRust()
	r.Feed("{")
	big := strings.Repeat("x", 4<<10)
	for i := 0; i < 5; i++ {
		r.Feed(`  "k": "` + big + `",`)
	}
	if r.open || r.size != 0 {
		t.Fatal("block still open after the byte cap")
	}
}

func TestRustNestedBraceDoesNotRestart(t *testing.T) {
	block := []string{`{`, `  "Channel": 0,`, `  "Extra": {`, `    "a": 1`, `  },`, `  "Message": "hi",`, `  "UserId": "76561198000000001",`, `  "Username": "bob"`, `}`}
	got := feed(NewRust(), block...)
	if len(got) != 1 || got[0].Text != "hi" {
		t.Fatalf("got %+v", got)
	}
}

func TestMinecraft(t *testing.T) {
	const uuid = "069a79f4-44e9-4726-a5be-fca90e38aaf5"
	for _, style := range []struct{ name, pre string }{
		{"vanilla", "[12:34:56] [Server thread/INFO]: "},
		{"paper", "[12:34:56 INFO]: "},
		{"forge", "[04Oct2026 12:34:56.789] [Server thread/INFO] [net.minecraft.server.dedicated.DedicatedServer/]: "},
	} {
		t.Run(style.name, func(t *testing.T) {
			p := NewMinecraft()
			got := feed(p,
				style.pre+"UUID of player Steve is "+uuid,
				"[12:34:56] [User Authenticator #1/INFO]: UUID of player Steve is "+uuid,
				style.pre+"Steve[/10.0.0.7:51234] logged in with entity id 123 at (1.5, 64.0, -2.5)",
				style.pre+"Steve joined the game",
				style.pre+"<Steve> hello there",
				style.pre+"[Not Secure] <Steve> unsigned",
				style.pre+"[Server] Alex joined the game",
				style.pre+"Steve lost connection: Disconnected",
				style.pre+"Steve left the game",
				style.pre+"Done (3.1s)! For help, type \"help\"",
			)
			want := []Event{
				{Kind: Join, PlayerID: uuid, Name: "Steve", IP: "10.0.0.7"},
				{Kind: Chat, PlayerID: uuid, Name: "Steve", Channel: "global", Text: "hello there"},
				{Kind: Chat, PlayerID: uuid, Name: "Steve", Channel: "global", Text: "unsigned"},
				{Kind: Leave, PlayerID: uuid, Name: "Steve"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestMinecraftWithoutUUID(t *testing.T) {
	got := feed(NewMinecraft(), "[12:00:00] [Server thread/INFO]: Alex joined the game", "[12:00:01 INFO]: <Alex> <3")
	if len(got) != 2 || got[0].PlayerID != "name:Alex" || got[1].Text != "<3" {
		t.Fatalf("got %+v", got)
	}
}

func TestMinecraftChatCannotForgeJoin(t *testing.T) {
	got := NewMinecraft().Feed("[12:00:00] [Server thread/INFO]: <Alex> Bob joined the game")
	if len(got) != 1 || got[0].Kind != Chat {
		t.Fatalf("got %+v", got)
	}
}

func TestMinecraftRememberIsBounded(t *testing.T) {
	m := NewMinecraft()
	for i := 0; i < 3*mcRemember; i++ {
		m.Feed(fmt.Sprintf("[12:00:00] [Server thread/INFO]: UUID of player p%d is 069a79f4-44e9-4726-a5be-fca90e38aaf5", i))
	}
	if len(m.uuids) > mcRemember {
		t.Fatalf("remembers %d names", len(m.uuids))
	}
}

func TestParsersRefuseIdsNoGameWouldPrint(t *testing.T) {
	long := strings.Repeat("x", 8000)
	for name, got := range map[string][]Event{
		"minecraft name": NewMinecraft().Feed("[12:00:00] [Server thread/INFO]: " + long + " joined the game"),
		"minecraft chat": NewMinecraft().Feed("[12:00:00] [Server thread/INFO]: <" + long + "> hi"),
		"rust chat":      NewRust().Feed(`{"Channel":0,"Message":"hi","UserId":"` + long + `","Username":"bob"}`),
		"rust report":    NewRust().Feed(`{"PlayerId":"76561198000000001","TargetId":"` + long + `","Message":"he flies"}`),
	} {
		if len(got) != 1 || got[0].Kind != NearMiss || got[0].PlayerID != "" || got[0].Target != "" || got[0].Reason != "id too long" {
			t.Errorf("%s: got %+v", name, got)
		}
	}

	// The ids of real players pass.
	got := NewRust().Feed(`{"Channel":0,"Message":"hi","UserId":"76561198000000001","Username":"bob"}`)
	if len(got) != 1 || got[0].Kind != Chat {
		t.Errorf("rust chat: %+v", got)
	}
	got = NewMinecraft().Feed("[12:00:00] [Server thread/INFO]: Alex_01 joined the game")
	if len(got) != 1 || got[0].Kind != Join || got[0].PlayerID != "name:Alex_01" {
		t.Errorf("minecraft join: %+v", got)
	}
}

func TestAddressTooLongToBeOneIsDropped(t *testing.T) {
	long := strings.Repeat("9", 8000)
	p := NewMinecraft()
	got := feed(p,
		"[12:00:00] [Server thread/INFO]: Steve[/"+long+"] logged in with entity id 4 at (0, 0, 0)",
		"[12:00:00] [Server thread/INFO]: Steve joined the game")
	if len(got) != 1 || got[0].Kind != Join || got[0].IP != "" {
		t.Errorf("minecraft: %+v", got)
	}
	got = NewRust().Feed(long + "/76561198000000001/Bob joined [windows/76561198000000001]")
	if len(got) != 1 || got[0].Kind != Join || got[0].IP != "" {
		t.Errorf("rust: %+v", got)
	}
	if got := stripPort("10.0.0.7:5555"); got != "10.0.0.7" {
		t.Errorf("stripPort: %q", got)
	}
}

func TestPlayerLines(t *testing.T) {
	marked := func(catalogID string, lines ...string) []int {
		var out []int
		for i, mine := range PlayerLines(catalogID, lines) {
			if mine {
				out = append(out, i)
			}
		}
		return out
	}
	mc := marked("minecraft-paper",
		"[12:00:00 INFO]: Steve joined the game",
		"[12:00:01 INFO]: <Steve> Unable to access jarfile",
		"[12:00:02 INFO]: [Not Secure] <Steve> Address already in use",
		"[12:00:03 INFO]: Steve issued server command: /class file version 70",
		"[12:00:04 INFO]: Done (3.1s)! For help, type \"help\"",
		"Unable to access jarfile server.jar",
		"[12:00:05 INFO]: * Steve Unable to access jarfile",
		"[12:00:06 INFO]: [Not Secure] * Steve Address already in use",
		"[12:00:07 INFO]: Named entity Wolf['Unable to access jarfile'/12, l='ServerLevel[world]', x=1.00] died: Wolf was slain",
		"[12:00:08 INFO]: UUID of player Steve is 069a79f4-44e9-4726-a5be-fca90e38aaf5",
		"[12:00:09 INFO]: Steve[/10.0.0.7:5555] logged in with entity id 4 at (0.0, 64.0, 0.0)",
		"[12:00:10 INFO]: Steve left the game")
	if !reflect.DeepEqual(mc, []int{0, 1, 2, 3, 6, 7, 8, 9, 10, 11}) {
		t.Errorf("minecraft: %v", mc)
	}

	rust := marked("rust",
		"[CHAT] Bob: Address already in use",
		"Bob/76561198000000001/Bob joined [windows/76561198000000001]",
		"{",
		`  "Channel": 0,`,
		`  "Message": "Address already in use",`,
		`  "UserId": "76561198000000001",`,
		`  "Username": "Bob"`,
		"}",
		"Address already in use",
		`{"Channel":1,"Message":"hi","UserId":"76561198000000001","Username":"Bob"}`,
		"[Better Chat] Bob: hi",
		// The name is the player's own, and a Steam name can be any text.
		"1.2.3.4:5/76561198000000002/Address already in use joined [windows/76561198000000002]",
		"1.2.3.4:5/76561198000000002/Unable to access jarfile disconnecting: Disconnected",
		"Address already in use[76561198000000002] disconnecting: Kicked",
		"Server startup complete")
	if !reflect.DeepEqual(rust, []int{0, 1, 2, 3, 4, 5, 6, 7, 9, 10, 11, 12, 13}) {
		t.Errorf("rust: %v", rust)
	}

	if got := marked("valheim", "<Steve> hi"); len(got) != 0 {
		t.Errorf("a game without a parser: %v", got)
	}
}
