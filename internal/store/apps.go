package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// App sources.
const (
	SourceImage  = "image"
	SourceGitHub = "github"
)

// App is something Zelie keeps running.
type App struct {
	ID        string
	Source    string
	Image     string // for SourceImage
	Repo      string // owner/name, for SourceGitHub
	Branch    string
	Port      int
	Domain    string
	MemoryMB  int64
	CPUs      float64
	CreatedAt time.Time
}

// ErrExists is returned when a name or domain is already taken.
var ErrExists = errors.New("already exists")

const appColumns = "id, source, image, repo, branch, port, domain, memory_mb, cpus, created_at"

func scanApp(row scanner) (App, error) {
	var a App
	var created int64
	err := row.Scan(&a.ID, &a.Source, &a.Image, &a.Repo, &a.Branch, &a.Port, &a.Domain, &a.MemoryMB, &a.CPUs, &created)
	a.CreatedAt = time.Unix(created, 0)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func (s *Store) CreateApp(ctx context.Context, a App) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO apps ("+appColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		a.ID, a.Source, a.Image, a.Repo, a.Branch, a.Port, a.Domain, a.MemoryMB, a.CPUs, a.CreatedAt.Unix())
	return uniqueErr(err)
}

// UpdateApp saves everything about an app except its id, source and
// creation time.
func (s *Store) UpdateApp(ctx context.Context, a App) error {
	res, err := s.db.ExecContext(ctx, "UPDATE apps SET image = ?, repo = ?, branch = ?, port = ?, domain = ?, memory_mb = ?, cpus = ? WHERE id = ?",
		a.Image, a.Repo, a.Branch, a.Port, a.Domain, a.MemoryMB, a.CPUs, a.ID)
	return oneRow(res, uniqueErr(err))
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
// one of the last three.
const (
	DeployQueued   = "queued"
	DeployBuilding = "building"
	DeployStarting = "starting"
	DeployLive     = "live"
	DeployFailed   = "failed"
	DeployReplaced = "replaced" // was live until a newer one took over
)

// Deployment is one attempt to put a version of an app live.
type Deployment struct {
	ID         int64
	AppID      string
	Version    string
	Image      string
	State      string
	Error      string
	CreatedAt  time.Time
	FinishedAt time.Time // zero while in progress
}

const deploymentColumns = "id, app_id, version, image, state, error, created_at, coalesce(finished_at, 0)"

func scanDeployment(row scanner) (Deployment, error) {
	var d Deployment
	var created, finished int64
	err := row.Scan(&d.ID, &d.AppID, &d.Version, &d.Image, &d.State, &d.Error, &created, &finished)
	d.CreatedAt = time.Unix(created, 0)
	if finished != 0 {
		d.FinishedAt = time.Unix(finished, 0)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// CreateDeployment adds a queued deployment and returns its id.
func (s *Store) CreateDeployment(ctx context.Context, appID, version string, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO deployments (app_id, version, state, created_at) VALUES (?, ?, ?, ?)",
		appID, version, DeployQueued, now.Unix())
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

// LiveDeployment returns the deployment serving an app, if any.
func (s *Store) LiveDeployment(ctx context.Context, appID string) (Deployment, error) {
	return scanDeployment(s.db.QueryRowContext(ctx, "SELECT "+deploymentColumns+" FROM deployments WHERE app_id = ? AND state = ?", appID, DeployLive))
}

// SetDeployment records progress. Ending states also set the finish time.
func (s *Store) SetDeployment(ctx context.Context, d Deployment, now time.Time) error {
	var finished any
	if d.State == DeployLive || d.State == DeployFailed || d.State == DeployReplaced {
		finished = now.Unix()
	}
	_, err := s.db.ExecContext(ctx, "UPDATE deployments SET version = ?, image = ?, state = ?, error = ?, finished_at = coalesce(finished_at, ?) WHERE id = ?",
		d.Version, d.Image, d.State, d.Error, finished, d.ID)
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
	_, err := s.db.ExecContext(ctx, "UPDATE deployments SET state = ?, error = ?, finished_at = ? WHERE state IN (?, ?, ?)",
		DeployFailed, "the panel restarted during this deployment", now.Unix(), DeployQueued, DeployBuilding, DeployStarting)
	return err
}

func uniqueErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrExists
	}
	return err
}
