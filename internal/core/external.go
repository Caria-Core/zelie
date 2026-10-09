package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/containerd/errdefs"
)

// External access lets a desktop tool such as TablePlus reach a database
// through an SSH tunnel. The core listens on a loopback port and passes
// each connection on to the database's container. Nothing listens beyond
// loopback: SSH is the door, and the database's own user the lock.

// ExternalUser is the database user desktop tools sign in as. It is not
// the apps' user, so its password can change or go without touching them.
const ExternalUser = "zelie_external"

var (
	validExternalPassword = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9]{32,64}$`) })

	errPortTaken = msg.Define(http.StatusConflict, "external.port_taken", "Port {port} is already in use on this server.")
	errNoRoot    = msg.Define(http.StatusConflict, "external.no_root", "This database was made before outside access existed. Back it up, make a new one and restore the backup into it.")
)

// ExternalListener is one loopback port and where it leads.
type ExternalListener struct {
	App    string `json:"app"`
	Port   int    `json:"port"`   // on 127.0.0.1
	Target int    `json:"target"` // the database's port in its container
}

// External keeps the loopback listeners, and saves them so they come back
// when the core restarts.
type External struct {
	path string
	log  *slog.Logger
	// find returns the address of the app's running container.
	find func(ctx context.Context, app string) (netip.Addr, error)

	mu   sync.Mutex
	open map[string]*forward
}

type forward struct {
	ExternalListener
	ln    net.Listener
	mu    sync.Mutex
	conns map[net.Conn]bool
}

// LoadExternal reads the saved listeners. Start opens them.
func LoadExternal(path string) (*External, []ExternalListener, error) {
	x := &External{path: path, open: map[string]*forward{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return x, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var list []ExternalListener
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return x, list, nil
}

// Start opens the saved listeners. One that cannot open is logged and left
// for the panel to set again.
func (x *External) Start(list []ExternalListener) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, l := range list {
		if err := x.listen(l); err != nil {
			x.log.Error("external access", "app", l.App, "port", l.Port, "err", err)
		}
	}
}

// Set opens or moves an app's listener.
func (x *External) Set(l ExternalListener) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if f, ok := x.open[l.App]; ok {
		if f.ExternalListener == l {
			return nil
		}
		f.close()
		delete(x.open, l.App)
	}
	if err := x.listen(l); err != nil {
		x.save()
		return err
	}
	return x.save()
}

// Remove closes an app's listener and the connections through it.
func (x *External) Remove(app string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if f, ok := x.open[app]; ok {
		f.close()
		delete(x.open, app)
	}
	return x.save()
}

// Sync makes the listeners exactly list, which the panel sends when it
// starts.
func (x *External) Sync(list []ExternalListener) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	want := map[string]ExternalListener{}
	for _, l := range list {
		want[l.App] = l
	}
	for app, f := range x.open {
		if w, ok := want[app]; !ok || w != f.ExternalListener {
			f.close()
			delete(x.open, app)
		}
	}
	var errs []error
	for _, l := range list {
		if _, ok := x.open[l.App]; !ok {
			if err := x.listen(l); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", l.App, err))
			}
		}
	}
	return errors.Join(append(errs, x.save())...)
}

func (x *External) listen(l ExternalListener) error {
	ln, err := net.Listen("tcp", netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(l.Port)).String())
	if errors.Is(err, syscall.EADDRINUSE) {
		return errPortTaken.Err("port", l.Port)
	}
	if err != nil {
		return err
	}
	f := &forward{ExternalListener: l, ln: ln, conns: map[net.Conn]bool{}}
	x.open[l.App] = f
	go x.serve(f)
	return nil
}

func (x *External) serve(f *forward) {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		if !f.track(c, true) {
			c.Close()
			return
		}
		go func() {
			defer f.track(c, false)
			defer c.Close()
			if err := x.pass(c, f.ExternalListener); err != nil {
				x.log.Warn("external connection", "app", f.App, "err", err)
			}
		}()
	}
}

// pass joins a connection to the database, both ways, until either ends.
func (x *External) pass(c net.Conn, l ExternalListener) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ip, err := x.find(ctx, l.App)
	if err != nil {
		return err
	}
	var d net.Dialer
	db, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(ip, uint16(l.Target)).String())
	if err != nil {
		return err
	}
	defer db.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(db, c); db.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
	go func() { io.Copy(c, db); c.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
	<-done
	<-done
	return nil
}

// track adds or drops a connection; it refuses new ones once closed.
func (f *forward) track(c net.Conn, add bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !add {
		delete(f.conns, c)
		return true
	}
	if f.conns == nil {
		return false
	}
	f.conns[c] = true
	return true
}

func (f *forward) close() {
	f.ln.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.conns {
		c.Close()
	}
	f.conns = nil
}

func (x *External) save() error {
	list := make([]ExternalListener, 0, len(x.open))
	for _, f := range x.open {
		list = append(list, f.ExternalListener)
	}
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp := x.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, x.path)
}

// runningIP is where the app's running container can be reached.
func (s *Server) runningIP(ctx context.Context, app string) (netip.Addr, error) {
	list, err := s.Engine.List(ctx)
	if err != nil {
		return netip.Addr{}, err
	}
	for _, c := range list {
		if c.App == app && c.State == "running" && c.IP.IsValid() {
			return c.IP, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("%s is not running", app)
}

// StartExternal opens the saved listeners once the server can find
// containers.
func (s *Server) StartExternal(list []ExternalListener) {
	s.External.log = s.Log
	s.External.find = s.runningIP
	s.External.Start(list)
}

type externalRequest struct {
	Engine    string `json:"engine"`
	Container string `json:"container"`
	Port      int    `json:"port"`
	Target    int    `json:"target"`
	// Password is the external user's, sealed for the app. It is only
	// sent to set the user up or change the password.
	Password string `json:"password,omitempty"`
}

// setExternal makes the external user, or changes its password, and opens
// the loopback port.
func (s *Server) setExternal(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	var req externalRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	l := ExternalListener{App: app, Port: req.Port, Target: req.Target}
	if err := checkListener(l); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx := r.Context()
	if req.Password != "" {
		if err := s.containerOf(ctx, app, req.Container); err != nil {
			s.backupFailed(w, "set up external access", app, err)
			return
		}
		password, err := s.openPassword(app, req.Password)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.externalUser(ctx, req.Engine, req.Container, password); err != nil {
			s.backupFailed(w, "set up external access", app, err)
			return
		}
	}
	if err := s.External.Set(l); err != nil {
		s.backupFailed(w, "open external port", app, err)
		return
	}
	s.Log.Info("external access set", "app", app, "port", l.Port, "user", req.Password != "")
	w.WriteHeader(http.StatusNoContent)
}

// removeExternal closes the port and drops the external user. The user is
// only dropped from a running database; one that is stopped keeps it until
// it is set up again, which resets the password.
func (s *Server) removeExternal(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	var req externalRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.External.Remove(app); err != nil {
		s.fail(w, "close external port", app, err)
		return
	}
	if req.Container != "" {
		ctx := r.Context()
		if err := s.containerOf(ctx, app, req.Container); err == nil {
			if err := s.externalUser(ctx, req.Engine, req.Container, ""); err != nil {
				s.backupFailed(w, "remove external user", app, err)
				return
			}
		}
	}
	s.Log.Info("external access removed", "app", app)
	w.WriteHeader(http.StatusNoContent)
}

type externalSync struct {
	Listeners []ExternalListener `json:"listeners"`
}

func (s *Server) syncExternal(w http.ResponseWriter, r *http.Request) {
	var req externalSync
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	for _, l := range req.Listeners {
		if err := checkListener(l); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	// The listeners that could open are open. The panel hears about the
	// others rather than a success, since a port something else holds would
	// otherwise look like the database's.
	if err := s.External.Sync(req.Listeners); err != nil {
		var me *msg.Error
		if errors.As(err, &me) {
			s.Log.Error("sync external access", "err", err)
			writeError(w, me.Status, err)
			return
		}
		s.fail(w, "sync external access", "", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Loopback ports below 1024 need no one's; a panel asking for one is
// confused.
func checkListener(l ExternalListener) error {
	switch {
	case !engine.ValidID(l.App):
		return fmt.Errorf("invalid app %q", l.App)
	case l.Port < 1024 || l.Port > 65535, l.Target < 1 || l.Target > 65535:
		return errors.New("invalid port")
	}
	return nil
}

func (s *Server) openPassword(app, sealed string) (string, error) {
	if s.Secrets == nil {
		return "", errors.New("this core has no secret key")
	}
	kv, err := s.Secrets.Open(sealed, app)
	if err != nil {
		return "", err
	}
	_, password, _ := strings.Cut(kv, "=")
	if !validExternalPassword().MatchString(password) {
		return "", errors.New("the password must be 32 to 64 letters and digits")
	}
	return password, nil
}

// externalUser makes the external user with password, or drops it when
// password is empty. The password is only letters and digits, so it needs
// no escaping in any of the languages below.
func (s *Server) externalUser(ctx context.Context, kind, container, password string) error {
	var args []string
	var script string
	switch kind {
	case "postgres":
		// The user signs in as itself but works as app, so what it makes
		// belongs to app and a backup restores without it.
		args = []string{"psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", "app", "-d", "app"}
		if password != "" {
			script = `DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '` + ExternalUser + `') THEN CREATE ROLE ` + ExternalUser + `; END IF; END $$;
ALTER ROLE ` + ExternalUser + ` WITH LOGIN PASSWORD '` + password + `';
GRANT app TO ` + ExternalUser + `;
ALTER ROLE ` + ExternalUser + ` SET role = 'app';
`
		} else {
			script = `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = '` + ExternalUser + `';
DO $$ BEGIN IF EXISTS (SELECT FROM pg_roles WHERE rolname = '` + ExternalUser + `') THEN
	REASSIGN OWNED BY ` + ExternalUser + ` TO app; DROP OWNED BY ` + ExternalUser + `; DROP ROLE ` + ExternalUser + `;
END IF; END $$;
`
		}
	case "mariadb":
		// The app's user cannot make users; root can, from inside the
		// container only.
		args = []string{"sh", "-c", `[ -n "$MARIADB_ROOT_PASSWORD" ] || exit 3
export MYSQL_PWD="$MARIADB_ROOT_PASSWORD"
if [ "$1" = drop ]; then
	mariadb -u root -N -B -e "SELECT ID FROM information_schema.PROCESSLIST WHERE USER = '` + ExternalUser + `'" |
		while read -r id; do mariadb -u root -e "KILL $id" || true; done
fi
exec mariadb -u root`, "sh", map[bool]string{true: "set", false: "drop"}[password != ""]}
		if password != "" {
			script = "CREATE USER IF NOT EXISTS '" + ExternalUser + "'@'%' IDENTIFIED BY '" + password + "';\n" +
				"ALTER USER '" + ExternalUser + "'@'%' IDENTIFIED BY '" + password + "';\n" +
				"GRANT ALL PRIVILEGES ON `app`.* TO '" + ExternalUser + "'@'%';\n"
		} else {
			script = "DROP USER IF EXISTS '" + ExternalUser + "'@'%';\n"
		}
	case "redis":
		// The user also comes back from the server's command line, which
		// the panel sets with the password; this makes it live now.
		// Deleting a user closes its connections.
		args = []string{"sh", "-c", `REDISCLI_AUTH="$REDIS_PASSWORD" exec redis-cli --no-auth-warning`}
		if password != "" {
			script = "ACL SETUSER " + ExternalUser + " reset on >" + password + " ~* &* +@all\n"
		} else {
			script = "ACL DELUSER " + ExternalUser + "\n"
		}
	default:
		return fmt.Errorf("no external access for %q: %w", kind, errdefs.ErrInvalidArgument)
	}
	var out strings.Builder
	code, err := s.Engine.Exec(ctx, container, args, strings.NewReader(script), &out, &out)
	if err != nil {
		return err
	}
	if kind == "mariadb" && code == 3 {
		return errNoRoot.Err()
	}
	// psql quotes the statement it failed on; the password stays here.
	text := strings.TrimSpace(out.String())
	if password != "" {
		text = strings.ReplaceAll(text, password, "…")
	}
	if code != 0 || (kind == "redis" && !redisDone(text)) {
		return fmt.Errorf("%s exited with %s: %s", args[0], strconv.Itoa(int(code)), text)
	}
	return nil
}

// redisDone reports whether redis-cli's replies are all fine. Reading
// commands from stdin, it exits 0 even when one fails.
func redisDone(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if _, err := strconv.Atoi(line); line != "OK" && err != nil {
			return false
		}
	}
	return true
}
