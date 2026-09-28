package panel

import (
	"context"
	"errors"
	"net/http"
	"slices"
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

var (
	errExternalOff    = msg.Define(http.StatusNotFound, "external.off", "Outside access is not set up for this database.")
	errExternalNotDB  = msg.Define(http.StatusConflict, "external.not_database", "Only a database can be opened to desktop tools.")
	errExternalNoPort = msg.Define(http.StatusConflict, "external.no_port", "No free port was found on this server for outside access.")
)

type externalJSON struct {
	Enabled  bool      `json:"enabled"`
	Port     int       `json:"port,omitempty"`
	User     string    `json:"user,omitempty"`
	Database string    `json:"database,omitempty"` // empty for Redis
	Since    time.Time `json:"since,omitzero"`
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

func (s *Server) getExternal(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.databaseFrom(w, r)
	if !ok {
		return
	}
	x, err := s.Store.ExternalAccess(r.Context(), a.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, externalJSON{})
		return
	}
	if err != nil {
		s.fail(w, "read outside access", err)
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
		err = s.Core.SetExternal(ctx, a.ID, a.Engine, container, x.Port, e.Port, sealed)
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
		var ce *core.Error
		if !errors.As(err, &ce) || ce.Code != "external.port_taken" {
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
	list, err := s.Store.ExternalAccesses(ctx)
	if err != nil {
		s.Log.Error("outside access: list", "err", err)
		return
	}
	listeners := make([]core.ExternalListener, 0, len(list))
	for _, x := range list {
		a, err := s.Store.App(ctx, x.AppID)
		if err != nil {
			continue
		}
		if e, ok := engineOf(a); ok {
			listeners = append(listeners, core.ExternalListener{App: a.ID, Port: x.Port, Target: e.Port})
		}
	}
	if err := s.Core.SyncExternal(ctx, listeners); err != nil {
		s.Log.Error("outside access: sync", "err", err)
	}
}
