// Package migrations embeds the SQL migration files so the binaries can run
// goose without shipping the .sql files alongside them.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
