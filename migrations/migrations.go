// Package migrations embeds idp-web's schema migrations (applied in file name order by `idp-web migrate`).
package migrations

import "embed"

// FS holds NNNN_name.sql files.
//
//go:embed *.sql
var FS embed.FS
