package store

// migrations run in order, each once. Released migrations are never edited;
// a change to the schema is a new entry at the end.
var migrations = []string{
	`
CREATE TABLE users (
	id          INTEGER PRIMARY KEY,
	email       TEXT NOT NULL UNIQUE COLLATE NOCASE,
	password    TEXT NOT NULL,  -- argon2id, in PHC string format
	admin       INTEGER NOT NULL DEFAULT 0,
	totp_secret BLOB,           -- encrypted with the panel key
	totp_step   INTEGER NOT NULL DEFAULT 0, -- last time step used, so a code works once
	created_at  INTEGER NOT NULL
) STRICT;

CREATE TABLE passkeys (
	id         BLOB PRIMARY KEY, -- credential ID chosen by the authenticator
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name       TEXT NOT NULL,
	credential BLOB NOT NULL,    -- the WebAuthn credential as JSON
	created_at INTEGER NOT NULL,
	used_at    INTEGER
) STRICT;

CREATE TABLE recovery_codes (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	hash    BLOB NOT NULL,
	PRIMARY KEY (user_id, hash)
) STRICT;

-- Only a hash of the session token is stored, so a copy of the database
-- cannot be used to log in.
CREATE TABLE sessions (
	hash       BLOB PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	verified   INTEGER NOT NULL, -- the second factor has been checked
	created_at INTEGER NOT NULL,
	seen_at    INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	ip         TEXT NOT NULL,
	agent      TEXT NOT NULL
) STRICT;
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE setup_token (
	one        INTEGER PRIMARY KEY CHECK (one = 1),
	hash       BLOB NOT NULL,
	expires_at INTEGER NOT NULL
) STRICT;
`,
	`
-- When the person behind the session last proved it was them, by logging
-- in or confirming. Changing how the account logs in needs a recent one.
ALTER TABLE sessions ADD COLUMN confirmed_at INTEGER NOT NULL DEFAULT 0;
`, `
-- An app is something Zelie keeps running: built from a GitHub repository
-- or run from an image. Its id also names its containers, images and
-- network.
CREATE TABLE apps (
	id         TEXT PRIMARY KEY,
	source     TEXT NOT NULL CHECK (source IN ('image', 'github')),
	image      TEXT NOT NULL DEFAULT '', -- for source image
	repo       TEXT NOT NULL DEFAULT '', -- owner/name, for source github
	branch     TEXT NOT NULL DEFAULT '',
	port       INTEGER NOT NULL,         -- the port the app listens on
	domain     TEXT NOT NULL DEFAULT '' COLLATE NOCASE,
	memory_mb  INTEGER NOT NULL,
	cpus       REAL NOT NULL,
	created_at INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX apps_domain ON apps(domain) WHERE domain != '';

-- A secret value is sealed for the core (package secret); the panel cannot
-- read it back.
CREATE TABLE app_env (
	app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	name   TEXT NOT NULL,
	value  TEXT NOT NULL,
	secret INTEGER NOT NULL,
	PRIMARY KEY (app_id, name)
) STRICT;

-- Each attempt to put a new version of an app live.
CREATE TABLE deployments (
	id          INTEGER PRIMARY KEY,
	app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	version     TEXT NOT NULL,           -- commit for github, image otherwise
	image       TEXT NOT NULL DEFAULT '', -- what runs, once known
	state       TEXT NOT NULL,           -- see store.Deploy* constants
	error       TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	finished_at INTEGER
) STRICT;
CREATE INDEX deployments_app ON deployments(app_id, id);
`,
	`
-- The panel's own GitHub App (package github). There is at most one. Its
-- private key and webhook secret are encrypted with the panel key.
CREATE TABLE github_app (
	one            INTEGER PRIMARY KEY CHECK (one = 1),
	app_id         INTEGER NOT NULL,
	slug           TEXT NOT NULL,
	owner          TEXT NOT NULL,
	html_url       TEXT NOT NULL,
	base_url       TEXT NOT NULL, -- the panel's address the App was made for
	private_key    BLOB NOT NULL,
	webhook_secret BLOB NOT NULL,
	created_at     INTEGER NOT NULL
) STRICT;

-- Webhook deliveries already acted on, so a recorded one sent again does
-- nothing.
CREATE TABLE github_deliveries (
	id          TEXT PRIMARY KEY,
	received_at INTEGER NOT NULL
) STRICT;

ALTER TABLE apps ADD COLUMN auto_deploy INTEGER NOT NULL DEFAULT 1;
UPDATE apps SET auto_deploy = 0 WHERE source != 'github';

-- What started a deployment (manual or push), and for a push the first
-- line of its commit message.
ALTER TABLE deployments ADD COLUMN cause TEXT NOT NULL DEFAULT 'manual';
ALTER TABLE deployments ADD COLUMN message TEXT NOT NULL DEFAULT '';
`, `
-- The path the health check asks for, once the app has a domain.
ALTER TABLE apps ADD COLUMN health_path TEXT NOT NULL DEFAULT '/';

-- Set once a deployment's image has been deleted to free disk space; it
-- can no longer be rolled back to.
ALTER TABLE deployments ADD COLUMN pruned INTEGER NOT NULL DEFAULT 0;
`, `
-- The command that runs the app's tests before a new build goes live.
-- NULL until it is known: the first build suggests one, and whatever the
-- user sets after that, an empty string included, is kept.
ALTER TABLE apps ADD COLUMN test_command TEXT;
`, `
-- The user stopped the app. Zelie does not bring a stopped app back up,
-- after a crash or when the server starts.
ALTER TABLE apps ADD COLUMN stopped INTEGER NOT NULL DEFAULT 0;
`, `
-- The user's own build and start commands; empty uses what the build
-- chose. detected holds what the last build chose, as JSON, to show next
-- to them.
ALTER TABLE apps ADD COLUMN build_command TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN start_command TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN detected TEXT NOT NULL DEFAULT '{}';

-- Restart fetches and builds the branch's newest commit instead of running
-- the live image again, the way Pelican's auto update does.
ALTER TABLE apps ADD COLUMN restart_pulls INTEGER NOT NULL DEFAULT 0;
`, `
-- Directories an app keeps between deployments. name is the core's, and
-- random: a database restored from an older copy must not find a directory
-- made later for another app under a reused id.
CREATE TABLE volumes (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	app_id     TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	path       TEXT NOT NULL, -- where the app sees it
	limit_mb   INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	UNIQUE (app_id, path)
) STRICT;
`,
	`
-- A database is an app run from its engine's image. engine is empty for
-- every other app.
ALTER TABLE apps ADD COLUMN engine TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN engine_version TEXT NOT NULL DEFAULT '';

-- An app linked to a database reaches it and gets variables to connect
-- with, their names starting with prefix.
CREATE TABLE links (
	app_id     TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	db_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	prefix     TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (app_id, db_id)
) STRICT;
CREATE INDEX links_db ON links(db_id);
`, `
-- When an app is backed up and how long its backups are kept. Databases
-- get one when they are made; without one an app is not backed up.
CREATE TABLE backup_plans (
	app_id    TEXT PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
	enabled   INTEGER NOT NULL,
	minute    INTEGER NOT NULL, -- of the day, server time
	keep_days INTEGER NOT NULL
) STRICT;
INSERT INTO backup_plans SELECT id, 1, 180, 7 FROM apps WHERE engine != '';

-- Each backup, made or tried. Not tied to the app: the backups of a
-- deleted database stay until keep_until, in case it was deleted by
-- mistake. file is the core's name for it, empty until it is made.
CREATE TABLE backups (
	id          INTEGER PRIMARY KEY,
	app_id      TEXT NOT NULL,
	engine      TEXT NOT NULL,
	reason      TEXT NOT NULL, -- see store.Backup* constants
	state       TEXT NOT NULL,
	file        TEXT NOT NULL DEFAULT '',
	bytes       INTEGER NOT NULL DEFAULT 0,
	error       TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	finished_at INTEGER,
	keep_until  INTEGER NOT NULL,
	restored_at INTEGER
) STRICT;
CREATE INDEX backups_app ON backups(app_id, id);

-- When the recovery file for the backup key was last saved.
CREATE TABLE backup_key (
	one      INTEGER PRIMARY KEY CHECK (one = 1),
	saved_at INTEGER NOT NULL
) STRICT;
`, `
-- Backups of an app's volumes. stop is set when the app is stopped for its
-- backup rather than copied while it runs. volumes lists the folders in the
-- backup, one per line: where the app saw each volume. size is how much the
-- files take unpacked, and changed how many changed while being copied.
ALTER TABLE backup_plans ADD COLUMN stop INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backups ADD COLUMN volumes TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN size INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backups ADD COLUMN changed INTEGER NOT NULL DEFAULT 0;
`, `
-- Why a deployment or backup failed, as a message the web interface can
-- translate: JSON of msg.Msg. The error columns keep the English text.
ALTER TABLE deployments ADD COLUMN error_msg TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN error_msg TEXT NOT NULL DEFAULT '';
`, `
-- Copies of backups in off-site storage (S3). offsite is '' for a backup
-- that is not sent, then pending, done or failed; offsite_at is when it
-- was sent, or when to try again. local is 0 once the file here is gone
-- and only the off-site copy is left, until offsite_until.
ALTER TABLE backup_plans ADD COLUMN offsite INTEGER NOT NULL DEFAULT 1;
ALTER TABLE backup_plans ADD COLUMN offsite_days INTEGER NOT NULL DEFAULT 30;
ALTER TABLE backups ADD COLUMN offsite TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN offsite_error TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN offsite_error_msg TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN offsite_tries INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backups ADD COLUMN offsite_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backups ADD COLUMN offsite_until INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backups ADD COLUMN local INTEGER NOT NULL DEFAULT 1;
`, `
-- For a dump someone uploaded: the statements taken out or changed so it
-- loads here, as JSON counts by kind.
ALTER TABLE backups ADD COLUMN adapted TEXT NOT NULL DEFAULT '';
`, `
-- A database desktop tools can reach through an SSH tunnel, on a loopback
-- port of this server. The password is not kept: it is shown once.
CREATE TABLE external_access (
	app_id     TEXT PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
	port       INTEGER NOT NULL UNIQUE,
	created_at INTEGER NOT NULL
) STRICT;
`, `
-- A database's volume from before an upgrade to a new major version, kept
-- so its data is not lost until the user removes it. version is the one
-- the files belong to. Nothing mounts it.
CREATE TABLE kept_volumes (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	app_id     TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	path       TEXT NOT NULL,
	limit_mb   INTEGER NOT NULL,
	version    TEXT NOT NULL,
	kept_at    INTEGER NOT NULL
) STRICT;
`, `
-- What each app used, a row a minute, for the last day. cpu is in CPUs on
-- average over the minute; the byte and request columns count that minute.
CREATE TABLE metrics (
	app_id        TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	at            INTEGER NOT NULL,
	memory_bytes  INTEGER NOT NULL,
	cpu           REAL NOT NULL,
	rx_bytes      INTEGER NOT NULL,
	tx_bytes      INTEGER NOT NULL,
	requests      INTEGER NOT NULL,
	client_errors INTEGER NOT NULL,
	server_errors INTEGER NOT NULL,
	PRIMARY KEY (app_id, at)
) STRICT, WITHOUT ROWID;
`, `
-- Failed attempts at logging in, so the limits hold across restarts. kind
-- is what is limited (an address, an account, second steps); key is a hash
-- of which one, so addresses and mistyped emails are not kept as such.
CREATE TABLE login_failures (
	kind TEXT NOT NULL,
	key  BLOB NOT NULL,
	at   INTEGER NOT NULL
) STRICT;
CREATE INDEX login_failures_key ON login_failures (kind, key, at);
`, `
-- A link made on the server with zelie reset-login, for an owner locked out
-- of the panel. Like the setup token, only its hash is kept.
CREATE TABLE reset_token (
	one        INTEGER PRIMARY KEY CHECK (one = 1),
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	hash       BLOB NOT NULL,
	expires_at INTEGER NOT NULL
) STRICT;
`, `
-- Machines that run servers. Zelie has one, this one; a row per machine is
-- what lets servers and their ports say which machine they belong to.
CREATE TABLE nodes (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	created_at INTEGER NOT NULL
) STRICT;
INSERT INTO nodes (id, name, created_at) VALUES (1, 'This server', unixepoch());

-- The ports a node may give to game servers, one row per address and port.
-- A row covers TCP and UDP. ip 0.0.0.0 means every address of the machine.
-- app_id is the server using it, if any.
CREATE TABLE allocations (
	id         INTEGER PRIMARY KEY,
	node_id    INTEGER NOT NULL REFERENCES nodes(id),
	ip         TEXT NOT NULL DEFAULT '0.0.0.0',
	port       INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
	app_id     TEXT REFERENCES apps(id) ON DELETE SET NULL,
	created_at INTEGER NOT NULL,
	UNIQUE (node_id, ip, port)
) STRICT;
CREATE INDEX allocations_app ON allocations(app_id);
`, `
-- What an app is: a plain app, or a game server. A database is told apart
-- by its engine and keeps kind 'app'.
ALTER TABLE apps ADD COLUMN kind TEXT NOT NULL DEFAULT 'app' CHECK (kind IN ('app', 'game'));

-- Egg files as they were fetched, so a server never needs the network to
-- read its egg again. source is a catalog id or the address it came from.
CREATE TABLE eggs (
	id          INTEGER PRIMARY KEY,
	name        TEXT NOT NULL,
	source      TEXT NOT NULL,
	raw         BLOB NOT NULL,
	imported_at INTEGER NOT NULL
) STRICT;
CREATE INDEX eggs_source ON eggs(source);

-- What a game server has beyond its app row. image and startup are what
-- the egg offered and the server uses; variables is a JSON object from
-- environment variable name to value. install_id is the deployment whose
-- log holds the last install.
CREATE TABLE game_servers (
	app_id        TEXT PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
	egg_id        INTEGER NOT NULL REFERENCES eggs(id),
	image         TEXT NOT NULL,
	startup       TEXT NOT NULL,
	variables     TEXT NOT NULL DEFAULT '{}',
	install_state TEXT NOT NULL CHECK (install_state IN ('installing', 'installed', 'failed')),
	install_id    INTEGER,
	installed_at  INTEGER
) STRICT;
`, `
-- The name or address players use to reach this node's game servers, when
-- the administrator sets one. Empty means the address Zelie finds on the
-- machine's network cards.
ALTER TABLE nodes ADD COLUMN public_address TEXT NOT NULL DEFAULT '';
`, `
-- When an administrator accepted the game's EULA for this server, for eggs
-- that ask for it. NULL until then.
ALTER TABLE game_servers ADD COLUMN eula_accepted_at INTEGER;
`, `
-- The Steam app a game server installs and updates, from its egg, and whether
-- the panel may update it when no one is playing. 0 means it is not a Steam
-- game, or has not been looked at yet.
ALTER TABLE game_servers ADD COLUMN steam_app_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE game_servers ADD COLUMN steam_auto_update INTEGER NOT NULL DEFAULT 0;
`, `
-- A files app runs an egg like a game server, but is an app: it has a domain,
-- database links and metrics, and no ports of its own. Its source column
-- stays 'image', because a CHECK cannot be widened in place; the store
-- reports the source as 'files' when this is set.
ALTER TABLE apps ADD COLUMN files INTEGER NOT NULL DEFAULT 0;
`, `
-- Schedules of a game server: a cron expression and an ordered list of tasks
-- to run when it fires. last_slot is the minute the schedule last fired for
-- (or was created or changed at), so a restart does not run a slot twice.
-- last_run is when a run last started; last_state is running, done, failed
-- or skipped.
CREATE TABLE schedules (
	id           INTEGER PRIMARY KEY,
	app_id       TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	cron         TEXT NOT NULL,
	only_running INTEGER NOT NULL DEFAULT 0,
	enabled      INTEGER NOT NULL DEFAULT 1,
	created_at   INTEGER NOT NULL,
	last_slot    INTEGER NOT NULL DEFAULT 0,
	last_run     INTEGER NOT NULL DEFAULT 0,
	last_state   TEXT NOT NULL DEFAULT '',
	last_error   TEXT NOT NULL DEFAULT '',
	last_error_msg TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX schedules_app ON schedules(app_id, id);

-- The tasks of a schedule in the order they run. action is command, power or
-- backup; data is the console command or the power action. delay is the
-- seconds to wait before the task.
CREATE TABLE schedule_tasks (
	id          INTEGER PRIMARY KEY,
	schedule_id INTEGER NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
	position    INTEGER NOT NULL,
	action      TEXT NOT NULL CHECK (action IN ('command', 'power', 'backup')),
	data        TEXT NOT NULL DEFAULT '',
	delay       INTEGER NOT NULL DEFAULT 0,
	keep_going  INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX schedule_tasks_schedule ON schedule_tasks(schedule_id, position);
`, `
-- SFTP for game servers. An account's public keys let it log in to the
-- servers it may manage; a key belongs to one account only, so it says who
-- is logging in. A server's SFTP password is kept as an argon2id hash and
-- shown once. The port is the one the SFTP service listens on.
CREATE TABLE ssh_keys (
	id          INTEGER PRIMARY KEY,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	fingerprint TEXT NOT NULL UNIQUE,
	public_key  BLOB NOT NULL,
	created_at  INTEGER NOT NULL
) STRICT;
CREATE INDEX ssh_keys_user ON ssh_keys(user_id);

CREATE TABLE sftp_passwords (
	app_id     TEXT PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
	hash       TEXT NOT NULL,
	created_at INTEGER NOT NULL
) STRICT;

CREATE TABLE sftp_config (
	node_id INTEGER PRIMARY KEY REFERENCES nodes(id),
	port    INTEGER NOT NULL CHECK (port BETWEEN 1024 AND 65535)
) STRICT;
`, `
-- The last deployment id handed out. SQLite gives a new row the highest id
-- plus one, so deleting an app's newest deployments made their ids come
-- round again, and with them the old log files kept under those ids.
CREATE TABLE deployment_seq (last INTEGER NOT NULL) STRICT;
INSERT INTO deployment_seq (last) SELECT coalesce(max(id), 0) FROM deployments;
`, `
-- Files and folders an account has starred in a server's file manager. They
-- belong to the account, so they follow it from one browser to another.
CREATE TABLE file_favorites (
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	app_id     TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	path       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, app_id, path)
) STRICT;
`, `
-- What the panel learned about the players of a game server from its
-- console. A player is one id in one server: a SteamID64 for Rust, a UUID
-- for Minecraft (or name:<name> before the log showed the UUID). play_seconds
-- only counts sessions that have ended.
CREATE TABLE players (
	app_id       TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	player_id    TEXT NOT NULL,
	name         TEXT NOT NULL,
	first_seen   INTEGER NOT NULL,
	last_seen    INTEGER NOT NULL,
	last_ip      TEXT NOT NULL DEFAULT '',
	play_seconds INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (app_id, player_id)
) STRICT;
CREATE INDEX players_seen ON players(app_id, last_seen);

-- left_at is null while the player is in. The same player is never in twice.
CREATE TABLE player_sessions (
	id        INTEGER PRIMARY KEY,
	app_id    TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	player_id TEXT NOT NULL,
	name      TEXT NOT NULL,
	ip        TEXT NOT NULL DEFAULT '',
	joined_at INTEGER NOT NULL,
	left_at   INTEGER,
	reason    TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX player_sessions_time ON player_sessions(app_id, joined_at);
CREATE INDEX player_sessions_ip ON player_sessions(app_id, ip);
CREATE INDEX player_sessions_player ON player_sessions(app_id, player_id, joined_at);

CREATE TABLE player_chat (
	id        INTEGER PRIMARY KEY,
	app_id    TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	player_id TEXT NOT NULL,
	name      TEXT NOT NULL,
	channel   TEXT NOT NULL,
	text      TEXT NOT NULL,
	at        INTEGER NOT NULL
) STRICT;
CREATE INDEX player_chat_time ON player_chat(app_id, at);
CREATE INDEX player_chat_player ON player_chat(app_id, player_id, at);

CREATE TABLE player_reports (
	id            INTEGER PRIMARY KEY,
	app_id        TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	reporter_id   TEXT NOT NULL,
	reporter_name TEXT NOT NULL DEFAULT '',
	target_id     TEXT NOT NULL,
	target_name   TEXT NOT NULL DEFAULT '',
	subject       TEXT NOT NULL DEFAULT '',
	message       TEXT NOT NULL DEFAULT '',
	at            INTEGER NOT NULL
) STRICT;
CREATE INDEX player_reports_time ON player_reports(app_id, at);
CREATE INDEX player_reports_target ON player_reports(app_id, target_id, at);

-- A ban stands until expires_at (null: for good) or until it is lifted.
CREATE TABLE player_bans (
	id         INTEGER PRIMARY KEY,
	app_id     TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	player_id  TEXT NOT NULL,
	name       TEXT NOT NULL DEFAULT '',
	reason     TEXT NOT NULL DEFAULT '',
	created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER,
	lifted_at  INTEGER,
	lifted_by  INTEGER REFERENCES users(id) ON DELETE SET NULL
) STRICT;
CREATE INDEX player_bans_player ON player_bans(app_id, player_id);

CREATE TABLE player_notes (
	id         INTEGER PRIMARY KEY,
	app_id     TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	player_id  TEXT NOT NULL,
	tag        TEXT NOT NULL DEFAULT '',
	note       TEXT NOT NULL DEFAULT '',
	created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX player_notes_player ON player_notes(app_id, player_id);

-- Who did what in the panel. app_id is plain text, not a reference: the
-- record of a deleted server stays.
CREATE TABLE audit_log (
	id         INTEGER PRIMARY KEY,
	at         INTEGER NOT NULL,
	account_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
	app_id     TEXT,
	action     TEXT NOT NULL,
	target     TEXT NOT NULL DEFAULT '',
	detail     TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX audit_log_app ON audit_log(app_id, at);
CREATE INDEX audit_log_time ON audit_log(at);
`, `
-- The Steam Web API key an administrator gave, encrypted with the panel
-- key. It is only used to look up the profiles of players.
CREATE TABLE steam_web_key (
	one INTEGER PRIMARY KEY CHECK (one = 1),
	sealed BLOB NOT NULL
) STRICT;
`, `
-- What an app ran with once a deployment of it went live: start command,
-- port, limits and health path, as JSON. Empty for older deployments.
ALTER TABLE deployments ADD COLUMN settings TEXT NOT NULL DEFAULT '';
`,
}
