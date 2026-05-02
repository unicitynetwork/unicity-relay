package zooid

import (
	"context"
	"fmt"
	"log"
	"sync"
)

var (
	kv     *KeyValueStore
	kvOnce sync.Once
)

type KeyValueStore struct{}

// GetKeyValueStore is a startup-time singleton — Migrate creates the kv
// table once, before the connection pool sees production load. The ctx is
// used for the table-create Exec so a stalled DB during boot fails fast
// instead of hanging forever.
func GetKeyValueStore(ctx context.Context) *KeyValueStore {
	kvOnce.Do(func() {
		kv = &KeyValueStore{}
		kv.Migrate(ctx)
	})

	return kv
}

func (kv *KeyValueStore) Migrate(ctx context.Context) {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS kv (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}

	for _, stmt := range statements {
		if _, err := GetDb().ExecContext(ctx, stmt); err != nil {
			log.Fatalf("failed to migrate database: %v", err)
		}
	}
}

// Get/Set take ctx so the caller's deadline (derived from the service root
// ctx) bounds the pool wait and query — no business code creates its own
// background context.
func (kv *KeyValueStore) Get(ctx context.Context, key string) (string, error) {
	subctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()

	rows, err := sb.Select("value").
		From("kv").
		Where("key = ?", key).
		RunWith(GetDb()).
		QueryContext(subctx)

	if err != nil {
		return "", err
	}

	defer rows.Close()

	for rows.Next() {
		var value string

		err := rows.Scan(&value)
		if err != nil {
			return "", err
		}

		return value, nil
	}

	return "", fmt.Errorf("%s not found", key)
}

func (kv *KeyValueStore) Set(ctx context.Context, key string, value string) error {
	subctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()

	_, err := sb.Insert("kv").
		Columns("key", "value").
		Values(key, value).
		Suffix("ON CONFLICT(key) DO UPDATE SET value = EXCLUDED.value").
		RunWith(GetDb()).
		ExecContext(subctx)

	return err
}

// Namespaced kv. Currently unused by anything in the codebase but exposed
// for future callers; kept ctx-aware for the same reason as the underlying
// KeyValueStore.

type KV struct {
	Name string
}

func (kv *KV) Key(key string) string {
	return fmt.Sprintf("%s:%s", kv.Name, key)
}

func (kv *KV) Get(ctx context.Context, key string) (string, error) {
	return GetKeyValueStore(ctx).Get(ctx, kv.Key(key))
}

func (kv *KV) Set(ctx context.Context, key string, value string) error {
	return GetKeyValueStore(ctx).Set(ctx, kv.Key(key), value)
}
