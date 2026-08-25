// Package db embeds the SQL migration files applied by store.Migrate.
package db

import "embed"

//go:embed migrations/*.sql
var FS embed.FS
