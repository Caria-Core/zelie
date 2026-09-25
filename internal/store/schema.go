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
}
