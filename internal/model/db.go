package model

import (
	"database/sql"
	"fmt"
	"log"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS counters (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	counter_id TEXT UNIQUE NOT NULL,
	oauth_token TEXT NOT NULL,
	poll_interval_minutes INTEGER NOT NULL DEFAULT 60,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
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
	type TEXT NOT NULL CHECK(type IN ('telegram','webhook')),
	chat_id INTEGER,
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
CREATE INDEX IF NOT EXISTS idx_snapshots_counter_period ON report_snapshots(counter_id, period, period_key);
`

// DB wraps a SQLite database with our schema.
type DB struct {
	*sql.DB
}

// OpenDB creates or opens the database at path and ensures the schema exists.
func OpenDB(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	log.Printf("db ready at %s", path)
	return &DB{db}, nil
}
