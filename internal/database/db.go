package database

import (
	"database/sql"
	"fmt"
	"strings"
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
			cached_prompt_tokens INTEGER DEFAULT 0,
			provider_alias TEXT,
			requested_model TEXT,
			assigned_key TEXT,
			receiver_name TEXT,
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

	// Migrations for existing databases:
	// 1. Add cached_prompt_tokens column if missing
	if _, err := db.conn.Exec("ALTER TABLE request_logs ADD COLUMN cached_prompt_tokens INTEGER DEFAULT 0"); err != nil {
		// Ignore - column already exists
	}
	// 2. Remove receiver_key column if it exists (SQLite 3.35.0+)
	db.conn.Exec("ALTER TABLE request_logs DROP COLUMN receiver_key")
	// 3. Remove request_content column if it exists (SQLite 3.35.0+)
	db.conn.Exec("ALTER TABLE request_logs DROP COLUMN request_content")

	// 4. Add is_deleted column to key_states
	if _, err := db.conn.Exec("ALTER TABLE key_states ADD COLUMN is_deleted INTEGER DEFAULT 0"); err != nil {
		// Ignore - column already exists
	}

	// 5. Create model_cache table
	if _, err := db.conn.Exec(`CREATE TABLE IF NOT EXISTS model_cache (
		provider_alias TEXT,
		model_id TEXT,
		fetched_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (provider_alias, model_id)
	)`); err != nil {
		return fmt.Errorf("create model_cache: %w", err)
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

// SyncKeysExclusive aligns the key_states table with the YAML config for one provider.
// It inserts new keys, and deactivates keys that exist in the DB but no longer in YAML.
// This ensures only YAML-declared keys participate in retry/fallback.
func (db *DB) SyncKeysExclusive(alias string, yamlKeys []string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. Insert any new keys from YAML
	insertStmt, err := tx.Prepare(`INSERT OR IGNORE INTO key_states (key_value, provider_alias, is_active) VALUES (?, ?, 1)`)
	if err != nil {
		return err
	}
	defer insertStmt.Close()

	for _, key := range yamlKeys {
		if _, err := insertStmt.Exec(key, alias); err != nil {
			return err
		}
	}

	// 2. Deactivate keys in DB that are no longer in YAML
	// Build placeholders for the IN clause
	if len(yamlKeys) > 0 {
		placeholders := make([]string, len(yamlKeys))
		args := make([]any, len(yamlKeys)+1)
		args[0] = alias
		for i, k := range yamlKeys {
			placeholders[i] = "?"
			args[i+1] = k
		}

		query := fmt.Sprintf(
			`UPDATE key_states SET is_active = 0, updated_at = CURRENT_TIMESTAMP WHERE provider_alias = ? AND key_value NOT IN (%s) AND COALESCE(is_deleted, 0) = 0`,
			strings.Join(placeholders, ","),
		)
		if _, err := tx.Exec(query, args...); err != nil {
			return err
		}
	} else {
		// No YAML keys for this provider — deactivate all
		if _, err := tx.Exec(`UPDATE key_states SET is_active = 0, updated_at = CURRENT_TIMESTAMP WHERE provider_alias = ? AND COALESCE(is_deleted, 0) = 0`, alias); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// GetActiveKeys returns all active keys for a given provider.
func (db *DB) GetActiveKeys(alias string) ([]models.KeyState, error) {
	rows, err := db.conn.Query(
		`SELECT key_value, provider_alias, fail_count, is_active, cool_down_until, updated_at
			 FROM key_states WHERE provider_alias = ? AND is_active = 1 AND COALESCE(is_deleted, 0) = 0`, alias)
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

// GetActiveKeysInList returns active keys for a provider, filtered to only the given YAML keys.
// This ensures only keys declared in the YAML config participate in routing.
func (db *DB) GetActiveKeysInList(alias string, validKeys []string) ([]models.KeyState, error) {
	if len(validKeys) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(validKeys))
	args := make([]any, len(validKeys)+1)
	args[0] = alias
	for i, k := range validKeys {
		placeholders[i] = "?"
		args[i+1] = k
	}

	query := fmt.Sprintf(
		`SELECT key_value, provider_alias, fail_count, is_active, cool_down_until, updated_at
		 FROM key_states WHERE provider_alias = ? AND is_active = 1 AND COALESCE(is_deleted, 0) = 0 AND key_value IN (%s)`,
		strings.Join(placeholders, ","),
	)

	rows, err := db.conn.Query(query, args...)
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

// GetAllKeysInList returns ALL keys (active and inactive) for a provider, filtered to only the given YAML keys.
// Used in Phase 2 (dead-key fallback) when no active keys are available.
func (db *DB) GetAllKeysInList(alias string, validKeys []string) ([]models.KeyState, error) {
	if len(validKeys) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(validKeys))
	args := make([]any, len(validKeys)+1)
	args[0] = alias
	for i, k := range validKeys {
		placeholders[i] = "?"
		args[i+1] = k
	}

	query := fmt.Sprintf(
		`SELECT key_value, provider_alias, fail_count, is_active, cool_down_until, updated_at
		 FROM key_states WHERE provider_alias = ? AND COALESCE(is_deleted, 0) = 0 AND key_value IN (%s)`,
		strings.Join(placeholders, ","),
	)

	rows, err := db.conn.Query(query, args...)
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
		 FROM key_states WHERE provider_alias = ? AND COALESCE(is_deleted, 0) = 0`, alias)
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

// GetAllDisabledKeys returns all disabled keys (is_active = 0) grouped by provider.
func (db *DB) GetAllDisabledKeys() ([]models.KeyState, error) {
	rows, err := db.conn.Query(
		`SELECT key_value, provider_alias, fail_count, is_active, cool_down_until, updated_at
		 FROM key_states WHERE is_active = 0 AND COALESCE(is_deleted, 0) = 0`)
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

// GetKey returns a specific key state by key value.
func (db *DB) GetKey(keyValue string) (*models.KeyState, error) {
	var ks models.KeyState
	var coolDown sql.NullString
	var updatedAt string
	err := db.conn.QueryRow(
		`SELECT key_value, provider_alias, fail_count, is_active, cool_down_until, updated_at
		 FROM key_states WHERE key_value = ?`, keyValue).Scan(
		&ks.KeyValue, &ks.ProviderAlias, &ks.FailCount, &ks.IsActive, &coolDown, &updatedAt)
	if err != nil {
		return nil, err
	}
	if coolDown.Valid {
		t, err := time.Parse("2006-01-02 15:04:05", coolDown.String)
		if err == nil {
			ks.CoolDownUntil = &t
		}
	}
	ks.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
	return &ks, nil
}

// ReenableKey activates a previously disabled key and resets its fail count.
func (db *DB) ReenableKey(keyValue string) error {
	_, err := db.conn.Exec(
		`UPDATE key_states SET is_active = 1, fail_count = 0, cool_down_until = NULL, updated_at = CURRENT_TIMESTAMP WHERE key_value = ?`, keyValue)
	return err
}

// ResetFailCount sets fail_count to 0 for a key.
func (db *DB) ResetFailCount(keyValue string) error {
	_, err := db.conn.Exec(
		`UPDATE key_states SET fail_count = 0, updated_at = CURRENT_TIMESTAMP WHERE key_value = ?`, keyValue)
	return err
}

// IncrementFailCount increments fail_count. If it reaches tolerance and disableOnTolerance
// is true, auto-deactivates the key. Returns true if the key was deactivated (circuit broken).
func (db *DB) IncrementFailCount(keyValue string, tolerance int, disableOnTolerance bool) (bool, error) {
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

	if failCount >= tolerance && disableOnTolerance {
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
		  cached_prompt_tokens, provider_alias, requested_model, assigned_key,
		  receiver_name, is_test_request)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		log.StatusCode, log.PromptTokens, log.CompletionTokens, log.TotalTokens,
		log.CachedPromptTokens,
		log.ProviderAlias, log.RequestedModel, log.AssignedKey,
		log.ReceiverName, boolToInt(log.IsTestRequest))
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
			        COALESCE(SUM(total_tokens), 0),
			        COUNT(DISTINCT assigned_key)
			 FROM request_logs
			 WHERE status_code = 200 AND is_test_request = 0 AND timestamp >= ?
			 GROUP BY receiver_name`, since.Format("2006-01-02 15:04:05"))
	} else {
		rows, err = db.conn.Query(
			`SELECT receiver_name,
			        COUNT(*),
			        COALESCE(SUM(prompt_tokens), 0),
			        COALESCE(SUM(completion_tokens), 0),
			        COALESCE(SUM(total_tokens), 0),
			        COUNT(DISTINCT assigned_key)
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
		if err := rows.Scan(&rl.ReceiverName, &rl.RequestCount, &rl.PromptTokens, &rl.CompletionTokens, &rl.TotalTokens, &rl.KeyCount); err != nil {
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

// SoftDeleteKey marks a key as deleted (soft delete) and deactivates it.
func (db *DB) SoftDeleteKey(keyValue string) error {
	_, err := db.conn.Exec(
		`UPDATE key_states SET is_deleted = 1, is_active = 0, updated_at = CURRENT_TIMESTAMP WHERE key_value = ?`, keyValue)
	return err
}

// DisableKey deactivates a key without changing its fail count.
func (db *DB) DisableKey(keyValue string) error {
	_, err := db.conn.Exec(
		`UPDATE key_states SET is_active = 0, updated_at = CURRENT_TIMESTAMP WHERE key_value = ?`, keyValue)
	return err
}

// GetAllKeyStats returns all non-deleted key states with aggregated request stats.
// Used by the manage TUI to display per-key usage.
func (db *DB) GetAllKeyStats() ([]models.KeyStats, error) {
	rows, err := db.conn.Query(`
		SELECT
			ks.key_value, ks.provider_alias, ks.fail_count, ks.is_active,
			COALESCE(rl.req_count, 0),
			COALESCE(rl.prompt_tok, 0),
			COALESCE(rl.compl_tok, 0),
			COALESCE(rl.total_tok, 0)
		FROM key_states ks
		LEFT JOIN (
			SELECT assigned_key,
			       COUNT(*) as req_count,
			       SUM(prompt_tokens) as prompt_tok,
			       SUM(completion_tokens) as compl_tok,
			       SUM(total_tokens) as total_tok
			FROM request_logs
			WHERE is_test_request = 0
			GROUP BY assigned_key
		) rl ON ks.key_value = rl.assigned_key
		WHERE COALESCE(ks.is_deleted, 0) = 0
		ORDER BY ks.provider_alias, ks.key_value
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []models.KeyStats
	for rows.Next() {
		var s models.KeyStats
		if err := rows.Scan(&s.KeyValue, &s.ProviderAlias, &s.FailCount, &s.IsActive,
			&s.RequestCount, &s.PromptTokens, &s.CompletionTokens, &s.TotalTokens); err != nil {
			return nil, err
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

// SaveModelCache persists fetched model IDs for a provider to SQLite.
func (db *DB) SaveModelCache(alias string, modelIDs []string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM model_cache WHERE provider_alias = ?", alias); err != nil {
		return err
	}

	stmt, err := tx.Prepare("INSERT INTO model_cache (provider_alias, model_id) VALUES (?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, m := range modelIDs {
		if _, err := stmt.Exec(alias, m); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetModelCache reads all cached model IDs from SQLite.
func (db *DB) GetModelCache() (map[string][]string, error) {
	rows, err := db.conn.Query("SELECT provider_alias, model_id FROM model_cache ORDER BY provider_alias, model_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string][]string)
	for rows.Next() {
		var alias, modelID string
		if err := rows.Scan(&alias, &modelID); err != nil {
			return nil, err
		}
		result[alias] = append(result[alias], modelID)
	}
	return result, rows.Err()
}
