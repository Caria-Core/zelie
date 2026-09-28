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
`,
}
