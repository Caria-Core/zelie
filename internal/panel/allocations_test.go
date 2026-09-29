package panel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/store"
)

func TestSuggestRange(t *testing.T) {
	pool := func(ports ...int) []store.Allocation {
		var out []store.Allocation
		for _, p := range ports {
			out = append(out, store.Allocation{Port: p})
		}
		return out
	}
	cases := []struct {
		name string
		used []int
		pool []store.Allocation
		size int
		want int
		ok   bool
	}{
		{"empty machine", nil, nil, 100, 25565, true},
		// Wings and the like sit far from the Minecraft port.
		{"unrelated ports", []int{22, 2022, 8080, 3306}, nil, 100, 25565, true},
		{"a port inside the block", []int{25600}, nil, 100, 25601, true},
		{"the first port of the block", []int{25565}, nil, 100, 25566, true},
		{"just after the block", []int{25665}, nil, 100, 25565, true},
		{"two obstacles", []int{25580, 25650}, nil, 100, 25651, true},
		{"the pool counts", nil, pool(25565, 25566), 100, 25567, true},
		{"the pool and a listener", []int{25700}, pool(25600), 100, 25701, true},
		{"not enough room", []int{30000}, nil, 7200, 0, false},
		{"no room at all", []int{32700}, pool(25565, 26000), 7000, 0, false},
	}
	for _, c := range cases {
		got, ok := suggestRange(c.used, c.pool, c.size)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: %d, %v; want %d, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestParsePorts(t *testing.T) {
	good := map[string][]int{
		"25565":             {25565},
		"25565-25567":       {25565, 25566, 25567},
		"25565-25566,27015": {25565, 25566, 27015},
		" 1024 , 2000-2001": {1024, 2000, 2001},
	}
	for in, want := range good {
		got, err := parsePorts(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%q: %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "abc", "1023", "70000", "25566-25565", "25565-", "-25565", "25565,,25566", "25565-25566-25567", "1024-7000"} {
		if got, err := parsePorts(in); err == nil {
			t.Errorf("%q gave %v", in, got)
		}
	}
}

func TestAllocationsAPI(t *testing.T) {
	e := newAppEnv(t)
	base := "/api/nodes/1/allocations"

	if code, out := e.b.do("GET", "/api/nodes/1/allocations", nil); code != http.StatusOK || out != nil {
		t.Fatalf("the pool starts empty: %d %v", code, out)
	}
	if code, out := e.b.do("GET", "/api/nodes/7/allocations", nil); code != http.StatusNotFound || out["code"] != "allocation.no_node" {
		t.Errorf("another node: %d %v", code, out)
	}
	if code, _ := e.b.do("GET", "/api/nodes/x/allocations", nil); code != http.StatusNotFound {
		t.Errorf("a node that is not a number: %d", code)
	}

	// Something else holds the start of the range, and a listener of
	// another panel sits elsewhere.
	e.core.usedPorts = []int{25570, 8080, 2022}
	code, out := e.b.do("GET", base+"/suggest", nil)
	if code != http.StatusOK || out["first"] != 25571.0 || out["last"] != 25670.0 || out["ports"] != "25571-25670" || out["ip"] != "0.0.0.0" {
		t.Fatalf("suggest: %d %v", code, out)
	}

	if code, out := e.b.do("POST", base, map[string]any{"ip": "0.0.0.0", "ports": "25571-25573,27015"}); code != http.StatusCreated || out["added"] != 4.0 {
		t.Fatalf("add: %d %v", code, out)
	}
	rec := e.b.record("GET", base, nil)
	if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"port":25571`, `"port":27015`, `"ip":"0.0.0.0"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	// The pool is left out of the next suggestion.
	if _, out := e.b.do("GET", base+"/suggest", nil); out["first"] != 25574.0 {
		t.Errorf("suggest after adding: %v", out)
	}

	for name, c := range map[string]struct {
		body map[string]any
		code int
		err  string
	}{
		"overlap":   {map[string]any{"ports": "25573-25580"}, http.StatusConflict, "allocation.taken"},
		"bad ports": {map[string]any{"ports": "22"}, http.StatusBadRequest, "allocation.bad_ports"},
		"bad ip":    {map[string]any{"ip": "::1", "ports": "30000"}, http.StatusBadRequest, "allocation.bad_ip"},
		"too many":  {map[string]any{"ports": "1024-7000"}, http.StatusBadRequest, "allocation.too_many"},
		"unknown":   {map[string]any{"ports": "30000", "app": "x"}, http.StatusBadRequest, "server.bad_request"},
	} {
		if code, out := e.b.do("POST", base, c.body); code != c.code || out["code"] != c.err {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if code, out := e.b.do("POST", base, map[string]any{"ports": "25573-25580"}); code != http.StatusConflict || out["params"].(map[string]any)["port"] != 25573.0 {
		t.Errorf("the clashing port is named: %d %v", code, out)
	}
	if _, err := e.s.Store.Allocations(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.s.Store.Allocations(context.Background(), 1); len(list) != 4 {
		t.Errorf("a refused request added ports: %d in the pool", len(list))
	}

	// A port a server uses cannot be deleted; a free one can.
	list, _ := e.s.Store.Allocations(context.Background(), 1)
	if err := e.s.Store.CreateApp(context.Background(), store.App{ID: "mc", Source: store.SourceImage, Image: "x", Port: 25565, MemoryMB: 512, CPUs: 1, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Store.AssignAllocations(context.Background(), "mc", []int64{list[0].ID}); err != nil {
		t.Fatal(err)
	}
	del := func(id int64) (int, map[string]any) {
		return e.b.do("DELETE", base+"/"+strconv.FormatInt(id, 10), nil)
	}
	if code, out := del(list[0].ID); code != http.StatusConflict || out["code"] != "allocation.in_use" {
		t.Errorf("delete a used port: %d %v", code, out)
	}
	if code, _ := del(list[1].ID); code != http.StatusNoContent {
		t.Errorf("delete a free port: %d", code)
	}
	if code, out := del(list[1].ID); code != http.StatusNotFound || out["code"] != "allocation.not_found" {
		t.Errorf("delete it twice: %d %v", code, out)
	}
	if code, _ := e.b.do("DELETE", base+"/abc", nil); code != http.StatusNotFound {
		t.Errorf("delete a port that is not a number: %d", code)
	}
}

func TestAllocationsNeedLogin(t *testing.T) {
	e := newAppEnv(t)
	out := &browser{t: t, h: e.b.h, ip: "198.51.100.8"}
	for _, r := range [][2]string{
		{"GET", "/api/nodes/1/allocations"},
		{"GET", "/api/nodes/1/allocations/suggest"},
		{"POST", "/api/nodes/1/allocations"},
		{"GET", "/api/nodes/1"},
		{"PUT", "/api/nodes/1"},
		{"DELETE", "/api/nodes/1/allocations/1"},
	} {
		if code, _ := out.do(r[0], r[1], nil); code != http.StatusUnauthorized {
			t.Errorf("%s %s without logging in: %d", r[0], r[1], code)
		}
	}
}

func TestRequireAdmin(t *testing.T) {
	called := 0
	h := requireAdmin(func(w http.ResponseWriter, r *http.Request) { called++ })
	for _, admin := range []bool{false, true} {
		var l login
		l.account.Admin = admin
		req := httptest.NewRequest("GET", "/", nil)
		req = req.WithContext(context.WithValue(req.Context(), loginKey{}, l))
		rec := httptest.NewRecorder()
		h(rec, req)
		if admin && (called != 1 || rec.Code != http.StatusOK) {
			t.Errorf("an administrator: called %d, status %d", called, rec.Code)
		}
		if !admin && (called != 0 || rec.Code != http.StatusForbidden) {
			t.Errorf("someone else: called %d, status %d", called, rec.Code)
		}
	}
}

func TestPlace(t *testing.T) {
	e := newAppEnv(t)
	ctx := context.Background()
	if err := e.s.Store.AddAllocations(ctx, store.ThisNode, store.AnyAddress, []int{25566, 25565, 25567}, e.s.now()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := e.s.Store.CreateApp(ctx, store.App{ID: id, Source: store.SourceImage, Image: "x", Port: 1, MemoryMB: 512, CPUs: 1, CreatedAt: e.s.now()}); err != nil {
			t.Fatal(err)
		}
	}

	p, err := e.s.place(ctx, store.ThisNode, needs{Ports: 2})
	if err != nil || p.Node != store.ThisNode || len(p.Allocations) != 2 || p.Allocations[0].Port != 25565 || p.Allocations[1].Port != 25566 {
		t.Fatalf("place = %+v, %v", p, err)
	}
	if err := e.s.assign(ctx, "a", p); err != nil {
		t.Fatal(err)
	}
	// The first server's ports are not offered again.
	p2, err := e.s.place(ctx, store.ThisNode, needs{Ports: 1})
	if err != nil || p2.Allocations[0].Port != 25567 {
		t.Fatalf("second place = %+v, %v", p2, err)
	}
	// Two requests that picked the same port: the later one loses.
	if err := e.s.assign(ctx, "b", p); !errors.Is(err, store.ErrInUse) {
		t.Errorf("assigning taken ports: %v", err)
	}
	if _, err := e.s.place(ctx, store.ThisNode, needs{Ports: 2}); err == nil {
		t.Error("placed two ports when one is left")
	}
	if _, err := e.s.place(ctx, store.ThisNode, needs{Ports: 1}); err != nil {
		t.Errorf("the last port: %v", err)
	}
}

func TestDeleteAppClosesPorts(t *testing.T) {
	e := newAppEnv(t)
	ctx := context.Background()
	e.b.do("POST", "/api/apps", map[string]any{"id": "mc", "source": "image", "image": "nginx"})
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "mc")
	e.settle(t, "web")
	e.s.Store.AddAllocations(ctx, store.ThisNode, store.AnyAddress, []int{25565, 25566}, e.s.now())
	list, _ := e.s.Store.Allocations(ctx, store.ThisNode)
	if err := e.s.Store.AssignAllocations(ctx, "mc", []int64{list[0].ID}); err != nil {
		t.Fatal(err)
	}

	if code, _ := e.b.do("DELETE", "/api/apps/web", nil); code != http.StatusNoContent {
		t.Fatalf("delete web: %d", code)
	}
	if code, _ := e.b.do("DELETE", "/api/apps/mc", nil); code != http.StatusNoContent {
		t.Fatalf("delete mc: %d", code)
	}
	e.core.mu.Lock()
	cleared := slices.Clone(e.core.cleared)
	e.core.mu.Unlock()
	if !slices.Equal(cleared, []string{"mc"}) {
		t.Errorf("core was told to clear %v: only the app with ports needs it", cleared)
	}
	list, _ = e.s.Store.Allocations(ctx, store.ThisNode)
	if len(list) != 2 || list[0].AppID != "" {
		t.Errorf("the pool after the app went: %+v", list)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func TestValidAddress(t *testing.T) {
	for _, s := range []string{"play.example.com", "example", "a-b.example.org", "203.0.113.7", "2001:db8::1", "Play.Example.COM.", strings.Repeat("a", 63) + ".com"} {
		if !validAddress(s) {
			t.Errorf("%q refused", s)
		}
	}
	for _, s := range []string{"", "-a.com", "a-.com", "a..com", "a b.com", "http://x.com", "x.com:25565", "under_score.com", "fe80::1%eth0", strings.Repeat("a", 64) + ".com", strings.Repeat("a.", 130) + "com", "é.com"} {
		if validAddress(s) {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestNodeAddressAPI(t *testing.T) {
	e := newAppEnv(t)
	e.core.address = core.PublicAddress{Address: "192.168.1.20", Private: true}

	code, out := e.b.do("GET", "/api/nodes/1", nil)
	if code != http.StatusOK || out["address"] != "192.168.1.20" || out["detected"] != "192.168.1.20" || out["private"] != true || out["override"] != "" || out["name"] != "This server" {
		t.Fatalf("detected: %d %v", code, out)
	}
	if code, out := e.b.do("GET", "/api/nodes/7", nil); code != http.StatusNotFound || out["code"] != "allocation.no_node" {
		t.Errorf("another node: %d %v", code, out)
	}

	code, out = e.b.do("PUT", "/api/nodes/1", map[string]any{"public_address": " play.example.com "})
	if code != http.StatusOK || out["address"] != "play.example.com" || out["override"] != "play.example.com" || out["private"] != false || out["detected"] != "192.168.1.20" {
		t.Fatalf("override: %d %v", code, out)
	}
	if code, out := e.b.do("GET", "/api/nodes/1", nil); code != http.StatusOK || out["address"] != "play.example.com" {
		t.Errorf("override kept: %d %v", code, out)
	}
	if code, out := e.b.do("PUT", "/api/nodes/1", map[string]any{"public_address": "not a name"}); code != http.StatusBadRequest || out["code"] != "node.bad_address" {
		t.Errorf("bad address: %d %v", code, out)
	}
	if n, _ := e.s.Store.Node(context.Background(), 1); n.PublicAddress != "play.example.com" {
		t.Errorf("a refused address changed the node: %q", n.PublicAddress)
	}
	code, out = e.b.do("PUT", "/api/nodes/1", map[string]any{"public_address": ""})
	if code != http.StatusOK || out["address"] != "192.168.1.20" || out["private"] != true || out["override"] != "" {
		t.Errorf("cleared: %d %v", code, out)
	}
}
