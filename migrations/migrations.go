// Package migrations embeds the SQL schema migrations that internal/store
// applies at startup. It is its own package because go:embed cannot reference
// files outside the embedding package's directory.
package migrations

import "embed"

// FS holds the numbered migration files, applied in lexical order.
//
//go:embed *.sql
var FS embed.FS
