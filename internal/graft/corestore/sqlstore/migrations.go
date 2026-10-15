package sqlstore

import migrate "github.com/rubenv/sql-migrate"

// migrationTable is the sql-migrate bookkeeping table of the storage core.
const migrationTable = "graft_storage_migrations"

// The schema is fixed by doc/graft/PLAN6.md ("Data model"). Both dialects have
// the same tables and columns. A later schema change is a new migration, never
// an edit of an applied one.

// sqliteMigrations is the schema for SQLite. Reference names are BLOBs because
// they may hold bytes that are not valid UTF-8. Times are RFC 3339 text in UTC
// with nanoseconds.
var sqliteMigrations = []*migrate.Migration{
	{
		Id: "20261015000000_graft_storage_core",
		Up: []string{
			`CREATE TABLE graft_repositories (
				repo_id        INTEGER PRIMARY KEY AUTOINCREMENT,
				storage        TEXT    NOT NULL,
				relative_path  TEXT    NOT NULL,
				object_format  TEXT    NOT NULL,
				head_seq       INTEGER NOT NULL DEFAULT 0,
				log_seq        INTEGER NOT NULL DEFAULT 0,
				next_review_id INTEGER NOT NULL DEFAULT 1,
				created_at     TEXT    NOT NULL,
				UNIQUE (storage, relative_path)
			)`,
			`CREATE TABLE graft_refs (
				repo_id INTEGER NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				name    BLOB    NOT NULL,
				target  TEXT    NOT NULL,
				seq     INTEGER NOT NULL,
				PRIMARY KEY (repo_id, name)
			)`,
			`CREATE TABLE graft_changesets (
				repo_id    INTEGER NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				seq        INTEGER NOT NULL,
				operation  TEXT    NOT NULL,
				actor      TEXT    NOT NULL,
				node_id    TEXT    NOT NULL,
				pack_key   TEXT,
				pack_bytes INTEGER NOT NULL DEFAULT 0,
				forced     INTEGER NOT NULL DEFAULT 0,
				created_at TEXT    NOT NULL,
				PRIMARY KEY (repo_id, seq)
			)`,
			`CREATE TABLE graft_changeset_refs (
				repo_id    INTEGER NOT NULL,
				seq        INTEGER NOT NULL,
				ordinal    INTEGER NOT NULL,
				name       BLOB    NOT NULL,
				old_target TEXT,
				new_target TEXT,
				PRIMARY KEY (repo_id, seq, ordinal),
				FOREIGN KEY (repo_id, seq) REFERENCES graft_changesets (repo_id, seq) ON DELETE CASCADE
			)`,
			`CREATE TABLE graft_changelog (
				repo_id INTEGER NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				log_seq INTEGER NOT NULL,
				entry   TEXT    NOT NULL,
				PRIMARY KEY (repo_id, log_seq)
			)`,
			`CREATE TABLE graft_nodes (
				repo_id     INTEGER NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				node_id     TEXT    NOT NULL,
				applied_seq INTEGER NOT NULL,
				updated_at  TEXT    NOT NULL,
				PRIMARY KEY (repo_id, node_id)
			)`,
			`CREATE TABLE graft_reviews (
				repo_id       INTEGER NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				review_id     INTEGER NOT NULL,
				version       INTEGER NOT NULL,
				target_branch BLOB    NOT NULL,
				closed        INTEGER NOT NULL,
				body          TEXT    NOT NULL,
				updated_at    TEXT    NOT NULL,
				PRIMARY KEY (repo_id, review_id)
			)`,
		},
		Down: []string{
			`DROP TABLE graft_reviews`,
			`DROP TABLE graft_nodes`,
			`DROP TABLE graft_changelog`,
			`DROP TABLE graft_changeset_refs`,
			`DROP TABLE graft_changesets`,
			`DROP TABLE graft_refs`,
			`DROP TABLE graft_repositories`,
		},
	},
}

// postgresMigrations is the same schema for Postgres.
var postgresMigrations = []*migrate.Migration{
	{
		Id: "20261015000000_graft_storage_core",
		Up: []string{
			`CREATE TABLE graft_repositories (
				repo_id        BIGSERIAL   PRIMARY KEY,
				storage        TEXT        NOT NULL,
				relative_path  TEXT        NOT NULL,
				object_format  TEXT        NOT NULL,
				head_seq       BIGINT      NOT NULL DEFAULT 0,
				log_seq        BIGINT      NOT NULL DEFAULT 0,
				next_review_id BIGINT      NOT NULL DEFAULT 1,
				created_at     TIMESTAMPTZ NOT NULL,
				UNIQUE (storage, relative_path)
			)`,
			`CREATE TABLE graft_refs (
				repo_id BIGINT NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				name    BYTEA  NOT NULL,
				target  TEXT   NOT NULL,
				seq     BIGINT NOT NULL,
				PRIMARY KEY (repo_id, name)
			)`,
			`CREATE TABLE graft_changesets (
				repo_id    BIGINT      NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				seq        BIGINT      NOT NULL,
				operation  TEXT        NOT NULL,
				actor      TEXT        NOT NULL,
				node_id    TEXT        NOT NULL,
				pack_key   TEXT,
				pack_bytes BIGINT      NOT NULL DEFAULT 0,
				forced     BOOLEAN     NOT NULL DEFAULT FALSE,
				created_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (repo_id, seq)
			)`,
			`CREATE TABLE graft_changeset_refs (
				repo_id    BIGINT  NOT NULL,
				seq        BIGINT  NOT NULL,
				ordinal    INTEGER NOT NULL,
				name       BYTEA   NOT NULL,
				old_target TEXT,
				new_target TEXT,
				PRIMARY KEY (repo_id, seq, ordinal),
				FOREIGN KEY (repo_id, seq) REFERENCES graft_changesets (repo_id, seq) ON DELETE CASCADE
			)`,
			`CREATE TABLE graft_changelog (
				repo_id BIGINT NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				log_seq BIGINT NOT NULL,
				entry   TEXT   NOT NULL,
				PRIMARY KEY (repo_id, log_seq)
			)`,
			`CREATE TABLE graft_nodes (
				repo_id     BIGINT      NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				node_id     TEXT        NOT NULL,
				applied_seq BIGINT      NOT NULL,
				updated_at  TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (repo_id, node_id)
			)`,
			`CREATE TABLE graft_reviews (
				repo_id       BIGINT      NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				review_id     BIGINT      NOT NULL,
				version       BIGINT      NOT NULL,
				target_branch BYTEA       NOT NULL,
				closed        BOOLEAN     NOT NULL,
				body          TEXT        NOT NULL,
				updated_at    TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (repo_id, review_id)
			)`,
		},
		Down: []string{
			`DROP TABLE graft_reviews`,
			`DROP TABLE graft_nodes`,
			`DROP TABLE graft_changelog`,
			`DROP TABLE graft_changeset_refs`,
			`DROP TABLE graft_changesets`,
			`DROP TABLE graft_refs`,
			`DROP TABLE graft_repositories`,
		},
	},
}
