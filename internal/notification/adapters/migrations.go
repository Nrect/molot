package adapters

import "embed"

// Migrations holds the context's goose migrations. The .sql files live
// under "migrations/" inside the FS, while goose's provider globs
// *.sql at the FS root — root the FS before handing it over:
//
//	fsys, _ := fs.Sub(adapters.Migrations, "migrations")
//	goose.NewProvider(goose.DialectPostgres, db, fsys,
//	    goose.WithTableName("goose_db_version_notification"))
//
//go:embed migrations/*.sql
var Migrations embed.FS
