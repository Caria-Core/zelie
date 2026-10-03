// Command upgrade checks that updating Zelie keeps what a server already
// holds. CI installs the last release, runs "upgrade seed" to give it an
// administrator, an app, a database and a backup, puts the new binary in
// place the way the panel's update does, and runs "upgrade check". After
// that, "upgrade game" runs a real game server on the updated panel and
// reads its files over SFTP.
//
// It talks to the panel over HTTP as a person would, through the proxy's
// tunnel port, so it needs a tunnel-mode install.
package main

import (
	"bytes"
	"encoding/base32"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/auth"
	"github.com/Caria-Core/zelie/internal/update"
)

const (
	panelHost = "panel.upgrade.test"
	appHost   = "web.upgrade.test"
	email     = "admin@upgrade.test"
	// The app writes when it first started to its volume, and serves that
	// with a secret variable and the scheme of the database it is linked
	// to. After the update all three must read the same.
	startCommand = `[ -s /data/first ] || date +%s%N > /data/first; mkdir -p /data/www; ` +
		`printf '%s %s %s' "$GREETING" "$(cat /data/first)" "${DATABASE_URL%%:*}" > /data/www/index.html; ` +
		`exec httpd -f -p 8080 -h /data/www`
	row = "written before the update"
)

// state is what seed leaves for check.
type state struct {
	Password   string `json:"password"`
	TOTPSecret string `json:"totp_secret"`
	LastStep   int64  `json:"last_step"`
	Page       string `json:"page"`
	Backup     int64  `json:"backup"`
	DBPort     int    `json:"db_port"`
	DBUser     string `json:"db_user"`
	DBName     string `json:"db_name"`
	DBPassword string `json:"db_password"`
	// HostKey is the fingerprint of the SFTP host key before the update,
	// when the old release had SFTP. Clients trusted it, so it must stay.
	HostKey string `json:"host_key,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: upgrade seed|check|game [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	stateFile := fs.String("state", "upgrade-state.json", "where seed leaves what check needs")
	base := fs.String("url", "http://127.0.0.1:8480", "the proxy's tunnel port")
	want := fs.String("want", "", "the version check expects to run")
	fs.Parse(os.Args[2:])

	c := &client{base: *base, cookies: map[string]string{}}
	var err error
	switch os.Args[1] {
	case "seed":
		err = seed(c, *stateFile)
	case "check":
		err = check(c, *stateFile, *want)
	case "game":
		err = game(c, *stateFile)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "upgrade:", err)
		os.Exit(1)
	}
}

func seed(c *client, stateFile string) error {
	st := &state{Password: "upgrade-" + fmt.Sprint(time.Now().UnixNano())}
	c.st = st

	out, err := exec.Command("zelie", "setup-link").CombinedOutput()
	if err != nil {
		return fmt.Errorf("setup-link: %v: %s", err, out)
	}
	m := regexp.MustCompile(`/setup#(\S+)`).FindSubmatch(out)
	if m == nil {
		return fmt.Errorf("no token in %q", out)
	}
	step("create the administrator")
	if err := c.do("POST", "/api/setup", map[string]string{"token": string(m[1]), "email": email, "password": st.Password}, nil); err != nil {
		return err
	}
	var totp struct {
		Secret string `json:"secret"`
	}
	if err := c.do("POST", "/api/2fa/totp/new", nil, &totp); err != nil {
		return err
	}
	st.TOTPSecret = totp.Secret
	if err := c.do("POST", "/api/2fa/totp", map[string]string{"code": c.code()}, nil); err != nil {
		return err
	}

	step("create a PostgreSQL database")
	if err := c.do("POST", "/api/databases", map[string]string{"id": "db", "engine": "postgres"}, nil); err != nil {
		return err
	}
	if err := waitFor("the database to run", 3*time.Minute, func() error {
		var a struct {
			State string `json:"state"`
		}
		if err := c.do("GET", "/api/apps/db", nil, &a); err != nil {
			return err
		}
		if a.State != "running" {
			return fmt.Errorf("state %s", a.State)
		}
		return nil
	}); err != nil {
		return err
	}
	step("reach it from outside and write a row")
	if err := c.confirm(); err != nil {
		return err
	}
	var ext struct {
		Port     int    `json:"port"`
		User     string `json:"user"`
		Database string `json:"database"`
		Password string `json:"password"`
	}
	if err := c.do("POST", "/api/apps/db/external", nil, &ext); err != nil {
		return err
	}
	st.DBPort, st.DBUser, st.DBName, st.DBPassword = ext.Port, ext.User, ext.Database, ext.Password
	if _, err := psql(st, "CREATE TABLE notes (body text); INSERT INTO notes VALUES ('"+row+"')"); err != nil {
		return err
	}

	step("create an app with a volume, a secret and a link")
	app := map[string]any{
		"id": "web", "source": "image", "image": "busybox:1.37", "port": 8080, "domain": appHost,
		"start_command": startCommand,
		"volumes":       []map[string]any{{"path": "/data", "limit_mb": 64}},
	}
	if err := c.do("POST", "/api/apps", app, nil); err != nil {
		return err
	}
	greeting := "sealed-" + fmt.Sprint(time.Now().UnixNano())
	if err := c.do("PUT", "/api/apps/web/env", []map[string]any{{"name": "GREETING", "value": greeting, "secret": true}}, nil); err != nil {
		return err
	}
	if err := c.do("POST", "/api/apps/web/links", map[string]string{"db": "db"}, nil); err != nil {
		return err
	}
	var dep struct {
		ID int64 `json:"id"`
	}
	if err := c.do("POST", "/api/apps/web/deployments", nil, &dep); err != nil {
		return err
	}
	if err := c.waitLive("web", dep.ID); err != nil {
		return err
	}
	page, err := c.page()
	if err != nil {
		return err
	}
	if f := strings.Fields(page); len(f) != 3 || f[0] != greeting || !strings.HasPrefix(f[2], "postgres") {
		return fmt.Errorf("the app serves %q", page)
	}
	st.Page = page

	step("back up the database")
	if err := c.do("POST", "/api/apps/db/backups", nil, nil); err != nil {
		return err
	}
	if err := waitFor("the backup", 3*time.Minute, func() error {
		var b struct {
			Backups []struct {
				ID    int64  `json:"id"`
				State string `json:"state"`
			} `json:"backups"`
		}
		if err := c.do("GET", "/api/apps/db/backups", nil, &b); err != nil {
			return err
		}
		for _, x := range b.Backups {
			if x.State == "done" {
				st.Backup = x.ID
				return nil
			}
		}
		return errors.New("not done")
	}); err != nil {
		return err
	}

	if st.HostKey, err = oldHostKey(); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(stateFile, b, 0o600)
}

func check(c *client, stateFile, want string) error {
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return err
	}
	st := &state{}
	if err := json.Unmarshal(b, st); err != nil {
		return err
	}
	c.st = st

	step("the update finished")
	last, err := update.Last("")
	if err != nil {
		return err
	}
	if !last.OK || last.To != want {
		return fmt.Errorf("last update: %+v", last)
	}

	step("log in again")
	if err := c.login(); err != nil {
		return err
	}
	var server struct {
		Version string `json:"version"`
	}
	if err := c.do("GET", "/api/server", nil, &server); err != nil {
		return err
	}
	if server.Version != want {
		return fmt.Errorf("the panel runs %s, not %s", server.Version, want)
	}

	step("the app and the database run images pinned by digest")
	// The panel pins what older versions left unpinned once the core is up,
	// asking the registry, so it may take a moment after the update.
	for _, app := range []string{"web", "db"} {
		if err := waitFor(app+" to run a pinned image", time.Minute, func() error { return c.livePinned(app) }); err != nil {
			return err
		}
	}

	step("the app serves what it did")
	if err := c.samePage(); err != nil {
		return err
	}
	step("the new version deploys it again")
	var dep struct {
		ID int64 `json:"id"`
	}
	if err := c.do("POST", "/api/apps/web/restart", nil, &dep); err != nil {
		return err
	}
	if err := c.waitLive("web", dep.ID); err != nil {
		return err
	}
	if err := c.samePage(); err != nil {
		return err
	}

	step("the database kept its row and its outside access")
	if err := hasRow(st); err != nil {
		return err
	}
	step("the backup made before the update restores")
	if _, err := psql(st, "DELETE FROM notes"); err != nil {
		return err
	}
	if err := c.confirm(); err != nil {
		return err
	}
	if err := c.do("POST", fmt.Sprintf("/api/backups/%d/restore", st.Backup), nil, nil); err != nil {
		return err
	}
	if err := waitFor("the restore", 3*time.Minute, func() error {
		var b struct {
			Restore *struct {
				Backup int64           `json:"backup"`
				State  string          `json:"state"`
				Error  json.RawMessage `json:"error"`
			} `json:"restore"`
		}
		if err := c.do("GET", "/api/apps/db/backups", nil, &b); err != nil {
			return err
		}
		switch {
		case b.Restore == nil || b.Restore.Backup != st.Backup:
			return errors.New("not started")
		case b.Restore.State == "failed":
			return stop{fmt.Errorf("restore failed: %s", b.Restore.Error)}
		case b.Restore.State != "done":
			return errors.New(b.Restore.State)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := waitFor("the restored row", time.Minute, func() error { return hasRow(st) }); err != nil {
		return err
	}
	if err := c.upgradeDatabase(); err != nil {
		return err
	}
	return c.metrics()
}

// metrics checks the app's readings: the requests the proxy counted and
// the traffic its container saw.
func (c *client) metrics() error {
	step("the app's requests and traffic are measured")
	return waitFor("a reading with requests", 3*time.Minute, func() error {
		if _, err := c.page(); err != nil {
			return err
		}
		var m struct {
			Points []struct {
				Requests int64 `json:"requests"`
				RxBytes  int64 `json:"rx_bytes"`
				TxBytes  int64 `json:"tx_bytes"`
				Memory   int64 `json:"memory_bytes"`
			} `json:"points"`
		}
		if err := c.do("GET", "/api/apps/web/metrics", nil, &m); err != nil {
			return err
		}
		for _, p := range m.Points {
			if p.Requests > 0 && p.RxBytes > 0 && p.TxBytes > 0 && p.Memory > 0 {
				return nil
			}
		}
		return fmt.Errorf("readings %+v", m.Points)
	})
}

// upgradeDatabase moves a PostgreSQL 17 database to 18 the way the panel
// does, and checks its rows came along.
func (c *client) upgradeDatabase() error {
	step("a PostgreSQL 17 database upgrades to 18 with its rows")
	if err := c.do("POST", "/api/databases", map[string]string{"id": "old", "engine": "postgres", "version": "17"}, nil); err != nil {
		return err
	}
	if err := c.waitRunning("old"); err != nil {
		return err
	}
	db, err := c.external("old")
	if err != nil {
		return err
	}
	if _, err := psql(db, "CREATE TABLE notes (body text); INSERT INTO notes VALUES ('"+row+"')"); err != nil {
		return err
	}
	if err := c.confirm(); err != nil {
		return err
	}
	var dep struct {
		ID int64 `json:"id"`
	}
	if err := c.do("POST", "/api/apps/old/upgrade", map[string]string{"version": "18"}, &dep); err != nil {
		return err
	}
	if err := c.waitLive("old", dep.ID); err != nil {
		return err
	}
	// Its user for outside access was in the old files.
	if db, err = c.external("old"); err != nil {
		return err
	}
	version, err := psql(db, "SHOW server_version_num")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(version), "18") {
		return fmt.Errorf("the database runs %s after the upgrade", version)
	}
	if err := hasRow(db); err != nil {
		return err
	}
	var kept []struct {
		Version string `json:"version"`
	}
	if err := c.do("GET", "/api/apps/old/kept-volumes", nil, &kept); err != nil {
		return err
	}
	if len(kept) != 1 || kept[0].Version != "17" {
		return fmt.Errorf("kept volumes %+v", kept)
	}
	return nil
}

// login signs in as the administrator seed made.
func (c *client) login() error {
	if err := c.do("POST", "/api/login", map[string]string{"email": email, "password": c.st.Password}, nil); err != nil {
		return err
	}
	return c.do("POST", "/api/login/totp", map[string]string{"code": c.code()}, nil)
}

func (c *client) waitRunning(app string) error {
	return waitFor(app+" to run", 3*time.Minute, func() error {
		var a struct {
			State string `json:"state"`
		}
		if err := c.do("GET", "/api/apps/"+app, nil, &a); err != nil {
			return err
		}
		if a.State != "running" {
			return fmt.Errorf("state %s", a.State)
		}
		return nil
	})
}

// external turns on outside access to a database, or gives it a new
// password, and returns how to sign in.
func (c *client) external(app string) (*state, error) {
	if err := c.confirm(); err != nil {
		return nil, err
	}
	var ext struct {
		Port     int    `json:"port"`
		User     string `json:"user"`
		Database string `json:"database"`
		Password string `json:"password"`
	}
	if err := c.do("POST", "/api/apps/"+app+"/external", nil, &ext); err != nil {
		return nil, err
	}
	return &state{DBPort: ext.Port, DBUser: ext.User, DBName: ext.Database, DBPassword: ext.Password}, nil
}

func hasRow(st *state) error {
	out, err := psql(st, "SELECT body FROM notes")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != row {
		return fmt.Errorf("the table holds %q", out)
	}
	return nil
}

// psql runs SQL through the database's outside access, as a desktop client
// would through an SSH tunnel.
func psql(st *state, sql string) (string, error) {
	cmd := exec.Command("psql", "-X", "-q", "-t", "-A", "-v", "ON_ERROR_STOP=1",
		"-h", "127.0.0.1", "-p", fmt.Sprint(st.DBPort), "-U", st.DBUser, "-d", st.DBName, "-c", sql)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+st.DBPassword, "PGCONNECT_TIMEOUT=5")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("psql: %v: %s", err, out)
	}
	return string(out), nil
}

type client struct {
	base    string
	cookies map[string]string
	st      *state
}

// do sends a request to the panel and decodes the answer into out.
func (c *client) do(method, path string, in, out any) error {
	b, err := c.send(method, path, in)
	if err != nil {
		return err
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// send sends a request to the panel and returns the answer's body.
func (c *client) send(method, path string, in any) ([]byte, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, body)
	req.Host = panelHost
	req.Header.Set("Content-Type", "application/json")
	// The session cookie is only for HTTPS, which the tunnel would add in
	// front of the proxy; a cookie jar would not send it here.
	for k, v := range c.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	for _, ck := range resp.Cookies() {
		c.cookies[ck.Name] = ck.Value
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, b)
	}
	return b, nil
}

// code plays the authenticator app. The panel takes each step's code once
// and accepts the next step's early, so every call moves one step on.
func (c *client) code() string {
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(c.st.TOTPSecret)
	if err != nil {
		panic(err)
	}
	s := max(c.st.LastStep+1, time.Now().Unix()/30)
	for s > time.Now().Unix()/30+1 {
		time.Sleep(time.Second)
	}
	c.st.LastStep = s
	return auth.TOTPCode(secret, s)
}

// confirm gives the second step again, as the panel asks before sensitive
// changes.
func (c *client) confirm() error {
	return c.do("POST", "/api/confirm", map[string]string{"totp": c.code()}, nil)
}

func (c *client) waitLive(app string, id int64) error {
	return waitFor("deployment "+fmt.Sprint(id), 3*time.Minute, func() error {
		var a struct {
			Deployments []struct {
				ID    int64           `json:"id"`
				State string          `json:"state"`
				Error json.RawMessage `json:"error"`
			} `json:"deployments"`
		}
		if err := c.do("GET", "/api/apps/"+app, nil, &a); err != nil {
			return err
		}
		for _, d := range a.Deployments {
			if d.ID != id {
				continue
			}
			switch d.State {
			case "live":
				return nil
			case "failed":
				return stop{fmt.Errorf("deployment %d failed: %s", id, d.Error)}
			}
			return errors.New(d.State)
		}
		return errors.New("not listed")
	})
}

// livePinned checks that a restart of app would start exactly what runs
// now, not whatever its tag points at by then.
func (c *client) livePinned(app string) error {
	var a struct {
		Deployments []struct {
			State string `json:"state"`
			Image string `json:"image"`
		} `json:"deployments"`
	}
	if err := c.do("GET", "/api/apps/"+app, nil, &a); err != nil {
		return err
	}
	for _, d := range a.Deployments {
		if d.State == "live" {
			if !strings.Contains(d.Image, "@sha256:") {
				return fmt.Errorf("%s runs %s, not pinned", app, d.Image)
			}
			return nil
		}
	}
	return fmt.Errorf("%s has no live deployment", app)
}

// page is what the app serves through the proxy.
func (c *client) page() (string, error) {
	req, _ := http.NewRequest("GET", c.base+"/", nil)
	req.Host = appHost
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, b)
	}
	return string(b), nil
}

func (c *client) samePage() error {
	return waitFor("the app's page", time.Minute, func() error {
		page, err := c.page()
		if err != nil {
			return err
		}
		if page != c.st.Page {
			return fmt.Errorf("it serves %q, and served %q", page, c.st.Page)
		}
		return nil
	})
}

// stop ends a wait early.
type stop struct{ error }

func waitFor(what string, limit time.Duration, try func() error) error {
	until := time.Now().Add(limit)
	for {
		err := try()
		if err == nil {
			return nil
		}
		if s, ok := err.(stop); ok {
			return s.error
		}
		if time.Now().After(until) {
			return fmt.Errorf("waiting for %s: %w", what, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func step(s string) { fmt.Println("==", s) }
