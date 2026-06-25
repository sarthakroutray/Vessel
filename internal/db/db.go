package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"
)

// Connect opens a connection pool to PostgreSQL and verifies it with a ping.
//
// Pool settings are tuned for a modest VPS instance talking to Neon:
//   - MaxOpenConns: 25 — burst ceiling, avoids overwhelming the VPS or Neon.
//   - MaxIdleConns:  5 — keeps a handful of warm connections ready.
//   - ConnMaxLifetime: 5 minutes — recycles connections periodically; beneficial
//     with Neon's serverless/connection-pooled architecture.
//   - ConnMaxIdleTime: 1 minute — drops idle connections fairly quickly so the
//     pool doesn't hold unused slots open.
func Connect(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	// --- Pool configuration ---
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(1 * time.Minute)

	// Verify the connection is actually reachable.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("db.Ping: %w", err)
	}

	return db, nil
}
