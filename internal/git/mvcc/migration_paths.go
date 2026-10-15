package mvcc

// MigrationOldRefsDir is where a migration to the MVCC backend moves the
// repository's refs/ aside while swapping in the staged one. Housekeeping
// removes one left behind by an interrupted migration.
const MigrationOldRefsDir = "refs.mvcc-old"

// MigrationStageGlob matches the directory a migration to the MVCC backend
// stages the new store in, named by git-refs-migrate(1). Housekeeping removes
// one left behind by an interrupted migration.
const MigrationStageGlob = "ref_migration.*"
