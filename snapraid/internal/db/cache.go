package db

import (
	"database/sql"
	"fmt"
	"time"
)

// SetCacheValue upserts a key-value pair in the config_cache table.
func SetCacheValue(db *sql.DB, key, value string) error {
	_, err := db.Exec(
		`INSERT INTO config_cache (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().Format(timeFmt),
	)
	if err != nil {
		return fmt.Errorf("set cache %s: %w", key, err)
	}
	return nil
}

// GetCacheValue retrieves a value from the config_cache table.
// Returns empty string and no error if the key does not exist.
func GetCacheValue(db *sql.DB, key string) (string, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM config_cache WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get cache %s: %w", key, err)
	}
	return value, nil
}

// GetAllCacheValues returns all key-value pairs from the config_cache table.
func GetAllCacheValues(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`SELECT key, value FROM config_cache`)
	if err != nil {
		return nil, fmt.Errorf("query config cache: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan cache row: %w", err)
		}
		result[k] = v
	}
	return result, rows.Err()
}
