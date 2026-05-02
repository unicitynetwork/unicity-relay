package zooid

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
)

// ErrKVNotFound is the sentinel returned by KeyValueStore.Get when the key
// doesn't exist. Callers must `errors.Is(err, ErrKVNotFound)` instead of
// treating any error as missing — context deadlines and DB faults look
// indistinguishable from "no row" if you only inspect the bool/string.
var ErrKVNotFound = errors.New("kv key not found")

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
		// Per-statement deadline — the caller's ctx may be the long-lived
		// service root, which has no timeout, so a stalled DB at startup
		// would otherwise hang here forever.
		subctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
		_, err := GetDb().ExecContext(subctx, stmt)
		cancel()
		if err != nil {
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

	// rows.Err() surfaces context-cancel and driver errors that ended the
	// iteration before any row arrived — without this check, the not-found
	// branch below would mask them.
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("kv get %q: %w", key, err)
	}

	return "", fmt.Errorf("%w: %s", ErrKVNotFound, key)
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
