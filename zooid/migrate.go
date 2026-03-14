package zooid

import (
	"embed"
	"fmt"
	"log"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// RunMigrations executes pending SQL migration files for the given schema.
// Migrations are embedded from zooid/migrations/, templated with the schema
// prefix, and tracked in the global kv table so each runs at most once per
// schema. Statements within a file are split on ";" and executed individually
// (required for CREATE INDEX CONCURRENTLY which cannot run inside a
// transaction).
func RunMigrations(schema *Schema) error {
	kv := GetKeyValueStore()

	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("reading migrations dir: %w", err)
	}

	// Sort by filename to ensure execution order.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		kvKey := fmt.Sprintf("migration:%s:%s", schema.Name, entry.Name())

		if _, err := kv.Get(kvKey); err == nil {
			// Already applied.
			continue
		}

		raw, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", entry.Name(), err)
		}

		sql := schema.Render(string(raw))

		// Split on semicolons to execute each statement individually.
		// This is necessary because CREATE INDEX CONCURRENTLY cannot run
		// inside a multi-statement implicit transaction.
		for _, stmt := range strings.Split(sql, ";") {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if _, err := GetDb().Exec(stmt); err != nil {
				return fmt.Errorf("migration %s failed: %w", entry.Name(), err)
			}
		}

		if err := kv.Set(kvKey, "applied"); err != nil {
			return fmt.Errorf("recording migration %s: %w", entry.Name(), err)
		}

		log.Printf("Applied migration %s for schema %s", entry.Name(), schema.Name)
	}

	return nil
}
