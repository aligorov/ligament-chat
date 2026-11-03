package migrations

import "embed"

// FS содержит встроенные SQL-миграции PostgreSQL для Ligament Chat.
//
//go:embed *.sql
var FS embed.FS
