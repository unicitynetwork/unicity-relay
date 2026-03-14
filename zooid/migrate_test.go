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

func TestRunMigrations_IndexesExist(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	// PostgreSQL lowercases unquoted identifiers in system catalogs.
	coveringIdx := strings.ToLower(store.Schema.Prefix("idx_event_tags_key_value_event_id"))
	var count int
	err := GetDb().QueryRow(
		"SELECT COUNT(*) FROM pg_indexes WHERE indexname = $1", coveringIdx,
	).Scan(&count)
	if err != nil {
		t.Fatalf("Failed to query pg_indexes: %v", err)
	}
	if count != 1 {
		t.Errorf("Covering index %s not found in pg_indexes", coveringIdx)
	}

	compositeIdx := strings.ToLower(store.Schema.Prefix("idx_events_kind_created_at"))
	err = GetDb().QueryRow(
		"SELECT COUNT(*) FROM pg_indexes WHERE indexname = $1", compositeIdx,
	).Scan(&count)
	if err != nil {
		t.Fatalf("Failed to query pg_indexes: %v", err)
	}
	if count != 1 {
		t.Errorf("Composite index %s not found in pg_indexes", compositeIdx)
	}
}
