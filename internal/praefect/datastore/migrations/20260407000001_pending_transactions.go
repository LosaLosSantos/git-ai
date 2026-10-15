package migrations

import migrate "github.com/rubenv/sql-migrate"

func init() {
	m := &migrate.Migration{
		Id: "20260407000001_pending_transactions",
		Up: []string{
			`CREATE TABLE pending_transactions (
				id                BIGSERIAL PRIMARY KEY,
				repository_id     BIGINT NOT NULL REFERENCES repositories(repository_id) ON DELETE CASCADE,
				virtual_storage   TEXT NOT NULL,
				relative_path     TEXT NOT NULL,
				replica_path      TEXT NOT NULL,
				primary_storage   TEXT NOT NULL,
				expected_voters   TEXT[] NOT NULL,
				created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
			)`,
		},
		Down: []string{
			`DROP TABLE IF EXISTS pending_transactions`,
		},
	}

	allMigrations = append(allMigrations, m)
}
