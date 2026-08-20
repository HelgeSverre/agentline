package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// room creates a room with one extra participant and returns both ids.
func room(t *testing.T, s Store, name string) (string, string) {
	t.Helper()
	created, err := s.CreateRoom(context.Background(), CreateRoomParams{Name: name, CreatorName: "creator", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return created.Room.ID, created.Creator.ID
}

// TestMessageIDsAreScopedToTheirRoom pins the rule that clients choose these
// keys and agents derive them from message content, so two rooms independently
// picking "barcode-library-question" are two messages, not a retry of one.
func TestMessageIDsAreScopedToTheirRoom(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "relay.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	firstRoom, firstSender := room(t, s, "one")
	secondRoom, secondSender := room(t, s, "two")

	const shared = "barcode-library-question"
	a, err := s.Append(ctx, AppendParams{RoomID: firstRoom, ParticipantID: firstSender, MessageID: shared, Body: "which scanner?", Kind: "message"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Append(ctx, AppendParams{RoomID: secondRoom, ParticipantID: secondSender, MessageID: shared, Body: "which scanner?", Kind: "message"})
	if err != nil {
		t.Fatalf("the same key in another room was rejected: %v", err)
	}
	if a.RoomID == b.RoomID {
		t.Fatal("messages landed in the same room")
	}
	if a.Sequence != 1 || b.Sequence != 1 {
		t.Fatalf("each room sequences independently: %d, %d", a.Sequence, b.Sequence)
	}
}

// TestRetryAfterALostResponseReturnsTheOriginal is why the key exists: a client
// that never saw the response resends with the same key and must get the
// original message back rather than a duplicate or an error.
func TestRetryAfterALostResponseReturnsTheOriginal(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "relay.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	roomID, sender := room(t, s, "retry")

	params := AppendParams{RoomID: roomID, ParticipantID: sender, MessageID: "k1", Body: "hello", Kind: "message"}
	first, err := s.Append(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Append(ctx, params)
	if err != nil {
		t.Fatalf("retry rejected: %v", err)
	}
	if again.Sequence != first.Sequence || again.ID != first.ID {
		t.Fatalf("retry produced a different message: %+v vs %+v", again, first)
	}

	// Reusing a key for genuinely different content stays a conflict.
	params.Body = "something else"
	if _, err := s.Append(ctx, params); err != ErrConflict {
		t.Fatalf("reusing a key for new content = %v, want ErrConflict", err)
	}
}

// TestMigrationRescopesGloballyKeyedMessages upgrades a database written with
// the old global primary key and checks both that history survives and that the
// new constraint is in force.
func TestMigrationRescopesGloballyKeyedMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(0)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
CREATE TABLE rooms (id TEXT PRIMARY KEY, public_name TEXT NOT NULL, max_participants INTEGER CHECK(max_participants > 0), status TEXT NOT NULL, next_sequence INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, ended_at INTEGER, ended_by TEXT);
CREATE TABLE participants (id TEXT PRIMARY KEY, room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE, name TEXT NOT NULL, token_hash BLOB NOT NULL UNIQUE, joined_at INTEGER NOT NULL, UNIQUE(room_id, id));
CREATE TABLE invites (id TEXT PRIMARY KEY, room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE, token_hash BLOB NOT NULL UNIQUE);
CREATE TABLE inspectors (room_id TEXT PRIMARY KEY REFERENCES rooms(id) ON DELETE CASCADE, token_hash BLOB NOT NULL UNIQUE);
CREATE TABLE messages (id TEXT PRIMARY KEY, room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE, sequence INTEGER NOT NULL, sender_id TEXT NOT NULL, recipient_id TEXT, kind TEXT NOT NULL, body TEXT NOT NULL, reply_to TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, UNIQUE(room_id, sequence), FOREIGN KEY(room_id, sender_id) REFERENCES participants(room_id, id), FOREIGN KEY(room_id, recipient_id) REFERENCES participants(room_id, id));
INSERT INTO rooms(id,public_name,max_participants,status,next_sequence,created_at,expires_at) VALUES ('r1','old',NULL,'active',2,0,9000000000000000000);
INSERT INTO participants(id,room_id,name,token_hash,joined_at) VALUES ('p1','r1','alice',x'00',0);
INSERT INTO messages(id,room_id,sequence,sender_id,kind,body,reply_to,created_at) VALUES ('shared','r1',1,'p1','message','history',	'',0);`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s, err := OpenSQLite(path, nil)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// History survived.
	visible, err := s.MessagesAfter(ctx, "r1", "p1", 0, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible.Messages) != 1 || visible.Messages[0].Body != "history" {
		t.Fatalf("history lost in migration: %+v", visible.Messages)
	}

	// The key that was globally taken is now reusable in another room.
	other, sender := room(t, s, "new")
	if _, err := s.Append(ctx, AppendParams{RoomID: other, ParticipantID: sender, MessageID: "shared", Body: "reused", Kind: "message"}); err != nil {
		t.Fatalf("key still globally reserved after migration: %v", err)
	}
	// Running migration again on the upgraded database is a no-op.
	if again, err := OpenSQLite(path, nil); err != nil {
		t.Fatalf("reopening a migrated database failed: %v", err)
	} else {
		again.Close()
	}
}
