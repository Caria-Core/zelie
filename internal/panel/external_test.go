package panel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/store"
)

func TestExternalAccess(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	if code, out := e.b.do("GET", "/api/apps/pg/external", nil); code != http.StatusOK || out["enabled"] != false {
		t.Fatalf("before: %d %v", code, out)
	}

	// Something else on the server holds the first port.
	e.core.takenPorts = map[int]bool{15432: true}
	code, out := e.b.do("POST", "/api/apps/pg/external", nil)
	password, _ := out["password"].(string)
	if code != http.StatusOK || out["port"] != 15433.0 || out["user"] != "zelie_external" || out["database"] != "app" || len(password) != 32 {
		t.Fatalf("set up: %d %v", code, out)
	}
	if code, out := e.b.do("GET", "/api/apps/pg/external", nil); code != http.StatusOK || out["port"] != 15433.0 || out["password"] != nil {
		t.Errorf("after: %d %v", code, out)
	}

	// A new password keeps the port.
	code, out = e.b.do("POST", "/api/apps/pg/external", nil)
	if code != http.StatusOK || out["port"] != 15433.0 || out["password"] == password {
		t.Errorf("new password: %d %v", code, out)
	}
	e.core.mu.Lock()
	sets := slices.Clone(e.core.extSets)
	e.core.mu.Unlock()
	// The page's read and the new password each ask the core to open the
	// port first, with no password.
	if !slices.Equal(sets, []string{"pg 15432 true", "pg 15433 true", "pg 15433 false", "pg 15433 false", "pg 15433 true"}) {
		t.Errorf("core asked %v", sets)
	}
	// The password never lands in the panel's database.
	if vars, _ := e.s.Store.Env(context.Background(), "pg"); slices.ContainsFunc(vars, func(v store.EnvVar) bool { return v.Name == externalPasswordVar }) {
		t.Error("a Postgres keeps the external password")
	}

	// Setting it up again asks to confirm first; turning it off does not.
	later := e.s.now().Add(time.Hour)
	e.s.Now = func() time.Time { return later }
	if code, out := e.b.do("POST", "/api/apps/pg/external", nil); code != http.StatusForbidden || out["confirm"] != true {
		t.Errorf("without confirming: %d %v", code, out)
	}
	if code, _ := e.b.do("DELETE", "/api/apps/pg/external", nil); code != http.StatusNoContent {
		t.Fatalf("turn off: %d", code)
	}
	if code, out := e.b.do("GET", "/api/apps/pg/external", nil); out["enabled"] != false {
		t.Errorf("after turning off: %d %v", code, out)
	}
	if code, _ := e.b.do("DELETE", "/api/apps/pg/external", nil); code != http.StatusNotFound {
		t.Errorf("turn off twice: %d", code)
	}
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	if len(e.core.external) != 0 {
		t.Errorf("core still listens: %v", e.core.external)
	}
}

func TestExternalRedisAndDelete(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "cache", "engine": "redis"})
	e.settle(t, "cache")
	code, out := e.b.do("POST", "/api/apps/cache/external", nil)
	if code != http.StatusOK || out["port"] != 16379.0 || out["database"] != nil {
		t.Fatalf("set up: %d %v", code, out)
	}
	// Redis gets the password on its command line, sealed.
	vars, _ := e.s.Store.Env(context.Background(), "cache")
	i := slices.IndexFunc(vars, func(v store.EnvVar) bool { return v.Name == externalPasswordVar })
	if i < 0 || !vars[i].Secret || strings.Contains(vars[i].Value, out["password"].(string)) {
		t.Fatalf("variables %+v", vars)
	}
	if code, _ := e.b.do("DELETE", "/api/apps/cache", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	if len(e.core.external) != 0 {
		t.Errorf("a deleted database's port is open: %v", e.core.external)
	}
}

func TestExternalNotForApps(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")
	if code, out := e.b.do("POST", "/api/apps/web/external", nil); code != http.StatusConflict || out["code"] != "external.not_database" {
		t.Errorf("an app: %d", code)
	}
}

// holdingEnv is an environment for ports that something else holds: a port
// counts as free when the fake core is not told it is taken.
func holdingEnv(t *testing.T) *appEnv {
	t.Helper()
	e := newAppEnv(t)
	e.s.testPortFree = func(port int) bool {
		e.core.mu.Lock()
		defer e.core.mu.Unlock()
		return !e.core.takenPorts[port]
	}
	return e
}

// startRetries lets the job that tries held ports again run, with a short
// wait, and ends it with the test. It comes after the databases are made, so
// the jobs their deployments wake do not start with it.
func (e *appEnv) startRetries(t *testing.T) {
	t.Helper()
	old := externalRetryEvery
	externalRetryEvery = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	e.s.loops.start(ctx)
	t.Cleanup(func() {
		cancel()
		e.s.deploys.wg.Wait()
		e.s.jobs.Wait()
		waitFor(t, func() bool { return !e.s.loops.running("external") })
		externalRetryEvery = old
	})
}

// takePort is a program starting on the server and taking the port while
// the core is away, so the core finds it held when it comes back.
func (e *appEnv) takePort(app string, port int) {
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	if e.core.takenPorts == nil {
		e.core.takenPorts = map[int]bool{}
	}
	e.core.takenPorts[port] = true
	delete(e.core.external, app)
}

func (e *appEnv) letGo(port int) {
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	delete(e.core.takenPorts, port)
}

// reopens counts the times the panel asked the core to open the port again,
// without a password.
func (e *appEnv) reopens(app string, port int) int {
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	return strings.Count("\n"+strings.Join(e.core.extSets, "\n")+"\n", fmt.Sprintf("\n%s %d false\n", app, port))
}

// passwordSets counts the times the panel asked the core to set the
// external user's password.
func (e *appEnv) passwordSets() int {
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	n := 0
	for _, set := range e.core.extSets {
		if strings.HasSuffix(set, " true") {
			n++
		}
	}
	return n
}

// listens is the port the fake core listens on for the database, or zero.
func (e *appEnv) listens(app string) int {
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	return e.core.external[app].Port
}

// outsideDatabase makes a Postgres called pg open to desktop tools.
func (e *appEnv) outsideDatabase(t *testing.T, id string) {
	t.Helper()
	e.b.do("POST", "/api/databases", map[string]any{"id": id, "engine": "postgres"})
	e.settle(t, id)
	if code, out := e.b.do("POST", "/api/apps/"+id+"/external", nil); code != http.StatusOK {
		t.Fatalf("set up: %d %v", code, out)
	}
}

// A program took the port while the panel was down. The page must not
// offer a way in to it, and the panel tries again until it is let go, and
// not any longer.
func TestExternalPortHeldAtStart(t *testing.T) {
	e := holdingEnv(t)
	ctx := context.Background()
	e.outsideDatabase(t, "pg")
	e.startRetries(t)

	// Nothing is held, so nothing runs.
	e.s.syncExternal(ctx)
	if e.s.loops.running("external") {
		t.Fatal("the job runs while every port is open")
	}
	if code, out := e.b.do("GET", "/api/apps/pg/external", nil); code != http.StatusOK || out["port_taken"] != nil || out["user"] != "zelie_external" {
		t.Fatalf("open: %d %v", code, out)
	}

	e.takePort("pg", 15432)
	e.takePort("other", 15433) // the next port is busy too
	e.s.syncExternal(ctx)
	code, out := e.b.do("GET", "/api/apps/pg/external", nil)
	if code != http.StatusOK || out["enabled"] != true || out["port_taken"] != true || out["port"] != 15432.0 || out["free_port"] != 15434.0 {
		t.Fatalf("held: %d %v", code, out)
	}
	// Nothing to connect a tool with, and the password was never in it.
	for _, k := range []string{"user", "database", "password"} {
		if _, ok := out[k]; ok {
			t.Errorf("a held port's answer has %q: %v", k, out)
		}
	}

	// It keeps trying while the port is held.
	waitFor(t, func() bool { return e.reopens("pg", 15432) >= 6 })
	// Then the program lets go.
	e.letGo(15432)
	waitFor(t, func() bool { return e.listens("pg") == 15432 && !e.s.held.holds("pg", 15432) })

	// Nothing is held any more, so the job ends and stops asking.
	waitFor(t, func() bool { return !e.s.loops.running("external") })
	asked := e.reopens("pg", 15432)
	time.Sleep(50 * time.Millisecond)
	if again := e.reopens("pg", 15432); again != asked {
		t.Errorf("the panel kept asking: %d times, then %d", asked, again)
	}
}

// A round is one try at every held port. With nothing held it does nothing
// and the job it belongs to ends.
func TestExternalRetryRound(t *testing.T) {
	e := holdingEnv(t)
	ctx := context.Background()
	e.outsideDatabase(t, "pg")
	sets := e.reopens("pg", 15432)
	if e.s.retryExternalOnce(ctx) || e.reopens("pg", 15432) != sets {
		t.Error("a round with nothing held asked the core")
	}

	e.takePort("pg", 15432)
	e.s.syncExternal(ctx)
	if !e.s.retryExternalOnce(ctx) || !e.s.held.holds("pg", 15432) {
		t.Fatal("a round let go of a port that is still held")
	}
	e.letGo(15432)
	if e.s.retryExternalOnce(ctx) || e.s.held.holds("pg", 15432) || e.listens("pg") != 15432 {
		t.Errorf("a round did not open the port that was let go: listens on %d", e.listens("pg"))
	}
}

// The core can restart alone, find something on the port and say nothing.
// The page asks the core about the port each time it is read.
func TestExternalPortHeldWhileThePanelRuns(t *testing.T) {
	e := holdingEnv(t)
	e.outsideDatabase(t, "pg")
	e.takePort("pg", 15432)
	if _, out := e.b.do("GET", "/api/apps/pg/external", nil); out["port_taken"] != true || out["user"] != nil || out["free_port"] != 15433.0 {
		t.Fatalf("held: %v", out)
	}
	// The next read finds it open again.
	e.letGo(15432)
	if _, out := e.b.do("GET", "/api/apps/pg/external", nil); out["port_taken"] != nil || out["user"] != "zelie_external" {
		t.Fatalf("let go: %v", out)
	}
	if e.listens("pg") != 15432 {
		t.Errorf("the core listens on %d", e.listens("pg"))
	}
}

// When every port the database may use is taken there is nothing to move
// to, and the answer says so by leaving the suggestion out.
func TestExternalNoFreePort(t *testing.T) {
	e := holdingEnv(t)
	e.outsideDatabase(t, "pg")
	for port := 15432; port < 15432+externalTries; port++ {
		e.takePort("pg", port)
	}
	code, out := e.b.do("GET", "/api/apps/pg/external", nil)
	if code != http.StatusOK || out["port_taken"] != true {
		t.Fatalf("%d %v", code, out)
	}
	if _, ok := out["free_port"]; ok {
		t.Errorf("a suggestion although every port is taken: %v", out)
	}
}

// The user asking for a new password finds a port the core could not take
// back: the page has to say so rather than fail without a word, and the
// user's password must not change for a page that cannot show it.
func TestExternalNewPasswordFindsPortHeld(t *testing.T) {
	e := holdingEnv(t)
	e.outsideDatabase(t, "pg")
	before := e.passwordSets()
	e.takePort("pg", 15432)
	if code, out := e.b.do("POST", "/api/apps/pg/external", nil); code != http.StatusConflict || out["code"] != "external.port_taken" {
		t.Fatalf("new password: %d %v", code, out)
	}
	if _, out := e.b.do("GET", "/api/apps/pg/external", nil); out["port_taken"] != true || out["user"] != nil {
		t.Errorf("after: %v", out)
	}
	if e.passwordSets() != before {
		t.Error("the database user got a password that was never shown")
	}
}

func TestExternalMoveOffAHeldPort(t *testing.T) {
	e := holdingEnv(t)
	ctx := context.Background()
	e.outsideDatabase(t, "pg")
	e.outsideDatabase(t, "pg2")
	e.b.do("POST", "/api/databases", map[string]any{"id": "idle", "engine": "postgres"})
	e.takePort("pg", 15432)
	e.s.syncExternal(ctx)
	if _, out := e.b.do("GET", "/api/apps/pg/external", nil); out["port_taken"] != true {
		t.Fatalf("held: %v", out)
	}

	for _, tc := range []struct {
		name, app string
		port      int
		status    int
		code      string
	}{
		{"below every range", "pg", 80, http.StatusBadRequest, "external.bad_port"},
		{"above every range", "pg", 70000, http.StatusBadRequest, "external.bad_port"},
		{"just below its range", "pg", 15431, http.StatusBadRequest, "external.bad_port"},
		{"just above its range", "pg", 15432 + externalTries, http.StatusBadRequest, "external.bad_port"},
		{"another engine's range", "pg", 13306, http.StatusBadRequest, "external.bad_port"},
		{"a port something else needs", "pg", 2222, http.StatusBadRequest, "external.bad_port"},
		{"the port that is held", "pg", 15432, http.StatusConflict, "external.port_taken"},
		{"another database's", "pg", 15433, http.StatusConflict, "external.port_used"},
		{"without outside access", "idle", 15440, http.StatusNotFound, "external.off"},
	} {
		if code, out := e.b.do("PUT", "/api/apps/"+tc.app+"/external", map[string]any{"port": tc.port}); code != tc.status || out["code"] != tc.code {
			t.Errorf("%s: %d %v", tc.name, code, out)
		}
	}
	// The refusal names the range, so a person knows what to ask for.
	if code, out := e.b.do("PUT", "/api/apps/pg/external", map[string]any{"port": 15432 + externalTries}); code != http.StatusBadRequest || out["params"].(map[string]any)["to"] != float64(15432+externalTries-1) {
		t.Errorf("the range in the error: %d %v", code, out)
	}
	if _, out := e.b.do("GET", "/api/apps/pg/external", nil); out["port_taken"] != true {
		t.Fatalf("a refused move cleared it: %v", out)
	}

	code, out := e.b.do("PUT", "/api/apps/pg/external", map[string]any{"port": 15440})
	if code != http.StatusOK || out["port"] != 15440.0 || out["user"] != "zelie_external" || out["port_taken"] != nil {
		t.Fatalf("move: %d %v", code, out)
	}
	if _, out := e.b.do("GET", "/api/apps/pg/external", nil); out["port"] != 15440.0 || out["port_taken"] != nil || out["user"] != "zelie_external" {
		t.Errorf("after: %v", out)
	}
	if got := e.listens("pg"); got != 15440 {
		t.Errorf("the core listens on %d", got)
	}
	if x, err := e.s.Store.ExternalAccess(ctx, "pg"); err != nil || x.Port != 15440 {
		t.Errorf("saved %+v, %v", x, err)
	}
	// Nothing is held now.
	if e.s.retryExternalOnce(ctx) {
		t.Error("something is still held after the move")
	}
}

// The last port of a range can be moved to.
func TestExternalMoveToTheEndOfTheRange(t *testing.T) {
	e := holdingEnv(t)
	e.outsideDatabase(t, "pg")
	last := 15432 + externalTries - 1
	if code, out := e.b.do("PUT", "/api/apps/pg/external", map[string]any{"port": last}); code != http.StatusOK || out["port"] != float64(last) {
		t.Errorf("%d %v", code, out)
	}
}

// A move to a port that turns out to be held must not leave a working
// database without the port it had.
func TestExternalMoveKeepsTheOldPortWhenTheNewOneFails(t *testing.T) {
	e := holdingEnv(t)
	ctx := context.Background()
	e.outsideDatabase(t, "pg")
	e.takePort("nobody", 15440)
	if code, out := e.b.do("PUT", "/api/apps/pg/external", map[string]any{"port": 15440}); code != http.StatusConflict || out["code"] != "external.port_taken" {
		t.Fatalf("move: %d %v", code, out)
	}
	if got := e.listens("pg"); got != 15432 {
		t.Errorf("the core listens on %d", got)
	}
	if _, out := e.b.do("GET", "/api/apps/pg/external", nil); out["port"] != 15432.0 || out["port_taken"] != nil || out["user"] != "zelie_external" {
		t.Errorf("after: %v", out)
	}
	if e.s.retryExternalOnce(ctx) {
		t.Error("something is held although every port is open")
	}
}

func TestExternalHeldGoesWhenTurnedOffOrDeleted(t *testing.T) {
	e := holdingEnv(t)
	ctx := context.Background()
	e.outsideDatabase(t, "pg")
	e.outsideDatabase(t, "pg2")
	e.takePort("pg", 15432)
	e.takePort("pg2", 15433)
	e.s.syncExternal(ctx)
	if !e.s.held.holds("pg", 15432) || !e.s.held.holds("pg2", 15433) {
		t.Fatal("not held")
	}
	if code, _ := e.b.do("DELETE", "/api/apps/pg/external", nil); code != http.StatusNoContent {
		t.Fatalf("turn off: %d", code)
	}
	if e.s.held.holds("pg", 15432) {
		t.Error("still held after it was turned off")
	}
	if code, _ := e.b.do("DELETE", "/api/apps/pg2", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if e.s.held.holds("pg2", 15433) {
		t.Error("still held after the database went")
	}
	if e.s.retryExternalOnce(ctx) {
		t.Error("something is held although both are gone")
	}
}

// A retry works from what is held once it has the lock: a delete that ran
// first has closed the port, and the retry must not open it again.
func TestExternalRetryDoesNotReopenWhatWentWhileItWaited(t *testing.T) {
	e := holdingEnv(t)
	ctx := context.Background()
	e.outsideDatabase(t, "pg")
	e.takePort("pg", 15432)
	e.s.syncExternal(ctx)

	e.s.externalMu.Lock()
	done := make(chan bool)
	go func() { done <- e.s.retryExternalOnce(ctx) }()
	time.Sleep(20 * time.Millisecond)
	// What deleting the database does while holding the lock.
	e.letGo(15432)
	e.core.RemoveExternal(ctx, "pg", "postgres", "")
	e.s.held.drop("pg")
	e.s.externalMu.Unlock()
	if <-done {
		t.Error("the retry went on for a database that is gone")
	}
	if got := e.listens("pg"); got != 0 {
		t.Errorf("the retry opened port %d for a deleted database", got)
	}
}

// Deleting a database closes its port under the lock the retry takes, or a
// retry that is under way could open it again afterwards.
func TestDeleteDatabaseWaitsForOutsideAccessWork(t *testing.T) {
	e := holdingEnv(t)
	e.outsideDatabase(t, "pg")
	e.s.externalMu.Lock()
	done := make(chan int)
	go func() {
		code, _ := e.b.do("DELETE", "/api/apps/pg", nil)
		done <- code
	}()
	select {
	case <-done:
		e.s.externalMu.Unlock()
		t.Fatal("the delete went on while outside access was being changed")
	case <-time.After(100 * time.Millisecond):
	}
	e.s.externalMu.Unlock()
	if code := <-done; code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
}

// A delete that stops after the core let go of the port, as one does when
// the deployment logs cannot be removed, leaves the database in the panel.
// Reading its outside access must not open the port again for it: the saved
// port went with the listener.
func TestDeleteDatabaseLeavesNoPortToReopen(t *testing.T) {
	e := holdingEnv(t)
	ctx := context.Background()
	e.outsideDatabase(t, "pg")
	if e.listens("pg") != 15432 {
		t.Fatalf("the core listens on %d", e.listens("pg"))
	}
	// A folder with something in it cannot be removed as a log.
	d := e.settle(t, "pg")
	os.Remove(e.s.deployLogPath(d.ID))
	if err := os.MkdirAll(filepath.Join(e.s.deployLogPath(d.ID), "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out := e.b.do("DELETE", "/api/apps/pg", nil); code != http.StatusInternalServerError {
		t.Fatalf("delete: %d %v", code, out)
	}
	if _, err := e.s.Store.App(ctx, "pg"); err != nil {
		t.Fatalf("the delete went on although it failed: %v", err)
	}
	if code, out := e.b.do("GET", "/api/apps/pg/external", nil); code != http.StatusOK || out["enabled"] == true {
		t.Errorf("outside access after the port was closed: %d %v", code, out)
	}
	if got := e.listens("pg"); got != 0 {
		t.Errorf("reading it opened port %d for a database that is being deleted", got)
	}
	if _, err := e.s.Store.ExternalAccess(ctx, "pg"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the saved port is still there: %v", err)
	}
}

// The start-up sync reads the saved ports and sends them under the lock a
// request takes to move one, or it could open a port that was moved off.
func TestSyncExternalWaitsForOutsideAccessWork(t *testing.T) {
	e := holdingEnv(t)
	e.outsideDatabase(t, "pg")
	e.s.externalMu.Lock()
	done := make(chan struct{})
	go func() {
		e.s.syncExternal(context.Background())
		close(done)
	}()
	select {
	case <-done:
		e.s.externalMu.Unlock()
		t.Fatal("the sync went on while outside access was being changed")
	case <-time.After(100 * time.Millisecond):
	}
	e.s.externalMu.Unlock()
	<-done
}

// The suggestion is judged by trying the port on the machine.
func TestLoopbackFree(t *testing.T) {
	s := &Server{}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if s.loopbackFree(port) {
		t.Error("a port something listens on counts as free")
	}
	l.Close()
	if !s.loopbackFree(port) {
		t.Error("a port nothing listens on is not free")
	}
}
