package zooid

import (
	"context"
	"database/sql"
	"log"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	// Start PostgreSQL container for unit tests
	pgContainer, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("zooid_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		log.Fatalf("Failed to start PostgreSQL container: %v", err)
	}
	defer pgContainer.Terminate(ctx)

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Fatalf("Failed to get connection string: %v", err)
	}

	// Verify connection works before running tests
	testDb, err := sql.Open("pgx", connStr)
	if err != nil {
		log.Fatalf("Failed to open test database: %v", err)
	}
	if err := testDb.Ping(); err != nil {
		log.Fatalf("Failed to ping test database: %v", err)
	}
	testDb.Close()

	os.Setenv("DATABASE_URL", connStr)

	// Create required directories for tests (media and config still needed)
	os.MkdirAll("./media", 0755)
	os.MkdirAll("./config", 0755)

	// Run tests
	code := m.Run()

	os.Exit(code)
}
