package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func schemaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func TestMigrateBuildsAFreshDatabaseAndRecordsItsVersion(t *testing.T) {
	db := openRaw(t, filepath.Join(t.TempDir(), "fresh.db"))
	if err := migrate(context.Background(), db, schemaVersions); err != nil {
		t.Fatal(err)
	}
	if got := schemaVersion(t, db); got != len(schemaVersions) {
		t.Fatalf("schema version = %d, want %d", got, len(schemaVersions))
	}
	if _, err := db.Exec(`SELECT 1 FROM messages`); err != nil {
		t.Fatalf("schema not built: %v", err)
	}
}

// A second open must not repeat work that already landed.
func TestMigrateIsANoOpOnAnUpToDateDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "again.db")
	db := openRaw(t, path)
	ctx := context.Background()
	if err := migrate(ctx, db, schemaVersions); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO rooms(id,public_name,status,next_sequence,created_at,expires_at) VALUES('r','n','active',1,0,9000000000000000000)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, schemaVersions); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var rooms int
	if err := db.QueryRow(`SELECT count(*) FROM rooms`).Scan(&rooms); err != nil || rooms != 1 {
		t.Fatalf("rows = %d, err = %v; a no-op migration must not touch data", rooms, err)
	}
}

// Databases written by v0.3.0 and v0.3.1 hold the current tables but record no
// version, so version 0 must adopt them instead of failing on tables that exist.
func TestMigrateAdoptsAnUnversionedDatabaseBuiltByAnEarlierRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unversioned.db")
	db := openRaw(t, path)
	if _, err := db.Exec(schemaVersions[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO rooms(id,public_name,status,next_sequence,created_at,expires_at) VALUES('r','n','active',1,0,9000000000000000000)`); err != nil {
		t.Fatal(err)
	}
	if got := schemaVersion(t, db); got != 0 {
		t.Fatalf("fixture should be unversioned, got %d", got)
	}
	if err := migrate(context.Background(), db, schemaVersions); err != nil {
		t.Fatalf("adopting an unversioned database failed: %v", err)
	}
	if got := schemaVersion(t, db); got != len(schemaVersions) {
		t.Fatalf("schema version = %d, want %d", got, len(schemaVersions))
	}
	var rooms int
	if err := db.QueryRow(`SELECT count(*) FROM rooms`).Scan(&rooms); err != nil || rooms != 1 {
		t.Fatalf("adoption lost data: rows = %d, err = %v", rooms, err)
	}
}

// The point of the whole thing: a version appended later runs by itself, on a
// database that already holds data, and only that version runs.
func TestMigrateAppliesOnlyNewVersionsAndKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db := openRaw(t, path)
	ctx := context.Background()
	if err := migrate(ctx, db, schemaVersions); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO rooms(id,public_name,status,next_sequence,created_at,expires_at) VALUES('r','keepme','active',1,0,9000000000000000000)`); err != nil {
		t.Fatal(err)
	}

	// A later release appends a version. It must run against live data.
	future := append(append([]string{}, schemaVersions...),
		`ALTER TABLE rooms ADD COLUMN note TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE audit (id INTEGER PRIMARY KEY, note TEXT NOT NULL)`,
	)
	if err := migrate(ctx, db, future); err != nil {
		t.Fatalf("applying appended versions: %v", err)
	}
	if got := schemaVersion(t, db); got != len(future) {
		t.Fatalf("schema version = %d, want %d", got, len(future))
	}
	var name, note string
	if err := db.QueryRow(`SELECT public_name, note FROM rooms WHERE id='r'`).Scan(&name, &note); err != nil {
		t.Fatalf("data did not survive the upgrade: %v", err)
	}
	if name != "keepme" {
		t.Fatalf("public_name = %q, want keepme", name)
	}
	if _, err := db.Exec(`SELECT 1 FROM audit`); err != nil {
		t.Fatalf("appended version did not run: %v", err)
	}

	// Running the same set again changes nothing.
	if err := migrate(ctx, db, future); err != nil {
		t.Fatalf("re-running: %v", err)
	}
	if got := schemaVersion(t, db); got != len(future) {
		t.Fatalf("schema version drifted to %d", got)
	}
}

// A failed version must leave the database on the previous one, so the next
// start retries it rather than skipping it.
func TestMigrateLeavesTheVersionUnchangedWhenAVersionFails(t *testing.T) {
	db := openRaw(t, filepath.Join(t.TempDir(), "broken.db"))
	ctx := context.Background()
	broken := append(append([]string{}, schemaVersions...), `CREATE TABLE oops (this is not valid sql`)

	err := migrate(ctx, db, broken)
	if err == nil {
		t.Fatal("expected the invalid version to fail")
	}
	if !strings.Contains(err.Error(), "apply schema version 1") {
		t.Fatalf("error should name the failing version, got %v", err)
	}
	if got := schemaVersion(t, db); got != len(schemaVersions) {
		t.Fatalf("schema version = %d; a failed version must not be recorded", got)
	}
	// The versions that did succeed are still in place.
	if _, err := db.Exec(`SELECT 1 FROM messages`); err != nil {
		t.Fatalf("earlier versions rolled back: %v", err)
	}
}

// Downgrading a relay must fail loudly rather than run against a layout it does
// not understand.
func TestMigrateRefusesADatabaseFromANewerBuild(t *testing.T) {
	db := openRaw(t, filepath.Join(t.TempDir(), "newer.db"))
	ctx := context.Background()
	if err := migrate(ctx, db, schemaVersions); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	err := migrate(ctx, db, schemaVersions)
	if err == nil || !strings.Contains(err.Error(), "newer Agentline") {
		t.Fatalf("err = %v, want a refusal naming a newer build", err)
	}
}

// A version may rebuild a table, which requires foreign keys to be off. That
// pragma is connection state, so this also checks the pool is left the way
// ordinary traffic expects it: a connection still carrying foreign_keys=OFF
// would let later writes violate references silently.
func TestMigrateRebuildsTablesWithoutLeavingForeignKeysDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rebuild.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	// Put several connections in the pool first, so the migration cannot rely
	// on being handed the same one twice by chance.
	var held []*sql.Conn
	for i := 0; i < 5; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.PingContext(ctx); err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	for _, c := range held {
		c.Close()
	}

	if err := migrate(ctx, db, schemaVersions); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO rooms(id,public_name,status,next_sequence,created_at,expires_at) VALUES('r','n','active',1,0,9000000000000000000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO participants(id,room_id,name,token_hash,joined_at) VALUES('p','r','alice',x'00',0)`); err != nil {
		t.Fatal(err)
	}

	// A later version rebuilds participants, which rooms and messages reference.
	rebuild := append(append([]string{}, schemaVersions...), `
ALTER TABLE participants RENAME TO participants_old;
CREATE TABLE participants (id TEXT PRIMARY KEY, room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE, name TEXT NOT NULL, token_hash BLOB NOT NULL UNIQUE, joined_at INTEGER NOT NULL, nickname TEXT NOT NULL DEFAULT '', UNIQUE(room_id, id));
INSERT INTO participants(id,room_id,name,token_hash,joined_at) SELECT id,room_id,name,token_hash,joined_at FROM participants_old;
DROP TABLE participants_old;`)
	if err := migrate(ctx, db, rebuild); err != nil {
		t.Fatalf("rebuilding a referenced table failed: %v", err)
	}

	var nickname string
	if err := db.QueryRowContext(ctx, `SELECT nickname FROM participants WHERE id='p'`).Scan(&nickname); err != nil {
		t.Fatalf("rebuild lost the row: %v", err)
	}

	// Every connection the pool hands out must enforce foreign keys again.
	for i := 0; i < 10; i++ {
		var on int
		if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&on); err != nil {
			t.Fatal(err)
		}
		if on != 1 {
			t.Fatalf("a pooled connection reports foreign_keys=%d after migration", on)
		}
	}
	// And enforcement is real, not just reported.
	if _, err := db.ExecContext(ctx, `INSERT INTO participants(id,room_id,name,token_hash,joined_at) VALUES('x','nosuchroom','bob',x'01',0)`); err == nil {
		t.Fatal("foreign keys are not being enforced after migration")
	}
}
