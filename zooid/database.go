package zooid

import (
	"database/sql"
	"log"
	"strconv"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	db     *sql.DB
	dbOnce sync.Once
)

func GetDb() *sql.DB {
	dbOnce.Do(func() {
		dsn := Env("DATABASE_URL")
		if dsn == "" {
			log.Fatal("DATABASE_URL environment variable is required")
		}

		newDb, err := sql.Open("pgx", dsn)
		if err != nil {
			log.Fatalf("Failed to open database: %v", err)
		}

		maxOpen := envInt("DB_MAX_OPEN_CONNS", 20)
		maxIdle := envInt("DB_MAX_IDLE_CONNS", 5)
		connMaxLife := envInt("DB_CONN_MAX_LIFETIME_SECS", 300)

		newDb.SetMaxOpenConns(maxOpen)
		newDb.SetMaxIdleConns(maxIdle)
		newDb.SetConnMaxLifetime(time.Duration(connMaxLife) * time.Second)

		db = newDb
	})

	return db
}

func envInt(key string, fallback int) int {
	if v := Env(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
