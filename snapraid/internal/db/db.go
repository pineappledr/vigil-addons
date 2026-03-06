package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Open initializes the SQLite database at the given path and runs migrations.
// The database file and its parent directory are created with restricted
// permissions (0600 for the file) if they do not already exist.
func Open(dbPath string) (*sql.DB, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("create db directory %s: %w", dir, err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	// Restrict file permissions after SQLite creates the file
	if err := os.Chmod(dbPath, 0600); err != nil && !os.IsNotExist(err) {
		db.Close()
		return nil, fmt.Errorf("chmod db file: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return db, nil
}

func migrate(db *sql.DB) error {
	const schema = `
	CREATE TABLE IF NOT EXISTS job_history (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		job_type    TEXT    NOT NULL,
		trigger     TEXT    NOT NULL,
		started_at  DATETIME NOT NULL,
		finished_at DATETIME,
		exit_code   INTEGER,
		status      TEXT    NOT NULL DEFAULT 'running',
		output_log  TEXT
	);

	CREATE TABLE IF NOT EXISTS telemetry_queue (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		message_type TEXT     NOT NULL,
		payload      TEXT     NOT NULL,
		created_at   DATETIME NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS config_cache (
		key        TEXT PRIMARY KEY,
		value      TEXT NOT NULL,
		updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
	);
	`
	_, err := db.Exec(schema)
	return err
}
