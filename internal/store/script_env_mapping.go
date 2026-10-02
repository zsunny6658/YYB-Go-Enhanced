package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ScriptEnvMapping links a QingLong script key (e.g. "wxapp/mxbc.js")
// to the panel env variable name that carries the script's openid list.
// This table backs the aggregated-sync path: instead of creating one
// cron per account, the caller merges openids for all accounts that
// run the same script into a single &-delimited env value.
//
// source field values:
//   - "auto":     inferred from a static scan of the script source
//   - "manual":   set explicitly by the user via the API/UI
//   - "ground":   seeded from an existing QL panel env value
type ScriptEnvMapping struct {
	ScriptKey string `json:"script_key"`
	EnvName   string `json:"env_name"`
	Source    string `json:"source"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func (db *DB) UpsertScriptEnvMapping(ctx context.Context, scriptKey, envName, source string) (*ScriptEnvMapping, error) {
	now := time.Now().Unix()
	_, err := db.sql.ExecContext(ctx, `
INSERT INTO script_env_mapping(script_key, env_name, source, created_at, updated_at)
VALUES(?,?,?,?,?)
ON CONFLICT(script_key) DO UPDATE SET
  env_name=excluded.env_name,
  source=excluded.source,
  updated_at=excluded.updated_at`,
		scriptKey, envName, source, now, now)
	if err != nil {
		return nil, err
	}
	return db.GetScriptEnvMapping(ctx, scriptKey)
}

func (db *DB) GetScriptEnvMapping(ctx context.Context, scriptKey string) (*ScriptEnvMapping, error) {
	var m ScriptEnvMapping
	err := db.sql.QueryRowContext(ctx, `
SELECT script_key, env_name, source, created_at, updated_at
FROM script_env_mapping WHERE script_key=?`, scriptKey).
		Scan(&m.ScriptKey, &m.EnvName, &m.Source, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (db *DB) GetScriptEnvMappingByName(ctx context.Context, envName string) (*ScriptEnvMapping, error) {
	var m ScriptEnvMapping
	err := db.sql.QueryRowContext(ctx, `
SELECT script_key, env_name, source, created_at, updated_at
FROM script_env_mapping WHERE env_name=?`, envName).
		Scan(&m.ScriptKey, &m.EnvName, &m.Source, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (db *DB) ListScriptEnvMappings(ctx context.Context) ([]ScriptEnvMapping, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT script_key, env_name, source, created_at, updated_at
FROM script_env_mapping ORDER BY script_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ScriptEnvMapping, 0)
	for rows.Next() {
		var m ScriptEnvMapping
		if err := rows.Scan(&m.ScriptKey, &m.EnvName, &m.Source, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetScriptEnvMappingSourceForEnv upserts a mapping keyed by env_name
// when the caller only knows the panel env variable name (e.g. when
// seeding from an existing QingLong env). If a mapping already exists
// for the given env_name, only the source column is refreshed.
func (db *DB) SetScriptEnvMappingSourceForEnv(ctx context.Context, scriptKey, envName, source string) error {
	existing, err := db.GetScriptEnvMappingByName(ctx, envName)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = db.UpsertScriptEnvMapping(ctx, scriptKey, envName, source)
		return err
	}
	if err != nil {
		return err
	}
	if existing.Source == source {
		return nil
	}
	_, err = db.sql.ExecContext(ctx, `
UPDATE script_env_mapping SET source=?, updated_at=? WHERE script_key=?`,
		source, time.Now().Unix(), existing.ScriptKey)
	return err
}

func (db *DB) DeleteScriptEnvMapping(ctx context.Context, scriptKey string) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM script_env_mapping WHERE script_key=?`, scriptKey)
	return err
}
