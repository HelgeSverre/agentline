package store

import (
	"context"
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
