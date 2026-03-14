package zooid

import (
	"fmt"
	"strings"
	"testing"
)

func TestRunMigrations_AppliesAndTracks(t *testing.T) {
	store := createTestEventStore()
	store.Init() // Runs migrations internally

	kv := GetKeyValueStore()

	// Verify migration was tracked
	key := fmt.Sprintf("migration:%s:001_covering_indexes.sql", store.Schema.Name)
	val, err := kv.Get(key)
	if err != nil {
		t.Fatalf("Migration not tracked in kv: %v", err)
	}
	if val != "applied" {
		t.Errorf("Migration kv value = %q, want %q", val, "applied")
	}
}

func TestRunMigrations_Idempotent(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	// Running migrations again should not error (already applied).
	if err := RunMigrations(store.Schema); err != nil {
		t.Fatalf("Second RunMigrations() failed: %v", err)
	}
}

func TestEnsureCoveringIndexes_ExistAndValid(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	// PostgreSQL lowercases unquoted identifiers in system catalogs.
	indexes := []string{
		strings.ToLower(store.Schema.Prefix("idx_event_tags_key_value_event_id")),
		strings.ToLower(store.Schema.Prefix("idx_events_kind_created_at")),
	}

	for _, idx := range indexes {
		var valid bool
		err := GetDb().QueryRow(`
			SELECT pg_index.indisvalid
			FROM pg_class
			JOIN pg_index ON pg_index.indexrelid = pg_class.oid
			WHERE pg_class.relname = $1
		`, idx).Scan(&valid)
		if err != nil {
			t.Errorf("Index %s not found: %v", idx, err)
			continue
		}
		if !valid {
			t.Errorf("Index %s exists but is INVALID", idx)
		}
	}
}

func TestEnsureCoveringIndexes_RepairsInvalid(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	db := GetDb()
	idxName := strings.ToLower(store.Schema.Prefix("idx_event_tags_key_value_event_id"))

	// Simulate an INVALID index (happens when CREATE INDEX CONCURRENTLY fails).
	db.Exec(fmt.Sprintf(`
		UPDATE pg_index SET indisvalid = false
		WHERE indexrelid = (SELECT oid FROM pg_class WHERE relname = '%s')
	`, idxName))

	// Verify it's now invalid.
	var valid bool
	db.QueryRow(`
		SELECT pg_index.indisvalid FROM pg_class
		JOIN pg_index ON pg_index.indexrelid = pg_class.oid
		WHERE pg_class.relname = $1
	`, idxName).Scan(&valid)
	if valid {
		t.Skip("Could not mark index as invalid (may require superuser)")
	}

	// ensureCoveringIndexes should detect and repair it.
	if err := store.ensureCoveringIndexes(); err != nil {
		t.Fatalf("ensureCoveringIndexes with invalid index: %v", err)
	}

	// Verify it's valid now.
	err := db.QueryRow(`
		SELECT pg_index.indisvalid FROM pg_class
		JOIN pg_index ON pg_index.indexrelid = pg_class.oid
		WHERE pg_class.relname = $1
	`, idxName).Scan(&valid)
	if err != nil {
		t.Fatalf("Index not found after repair: %v", err)
	}
	if !valid {
		t.Error("Index should be valid after repair")
	}
}
