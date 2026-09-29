package core

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/peer"
)

func TestForwards(t *testing.T) {
	s, f := newServer()
	root := &peer.Peer{UID: 0}
	set := func(app, body string) *httptest.ResponseRecorder {
		return request(t, s, root, "PUT", "/v1/forwards/"+app, body)
	}

	if rec := set("mc", `{"forwards":[{"port":25565,"proto":"tcp","target":25565},{"port":25565,"proto":"udp","target":25565},{"ip":"192.0.2.1","port":30000,"proto":"tcp","target":1}]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("set: %d %s", rec.Code, rec.Body)
	}
	got := f.forwards["mc"]
	if len(got) != 3 || got[2].IP != netip.MustParseAddr("192.0.2.1") || got[0].IP.IsValid() {
		t.Fatalf("forwards %+v", got)
	}

	// Another app cannot take a port that is open to this one.
	rec := set("other", `{"forwards":[{"port":25565,"proto":"tcp","target":1}]}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"forward.port_taken"`) || !strings.Contains(rec.Body.String(), "25565") {
		t.Errorf("taken port: %d %s", rec.Code, rec.Body)
	}

	for name, body := range map[string]string{
		"a low port":   `{"forwards":[{"port":22,"proto":"tcp","target":22}]}`,
		"a protocol":   `{"forwards":[{"port":25570,"proto":"icmp","target":1}]}`,
		"a bad ip":     `{"forwards":[{"ip":"nope","port":25570,"proto":"tcp","target":1}]}`,
		"unknown key":  `{"forwards":[],"privileged":true}`,
		"a port twice": `{"forwards":[{"port":25570,"proto":"tcp","target":1},{"port":25570,"proto":"tcp","target":2}]}`,
	} {
		if rec := set("mc", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := set("BAD", `{"forwards":[]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad app: %d", rec.Code)
	}
	if len(f.forwards["mc"]) != 3 {
		t.Errorf("a refused request changed the forwards: %+v", f.forwards["mc"])
	}

	if rec := request(t, s, root, "DELETE", "/v1/forwards/mc", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body)
	}
	if len(f.forwards) != 0 {
		t.Errorf("after clearing: %+v", f.forwards)
	}
	if rec := set("other", `{"forwards":[{"port":25565,"proto":"tcp","target":1}]}`); rec.Code != http.StatusNoContent {
		t.Errorf("the port is free again: %d", rec.Code)
	}
}
