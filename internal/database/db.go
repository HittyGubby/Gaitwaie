package database

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/HittyGubby/gaitwaie/internal/models"
	_ "modernc.org/sqlite"
)

// DB wraps the SQLite connection and provides typed access methods.
type DB struct {
	conn *sql.DB
}

// Open opens (or creates) the SQLite database and ensures tables exist.
func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Enable WAL mode manually for better concurrent access
	if _, err := conn.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if _, err := conn.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}

	db := &DB{conn: conn}
	if err := db.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// Close closes the underlying SQLite connection.
func (db *DB) Close() error {
	return db.conn.Close()
}

// Conn returns the underlying *sql.DB for direct queries (e.g., in CLI commands).
func (db *DB) Conn() *sql.DB {
	return db.conn
}

func (db *DB) migrate() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS request_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			status_code INTEGER,
			prompt_tokens INTEGER,
			completion_tokens INTEGER,
			total_tokens INTEGER,
			provider_alias TEXT,
			requested_model TEXT,
			assigned_key TEXT,
			receiver_name TEXT,
			receiver_key TEXT,
			is_test_request INTEGER DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS key_states (
			key_value TEXT PRIMARY KEY,
			provider_alias TEXT,
			fail_count INTEGER DEFAULT 0,
			is_active INTEGER DEFAULT 1,
			cool_down_until DATETIME,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_timestamp ON request_logs(timestamp)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_provider ON request_logs(provider_alias)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_receiver ON request_logs(receiver_name)`,
		`CREATE INDEX IF NOT EXISTS idx_key_states_provider ON key_states(provider_alias)`,
	}

	for _, q := range queries {
		if _, err := db.conn.Exec(q); err != nil {
			return fmt.Errorf("exec migration: %w", err)
		}
	}
	return nil
}

// EnsureKeys populates key_states with keys from config if they don't already exist.
func (db *DB) EnsureKeys(alias string, keys []string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO key_states (key_value, provider_alias, is_active) VALUES (?, ?, 1)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, key := range keys {
		if _, err := stmt.Exec(key, alias); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetActiveKeys returns all active keys for a given provider.
func (db *DB) GetActiveKeys(alias string) ([]models.KeyState, error) {
	rows, err := db.conn.Query(
		`SELECT key_value, provider_alias, fail_count, is_active, cool_down_until, updated_at
		 FROM key_states WHERE provider_alias = ? AND is_active = 1`, alias)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []models.KeyState
	for rows.Next() {
		var ks models.KeyState
		var coolDown sql.NullString
		var updatedAt string
		if err := rows.Scan(&ks.KeyValue, &ks.ProviderAlias, &ks.FailCount, &ks.IsActive, &coolDown, &updatedAt); err != nil {
			return nil, err
		}
		if coolDown.Valid {
			t, err := time.Parse("2006-01-02 15:04:05", coolDown.String)
			if err == nil {
				ks.CoolDownUntil = &t
			}
		}
		ks.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
		keys = append(keys, ks)
	}
	return keys, rows.Err()
}

// GetProviderKeys returns all keys (active and inactive) for a provider.
func (db *DB) GetProviderKeys(alias string) ([]models.KeyState, error) {
	rows, err := db.conn.Query(
		`SELECT key_value, provider_alias, fail_count, is_active, cool_down_until, updated_at
		 FROM key_states WHERE provider_alias = ?`, alias)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []models.KeyState
	for rows.Next() {
		var ks models.KeyState
		var coolDown sql.NullString
		var updatedAt string
		if err := rows.Scan(&ks.KeyValue, &ks.ProviderAlias, &ks.FailCount, &ks.IsActive, &coolDown, &updatedAt); err != nil {
			return nil, err
		}
		if coolDown.Valid {
			t, err := time.Parse("2006-01-02 15:04:05", coolDown.String)
			if err == nil {
				ks.CoolDownUntil = &t
			}
		}
		ks.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
		keys = append(keys, ks)
	}
	return keys, rows.Err()
}

// ResetFailCount sets fail_count to 0 for a key.
func (db *DB) ResetFailCount(keyValue string) error {
	_, err := db.conn.Exec(
		`UPDATE key_states SET fail_count = 0, updated_at = CURRENT_TIMESTAMP WHERE key_value = ?`, keyValue)
	return err
}

// IncrementFailCount increments fail_count. If it reaches tolerance, auto-deactivates.
// Returns true if the key was deactivated (circuit broken).
func (db *DB) IncrementFailCount(keyValue string, tolerance int) (bool, error) {
	res, err := db.conn.Exec(
		`UPDATE key_states SET fail_count = fail_count + 1, updated_at = CURRENT_TIMESTAMP WHERE key_value = ?`, keyValue)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}

	var failCount int
	err = db.conn.QueryRow(
		`SELECT fail_count FROM key_states WHERE key_value = ?`, keyValue).Scan(&failCount)
	if err != nil {
		return false, err
	}

	if failCount >= tolerance {
		_, err = db.conn.Exec(
			`UPDATE key_states SET is_active = 0, updated_at = CURRENT_TIMESTAMP WHERE key_value = ?`, keyValue)
		return true, err
	}
	return false, nil
}

// DirectDeactivate immediately deactivates a key (for 401/403).
func (db *DB) DirectDeactivate(keyValue string) error {
	_, err := db.conn.Exec(
		`UPDATE key_states SET is_active = 0, fail_count = 0, updated_at = CURRENT_TIMESTAMP WHERE key_value = ?`, keyValue)
	return err
}

// DeactivateKeys sets is_active = 0 for the given keys.
func (db *DB) DeactivateKeys(keyValues []string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`UPDATE key_states SET is_active = 0, updated_at = CURRENT_TIMESTAMP WHERE key_value = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, kv := range keyValues {
		if _, err := stmt.Exec(kv); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// InsertRequestLog records a proxied request.
func (db *DB) InsertRequestLog(log *models.RequestLog) error {
	_, err := db.conn.Exec(
		`INSERT INTO request_logs
		 (status_code, prompt_tokens, completion_tokens, total_tokens,
		  provider_alias, requested_model, assigned_key,
		  receiver_name, receiver_key, is_test_request)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		log.StatusCode, log.PromptTokens, log.CompletionTokens, log.TotalTokens,
		log.ProviderAlias, log.RequestedModel, log.AssignedKey,
		log.ReceiverName, log.ReceiverKey, boolToInt(log.IsTestRequest))
	return err
}

// QueryStats returns request count and token usage grouped by receiver_name within a time window.
// If since is nil, queries all records (excluding test requests).
func (db *DB) QueryStats(since *time.Time) (map[string]*models.RequestLog, error) {
	var rows *sql.Rows
	var err error

	if since != nil {
		rows, err = db.conn.Query(
			`SELECT receiver_name,
			        COUNT(*),
			        COALESCE(SUM(prompt_tokens), 0),
			        COALESCE(SUM(completion_tokens), 0),
			        COALESCE(SUM(total_tokens), 0)
			 FROM request_logs
			 WHERE status_code = 200 AND is_test_request = 0 AND timestamp >= ?
			 GROUP BY receiver_name`, since.Format("2006-01-02 15:04:05"))
	} else {
		rows, err = db.conn.Query(
			`SELECT receiver_name,
			        COUNT(*),
			        COALESCE(SUM(prompt_tokens), 0),
			        COALESCE(SUM(completion_tokens), 0),
			        COALESCE(SUM(total_tokens), 0)
			 FROM request_logs
			 WHERE status_code = 200 AND is_test_request = 0
			 GROUP BY receiver_name`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := make(map[string]*models.RequestLog)
	for rows.Next() {
		var rl models.RequestLog
		if err := rows.Scan(&rl.ReceiverName, &rl.RequestCount, &rl.PromptTokens, &rl.CompletionTokens, &rl.TotalTokens); err != nil {
			return nil, err
		}
		stats[rl.ReceiverName] = &rl
	}
	return stats, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
