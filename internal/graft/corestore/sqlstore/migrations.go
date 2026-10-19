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
	{
		// Milestone 3 (doc/graft/PLAN7.md, "Data model"): checkpoints, the garbage
		// collector's orphan observations, maintenance leases, the tombstones of
		// removed repositories, and the trigger that refuses a pack key that is not
		// owned (PACK OWNERSHIP RULE 3).
		Id: "20261018000000_graft_storage_gc",
		Up: []string{
			`CREATE TABLE graft_checkpoints (
				repo_id    INTEGER NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				seq        INTEGER NOT NULL,
				pack_key   TEXT    NOT NULL,
				pack_bytes INTEGER NOT NULL DEFAULT 0,
				created_at TEXT    NOT NULL,
				PRIMARY KEY (repo_id, seq)
			)`,
			`CREATE TABLE graft_checkpoint_refs (
				repo_id INTEGER NOT NULL,
				seq     INTEGER NOT NULL,
				ordinal INTEGER NOT NULL,
				name    BLOB    NOT NULL,
				target  TEXT    NOT NULL,
				PRIMARY KEY (repo_id, seq, ordinal),
				FOREIGN KEY (repo_id, seq) REFERENCES graft_checkpoints (repo_id, seq) ON DELETE CASCADE
			)`,
			`CREATE TABLE graft_gc_orphans (
				repo_id    INTEGER NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				pack_key   TEXT    NOT NULL,
				first_seen TEXT    NOT NULL,
				PRIMARY KEY (repo_id, pack_key)
			)`,
			`CREATE TABLE graft_gc_leases (
				repo_id     INTEGER NOT NULL PRIMARY KEY REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				holder      TEXT    NOT NULL,
				acquired_at TEXT    NOT NULL,
				expires_at  TEXT    NOT NULL
			)`,
			`CREATE TABLE graft_gc_removed (
				repo_id       INTEGER NOT NULL PRIMARY KEY,
				storage       TEXT    NOT NULL,
				relative_path TEXT    NOT NULL,
				removed_at    TEXT    NOT NULL
			)`,
			`CREATE TRIGGER graft_changesets_owned_pack BEFORE INSERT ON graft_changesets
			WHEN NEW.pack_key IS NOT NULL AND NEW.pack_key NOT LIKE 'repos/%/packs/%-%.pack'
			BEGIN
				SELECT RAISE(ABORT, '` + notOwnedPackKeyMessage + `');
			END`,
		},
		Down: []string{
			`DROP TRIGGER graft_changesets_owned_pack`,
			`DROP TABLE graft_gc_removed`,
			`DROP TABLE graft_gc_leases`,
			`DROP TABLE graft_gc_orphans`,
			`DROP TABLE graft_checkpoint_refs`,
			`DROP TABLE graft_checkpoints`,
		},
	},
	{
		// Integrated review m4 (doc/graft/PLAN7.md, "Removed repositories"): every
		// deletion of a repository row records its tombstone, also one made by
		// hand, replacing a reconciled one of the same ID (its grace starts at
		// the deletion); and the state of the reconciliation that tombstones the
		// bucket prefixes of rows deleted without one before this migration.
		Id: "20261019000000_graft_storage_reconcile",
		Up: []string{
			`CREATE TRIGGER graft_repositories_tombstone AFTER DELETE ON graft_repositories
			BEGIN
				INSERT OR REPLACE INTO graft_gc_removed (repo_id, storage, relative_path, removed_at)
				VALUES (OLD.repo_id, OLD.storage, OLD.relative_path, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
			END`,
			`CREATE TABLE graft_gc_allocator_marks (
				observed_at TEXT    NOT NULL PRIMARY KEY,
				position    INTEGER NOT NULL
			)`,
			`CREATE TABLE graft_gc_reconcile (
				id           INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
				done_through INTEGER NOT NULL
			)`,
			`INSERT INTO graft_gc_reconcile (id, done_through) VALUES (1, 0)`,
		},
		Down: []string{
			`DROP TABLE graft_gc_reconcile`,
			`DROP TABLE graft_gc_allocator_marks`,
			`DROP TRIGGER graft_repositories_tombstone`,
		},
	},
	{
		// Land identity (doc/graft/PLAN11.md, 4.1): the last changelog entry that landed an
		// original commit on a branch, written with the entry in the same transaction.
		Id: "20261020000000_graft_land_identities",
		Up: []string{
			`CREATE TABLE graft_land_identities (
				repo_id         INTEGER NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				branch          BLOB    NOT NULL,
				original_commit TEXT    NOT NULL,
				log_seq         INTEGER NOT NULL,
				landed_commit   TEXT    NOT NULL,
				PRIMARY KEY (repo_id, branch, original_commit)
			)`,
		},
		Down: []string{
			`DROP TABLE graft_land_identities`,
		},
	},
}

// notOwnedPackKeyMessage is the error of the trigger that refuses a changeset
// whose pack key is not owned: a node that still runs a binary from before
// milestone 3 shares pack objects between writes (doc/graft/PLAN7.md, PACK
// OWNERSHIP RULE 3).
const notOwnedPackKeyMessage = "storage core: the pack key is not an owned key; every node must run the same version"

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
	{
		// See the SQLite migration of the same ID.
		Id: "20261018000000_graft_storage_gc",
		Up: []string{
			`CREATE TABLE graft_checkpoints (
				repo_id    BIGINT      NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				seq        BIGINT      NOT NULL,
				pack_key   TEXT        NOT NULL,
				pack_bytes BIGINT      NOT NULL DEFAULT 0,
				created_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (repo_id, seq)
			)`,
			`CREATE TABLE graft_checkpoint_refs (
				repo_id BIGINT  NOT NULL,
				seq     BIGINT  NOT NULL,
				ordinal INTEGER NOT NULL,
				name    BYTEA   NOT NULL,
				target  TEXT    NOT NULL,
				PRIMARY KEY (repo_id, seq, ordinal),
				FOREIGN KEY (repo_id, seq) REFERENCES graft_checkpoints (repo_id, seq) ON DELETE CASCADE
			)`,
			`CREATE TABLE graft_gc_orphans (
				repo_id    BIGINT      NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				pack_key   TEXT        NOT NULL,
				first_seen TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (repo_id, pack_key)
			)`,
			`CREATE TABLE graft_gc_leases (
				repo_id     BIGINT      NOT NULL PRIMARY KEY REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				holder      TEXT        NOT NULL,
				acquired_at TIMESTAMPTZ NOT NULL,
				expires_at  TIMESTAMPTZ NOT NULL
			)`,
			`CREATE TABLE graft_gc_removed (
				repo_id       BIGINT      NOT NULL PRIMARY KEY,
				storage       TEXT        NOT NULL,
				relative_path TEXT        NOT NULL,
				removed_at    TIMESTAMPTZ NOT NULL
			)`,
			`CREATE FUNCTION graft_changesets_owned_pack() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.pack_key IS NOT NULL AND NEW.pack_key NOT LIKE 'repos/%/packs/%-%.pack' THEN
					RAISE EXCEPTION '` + notOwnedPackKeyMessage + `' USING ERRCODE = 'check_violation';
				END IF;
				RETURN NEW;
			END
			$$`,
			`CREATE TRIGGER graft_changesets_owned_pack BEFORE INSERT ON graft_changesets
			FOR EACH ROW EXECUTE FUNCTION graft_changesets_owned_pack()`,
		},
		Down: []string{
			`DROP TRIGGER graft_changesets_owned_pack ON graft_changesets`,
			`DROP FUNCTION graft_changesets_owned_pack()`,
			`DROP TABLE graft_gc_removed`,
			`DROP TABLE graft_gc_leases`,
			`DROP TABLE graft_gc_orphans`,
			`DROP TABLE graft_checkpoint_refs`,
			`DROP TABLE graft_checkpoints`,
		},
	},
	{
		// See the SQLite migration of the same ID.
		Id: "20261019000000_graft_storage_reconcile",
		Up: []string{
			`CREATE FUNCTION graft_repositories_tombstone() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				INSERT INTO graft_gc_removed (repo_id, storage, relative_path, removed_at)
				VALUES (OLD.repo_id, OLD.storage, OLD.relative_path, clock_timestamp())
				ON CONFLICT (repo_id) DO UPDATE SET storage = EXCLUDED.storage,
					relative_path = EXCLUDED.relative_path, removed_at = EXCLUDED.removed_at;
				RETURN OLD;
			END
			$$`,
			`CREATE TRIGGER graft_repositories_tombstone AFTER DELETE ON graft_repositories
			FOR EACH ROW EXECUTE FUNCTION graft_repositories_tombstone()`,
			`CREATE TABLE graft_gc_allocator_marks (
				observed_at TIMESTAMPTZ NOT NULL PRIMARY KEY,
				position    BIGINT      NOT NULL
			)`,
			`CREATE TABLE graft_gc_reconcile (
				id           INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
				done_through BIGINT  NOT NULL
			)`,
			`INSERT INTO graft_gc_reconcile (id, done_through) VALUES (1, 0)`,
		},
		Down: []string{
			`DROP TABLE graft_gc_reconcile`,
			`DROP TABLE graft_gc_allocator_marks`,
			`DROP TRIGGER graft_repositories_tombstone ON graft_repositories`,
			`DROP FUNCTION graft_repositories_tombstone()`,
		},
	},
	{
		// See the SQLite migration of the same ID.
		Id: "20261020000000_graft_land_identities",
		Up: []string{
			`CREATE TABLE graft_land_identities (
				repo_id         BIGINT NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
				branch          BYTEA  NOT NULL,
				original_commit TEXT   NOT NULL,
				log_seq         BIGINT NOT NULL,
				landed_commit   TEXT   NOT NULL,
				PRIMARY KEY (repo_id, branch, original_commit)
			)`,
		},
		Down: []string{
			`DROP TABLE graft_land_identities`,
		},
	},
}
