package panel

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/secret"
	"github.com/Caria-Core/zelie/internal/store"
)

// A database is an app that runs its engine's official image, with one
// volume for its files and a password Zelie makes. Apps linked to it reach
// it by its name and get variables to connect with.

// dbUser is both the user and the database the apps connect to.
const dbUser = "app"

// dbEngine describes one database engine.
type dbEngine struct {
	Name     string
	Label    string
	Versions []string // newest first; the first is the default
	Port     int
	DataPath string
	MemoryMB int64
	// PasswordVar holds the password in the database's own container.
	PasswordVar string
	Env         []string
	// Args replace the image's command; empty keeps it.
	Args []string
	// vars are what a linked app gets, before its prefix. {host} is the
	// database's name and {secret} its password; a value with the password
	// in it only ever exists in the core.
	vars []linkedVar
}

type linkedVar struct {
	Name, Value string
}

var dbEngines = []dbEngine{
	{
		Name: "postgres", Label: "PostgreSQL", Versions: []string{"18", "17"}, Port: 5432,
		// Version 18 keeps its files under a directory named for the
		// version, older ones under data; both are inside this one.
		DataPath: "/var/lib/postgresql", MemoryMB: 512, PasswordVar: "POSTGRES_PASSWORD",
		Env: []string{"POSTGRES_USER=" + dbUser, "POSTGRES_DB=" + dbUser},
		vars: []linkedVar{
			{"DATABASE_URL", "postgresql://" + dbUser + ":{secret}@{host}:5432/" + dbUser},
			{"PGHOST", "{host}"}, {"PGPORT", "5432"}, {"PGUSER", dbUser}, {"PGPASSWORD", "{secret}"}, {"PGDATABASE", dbUser},
		},
	},
	{
		Name: "mariadb", Label: "MariaDB", Versions: []string{"11.8", "10.11"}, Port: 3306,
		DataPath: "/var/lib/mysql", MemoryMB: 512, PasswordVar: "MARIADB_PASSWORD",
		// Nobody needs root: the app's user owns its database, backups
		// included. Root gets a password no one keeps, made here because
		// the image's own random one is printed to the log.
		Env:  []string{"MARIADB_USER=" + dbUser, "MARIADB_DATABASE=" + dbUser, "MARIADB_ROOT_HOST=localhost"},
		Args: []string{"sh", "-c", `export MARIADB_ROOT_PASSWORD="$(head -c 24 /dev/urandom | base64)"; exec docker-entrypoint.sh mariadbd`},
		vars: []linkedVar{
			{"DATABASE_URL", "mysql://" + dbUser + ":{secret}@{host}:3306/" + dbUser},
			{"MYSQL_HOST", "{host}"}, {"MYSQL_PORT", "3306"}, {"MYSQL_USER", dbUser}, {"MYSQL_PASSWORD", "{secret}"}, {"MYSQL_DATABASE", dbUser},
		},
	},
	{
		Name: "redis", Label: "Redis", Versions: []string{"8"}, Port: 6379,
		DataPath: "/data", MemoryMB: 256, PasswordVar: "REDIS_PASSWORD",
		// Through the image's entrypoint, which drops root.
		Args: []string{"sh", "-c", `exec docker-entrypoint.sh redis-server --appendonly yes --requirepass "$REDIS_PASSWORD"`},
		vars: []linkedVar{
			{"REDIS_URL", "redis://default:{secret}@{host}:6379"},
			{"REDIS_HOST", "{host}"}, {"REDIS_PORT", "6379"}, {"REDIS_PASSWORD", "{secret}"},
		},
	},
}

// A database's volume starts at this limit; it can be raised like any.
const defaultDatabaseMB = 5120

func engineOf(a store.App) (dbEngine, bool) {
	for _, e := range dbEngines {
		if e.Name == a.Engine {
			return e, true
		}
	}
	return dbEngine{}, false
}

type databaseRequest struct {
	ID      string `json:"id"`
	Engine  string `json:"engine"`
	Version string `json:"version"`
}

type engineJSON struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Versions []string `json:"versions"`
	Port     int      `json:"port"`
	Vars     []string `json:"vars"`
}

func (s *Server) listEngines(w http.ResponseWriter, r *http.Request) {
	out := make([]engineJSON, 0, len(dbEngines))
	for _, e := range dbEngines {
		ej := engineJSON{Name: e.Name, Label: e.Label, Versions: e.Versions, Port: e.Port}
		for _, v := range e.vars {
			ej.Vars = append(ej.Vars, v.Name)
		}
		out = append(out, ej)
	}
	writeJSON(w, http.StatusOK, out)
}

// createDatabase makes a database and starts it.
func (s *Server) createDatabase(w http.ResponseWriter, r *http.Request) {
	var req databaseRequest
	if !decode(w, r, &req) {
		return
	}
	if err := checkAppID(req.ID); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	e, ok := engineOf(store.App{Engine: req.Engine})
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("the engine must be postgres, mariadb or redis"))
		return
	}
	if req.Version == "" {
		req.Version = e.Versions[0]
	}
	if !slices.Contains(e.Versions, req.Version) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s comes in versions %v", e.Label, e.Versions))
		return
	}
	ctx := r.Context()
	key, err := s.Core.SecretKey(ctx)
	if err != nil {
		s.coreFailed(w, "get secret key", err)
		return
	}
	a := store.App{
		ID: req.ID, Source: store.SourceImage, Image: e.Name + ":" + req.Version, Port: e.Port,
		MemoryMB: e.MemoryMB, CPUs: defaultCPUs, HealthPath: "/", Engine: e.Name, EngineVersion: req.Version, CreatedAt: s.now(),
	}
	sealed, err := secret.Seal(key, a.ID, e.PasswordVar, newPassword())
	if err != nil {
		s.fail(w, "seal password", err)
		return
	}
	switch err := s.Store.CreateApp(ctx, a); {
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, errors.New("an app or database with that name already exists"))
		return
	case err != nil:
		s.fail(w, "create database", err)
		return
	}
	undo := func() {
		ctx := context.WithoutCancel(ctx)
		s.removeAppVolumes(ctx, a.ID)
		s.Store.DeleteApp(ctx, a.ID)
	}
	vars := []store.EnvVar{{Name: e.PasswordVar, Value: sealed, Secret: true}}
	for _, v := range e.Env {
		name, value, _ := strings.Cut(v, "=")
		vars = append(vars, store.EnvVar{Name: name, Value: value})
	}
	if err := s.Store.SetEnv(ctx, a.ID, vars); err != nil {
		undo()
		s.fail(w, "save variables", err)
		return
	}
	if err := s.Store.SetBackupPlan(ctx, store.DefaultBackupPlan(a.ID)); err != nil {
		undo()
		s.fail(w, "save backup plan", err)
		return
	}
	path, limit := e.DataPath, int64(defaultDatabaseMB)
	if h, err := s.Core.Host(ctx); err == nil && limit > h.DiskBytes>>20/2 {
		// A small disk still fits the database with room to spare.
		limit = max(minVolumeMB, h.DiskBytes>>20/4)
	}
	if _, status, err := s.createVolume(ctx, a, volumeRequest{Path: &path, LimitMB: &limit}); err != nil {
		undo()
		if status == http.StatusBadGateway {
			s.coreFailed(w, "create volume", err)
		} else {
			writeError(w, status, err)
		}
		return
	}
	s.Log.Info("database created", "database", a.ID, "engine", a.Image, "user", loginFrom(ctx).account.ID)
	if _, err := s.deploy(ctx, a, store.Deployment{}); err != nil {
		s.fail(w, "deploy", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": a.ID})
}

// newPassword makes a password that needs no escaping in a URL or a shell.
func newPassword() string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 32)
	rand.Read(b)
	for i := range b {
		// 256 is not a multiple of 62; the slight lean to the first few
		// letters costs well under a bit of the password's 190.
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// portOpen reports whether the database accepts connections: that is when
// the apps linked to it can use it.
func (s *Server) portOpen(ctx context.Context, ip netip.Addr, port int) bool {
	if s.PortCheck != nil {
		return s.PortCheck(ctx, ip, port)
	}
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(ip, uint16(port)).String())
	if err != nil {
		return false
	}
	c.Close()
	return true
}

type linkJSON struct {
	DB     string `json:"db"`
	Engine string `json:"engine"`
	Prefix string `json:"prefix"`
	// Vars are the names the app gets.
	Vars      []string  `json:"vars"`
	CreatedAt time.Time `json:"created_at"`
}

type linkRequest struct {
	DB     string  `json:"db"`
	Prefix *string `json:"prefix"`
}

var validPrefix = regexp.MustCompile(`^([A-Z][A-Z0-9_]{0,30})?$`)

func (s *Server) listLinks(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	links, err := s.linksOut(r.Context(), a)
	if err != nil {
		s.fail(w, "list links", err)
		return
	}
	writeJSON(w, http.StatusOK, links)
}

// linksOut lists an app's databases, or for a database the apps linked to
// it.
func (s *Server) linksOut(ctx context.Context, a store.App) ([]linkJSON, error) {
	var links []store.Link
	var err error
	if a.IsDatabase() {
		links, err = s.Store.Links(ctx, "", a.ID)
	} else {
		links, err = s.Store.Links(ctx, a.ID, "")
	}
	if err != nil {
		return nil, err
	}
	out := make([]linkJSON, 0, len(links))
	for _, l := range links {
		db, err := s.Store.App(ctx, l.DBID)
		if err != nil {
			return nil, err
		}
		e, _ := engineOf(db)
		lj := linkJSON{DB: l.DBID, Engine: db.Engine, Prefix: l.Prefix, CreatedAt: l.CreatedAt}
		if a.IsDatabase() {
			lj.DB = l.AppID // the other end
		}
		for _, v := range e.vars {
			lj.Vars = append(lj.Vars, l.Prefix+v.Name)
		}
		out = append(out, lj)
	}
	return out, nil
}

// addLink links an app to a database. The app can reach it at once; its
// variables arrive with the app's next start.
func (s *Server) addLink(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	var req linkRequest
	if !decode(w, r, &req) {
		return
	}
	l := store.Link{AppID: a.ID, DBID: req.DB, CreatedAt: s.now()}
	if req.Prefix != nil {
		l.Prefix = *req.Prefix
	}
	ctx := r.Context()
	if status, err := s.checkLink(ctx, a, l); err != nil {
		writeError(w, status, err)
		return
	}
	switch err := s.Store.CreateLink(ctx, l); {
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, fmt.Errorf("%s is already linked to %s", a.ID, l.DBID))
		return
	case err != nil:
		s.fail(w, "link", err)
		return
	}
	if err := s.syncLinks(ctx, a.ID); err != nil {
		s.Store.DeleteLink(context.WithoutCancel(ctx), a.ID, l.DBID)
		s.coreFailed(w, "open the link", err)
		return
	}
	s.Log.Info("linked", "app", a.ID, "database", l.DBID, "user", loginFrom(ctx).account.ID)
	w.WriteHeader(http.StatusCreated)
}

// updateLink changes the prefix of a link's variables.
func (s *Server) updateLink(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	var req linkRequest
	if !decode(w, r, &req) {
		return
	}
	l := store.Link{AppID: a.ID, DBID: r.PathValue("db")}
	if req.Prefix != nil {
		l.Prefix = *req.Prefix
	}
	if status, err := s.checkLink(r.Context(), a, l); err != nil {
		writeError(w, status, err)
		return
	}
	switch err := s.Store.UpdateLink(r.Context(), l); {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, errors.New("no such link"))
	case err != nil:
		s.fail(w, "update link", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// deleteLink unlinks. The app cannot reach the database from then on,
// running or not.
func (s *Server) deleteLink(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	switch err := s.Store.DeleteLink(ctx, a.ID, r.PathValue("db")); {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, errors.New("no such link"))
		return
	case err != nil:
		s.fail(w, "unlink", err)
		return
	}
	if err := s.syncLinks(ctx, a.ID); err != nil {
		s.coreFailed(w, "close the link", err)
		return
	}
	s.Log.Info("unlinked", "app", a.ID, "database", r.PathValue("db"), "user", loginFrom(ctx).account.ID)
	w.WriteHeader(http.StatusNoContent)
}

// checkLink checks a new or changed link: the target is a database, and the
// variables it brings clash with no other variable of the app.
func (s *Server) checkLink(ctx context.Context, a store.App, l store.Link) (int, error) {
	if a.IsDatabase() {
		return http.StatusBadRequest, errors.New("a database cannot be linked to another")
	}
	if !validPrefix.MatchString(l.Prefix) {
		return http.StatusBadRequest, errors.New("the prefix must be capital letters, digits and underscores, starting with a letter")
	}
	db, err := s.Store.App(ctx, l.DBID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !db.IsDatabase()) {
		return http.StatusBadRequest, fmt.Errorf("there is no database named %q", l.DBID)
	}
	if err != nil {
		return http.StatusInternalServerError, err
	}
	taken := map[string]string{}
	vars, err := s.Store.Env(ctx, a.ID)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	for _, v := range vars {
		taken[v.Name] = "one of the app's own variables"
	}
	links, err := s.Store.Links(ctx, a.ID, "")
	if err != nil {
		return http.StatusInternalServerError, err
	}
	for _, other := range links {
		if other.DBID == l.DBID {
			continue
		}
		od, err := s.Store.App(ctx, other.DBID)
		if err != nil {
			return http.StatusInternalServerError, err
		}
		oe, _ := engineOf(od)
		for _, v := range oe.vars {
			taken[other.Prefix+v.Name] = "a variable of " + other.DBID
		}
	}
	e, _ := engineOf(db)
	for _, v := range e.vars {
		if what, ok := taken[l.Prefix+v.Name]; ok {
			return http.StatusConflict, fmt.Errorf("%s is already %s; give this link a prefix, such as %s_", l.Prefix+v.Name, what, prefixFor(l.DBID))
		}
	}
	return 0, nil
}

// prefixFor suggests a prefix from a database's name.
func prefixFor(db string) string {
	b := []byte(db)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z':
			b[i] = c - 'a' + 'A'
		case c == '-':
			b[i] = '_'
		}
	}
	if len(b) == 0 || b[0] < 'A' || b[0] > 'Z' {
		b = append([]byte("DB_"), b...)
	}
	return string(b)
}

// syncLinks tells the core which databases the app may reach.
func (s *Server) syncLinks(ctx context.Context, appID string) error {
	links, err := s.Store.Links(ctx, appID, "")
	if err != nil {
		return err
	}
	out := make([]engine.Link, 0, len(links))
	for _, l := range links {
		db, err := s.Store.App(ctx, l.DBID)
		if err != nil {
			return err
		}
		out = append(out, engine.Link{Name: db.ID, To: db.ID, Port: uint16(db.Port)})
	}
	return s.Core.SetLinks(ctx, appID, out)
}

// linkedEnv returns the variables an app gets from its databases: plain
// ones, and the ones the core makes from a database's password.
func (s *Server) linkedEnv(ctx context.Context, app store.App) (env []string, linked []core.LinkedVar, err error) {
	links, err := s.Store.Links(ctx, app.ID, "")
	if err != nil {
		return nil, nil, err
	}
	for _, l := range links {
		db, err := s.Store.App(ctx, l.DBID)
		if err != nil {
			return nil, nil, err
		}
		e, _ := engineOf(db)
		vars, err := s.Store.Env(ctx, db.ID)
		if err != nil {
			return nil, nil, err
		}
		sealed := ""
		for _, v := range vars {
			if v.Name == e.PasswordVar && v.Secret {
				sealed = v.Value
			}
		}
		if sealed == "" {
			return nil, nil, fmt.Errorf("the database %s has no password", db.ID)
		}
		for _, v := range e.vars {
			value := strings.ReplaceAll(v.Value, "{host}", db.ID)
			if strings.Contains(value, "{secret}") {
				linked = append(linked, core.LinkedVar{Name: l.Prefix + v.Name, Template: value, From: db.ID, Sealed: sealed})
			} else {
				env = append(env, l.Prefix+v.Name+"="+value)
			}
		}
	}
	return env, linked, nil
}

// unlinkAll removes every link to or from an app being deleted.
func (s *Server) unlinkAll(ctx context.Context, a store.App) error {
	if !a.IsDatabase() {
		return s.Core.SetLinks(ctx, a.ID, nil)
	}
	links, err := s.Store.Links(ctx, "", a.ID)
	if err != nil {
		return err
	}
	for _, l := range links {
		if err := s.Store.DeleteLink(ctx, l.AppID, a.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := s.syncLinks(ctx, l.AppID); err != nil {
			return err
		}
	}
	return nil
}

// syncAllLinks tells the core every app's links when the panel starts, in
// case the two went apart, such as after restoring the panel's database.
func (s *Server) syncAllLinks(ctx context.Context) {
	apps, err := s.Store.Apps(ctx)
	if err != nil {
		s.Log.Error("links: list apps", "err", err)
		return
	}
	for _, a := range apps {
		if a.IsDatabase() {
			continue
		}
		if err := s.syncLinks(ctx, a.ID); err != nil {
			s.Log.Error("links: sync", "app", a.ID, "err", err)
		}
	}
}
