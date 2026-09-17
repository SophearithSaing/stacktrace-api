// Package migrations embeds the ordered SQL release migrations in each binary.
package migrations

import "embed"

//go:embed *.sql
var Files embed.FS
