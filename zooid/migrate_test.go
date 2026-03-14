package zooid

import (
	"fmt"
	"strings"
	"testing"
)

func TestRunMigrations_AppliesAndTracks(t *testing.T) {
	store := createTestEventStore()
	store.Init()

	kv := GetKeyValueStore()

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

	if err := RunMigrations(store.Schema); err != nil {
		t.Fatalf("Second RunMigrations() failed: %v", err)
	}
}

func TestInit_CoveringIndexesExistAndValid(t *testing.T) {
	store := createTestEventStore()
	store.Init()

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
