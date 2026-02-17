package zooid

import (
	"fmt"
	"log"
	"sync"
)

var (
	kv     *KeyValueStore
	kvOnce sync.Once
)

type KeyValueStore struct{}

func GetKeyValueStore() *KeyValueStore {
	kvOnce.Do(func() {
		kv = &KeyValueStore{}
		kv.Migrate()
	})

	return kv
}

func (kv *KeyValueStore) Migrate() {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS kv (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kv_key ON kv(key)`,
	}

	for _, stmt := range statements {
		if _, err := GetDb().Exec(stmt); err != nil {
			log.Fatalf("failed to migrate database: %v", err)
		}
	}
}

func (kv *KeyValueStore) Get(key string) (string, error) {
	rows, err := sb.Select("value").
		From("kv").
		Where("key = ?", key).
		RunWith(GetDb()).
		Query()

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

func (kv *KeyValueStore) Set(key string, value string) error {
	_, err := sb.Insert("kv").
		Columns("key", "value").
		Values(key, value).
		Suffix("ON CONFLICT(key) DO UPDATE SET value = EXCLUDED.value").
		RunWith(GetDb()).
		Exec()

	return err
}

// Namespaced kv

type KV struct {
	Name string
}

func (kv *KV) Key(key string) string {
	return fmt.Sprintf("%s:%s", kv.Name, key)
}

func (kv *KV) Get(key string) (string, error) {
	return GetKeyValueStore().Get(kv.Key(key))
}

func (kv *KV) Set(key string, value string) error {
	return GetKeyValueStore().Set(kv.Key(key), value)
}
