package model

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS counters (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	counter_id TEXT UNIQUE NOT NULL,
	oauth_token TEXT NOT NULL,
	poll_interval_minutes INTEGER NOT NULL DEFAULT 60,
	-- Newest event already evaluated, so a restart does not replay history.
	last_event_at DATETIME,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- The Logs API is asynchronous: an export is ordered, prepared over minutes,
-- then downloaded. The order has to outlive the poll that placed it and the
-- process itself — an untracked request is one Metrika keeps counting against
-- the counter's quota while nothing ever downloads or cleans it.
CREATE TABLE IF NOT EXISTS log_requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	counter_id INTEGER NOT NULL REFERENCES counters(id),
	request_id INTEGER NOT NULL,
	date1 DATETIME NOT NULL,
	date2 DATETIME NOT NULL,
	status TEXT NOT NULL,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(counter_id, request_id)
);

CREATE TABLE IF NOT EXISTS page_monitors (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	counter_id INTEGER NOT NULL REFERENCES counters(id),
	name TEXT NOT NULL,
	url_pattern TEXT NOT NULL,
	metrics TEXT NOT NULL DEFAULT 'visits,bounces,goals,revenue',
	enabled BOOLEAN NOT NULL DEFAULT 1,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS triggers (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	counter_id INTEGER NOT NULL REFERENCES counters(id),
	monitor_id INTEGER REFERENCES page_monitors(id),
	name TEXT NOT NULL,
	condition TEXT NOT NULL,
	threshold INTEGER NOT NULL DEFAULT 1,
	window_minutes INTEGER NOT NULL DEFAULT 30,
	cooldown_minutes INTEGER NOT NULL DEFAULT 60,
	enabled BOOLEAN NOT NULL DEFAULT 1,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS alert_actions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	type TEXT NOT NULL CHECK(type IN ('telegram','vkteams','webhook')),
	chat_id INTEGER,
	target TEXT NOT NULL DEFAULT '',
	url TEXT,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS alerts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	trigger_id INTEGER NOT NULL REFERENCES triggers(id),
	counter_id INTEGER NOT NULL REFERENCES counters(id),
	title TEXT NOT NULL,
	message TEXT,
	event_count INTEGER NOT NULL DEFAULT 0,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS report_snapshots (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	counter_id INTEGER NOT NULL REFERENCES counters(id),
	monitor_id INTEGER REFERENCES page_monitors(id),
	period TEXT NOT NULL CHECK(period IN ('hour','day')),
	period_key TEXT NOT NULL,
	taken_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	visits INTEGER NOT NULL DEFAULT 0,
	unique_visits INTEGER NOT NULL DEFAULT 0,
	bounces INTEGER NOT NULL DEFAULT 0,
	avg_duration_sec REAL NOT NULL DEFAULT 0,
	avg_depth REAL NOT NULL DEFAULT 0,
	goals TEXT NOT NULL DEFAULT '{}',
	revenue REAL NOT NULL DEFAULT 0,
	orders INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS trigger_cooldowns (
	trigger_id INTEGER PRIMARY KEY REFERENCES triggers(id),
	last_fired_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_alerts_counter ON alerts(counter_id);
CREATE INDEX IF NOT EXISTS idx_log_requests_counter ON log_requests(counter_id, status);
CREATE INDEX IF NOT EXISTS idx_snapshots_counter_period ON report_snapshots(counter_id, period, period_key);
`

// migrateAlertActions upgrades databases created before VK Teams delivery
// existed. SQLite cannot alter a CHECK constraint in place, so the table is
// rebuilt; rows created earlier keep their IDs and get target backfilled.
const migrateAlertActions = `
CREATE TABLE alert_actions_new (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	type TEXT NOT NULL CHECK(type IN ('telegram','vkteams','webhook')),
	chat_id INTEGER,
	target TEXT NOT NULL DEFAULT '',
	url TEXT,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO alert_actions_new (id, name, type, chat_id, target, url, created_at)
	SELECT id, name, type, chat_id,
	       CASE WHEN chat_id IS NULL THEN '' ELSE CAST(chat_id AS TEXT) END,
	       url, created_at
	FROM alert_actions;
DROP TABLE alert_actions;
ALTER TABLE alert_actions_new RENAME TO alert_actions;
`

// migrate brings an existing database up to the current schema. It is a no-op
// on a database that CREATE TABLE just built in its final shape.
func migrate(db *sql.DB) error {
	var ddl string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'alert_actions'`).Scan(&ddl)
	if err != nil {
		return fmt.Errorf("inspect alert_actions: %w", err)
	}
	if !strings.Contains(ddl, "vkteams") {
		if err := rebuildAlertActions(db); err != nil {
			return err
		}
	}
	return addColumnIfMissing(db, "counters", "last_event_at", "DATETIME")
}

// addColumnIfMissing brings an older table up to the current shape. SQLite
// supports ALTER TABLE ADD COLUMN, so unlike the CHECK constraint above this
// needs no table rebuild.
func addColumnIfMissing(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + decl); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	log.Printf("db migrated: %s.%s added", table, column)
	return nil
}

func rebuildAlertActions(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(migrateAlertActions); err != nil {
		return fmt.Errorf("migrate alert_actions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	log.Printf("db migrated: alert_actions now supports vkteams destinations")
	return nil
}

// DB wraps a SQLite database with our schema.
type DB struct {
	*sql.DB
}

// OpenDB creates or opens the database at path and ensures the schema exists.
//
// The poller, the bots and the REST API all write concurrently, so the
// connection enables WAL and a busy timeout — without them SQLite returns
// "database is locked" as soon as two of them overlap.
func OpenDB(path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	log.Printf("db ready at %s", path)
	return &DB{db}, nil
}
