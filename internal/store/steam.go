package store

import "context"

// SetSteamAppID records the Steam app of a game server found in its egg.
func (s *Store) SetSteamAppID(ctx context.Context, appID string, steamApp int64) error {
	res, err := s.db.ExecContext(ctx, "UPDATE game_servers SET steam_app_id = ? WHERE app_id = ?", steamApp, appID)
	return oneRow(res, err)
}

// SetSteamAutoUpdate turns updating a game server when it is empty on or off.
func (s *Store) SetSteamAutoUpdate(ctx context.Context, appID string, on bool) error {
	res, err := s.db.ExecContext(ctx, "UPDATE game_servers SET steam_auto_update = ? WHERE app_id = ?", on, appID)
	return oneRow(res, err)
}
