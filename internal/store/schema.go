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
}
