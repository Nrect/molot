package adapters

import (
	"embed"
	"io/fs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations exposes the context's embedded goose migrations rooted at
// the .sql files (ready for goose.NewProvider). The composition root
// runs them with a per-context version table (goose_db_version_billing).
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic("billing migrations: " + err.Error())
	}
	return sub
}
