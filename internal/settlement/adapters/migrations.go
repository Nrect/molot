package adapters

import (
	"embed"
	"io/fs"
)

// Migrations is the settlement context's embedded goose migration set,
// re-exported by service/ and applied by the composition root before
// anything serves (per-context version table goose_db_version_settlement).
//
//go:embed migrations/*.sql
var Migrations embed.FS

// MigrationsForTests returns the migration files re-rooted at the .sql
// level, ready for goose.NewProvider (which scans the FS root).
func MigrationsForTests() fs.FS {
	sub, err := fs.Sub(Migrations, "migrations")
	if err != nil {
		panic("settlement adapters: migrations FS: " + err.Error())
	}
	return sub
}
