package zooid

import (
	"context"
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
// schema. Statements within a file are split on ";" and executed individually.
//
// ctx is the service root context; it bounds the kv lookups, kv writes, and
// the migration Execs so a stalled DB at startup fails fast instead of
// hanging the boot.
func RunMigrations(ctx context.Context, schema *Schema) error {
	kv := GetKeyValueStore(ctx)

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

		if _, err := kv.Get(ctx, kvKey); err == nil {
			// Already applied.
			continue
		}

		raw, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", entry.Name(), err)
		}

		rendered := schema.Render(string(raw))

		// Split on semicolons to execute each statement individually.
		for _, stmt := range strings.Split(rendered, ";") {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if _, err := GetDb().ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %s failed: %w", entry.Name(), err)
			}
		}

		if err := kv.Set(ctx, kvKey, "applied"); err != nil {
			return fmt.Errorf("recording migration %s: %w", entry.Name(), err)
		}

		log.Printf("Applied migration %s for schema %s", entry.Name(), schema.Name)
	}

	return nil
}
