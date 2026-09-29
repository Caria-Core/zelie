package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Install states of a game server.
const (
	InstallRunning = "installing"
	InstallDone    = "installed"
	InstallFailed  = "failed"
)

// Egg is an egg file kept as it was fetched.
type Egg struct {
	ID         int64
	Name       string
	Source     string // a catalog id or an address
	Raw        []byte
	ImportedAt time.Time
}

// GameServer is what a game server has besides its app row.
type GameServer struct {
	AppID   string
	EggID   int64
	Image   string // the image the server runs, one the egg offers
	Startup string
	// Variables maps environment variable names to their values.
	Variables    map[string]string
	InstallState string
	// InstallID is the deployment of the last install, whose log has its
	// output; zero before the first one is queued.
	InstallID   int64
	InstalledAt time.Time // zero until an install has finished
	// EULAAcceptedAt is when the game's EULA was accepted; zero until then.
	EULAAcceptedAt time.Time
	// SteamAppID is the app SteamCMD installs for this server, or 0.
	SteamAppID int64
	// SteamAutoUpdate lets the panel update the server once it is empty.
	SteamAutoUpdate bool
}

// AddEgg stores an egg file. The same file from the same source is stored
// once, so many servers of one game share a row.
func (s *Store) AddEgg(ctx context.Context, name, source string, raw []byte, now time.Time) (Egg, error) {
	var found Egg
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT id, name, raw, imported_at FROM eggs WHERE source = ?", source)
		if err != nil {
			return err
		}
		for rows.Next() {
			var e Egg
			var at int64
			if err := rows.Scan(&e.ID, &e.Name, &e.Raw, &at); err != nil {
				rows.Close()
				return err
			}
			if bytes.Equal(e.Raw, raw) {
				e.Source, e.ImportedAt = source, time.Unix(at, 0)
				found = e
			}
		}
		if err := rows.Close(); err != nil || found.ID != 0 {
			return err
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO eggs (name, source, raw, imported_at) VALUES (?, ?, ?, ?)", name, source, raw, now.Unix())
		if err != nil {
			return err
		}
		found = Egg{Name: name, Source: source, Raw: raw, ImportedAt: now}
		found.ID, err = res.LastInsertId()
		return err
	})
	return found, err
}

// Egg returns a stored egg, or ErrNotFound.
func (s *Store) Egg(ctx context.Context, id int64) (Egg, error) {
	var e Egg
	var at int64
	err := s.db.QueryRowContext(ctx, "SELECT id, name, source, raw, imported_at FROM eggs WHERE id = ?", id).Scan(&e.ID, &e.Name, &e.Source, &e.Raw, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	e.ImportedAt = time.Unix(at, 0)
	return e, err
}

const gameColumns = "app_id, egg_id, image, startup, variables, install_state, coalesce(install_id, 0), coalesce(installed_at, 0), coalesce(eula_accepted_at, 0), steam_app_id, steam_auto_update"

func scanGame(row scanner) (GameServer, error) {
	var g GameServer
	var vars string
	var installed, eula int64
	err := row.Scan(&g.AppID, &g.EggID, &g.Image, &g.Startup, &vars, &g.InstallState, &g.InstallID, &installed, &eula, &g.SteamAppID, &g.SteamAutoUpdate)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	if err != nil {
		return g, err
	}
	if err := json.Unmarshal([]byte(vars), &g.Variables); err != nil {
		return g, err
	}
	if installed != 0 {
		g.InstalledAt = time.Unix(installed, 0)
	}
	if eula != 0 {
		g.EULAAcceptedAt = time.Unix(eula, 0)
	}
	return g, nil
}

// CreateGameServer adds the game server data of an app that exists. Its
// install starts out running: the caller queues it next.
func (s *Store) CreateGameServer(ctx context.Context, g GameServer) error {
	vars, err := json.Marshal(g.Variables)
	if err != nil {
		return err
	}
	var eula any
	if !g.EULAAcceptedAt.IsZero() {
		eula = g.EULAAcceptedAt.Unix()
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO game_servers (app_id, egg_id, image, startup, variables, install_state, eula_accepted_at, steam_app_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		g.AppID, g.EggID, g.Image, g.Startup, string(vars), InstallRunning, eula, g.SteamAppID)
	return uniqueErr(err)
}

// AcceptEULA records that the game's EULA was accepted for the server.
func (s *Store) AcceptEULA(ctx context.Context, appID string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, "UPDATE game_servers SET eula_accepted_at = ? WHERE app_id = ?", at.Unix(), appID)
	return oneRow(res, err)
}

// GameServer returns the game server data of an app, or ErrNotFound.
func (s *Store) GameServer(ctx context.Context, appID string) (GameServer, error) {
	return scanGame(s.db.QueryRowContext(ctx, "SELECT "+gameColumns+" FROM game_servers WHERE app_id = ?", appID))
}

// GameServers lists every game server by app id.
func (s *Store) GameServers(ctx context.Context) ([]GameServer, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+gameColumns+" FROM game_servers ORDER BY app_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GameServer
	for rows.Next() {
		g, err := scanGame(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetInstall records how an install is going. deployment is the one whose
// log has its output; installed is when it finished successfully.
func (s *Store) SetInstall(ctx context.Context, appID, state string, deployment int64, installed time.Time) error {
	var at any
	if !installed.IsZero() {
		at = installed.Unix()
	}
	res, err := s.db.ExecContext(ctx, "UPDATE game_servers SET install_state = ?, install_id = ?, installed_at = coalesce(?, installed_at) WHERE app_id = ?",
		state, deployment, at, appID)
	return oneRow(res, err)
}

// BeginInstall marks the server as installing, unless it already is. It
// reports whether it did, so of two requests to install again only one
// goes ahead.
func (s *Store) BeginInstall(ctx context.Context, appID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE game_servers SET install_state = ? WHERE app_id = ? AND install_state != ?", InstallRunning, appID, InstallRunning)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FailUnfinishedInstalls marks installs that were running when the panel
// stopped as failed. The panel calls it on start.
func (s *Store) FailUnfinishedInstalls(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "UPDATE game_servers SET install_state = ? WHERE install_state = ?", InstallFailed, InstallRunning)
	return err
}
