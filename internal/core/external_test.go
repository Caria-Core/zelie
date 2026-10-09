package core

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/Caria-Core/zelie/internal/secret"
)

const extPassword = "Abc123Abc123Abc123Abc123Abc123Ab"

func externalServer(t *testing.T) (*Server, *fakeEngine, string) {
	s, f := newServer()
	keys, err := secret.LoadOrCreate(filepath.Join(t.TempDir(), "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	s.Secrets = keys
	path := filepath.Join(t.TempDir(), "external.json")
	x, list, err := LoadExternal(path)
	if err != nil {
		t.Fatal(err)
	}
	s.External = x
	s.StartExternal(list)
	t.Cleanup(func() { s.External.Sync(nil) })
	sealed, _ := secret.Seal(keys.Public(), "db", "ZELIE_EXTERNAL_PASSWORD", extPassword)
	return s, f, sealed
}

// freePort is a loopback port nothing listens on.
func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// echo stands in for a database: it answers each line with itself.
func echo(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func roundTrip(port int) error {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintln(c, "ping")
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if line != "ping\n" {
		return fmt.Errorf("got %q", line)
	}
	return nil
}

func TestExternalForwards(t *testing.T) {
	s, f, _ := externalServer(t)
	panel := &peer.Peer{UID: 999}
	target := echo(t)
	f.containers = []engine.Status{{ID: "db-1", App: "db", State: "running", IP: netip.MustParseAddr("127.0.0.1")}}
	port := freePort(t)

	body := fmt.Sprintf(`{"engine":"postgres","container":"db-1","port":%d,"target":%d}`, port, target)
	if rec := request(t, s, panel, "PUT", "/v1/external/db", body); rec.Code != http.StatusNoContent {
		t.Fatalf("set: %d %s", rec.Code, rec.Body)
	}
	if err := roundTrip(port); err != nil {
		t.Fatalf("through the port: %v", err)
	}
	saved, _ := os.ReadFile(s.External.path)
	if !strings.Contains(string(saved), fmt.Sprintf(`"port":%d`, port)) {
		t.Errorf("saved %s", saved)
	}

	// A core that starts again opens the saved port.
	x, list, err := LoadExternal(s.External.path)
	if err != nil || len(list) != 1 || list[0].Port != port {
		t.Fatalf("loaded %+v, %v", list, err)
	}
	s.External.Sync(nil)
	if err := roundTrip(port); err == nil {
		t.Fatal("the port is still open")
	}
	s.External = x
	s.StartExternal(list)
	if err := roundTrip(port); err != nil {
		t.Fatalf("after a restart: %v", err)
	}

	// A stopped database: the port answers, the connection goes nowhere.
	setState := func(state string) {
		f.listMu.Lock()
		f.containers[0].State = state
		f.listMu.Unlock()
	}
	setState("stopped")
	if err := roundTrip(port); err == nil {
		t.Error("reached a stopped database")
	}
	setState("running")

	if rec := request(t, s, panel, "DELETE", "/v1/external/db", `{"engine":"postgres"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if err := roundTrip(port); err == nil {
		t.Error("the port is open after removing it")
	}
}

func TestExternalPortTaken(t *testing.T) {
	s, _, _ := externalServer(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	body := fmt.Sprintf(`{"engine":"postgres","port":%d,"target":5432}`, port)
	rec := request(t, s, &peer.Peer{UID: 999}, "PUT", "/v1/external/db", body)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "external.port_taken") {
		t.Errorf("taken port: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{`{"port":80,"target":5432}`, `{"port":20000,"target":0}`} {
		if rec := request(t, s, &peer.Peer{UID: 999}, "PUT", "/v1/external/db", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
}

// The panel sets the ports again when it starts. One that something else
// holds by then is an error for the panel, not a success.
func TestExternalSyncReportsAPortThatIsTaken(t *testing.T) {
	s, _, _ := externalServer(t)
	panel := &peer.Peer{UID: 999}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	taken := l.Addr().(*net.TCPAddr).Port
	free := freePort(t)

	body := fmt.Sprintf(`{"listeners":[{"app":"db","port":%d,"target":5432},{"app":"cache","port":%d,"target":6379}]}`, taken, free)
	rec := request(t, s, panel, "PUT", "/v1/external", body)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "external.port_taken") || !strings.Contains(rec.Body.String(), "db:") {
		t.Errorf("taken port: %d %s", rec.Code, rec.Body)
	}
	// The one that could open stays open.
	s.External.mu.Lock()
	_, cacheOpen := s.External.open["cache"]
	_, dbOpen := s.External.open["db"]
	s.External.mu.Unlock()
	if !cacheOpen || dbOpen {
		t.Errorf("open: cache %v, db %v", cacheOpen, dbOpen)
	}

	l.Close()
	if rec := request(t, s, panel, "PUT", "/v1/external", body); rec.Code != http.StatusNoContent {
		t.Errorf("after the port was freed: %d %s", rec.Code, rec.Body)
	}
}

func TestExternalUser(t *testing.T) {
	s, f, sealed := externalServer(t)
	panel := &peer.Peer{UID: 999}
	f.containers = []engine.Status{{ID: "db-1", App: "db", State: "running", IP: netip.MustParseAddr("127.0.0.1")}}
	var script string
	var args []string
	var reply string
	var exit uint32
	f.exec = func(a []string, stdin io.Reader, stdout io.Writer) uint32 {
		b, _ := io.ReadAll(stdin)
		script, args = string(b), a
		io.WriteString(stdout, reply)
		return exit
	}
	set := func(engine string) *http.Response {
		body := fmt.Sprintf(`{"engine":%q,"container":"db-1","port":%d,"target":5432,"password":%q}`, engine, freePort(t), sealed)
		return request(t, s, panel, "PUT", "/v1/external/db", body).Result()
	}

	for engine, want := range map[string]string{
		"postgres": "ALTER ROLE zelie_external WITH LOGIN PASSWORD '" + extPassword + "'",
		"mariadb":  "IDENTIFIED BY '" + extPassword + "'",
		"redis":    "ACL SETUSER zelie_external reset on >" + extPassword,
	} {
		reply = ""
		if engine == "redis" {
			reply = "OK\n"
		}
		if res := set(engine); res.StatusCode != http.StatusNoContent {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("%s: %d %s", engine, res.StatusCode, b)
		}
		if !strings.Contains(script, want) {
			t.Errorf("%s script:\n%s", engine, script)
		}
		if strings.Contains(strings.Join(args, " "), extPassword) {
			t.Errorf("%s: the password is on the command line: %v", engine, args)
		}
	}

	// A failure does not show the password, which psql quotes back.
	reply, exit = "ERROR: syntax error at or near \"'"+extPassword+"'\"", 3
	res := set("postgres")
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode == http.StatusNoContent || strings.Contains(string(b), extPassword) {
		t.Errorf("failure: %d %s", res.StatusCode, b)
	}
	// A MariaDB made before outside access has no root password to use.
	res = set("mariadb")
	b, _ = io.ReadAll(res.Body)
	if !strings.Contains(string(b), "external.no_root") {
		t.Errorf("old MariaDB: %d %s", res.StatusCode, b)
	}
	// redis-cli exits 0 on an error.
	reply, exit = "ERR Error in ACL SETUSER modifier", 0
	if res := set("redis"); res.StatusCode == http.StatusNoContent {
		t.Error("a Redis error passed")
	}

	reply, exit = "", 0
	if rec := request(t, s, panel, "DELETE", "/v1/external/db", `{"engine":"postgres","container":"db-1"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(script, "DROP ROLE zelie_external") {
		t.Errorf("drop script:\n%s", script)
	}
}
