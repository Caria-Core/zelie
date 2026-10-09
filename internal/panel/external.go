package panel

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/secret"
	"github.com/Caria-Core/zelie/internal/store"
)

// Outside access: a desktop tool reaches a database through an SSH
// tunnel to a loopback port the core listens on, signing in as a user of
// its own. The password is shown once and never kept here.

// externalPasswordVar carries the external user's password into a Redis
// container, whose users would not outlive a restart otherwise.
const externalPasswordVar = "ZELIE_EXTERNAL_PASSWORD"

// The first loopback port tried for each engine: its usual port, moved up
// so it does not clash with a database installed on the server itself.
var externalBase = map[string]int{"postgres": 15432, "mariadb": 13306, "redis": 16379}

const externalTries = 50

// externalRetryEvery is how often a port that something else held is tried
// again, for as long as one is. Tests shorten it.
var externalRetryEvery = time.Minute

// externalRange is the ports freeExternalPort goes through for an engine,
// and the only ones a database's access can be moved to.
func externalRange(engine string) (from, to int) {
	from = externalBase[engine]
	return from, from + externalTries - 1
}

var (
	errExternalOff     = msg.Define(http.StatusNotFound, "external.off", "Outside access is not set up for this database.")
	errExternalNotDB   = msg.Define(http.StatusConflict, "external.not_database", "Only a database can be opened to desktop tools.")
	errExternalNoPort  = msg.Define(http.StatusConflict, "external.no_port", "No free port was found on this server for outside access.")
	errExternalBadPort = msg.Define(http.StatusBadRequest, "external.bad_port", "Outside access uses ports from {from} to {to}.")
	errExternalPortUse = msg.Define(http.StatusConflict, "external.port_used", "Port {port} is already used by another database.")
)

type externalJSON struct {
	Enabled  bool      `json:"enabled"`
	Port     int       `json:"port,omitempty"`
	User     string    `json:"user,omitempty"`
	Database string    `json:"database,omitempty"` // empty for Redis
	Since    time.Time `json:"since,omitzero"`
	// PortTaken says something else on the server holds the port, so the
	// database cannot be reached through it. The details above stay out of
	// the answer then, or a tunnel to that port would hand the user's
	// password to whatever holds it. FreePort is a port to move to, or
	// missing when none was found.
	PortTaken bool `json:"port_taken,omitempty"`
	FreePort  int  `json:"free_port,omitempty"`
	// Password is only there in the answer that made it.
	Password string `json:"password,omitempty"`
}

func externalOf(a store.App, x store.ExternalAccess) externalJSON {
	out := externalJSON{Enabled: true, Port: x.Port, User: core.ExternalUser, Since: x.CreatedAt}
	if a.Engine != "redis" {
		out.Database = dbUser
	}
	return out
}

// externalHeld remembers the databases whose outside access port could not
// be opened because something else on the server holds it. It lives in
// memory: the panel finds out again when it starts. Callers change it with
// externalMu held, so a retry never works from a list that was stale when it
// began.
type externalHeld struct {
	mu    sync.Mutex
	ports map[string]int // database to the port that was refused
}

// add notes the refused port, and reports whether that is news.
func (h *externalHeld) add(app string, port int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ports == nil {
		h.ports = map[string]int{}
	}
	fresh := h.ports[app] != port
	h.ports[app] = port
	return fresh
}

// drop forgets the database, and reports whether it was held.
func (h *externalHeld) drop(app string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.ports[app]
	delete(h.ports, app)
	return ok
}

// holds says whether port is the one refused for the database. A port it
// has been moved off since is not.
func (h *externalHeld) holds(app string, port int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.ports[app]
	return ok && p == port
}

// all returns a copy of what is held.
func (h *externalHeld) all() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Clone(h.ports)
}

// len is how many databases are held.
func (h *externalHeld) len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.ports)
}

// databaseFrom is the database in the path.
func (s *Server) databaseFrom(w http.ResponseWriter, r *http.Request) (store.App, dbEngine, bool) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return a, dbEngine{}, false
	}
	e, ok := engineOf(a)
	if !ok {
		writeError(w, errExternalNotDB.Err())
		return a, e, false
	}
	return a, e, true
}

// getExternal answers with the way in, unless the port is held. It asks
// the core to open the port first: a core that restarted on its own, and
// found something else on the port, tells nobody, and the details would
// send a tunnel there.
func (s *Server) getExternal(w http.ResponseWriter, r *http.Request) {
	a, e, ok := s.databaseFrom(w, r)
	if !ok {
		return
	}
	s.externalMu.Lock()
	defer s.externalMu.Unlock()
	ctx := context.WithoutCancel(r.Context())
	x, err := s.Store.ExternalAccess(ctx, a.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, externalJSON{})
		return
	}
	if err != nil {
		s.fail(w, "read outside access", err)
		return
	}
	s.reopenExternal(ctx, a, e, x)
	if s.held.holds(a.ID, x.Port) {
		writeJSON(w, http.StatusOK, externalJSON{Enabled: true, Port: x.Port, Since: x.CreatedAt, PortTaken: true, FreePort: s.freeExternalPort(ctx, a)})
		return
	}
	writeJSON(w, http.StatusOK, externalOf(a, x))
}

// setExternal sets up outside access, or gives the user a new password if
// it is already set up. Either way the answer has the password, once.
func (s *Server) setExternal(w http.ResponseWriter, r *http.Request) {
	a, e, ok := s.databaseFrom(w, r)
	if !ok {
		return
	}
	s.externalMu.Lock()
	defer s.externalMu.Unlock()
	ctx := context.WithoutCancel(r.Context())
	container, err := s.liveContainer(ctx, a)
	if err != nil {
		s.failWith(w, "find database", err)
		return
	}
	key, err := s.Core.SecretKey(ctx)
	if err != nil {
		s.coreFailed(w, "get secret key", err)
		return
	}
	password := newPassword()
	sealed, err := secret.Seal(key, a.ID, externalPasswordVar, password)
	if err != nil {
		s.fail(w, "seal password", err)
		return
	}
	x, err := s.Store.ExternalAccess(ctx, a.ID)
	switch {
	case err == nil:
		// The port first, with no password: the user's password changes
		// only if the page can show the new one.
		if err = s.checkExternalPort(ctx, a, e, x); err == nil {
			err = s.Core.SetExternal(ctx, a.ID, a.Engine, container, x.Port, e.Port, sealed)
			if isPortTaken(err) {
				s.holdExternal(a.ID, x.Port)
			}
		}
	case errors.Is(err, store.ErrNotFound):
		x = store.ExternalAccess{AppID: a.ID, CreatedAt: s.now()}
		x.Port, err = s.openExternal(ctx, a, e, container, sealed)
	}
	if err != nil {
		s.failWith(w, "set up outside access", err)
		return
	}
	if err := s.Store.SetExternalAccess(ctx, x); err != nil {
		s.Core.RemoveExternal(ctx, a.ID, a.Engine, container)
		s.fail(w, "save outside access", err)
		return
	}
	s.held.drop(a.ID)
	if a.Engine == "redis" {
		if err := s.setExternalVar(ctx, a.ID, sealed); err != nil {
			s.fail(w, "save variables", err)
			return
		}
	}
	s.Log.Info("outside access set", "database", a.ID, "port", x.Port, "user", loginFrom(r.Context()).account.ID)
	out := externalOf(a, x)
	out.Password = password
	writeJSON(w, http.StatusOK, out)
}

// openExternal finds a free loopback port, skipping those of other
// databases and those something else on the server holds.
func (s *Server) openExternal(ctx context.Context, a store.App, e dbEngine, container, sealed string) (int, error) {
	list, err := s.Store.ExternalAccesses(ctx)
	if err != nil {
		return 0, err
	}
	var used []int
	for _, x := range list {
		used = append(used, x.Port)
	}
	port := externalBase[a.Engine]
	for range externalTries {
		for slices.Contains(used, port) {
			port++
		}
		err := s.Core.SetExternal(ctx, a.ID, a.Engine, container, port, e.Port, sealed)
		if !isPortTaken(err) {
			return port, err
		}
		// The user is made by now; the next try only changes its password
		// to the same one.
		port++
	}
	return 0, errExternalNoPort.Err()
}

// removeExternal turns outside access off. The database must be running
// so its user can be dropped: a user left behind would still sign in from
// the apps' network.
func (s *Server) removeExternal(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.databaseFrom(w, r)
	if !ok {
		return
	}
	s.externalMu.Lock()
	defer s.externalMu.Unlock()
	ctx := context.WithoutCancel(r.Context())
	if _, err := s.Store.ExternalAccess(ctx, a.ID); errors.Is(err, store.ErrNotFound) {
		writeError(w, errExternalOff.Err())
		return
	}
	container, err := s.liveContainer(ctx, a)
	if err != nil {
		s.failWith(w, "find database", err)
		return
	}
	if err := s.Core.RemoveExternal(ctx, a.ID, a.Engine, container); err != nil {
		s.coreFailed(w, "remove outside access", err)
		return
	}
	if err := s.Store.RemoveExternalAccess(ctx, a.ID); err != nil {
		s.fail(w, "remove outside access", err)
		return
	}
	s.held.drop(a.ID)
	if a.Engine == "redis" {
		if err := s.setExternalVar(ctx, a.ID, ""); err != nil {
			s.fail(w, "save variables", err)
			return
		}
	}
	s.Log.Info("outside access removed", "database", a.ID, "user", loginFrom(r.Context()).account.ID)
	w.WriteHeader(http.StatusNoContent)
}

// setExternalVar puts the sealed password in the database's variables, or
// takes it out when sealed is empty.
func (s *Server) setExternalVar(ctx context.Context, app, sealed string) error {
	vars, err := s.Store.Env(ctx, app)
	if err != nil {
		return err
	}
	vars = slices.DeleteFunc(vars, func(v store.EnvVar) bool { return v.Name == externalPasswordVar })
	if sealed != "" {
		vars = append(vars, store.EnvVar{Name: externalPasswordVar, Value: sealed, Secret: true})
	}
	return s.Store.SetEnv(ctx, app, vars)
}

// syncExternal tells the core, when the panel starts, which ports should
// be open. The core keeps them itself; this mends a core that kept ports
// for databases the panel no longer has.
func (s *Server) syncExternal(ctx context.Context) {
	// Held all the way: a port moved after the rows were read would be
	// opened again at the old number by what follows.
	s.externalMu.Lock()
	defer s.externalMu.Unlock()
	list, err := s.Store.ExternalAccesses(ctx)
	if err != nil {
		s.Log.Error("outside access: list", "err", err)
		return
	}
	type target struct {
		app store.App
		e   dbEngine
		x   store.ExternalAccess
	}
	var targets []target
	listeners := make([]core.ExternalListener, 0, len(list))
	for _, x := range list {
		a, err := s.Store.App(ctx, x.AppID)
		if err != nil {
			continue
		}
		if e, ok := engineOf(a); ok {
			listeners = append(listeners, core.ExternalListener{App: a.ID, Port: x.Port, Target: e.Port})
			targets = append(targets, target{a, e, x})
		}
	}
	if err := s.Core.SyncExternal(ctx, listeners); err != nil {
		s.Log.Error("outside access: sync", "err", err)
		// The core says only that some port would not open. Opening each
		// one again shows which, and the panel keeps it from looking
		// reachable.
		for _, t := range targets {
			s.reopenExternal(ctx, t.app, t.e, t.x)
		}
	}
}

// checkExternalPort asks the core to open a database's saved port, with no
// password. That changes nothing while it is open, so the answer also tells
// whether something else holds it, and the panel notes which. The caller
// holds externalMu.
func (s *Server) checkExternalPort(ctx context.Context, a store.App, e dbEngine, x store.ExternalAccess) error {
	err := s.Core.SetExternal(ctx, a.ID, a.Engine, "", x.Port, e.Port, "")
	switch {
	case err == nil:
		if s.held.drop(a.ID) {
			s.Log.Info("outside access: the port is open again", "database", a.ID, "port", x.Port)
		}
	case isPortTaken(err):
		s.holdExternal(a.ID, x.Port)
	}
	return err
}

// reopenExternal is checkExternalPort for the callers that have no one to
// tell about a failure but the log.
func (s *Server) reopenExternal(ctx context.Context, a store.App, e dbEngine, x store.ExternalAccess) {
	if err := s.checkExternalPort(ctx, a, e, x); err != nil && !isPortTaken(err) {
		s.Log.Error("outside access: open the port", "database", a.ID, "port", x.Port, "err", err)
	}
}

// holdExternal notes that something else on the server holds the database's
// port, and starts the job that tries it again unless that is running.
func (s *Server) holdExternal(app string, port int) {
	if s.held.add(app, port) {
		s.Log.Warn("outside access: something else holds the port, so the database is not reachable through it", "database", app, "port", port)
	}
	s.loops.wake("external", externalRetryEvery, nil, s.retryExternalOnce)
}

// retryExternalOnce opens the ports something else held, for those it has
// let go of, and reports whether any is still held.
func (s *Server) retryExternalOnce(ctx context.Context) bool {
	s.externalMu.Lock()
	defer s.externalMu.Unlock()
	// Read under the lock: a request that turned access off or deleted the
	// database while this waited has dropped it by now.
	for app, port := range s.held.all() {
		x, err := s.Store.ExternalAccess(ctx, app)
		if errors.Is(err, store.ErrNotFound) || err == nil && x.Port != port {
			// Turned off, or moved, while it waited.
			s.held.drop(app)
			continue
		}
		if err != nil {
			s.Log.Error("outside access: read", "database", app, "err", err)
			continue
		}
		a, err := s.Store.App(ctx, app)
		if errors.Is(err, store.ErrNotFound) {
			s.held.drop(app)
			continue
		}
		if err != nil {
			s.Log.Error("outside access: read database", "database", app, "err", err)
			continue
		}
		if e, ok := engineOf(a); ok {
			s.reopenExternal(ctx, a, e, x)
		}
	}
	return s.held.len() > 0
}

// moveExternal gives outside access another loopback port, as when the one
// it had is held by something else. The user and its password stay.
func (s *Server) moveExternal(w http.ResponseWriter, r *http.Request) {
	a, e, ok := s.databaseFrom(w, r)
	if !ok {
		return
	}
	var req struct {
		Port int `json:"port"`
	}
	if !decode(w, r, &req) {
		return
	}
	// Only the ports outside access picks from: the core binds this one
	// as root, and again on every start before the other services.
	if from, to := externalRange(a.Engine); req.Port < from || req.Port > to {
		writeError(w, errExternalBadPort.Err("from", from, "to", to))
		return
	}
	s.externalMu.Lock()
	defer s.externalMu.Unlock()
	ctx := context.WithoutCancel(r.Context())
	x, err := s.Store.ExternalAccess(ctx, a.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, errExternalOff.Err())
		return
	}
	if err != nil {
		s.fail(w, "read outside access", err)
		return
	}
	list, err := s.Store.ExternalAccesses(ctx)
	if err != nil {
		s.fail(w, "list outside access", err)
		return
	}
	if slices.ContainsFunc(list, func(o store.ExternalAccess) bool { return o.AppID != a.ID && o.Port == req.Port }) {
		writeError(w, errExternalPortUse.Err("port", req.Port))
		return
	}
	moved := x
	moved.Port = req.Port
	if err := s.Core.SetExternal(ctx, a.ID, a.Engine, "", moved.Port, e.Port, ""); err != nil {
		// The core let go of the old port to try the new one.
		s.reopenExternal(ctx, a, e, x)
		s.failWith(w, "move outside access", err)
		return
	}
	if err := s.Store.SetExternalAccess(ctx, moved); err != nil {
		s.reopenExternal(ctx, a, e, x)
		s.fail(w, "save outside access", err)
		return
	}
	s.held.drop(a.ID)
	s.Log.Info("outside access moved", "database", a.ID, "from", x.Port, "port", moved.Port, "user", loginFrom(r.Context()).account.ID)
	writeJSON(w, http.StatusOK, externalOf(a, moved))
}

// freeExternalPort finds a port to move a database's outside access to: not
// another database's, and not held by anything on the server right now. It
// is a suggestion, since the core has the last word when the port is used.
// Zero means none was found.
func (s *Server) freeExternalPort(ctx context.Context, a store.App) int {
	list, err := s.Store.ExternalAccesses(ctx)
	if err != nil {
		s.Log.Error("outside access: list", "err", err)
		return 0
	}
	from, to := externalRange(a.Engine)
	for port := from; port <= to; port++ {
		if !slices.ContainsFunc(list, func(x store.ExternalAccess) bool { return x.Port == port }) && s.loopbackFree(port) {
			return port
		}
	}
	return 0
}

// loopbackFree reports whether nothing on the server holds the port on
// 127.0.0.1 right now.
func (s *Server) loopbackFree(port int) bool {
	if s.testPortFree != nil {
		return s.testPortFree(port)
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// isPortTaken reports whether the core could not open a port because
// something else holds it.
func isPortTaken(err error) bool {
	var ce *core.Error
	return errors.As(err, &ce) && ce.Code == "external.port_taken"
}
