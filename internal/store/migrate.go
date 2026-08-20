package store

import (
	"context"
	"database/sql"
	"fmt"
)

// schemaVersions is the ordered history of the database layout. Entry 0 builds
// the schema from nothing; every later entry upgrades a database from the state
// the previous entry left it in.
//
// A database records how many entries it has run in SQLite's own
// PRAGMA user_version, so each start applies only what that database has not
// seen yet, in order, one transaction per entry.
//
// Rules for adding one:
//
//   - Append. Never edit or reorder an entry that has shipped, because it has
//     already run somewhere and will not run again there.
//   - Write it to upgrade, not to describe. Entry 0 is the only one allowed to
//     assume an empty database.
//   - Rebuilding a table is fine. Foreign keys are disabled while migrations
//     run, so the create-copy-drop-rename dance works.
var schemaVersions = []string{
	// 0: the schema as of v0.3.0. IF NOT EXISTS so that databases created by
	// v0.3.0 and v0.3.1, which built this layout without recording a version,
	// adopt it rather than failing on tables they already have.
	`
CREATE TABLE IF NOT EXISTS rooms (
 id TEXT PRIMARY KEY, public_name TEXT NOT NULL,
 max_participants INTEGER CHECK(max_participants > 0),
 status TEXT NOT NULL, next_sequence INTEGER NOT NULL DEFAULT 1,
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
 ended_at INTEGER, ended_by TEXT
);
CREATE TABLE IF NOT EXISTS participants (
 id TEXT PRIMARY KEY, room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
 name TEXT NOT NULL, token_hash BLOB NOT NULL UNIQUE, joined_at INTEGER NOT NULL,
 UNIQUE(room_id, id)
);
CREATE TABLE IF NOT EXISTS invites (
 id TEXT PRIMARY KEY, room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
 token_hash BLOB NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS inspectors (
 room_id TEXT PRIMARY KEY REFERENCES rooms(id) ON DELETE CASCADE,
 token_hash BLOB NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS messages (
 id TEXT NOT NULL, room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
 sequence INTEGER NOT NULL, sender_id TEXT NOT NULL, recipient_id TEXT,
 kind TEXT NOT NULL, body TEXT NOT NULL, reply_to TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, PRIMARY KEY(room_id, id), UNIQUE(room_id, sequence),
 FOREIGN KEY(room_id, sender_id) REFERENCES participants(room_id, id),
 FOREIGN KEY(room_id, recipient_id) REFERENCES participants(room_id, id)
);
CREATE INDEX IF NOT EXISTS messages_after ON messages(room_id, sequence);
CREATE INDEX IF NOT EXISTS messages_recipient_after ON messages(room_id, recipient_id, sequence);
CREATE INDEX IF NOT EXISTS participants_room ON participants(room_id, joined_at, id);
CREATE INDEX IF NOT EXISTS rooms_expiry ON rooms(expires_at);`,
}

// migrate brings db up to the latest schema version, applying only the entries
// it has not run. Each entry commits with the version it produced, so an
// interrupted run resumes at the entry that failed rather than repeating work
// that already landed.
func migrate(ctx context.Context, db *sql.DB, versions []string) error {
	// Everything below runs on one pinned connection. PRAGMA foreign_keys is
	// connection state, not database state, so a pool could otherwise apply it
	// to one connection and run the migration on another, and could hand a
	// connection still carrying foreign_keys=OFF to ordinary traffic afterwards.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	var applied int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&applied); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if applied > len(versions) {
		return fmt.Errorf("database is at schema version %d but this build only knows %d: it was written by a newer Agentline", applied, len(versions))
	}
	if applied == len(versions) {
		return nil
	}

	// Rebuilding a table means dropping and renaming it, which foreign keys
	// would refuse. They are off only for this connection, and only until the
	// migration finishes, which happens before the store serves anything.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for migration: %w", err)
	}

	for version := applied; version < len(versions); version++ {
		if err := applyVersion(ctx, conn, version, versions[version]); err != nil {
			restoreForeignKeys(ctx, conn)
			return err
		}
	}

	// A rebuilt table leaves the old pages behind; reclaim them once, after the
	// last migration, rather than inside a transaction where VACUUM is illegal.
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		restoreForeignKeys(ctx, conn)
		return fmt.Errorf("vacuum after migration: %w", err)
	}
	return restoreForeignKeys(ctx, conn)
}

// restoreForeignKeys puts the connection back the way ordinary traffic expects
// it before it returns to the pool, and refuses to continue if it cannot: a
// relay serving requests with foreign keys unenforced would corrupt quietly.
func restoreForeignKeys(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		return fmt.Errorf("re-enable foreign keys after migration: %w", err)
	}
	var on int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&on); err != nil {
		return fmt.Errorf("verify foreign keys after migration: %w", err)
	}
	if on != 1 {
		return fmt.Errorf("foreign keys remained disabled after migration")
	}
	return nil
}

func applyVersion(ctx context.Context, conn *sql.Conn, version int, statements string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema version %d: %w", version, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, statements); err != nil {
		return fmt.Errorf("apply schema version %d: %w", version, err)
	}
	// PRAGMA takes no bind parameters, so the version is formatted in. It is an
	// int from a loop bound, never input.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version+1)); err != nil {
		return fmt.Errorf("record schema version %d: %w", version+1, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema version %d: %w", version+1, err)
	}
	return nil
}
