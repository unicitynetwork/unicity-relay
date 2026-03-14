package zooid

import (
	"database/sql"
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
// After applying migrations, it validates that all expected indexes exist and
// are not marked INVALID (which can happen if CREATE INDEX CONCURRENTLY was
// interrupted). Invalid indexes are dropped and recreated.
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

		rendered := schema.Render(string(raw))

		// Split on semicolons to execute each statement individually.
		for _, stmt := range strings.Split(rendered, ";") {
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

	// Validate indexes and repair any that are INVALID.
	if err := validateIndexes(schema); err != nil {
		return fmt.Errorf("index validation: %w", err)
	}

	return nil
}

// validateIndexes checks that critical indexes exist and are valid.
// If an index exists but is INVALID (e.g. from a failed CONCURRENTLY build),
// it is dropped and recreated with a regular CREATE INDEX.
func validateIndexes(schema *Schema) error {
	db := GetDb()

	type requiredIndex struct {
		name       string
		table      string
		definition string // column expression for CREATE INDEX
	}

	indexes := []requiredIndex{
		{
			name:       schema.Prefix("idx_event_tags_key_value_event_id"),
			table:      schema.Prefix("event_tags"),
			definition: "(key, value, event_id)",
		},
		{
			name:       schema.Prefix("idx_events_kind_created_at"),
			table:      schema.Prefix("events"),
			definition: "(kind, created_at DESC)",
		},
	}

	for _, idx := range indexes {
		var exists bool
		var valid bool

		err := db.QueryRow(`
			SELECT TRUE, pg_index.indisvalid
			FROM pg_class
			JOIN pg_index ON pg_index.indexrelid = pg_class.oid
			WHERE pg_class.relname = $1
		`, strings.ToLower(idx.name)).Scan(&exists, &valid)

		if err == sql.ErrNoRows {
			// Index doesn't exist at all — create it.
			log.Printf("Index %s missing, creating...", idx.name)
			stmt := fmt.Sprintf(
				"CREATE INDEX IF NOT EXISTS %s ON %s %s",
				idx.name, idx.table, idx.definition,
			)
			if _, err := db.Exec(stmt); err != nil {
				return fmt.Errorf("creating index %s: %w", idx.name, err)
			}
			log.Printf("Created index %s", idx.name)
		} else if err != nil {
			return fmt.Errorf("checking index %s: %w", idx.name, err)
		} else if !valid {
			// Index exists but is INVALID — drop and recreate.
			log.Printf("Index %s is INVALID, dropping and recreating...", idx.name)
			if _, err := db.Exec("DROP INDEX IF EXISTS " + idx.name); err != nil {
				return fmt.Errorf("dropping invalid index %s: %w", idx.name, err)
			}
			stmt := fmt.Sprintf(
				"CREATE INDEX %s ON %s %s",
				idx.name, idx.table, idx.definition,
			)
			if _, err := db.Exec(stmt); err != nil {
				return fmt.Errorf("recreating index %s: %w", idx.name, err)
			}
			log.Printf("Recreated index %s", idx.name)
		} else {
			log.Printf("Index %s: OK", idx.name)
		}
	}

	// Update planner statistics so it knows about the new indexes.
	db.Exec(fmt.Sprintf("ANALYZE %s", schema.Prefix("event_tags")))
	db.Exec(fmt.Sprintf("ANALYZE %s", schema.Prefix("events")))

	return nil
}
