package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/msg"
)

// App sources.
const (
	SourceImage  = "image"
	SourceGitHub = "github"
)

// App is something Zelie keeps running.
type App struct {
	ID       string
	Source   string
	Image    string // for SourceImage
	Repo     string // owner/name, for SourceGitHub
	Branch   string
	Port     int
	Domain   string
	MemoryMB int64
	CPUs     float64
	// AutoDeploy deploys each push to the branch, for SourceGitHub.
	AutoDeploy bool
	// HealthPath is what the health check asks for, when the app has a
	// domain.
	HealthPath string
	// TestCommand runs the app's tests; empty means none. TestSet is false
	// until it has been decided, by a build's suggestion or by the user.
	TestCommand string
	TestSet     bool
	// Stopped is set when the user stopped the app.
	Stopped bool
	// The user's own commands; empty means what the build chose.
	BuildCommand string
	StartCommand string
	// Detected is what the last build chose.
	Detected Detected
	// RestartPulls makes a restart build the branch's newest commit.
	RestartPulls bool
	// Engine is set for a database: postgres, mariadb or redis, in the
	// major version EngineVersion.
	Engine        string
	EngineVersion string
	// Kind is KindApp or KindGame. A database is a KindApp with an Engine.
	Kind      string
	CreatedAt time.Time
}

// What an app is.
const (
	KindApp  = "app"
	KindGame = "game"
)

// IsDatabase reports whether the app is one of Zelie's databases.
func (a App) IsDatabase() bool { return a.Engine != "" }

// IsGame reports whether the app is a game server.
func (a App) IsGame() bool { return a.Kind == KindGame }

// Detected is how the last build built an app.
type Detected struct {
	Builder string `json:"builder,omitempty"` // dockerfile or railpack
	Build   string `json:"build,omitempty"`
	Start   string `json:"start,omitempty"`
}

// ErrExists is returned when a name or domain is already taken.
var ErrExists = errors.New("already exists")

const appColumns = "id, source, image, repo, branch, port, domain, memory_mb, cpus, auto_deploy, health_path, test_command, stopped, build_command, start_command, detected, restart_pulls, engine, engine_version, kind, created_at"

func scanApp(row scanner) (App, error) {
	var a App
	var created int64
	var test sql.NullString
	var detected string
	err := row.Scan(&a.ID, &a.Source, &a.Image, &a.Repo, &a.Branch, &a.Port, &a.Domain, &a.MemoryMB, &a.CPUs, &a.AutoDeploy, &a.HealthPath, &test, &a.Stopped,
		&a.BuildCommand, &a.StartCommand, &detected, &a.RestartPulls, &a.Engine, &a.EngineVersion, &a.Kind, &created)
	a.CreatedAt = time.Unix(created, 0)
	json.Unmarshal([]byte(detected), &a.Detected)
	a.TestCommand, a.TestSet = test.String, test.Valid
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func (s *Store) CreateApp(ctx context.Context, a App) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO apps ("+appColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		a.ID, a.Source, a.Image, a.Repo, a.Branch, a.Port, a.Domain, a.MemoryMB, a.CPUs, a.AutoDeploy, a.HealthPath, a.testColumn(), a.Stopped,
		a.BuildCommand, a.StartCommand, "{}", a.RestartPulls, a.Engine, a.EngineVersion, a.kind(), a.CreatedAt.Unix())
	return uniqueErr(err)
}

// UpdateApp saves everything about an app except its id, source and
// creation time.
func (s *Store) UpdateApp(ctx context.Context, a App) error {
	res, err := s.db.ExecContext(ctx, "UPDATE apps SET image = ?, repo = ?, branch = ?, port = ?, domain = ?, memory_mb = ?, cpus = ?, auto_deploy = ?, health_path = ?, test_command = ?, build_command = ?, start_command = ?, restart_pulls = ? WHERE id = ?",
		a.Image, a.Repo, a.Branch, a.Port, a.Domain, a.MemoryMB, a.CPUs, a.AutoDeploy, a.HealthPath, a.testColumn(), a.BuildCommand, a.StartCommand, a.RestartPulls, a.ID)
	return oneRow(res, uniqueErr(err))
}

// SetEngineVersion moves a database to another version of its engine.
func (s *Store) SetEngineVersion(ctx context.Context, appID, image, version string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE apps SET image = ?, engine_version = ? WHERE id = ? AND engine != ''", image, version, appID)
	return oneRow(res, err)
}

// SetDetected records how the last build built the app.
func (s *Store) SetDetected(ctx context.Context, appID string, d Detected) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "UPDATE apps SET detected = ? WHERE id = ?", string(b), appID)
	return err
}

// SetStopped records whether the user stopped the app.
func (s *Store) SetStopped(ctx context.Context, appID string, stopped bool) error {
	res, err := s.db.ExecContext(ctx, "UPDATE apps SET stopped = ? WHERE id = ?", stopped, appID)
	return oneRow(res, err)
}

// kind is the app's kind, KindApp when none was set.
func (a App) kind() string {
	if a.Kind == "" {
		return KindApp
	}
	return a.Kind
}

func (a App) testColumn() any {
	if !a.TestSet {
		return nil
	}
	return a.TestCommand
}

// SuggestTest stores a build's suggested test command, unless one was
// decided already. It reports whether it was stored.
func (s *Store) SuggestTest(ctx context.Context, appID, command string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE apps SET test_command = ? WHERE id = ? AND test_command IS NULL", command, appID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) App(ctx context.Context, id string) (App, error) {
	return scanApp(s.db.QueryRowContext(ctx, "SELECT "+appColumns+" FROM apps WHERE id = ?", id))
}

func (s *Store) Apps(ctx context.Context) ([]App, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+appColumns+" FROM apps ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PushTargets lists the apps that deploy pushes to a repository's branch.
// GitHub names are case-insensitive.
func (s *Store) PushTargets(ctx context.Context, repo, branch string) ([]App, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+appColumns+" FROM apps WHERE source = ? AND repo = ? COLLATE NOCASE AND branch = ? AND auto_deploy ORDER BY id",
		SourceGitHub, repo, branch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteApp removes an app with its variables and deployments.
func (s *Store) DeleteApp(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM apps WHERE id = ?", id)
	return oneRow(res, err)
}

// EnvVar is one environment variable of an app. For a secret one, Value is
// sealed for the core and the panel cannot read it.
type EnvVar struct {
	Name   string
	Value  string
	Secret bool
}

func (s *Store) Env(ctx context.Context, appID string) ([]EnvVar, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, value, secret FROM app_env WHERE app_id = ? ORDER BY name", appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EnvVar
	for rows.Next() {
		var v EnvVar
		if err := rows.Scan(&v.Name, &v.Value, &v.Secret); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetEnv replaces all of an app's variables.
func (s *Store) SetEnv(ctx context.Context, appID string, vars []EnvVar) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM app_env WHERE app_id = ?", appID); err != nil {
			return err
		}
		for _, v := range vars {
			if _, err := tx.ExecContext(ctx, "INSERT INTO app_env (app_id, name, value, secret) VALUES (?, ?, ?, ?)", appID, v.Name, v.Value, v.Secret); err != nil {
				return uniqueErr(err)
			}
		}
		return nil
	})
}

// Deployment states. A deployment moves forward through them and ends in
// one of the last four.
const (
	DeployQueued     = "queued"
	DeployBuilding   = "building"
	DeployTesting    = "testing"
	DeployStarting   = "starting"
	DeployInstalling = "installing" // a game server's install container runs
	DeployInstalled  = "installed"  // the install finished; nothing goes live
	DeployLive       = "live"
	DeployFailed     = "failed"
	DeployReplaced   = "replaced" // was live until a newer one took over
	DeploySkipped    = "skipped"  // a newer one came before it started
)

// What started a deployment.
const (
	CauseManual    = "manual"
	CausePush      = "push"
	CauseRestart   = "restart"   // the live image again, without a build
	CauseRollback  = "rollback"  // an earlier deployment's image
	CauseRecover   = "recover"   // the live image again, after it stopped by itself
	CauseRestore   = "restore"   // the live image again, after a backup was put back
	CauseBackup    = "backup"    // the live image again, after the app was stopped for its backup
	CauseUpdate    = "update"    // what the image's tag points at now; a database is backed up first
	CauseUpgrade   = "upgrade"   // a database's new major version, with its data dumped and loaded
	CauseInstall   = "install"   // a new game server's install container
	CauseReinstall = "reinstall" // the install again, after a backup of the server's files
)

// Deployment is one attempt to put a version of an app live.
type Deployment struct {
	ID         int64
	AppID      string
	Version    string // a branch until the commit is known
	Image      string
	Cause      string
	Message    string // first line of the pushed commit's message
	Pruned     bool   // its image was deleted
	State      string
	Error      *msg.Msg // why it failed
	CreatedAt  time.Time
	FinishedAt time.Time // zero while in progress
}

const deploymentColumns = "id, app_id, version, image, state, error, error_msg, cause, message, pruned, created_at, coalesce(finished_at, 0)"

func scanDeployment(row scanner) (Deployment, error) {
	var d Deployment
	var created, finished int64
	var text, js string
	err := row.Scan(&d.ID, &d.AppID, &d.Version, &d.Image, &d.State, &text, &js, &d.Cause, &d.Message, &d.Pruned, &created, &finished)
	d.Error = readMsg(text, js)
	d.CreatedAt = time.Unix(created, 0)
	if finished != 0 {
		d.FinishedAt = time.Unix(finished, 0)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// CreateDeployment adds d as a queued deployment and returns its id.
func (s *Store) CreateDeployment(ctx context.Context, d Deployment, now time.Time) (int64, error) {
	if d.Cause == "" {
		d.Cause = CauseManual
	}
	res, err := s.db.ExecContext(ctx, "INSERT INTO deployments (app_id, version, image, cause, message, state, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		d.AppID, d.Version, d.Image, d.Cause, d.Message, DeployQueued, now.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Deployment(ctx context.Context, appID string, id int64) (Deployment, error) {
	return scanDeployment(s.db.QueryRowContext(ctx, "SELECT "+deploymentColumns+" FROM deployments WHERE app_id = ? AND id = ?", appID, id))
}

// Deployments lists an app's most recent deployments, newest first.
func (s *Store) Deployments(ctx context.Context, appID string, limit int) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+deploymentColumns+" FROM deployments WHERE app_id = ? ORDER BY id DESC LIMIT ?", appID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LiveDeployments returns the live deployment of every app.
func (s *Store) LiveDeployments(ctx context.Context) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+deploymentColumns+" FROM deployments WHERE state = ? ORDER BY app_id", DeployLive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetDeploymentImage records the image a deployment runs.
func (s *Store) SetDeploymentImage(ctx context.Context, id int64, image string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE deployments SET image = ? WHERE id = ?", image, id)
	return err
}

// HasNewer reports whether the app has a deployment created after d.
func (s *Store) HasNewer(ctx context.Context, d Deployment) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM deployments WHERE app_id = ? AND id > ?", d.AppID, d.ID).Scan(&n)
	return n > 0, err
}

// Images lists every image an app or a kept deployment refers to.
func (s *Store) Images(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT image FROM apps WHERE image != '' UNION SELECT image FROM deployments WHERE image != '' AND NOT pruned")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var img string
		if err := rows.Scan(&img); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

// Unpruned lists the app's deployments that still have an image, newest
// first.
func (s *Store) Unpruned(ctx context.Context, appID string) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+deploymentColumns+" FROM deployments WHERE app_id = ? AND image != '' AND NOT pruned ORDER BY id DESC", appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetPruned records that an image was deleted, for every deployment that
// used it.
func (s *Store) SetPruned(ctx context.Context, appID, image string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE deployments SET pruned = 1 WHERE app_id = ? AND image = ?", appID, image)
	return err
}

// LiveDeployment returns the deployment serving an app, if any.
func (s *Store) LiveDeployment(ctx context.Context, appID string) (Deployment, error) {
	return scanDeployment(s.db.QueryRowContext(ctx, "SELECT "+deploymentColumns+" FROM deployments WHERE app_id = ? AND state = ?", appID, DeployLive))
}

// SetDeployment records progress. Ending states also set the finish time.
func (s *Store) SetDeployment(ctx context.Context, d Deployment, now time.Time) error {
	var finished any
	if d.State == DeployLive || d.State == DeployFailed || d.State == DeployReplaced || d.State == DeploySkipped || d.State == DeployInstalled {
		finished = now.Unix()
	}
	text, js := msgColumns(d.Error)
	_, err := s.db.ExecContext(ctx, "UPDATE deployments SET version = ?, image = ?, state = ?, error = ?, error_msg = ?, finished_at = coalesce(finished_at, ?) WHERE id = ?",
		d.Version, d.Image, d.State, text, js, finished, d.ID)
	return err
}

// GoLive makes d the app's live deployment and marks the one before it
// replaced, in one step.
func (s *Store) GoLive(ctx context.Context, d Deployment, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE deployments SET state = ? WHERE app_id = ? AND state = ?", DeployReplaced, d.AppID, DeployLive); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE deployments SET image = ?, state = ?, error = '', finished_at = ? WHERE id = ?", d.Image, DeployLive, now.Unix(), d.ID)
		return err
	})
}

// FailUnfinished marks deployments that were in progress when the panel
// stopped as failed. The panel calls it on start.
func (s *Store) FailUnfinished(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE deployments SET state = ?, error = ?, finished_at = ? WHERE state IN (?, ?, ?, ?, ?)",
		DeployFailed, "the panel restarted during this deployment", now.Unix(), DeployQueued, DeployBuilding, DeployTesting, DeployStarting, DeployInstalling)
	return err
}

func uniqueErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrExists
	}
	return err
}
