package model

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	_ "modernc.org/sqlite"
)

// schemaTriggers and schemaCooldowns are separate so the migration can rebuild
// them without restating the DDL.
const schemaTriggers = `
CREATE TABLE IF NOT EXISTS triggers (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	counter_id INTEGER NOT NULL REFERENCES counters(id),
	name TEXT NOT NULL,
	metric TEXT NOT NULL,
	direction TEXT NOT NULL DEFAULT 'drop' CHECK(direction IN ('drop','rise','both')),
	deviation_percent INTEGER NOT NULL DEFAULT 30,
	min_baseline INTEGER NOT NULL DEFAULT 10,
	baseline_weeks INTEGER NOT NULL DEFAULT 4,
	url_filter TEXT NOT NULL DEFAULT '',
	url_match TEXT NOT NULL DEFAULT '',
	cooldown_minutes INTEGER NOT NULL DEFAULT 180,
	enabled BOOLEAN NOT NULL DEFAULT 1,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);`

const schemaCooldowns = `
CREATE TABLE IF NOT EXISTS trigger_cooldowns (
	trigger_id INTEGER PRIMARY KEY REFERENCES triggers(id),
	last_fired_at DATETIME DEFAULT CURRENT_TIMESTAMP
);`

const schema = schemaTriggers + schemaCooldowns + `
CREATE TABLE IF NOT EXISTS counters (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	counter_id TEXT UNIQUE NOT NULL,
	oauth_token TEXT NOT NULL,
	poll_interval_minutes INTEGER NOT NULL DEFAULT 60,
	-- Start of the most recent hour already judged, so a restart neither
	-- re-alerts on it nor skips the hours in between.
	last_hour_checked DATETIME,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);



-- Settings the bot can change at runtime. Keeping them here rather than in
-- config.yaml is what lets a schedule be retuned from a chat without editing a
-- file and restarting the service.
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
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

-- A report definition: what a periodic report covers. Without any, the service
-- reports each counter as a whole, which is what it always did.
CREATE TABLE IF NOT EXISTS reports (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	counter_id INTEGER NOT NULL REFERENCES counters(id),
	name TEXT NOT NULL,
	url_filter TEXT NOT NULL DEFAULT '',
	url_match TEXT NOT NULL DEFAULT '',
	-- Comma-separated goal IDs; empty means every goal of the counter.
	goal_ids TEXT NOT NULL DEFAULT '',
	group_by TEXT NOT NULL DEFAULT '',
	period TEXT NOT NULL DEFAULT '',
	enabled BOOLEAN NOT NULL DEFAULT 1,
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


CREATE INDEX IF NOT EXISTS idx_alerts_counter ON alerts(counter_id);
CREATE INDEX IF NOT EXISTS idx_reports_counter ON reports(counter_id);
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
	if err := migrateAlertActionsTable(db); err != nil {
		return err
	}
	if err := migrateTriggersTable(db); err != nil {
		return err
	}
	// CREATE TABLE IF NOT EXISTS leaves an existing table untouched, so columns
	// added since have to be applied explicitly.
	if err := addColumnIfMissing(db, "counters", "last_hour_checked", "DATETIME"); err != nil {
		return err
	}
	for _, column := range []string{"url_filter", "url_match"} {
		if err := addColumnIfMissing(db, "triggers", column, "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if err := addColumnIfMissing(db, "reports", "group_by", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "reports", "period", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return dropObsoleteTables(db)
}

func migrateAlertActionsTable(db *sql.DB) error {
	ddl, err := tableDDL(db, "alert_actions")
	if err != nil {
		return err
	}
	if strings.Contains(ddl, "vkteams") {
		return nil
	}
	return rebuildAlertActions(db)
}

// migrateTriggersTable replaces the per-event trigger table with the metric
// one. Old rows are dropped rather than converted: a condition like
// "status_code == 500" counted individual hits from the Logs API and has no
// equivalent among aggregated hourly metrics.
func migrateTriggersTable(db *sql.DB) error {
	ddl, err := tableDDL(db, "triggers")
	if err != nil {
		return err
	}
	if strings.Contains(ddl, "deviation_percent") {
		return nil
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM triggers`).Scan(&count)

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin trigger migration: %w", err)
	}
	defer tx.Rollback()

	// Cooldowns and past alerts reference trigger IDs that will not exist.
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS trigger_cooldowns`,
		`DROP TABLE triggers`,
		strings.Replace(schemaTriggers, "IF NOT EXISTS ", "", 1),
		schemaCooldowns,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate triggers: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit trigger migration: %w", err)
	}

	if count > 0 {
		log.Printf("db migrated: %d event-condition trigger(s) removed — "+
			"alerts now compare hourly metrics against the same hour last weeks; recreate them with /addtrigger", count)
	} else {
		log.Printf("db migrated: triggers now hold metric anomaly rules")
	}
	return nil
}

// dropObsoleteTables removes structures whose only consumer was the per-event
// engine: page monitors scoped event conditions to a URL, and log_requests
// tracked Logs API exports.
func dropObsoleteTables(db *sql.DB) error {
	for _, table := range []string{"page_monitors", "log_requests"} {
		exists, err := tableExists(db, table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := db.Exec(`DROP TABLE ` + table); err != nil {
			return fmt.Errorf("drop %s: %w", table, err)
		}
		log.Printf("db migrated: %s dropped — it served the retired per-event engine", table)
	}
	return nil
}

// addColumnIfMissing brings an older table up to the current shape. SQLite
// supports ALTER TABLE ADD COLUMN, so unlike a CHECK constraint this needs no
// table rebuild.
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

func tableDDL(db *sql.DB, table string) (string, error) {
	var ddl string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&ddl)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", table, err)
	}
	return ddl, nil
}

func tableExists(db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", table, err)
	}
	return true, nil
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
