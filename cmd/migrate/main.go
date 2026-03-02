package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"
)

var safeTableName = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	sqlitePath := os.Getenv("SQLITE_PATH")
	databaseURL := os.Getenv("DATABASE_URL")

	if sqlitePath == "" {
		log.Fatal("SQLITE_PATH environment variable is required")
	}
	if databaseURL == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	// Open SQLite (read-only)
	srcDb, err := sql.Open("sqlite3", sqlitePath+"?mode=ro")
	if err != nil {
		log.Fatalf("Failed to open SQLite: %v", err)
	}
	defer srcDb.Close()

	// Open PostgreSQL
	dstDb, err := sql.Open("pgx", databaseURL)
	if err != nil {
		log.Fatalf("Failed to open PostgreSQL: %v", err)
	}
	defer dstDb.Close()

	if err := dstDb.Ping(); err != nil {
		log.Fatalf("Failed to connect to PostgreSQL: %v", err)
	}

	log.Println("Connected to both databases")

	// Discover tables to migrate by listing SQLite tables
	tables, err := discoverTables(srcDb)
	if err != nil {
		log.Fatalf("Failed to discover tables: %v", err)
	}

	log.Printf("Found tables: %v", tables)

	// Create PostgreSQL schema
	if err := createSchema(dstDb, tables); err != nil {
		log.Fatalf("Failed to create PostgreSQL schema: %v", err)
	}

	// Migrate each table
	for _, table := range tables {
		if err := migrateTable(srcDb, dstDb, table); err != nil {
			log.Fatalf("Failed to migrate table %s: %v", table, err)
		}
	}

	// Backfill tsvector for events tables
	if err := backfillSearchVectors(dstDb, tables); err != nil {
		log.Printf("Warning: failed to backfill search vectors: %v", err)
	}

	// Verify row counts
	if err := verifyCounts(srcDb, dstDb, tables); err != nil {
		log.Printf("Warning: verification failed: %v", err)
	}

	log.Println("Migration completed successfully!")
}

func discoverTables(db *sql.DB) ([]string, error) {
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '%_fts%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if !safeTableName.MatchString(name) {
			log.Printf("Skipping table with unsafe name: %q", name)
			continue
		}
		tables = append(tables, name)
	}
	return tables, nil
}

func createSchema(db *sql.DB, tables []string) error {
	for _, table := range tables {
		switch {
		case strings.HasSuffix(table, "__events"):
			prefix := table[:len(table)-len("__events")]
			stmts := []string{
				fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
					id TEXT PRIMARY KEY,
					created_at BIGINT NOT NULL,
					kind INTEGER NOT NULL,
					pubkey TEXT NOT NULL,
					content TEXT NOT NULL,
					tags TEXT NOT NULL,
					sig TEXT NOT NULL
				)`, table),
				fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s__idx_events_created_at ON %s(created_at)`, prefix, table),
				fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s__idx_events_kind ON %s(kind)`, prefix, table),
				fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s__idx_events_pubkey ON %s(pubkey)`, prefix, table),
				fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s__idx_events_kind_pubkey ON %s(kind, pubkey)`, prefix, table),
				fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s__idx_events_kind_pubkey_created_at ON %s(kind, pubkey, created_at DESC)`, prefix, table),
				// FTS: tsvector column + GIN index + trigger
				fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS search_vector tsvector`, table),
				fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s__idx_events_search ON %s USING GIN(search_vector)`, prefix, table),
				fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s_update_search_vector() RETURNS trigger AS $$
					BEGIN
						NEW.search_vector := to_tsvector('english', COALESCE(NEW.content, ''));
						RETURN NEW;
					END;
					$$ LANGUAGE plpgsql`, prefix),
				fmt.Sprintf(`DROP TRIGGER IF EXISTS %s_events_search_update ON %s`, prefix, table),
				fmt.Sprintf(`CREATE TRIGGER %s_events_search_update
					BEFORE INSERT OR UPDATE ON %s
					FOR EACH ROW EXECUTE FUNCTION %s_update_search_vector()`, prefix, table, prefix),
			}
			for _, s := range stmts {
				if _, err := db.Exec(s); err != nil {
					return fmt.Errorf("creating schema for %s: %w", table, err)
				}
			}
			continue

		case strings.HasSuffix(table, "__event_tags"):
			prefix := table[:len(table)-len("__event_tags")]
			eventsTable := prefix + "__events"
			stmts := []string{
				fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
					event_id TEXT NOT NULL,
					key TEXT NOT NULL,
					value TEXT NOT NULL,
					FOREIGN KEY (event_id) REFERENCES %s(id) ON DELETE CASCADE
				)`, table, eventsTable),
				fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s__idx_event_tags_event_id ON %s(event_id)`, prefix, table),
				fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s__idx_event_tags_key ON %s(key)`, prefix, table),
				fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s__idx_event_tags_key_value ON %s(key, value)`, prefix, table),
			}
			for _, s := range stmts {
				if _, err := db.Exec(s); err != nil {
					return fmt.Errorf("creating schema for %s: %w", table, err)
				}
			}
			continue

		case table == "kv":
			stmts := []string{
				`CREATE TABLE IF NOT EXISTS kv (
					key TEXT PRIMARY KEY,
					value TEXT NOT NULL
				)`,
			}
			for _, s := range stmts {
				if _, err := db.Exec(s); err != nil {
					return fmt.Errorf("creating schema for kv: %w", err)
				}
			}
			continue

		default:
			log.Printf("Skipping unknown table: %s", table)
			continue
		}
	}
	return nil
}

func migrateTable(srcDb, dstDb *sql.DB, table string) error {
	// Count source rows
	var srcCount int64
	if err := srcDb.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&srcCount); err != nil {
		return fmt.Errorf("counting source rows: %w", err)
	}
	log.Printf("Migrating %s: %d rows", table, srcCount)

	if srcCount == 0 {
		return nil
	}

	// Get columns from source
	rows, err := srcDb.Query(fmt.Sprintf("SELECT * FROM %s LIMIT 0", table))
	if err != nil {
		return fmt.Errorf("getting columns: %w", err)
	}
	cols, err := rows.Columns()
	rows.Close()
	if err != nil {
		return fmt.Errorf("getting column names: %w", err)
	}

	// Read all rows from source
	srcRows, err := srcDb.Query(fmt.Sprintf("SELECT * FROM %s", table))
	if err != nil {
		return fmt.Errorf("querying source: %w", err)
	}
	defer srcRows.Close()

	// Batch insert into destination
	const batchSize = 500
	batch := make([][]interface{}, 0, batchSize)

	for srcRows.Next() {
		values := make([]interface{}, len(cols))
		valuePtrs := make([]interface{}, len(cols))
		for i := range values {
			valuePtrs[i] = &values[i]
		}

		if err := srcRows.Scan(valuePtrs...); err != nil {
			return fmt.Errorf("scanning row: %w", err)
		}

		batch = append(batch, values)

		if len(batch) >= batchSize {
			if err := insertBatch(dstDb, table, cols, batch); err != nil {
				return fmt.Errorf("inserting batch: %w", err)
			}
			batch = batch[:0]
		}
	}

	// Insert remaining rows
	if len(batch) > 0 {
		if err := insertBatch(dstDb, table, cols, batch); err != nil {
			return fmt.Errorf("inserting final batch: %w", err)
		}
	}

	log.Printf("Migrated %s: %d rows", table, srcCount)
	return nil
}

func insertBatch(db *sql.DB, table string, cols []string, rows [][]interface{}) error {
	if len(rows) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Build parameterized INSERT statement
	colList := ""
	for i, col := range cols {
		if i > 0 {
			colList += ", "
		}
		colList += col
	}

	for _, row := range rows {
		placeholders := ""
		for i := range row {
			if i > 0 {
				placeholders += ", "
			}
			placeholders += fmt.Sprintf("$%d", i+1)
		}

		query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT DO NOTHING", table, colList, placeholders)
		if _, err := tx.Exec(query, row...); err != nil {
			return fmt.Errorf("inserting into %s: %w", table, err)
		}
	}

	return tx.Commit()
}

func backfillSearchVectors(db *sql.DB, tables []string) error {
	for _, table := range tables {
		if strings.HasSuffix(table, "__events") {
			log.Printf("Backfilling search vectors for %s...", table)
			// Trigger fires on UPDATE, so update content to itself
			_, err := db.Exec(fmt.Sprintf("UPDATE %s SET content = content", table))
			if err != nil {
				return fmt.Errorf("backfilling %s: %w", table, err)
			}
		}
	}
	return nil
}

func verifyCounts(srcDb, dstDb *sql.DB, tables []string) error {
	for _, table := range tables {
		var srcCount, dstCount int64

		if err := srcDb.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&srcCount); err != nil {
			return fmt.Errorf("counting source %s: %w", table, err)
		}
		if err := dstDb.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&dstCount); err != nil {
			return fmt.Errorf("counting dest %s: %w", table, err)
		}

		status := "OK"
		if srcCount != dstCount {
			status = "MISMATCH"
		}
		log.Printf("  %s: source=%d dest=%d [%s]", table, srcCount, dstCount, status)
	}
	return nil
}
