// Package migrations embeds the SQL migration files so the binary can apply
// them on boot with no external files to ship.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
