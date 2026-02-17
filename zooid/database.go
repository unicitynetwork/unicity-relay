package zooid

import (
	"database/sql"
	"log"
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

		// Single ECS task — generous pool for concurrent WebSocket handlers
		newDb.SetMaxOpenConns(20)
		newDb.SetMaxIdleConns(5)
		newDb.SetConnMaxLifetime(5 * time.Minute)

		db = newDb
	})

	return db
}
