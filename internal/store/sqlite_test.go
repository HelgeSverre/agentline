package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/HelgeSverre/agentline/internal/model"
	_ "modernc.org/sqlite"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func openTestStore(t *testing.T) (Store, *testClock, string) {
	t.Helper()
	clock := &testClock{t: time.Date(2026, 8, 7, 12, 0, 0, 123456789, time.FixedZone("test", 3600))}
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := OpenSQLite(path, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clock, path
}

func createAndClaim(t *testing.T, s Store) (CreatedRoom, ClaimResult) {
	t.Helper()
	created, err := s.CreateRoom(context.Background(), CreateRoomParams{Name: "room", CreatorName: "alice", TTL: time.Hour, MaxParticipants: 2})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimInvite(context.Background(), created.InviteToken, "bob")
	if err != nil {
		t.Fatal(err)
	}
	return created, claimed
}

func TestCreateRoomUsesFixedExpiryAndStoresOnlyHashes(t *testing.T) {
	s, clock, path := openTestStore(t)
	created, err := s.CreateRoom(context.Background(), CreateRoomParams{Name: "alpha", CreatorName: "alice", TTL: 2 * time.Hour, MaxParticipants: 2})
	if err != nil {
		t.Fatal(err)
	}
	if created.Room.Status != "waiting_for_peer" || created.Room.CreatedAt != clock.now().UTC() || created.Room.ExpiresAt != clock.now().Add(2*time.Hour).UTC() {
		t.Fatalf("unexpected room: %+v", created.Room)
	}
	if created.CreatorToken == "" || created.InviteToken == "" || created.CreatorToken == created.InviteToken {
		t.Fatal("missing or shared credentials")
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var participantHash, inviteHash []byte
	if err := db.QueryRow(`SELECT token_hash FROM participants`).Scan(&participantHash); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT token_hash FROM invites`).Scan(&inviteHash); err != nil {
		t.Fatal(err)
	}
	if string(participantHash) == created.CreatorToken || string(inviteHash) == created.InviteToken {
		t.Fatal("raw token stored")
	}
	if len(participantHash) != 32 || len(inviteHash) != 32 {
		t.Fatalf("hash lengths = %d, %d", len(participantHash), len(inviteHash))
	}
}

func TestCreateRoomRejectsMoreThanTwoParticipantsInGoAndSchema(t *testing.T) {
	s, _, path := openTestStore(t)
	if _, err := s.CreateRoom(context.Background(), CreateRoomParams{Name: "crowd", CreatorName: "alice", TTL: time.Hour, MaxParticipants: 3}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("create room with 3 participants: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO rooms(id,public_name,max_participants,status,created_at,expires_at) VALUES('invalid','crowd',3,'waiting_for_peer',0,1)`)
	if err == nil {
		t.Fatal("schema accepted max_participants=3")
	}
}

func TestInviteClaimIsAtomicOneUseAndCredentialsAuthenticate(t *testing.T) {
	s, _, _ := openTestStore(t)
	created, err := s.CreateRoom(context.Background(), CreateRoomParams{Name: "alpha", CreatorName: "alice", TTL: time.Hour, MaxParticipants: 2})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type claimResult struct {
		claim ClaimResult
		err   error
	}
	results := make(chan claimResult, 2)
	for _, name := range []string{"bob", "eve"} {
		go func() {
			<-start
			claim, err := s.ClaimInvite(context.Background(), created.InviteToken, name)
			results <- claimResult{claim: claim, err: err}
		}()
	}
	close(start)
	var winner ClaimResult
	success, consumed := 0, 0
	for range 2 {
		result := <-results
		if result.err == nil {
			success++
			winner = result.claim
		} else if errors.Is(result.err, ErrInviteClaimed) {
			consumed++
		} else {
			t.Fatalf("claim error: %v", result.err)
		}
	}
	if success != 1 || consumed != 1 {
		t.Fatalf("success=%d consumed=%d", success, consumed)
	}
	if winner.ParticipantToken == created.CreatorToken {
		t.Fatal("participants share credential")
	}
	if _, err := s.Authenticate(context.Background(), created.Room.ID, created.CreatorToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(context.Background(), created.Room.ID, winner.ParticipantToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(context.Background(), created.Room.ID, "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong token: %v", err)
	}
	room, err := s.GetRoom(context.Background(), created.Room.ID)
	if err != nil || room.Status != "active" {
		t.Fatalf("room=%+v err=%v", room, err)
	}
}

func TestCapacityAndFixedExpiry(t *testing.T) {
	s, clock, _ := openTestStore(t)
	created, claimed := createAndClaim(t, s)
	if _, err := s.ClaimInvite(context.Background(), created.InviteToken, "third"); !errors.Is(err, ErrInviteClaimed) {
		t.Fatalf("second claim: %v", err)
	}
	clock.add(30 * time.Minute)
	room, err := s.GetRoom(context.Background(), created.Room.ID)
	if err != nil || !room.ExpiresAt.Equal(created.Room.ExpiresAt) {
		t.Fatalf("expiry changed: %+v %v", room, err)
	}
	if claimed.Room.MaxParticipants != 2 {
		t.Fatal("capacity changed")
	}
}

func TestInviteClaimRejectsRoomAtCapacity(t *testing.T) {
	s, _, _ := openTestStore(t)
	created, err := s.CreateRoom(context.Background(), CreateRoomParams{Name: "solo", CreatorName: "alice", TTL: time.Hour, MaxParticipants: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimInvite(context.Background(), created.InviteToken, "bob"); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("claim full room: %v", err)
	}
}

func TestAppendOrderingIdempotencyLimitAndDoneHistory(t *testing.T) {
	s, _, _ := openTestStore(t)
	created, claimed := createAndClaim(t, s)
	ctx := context.Background()
	first, err := s.Append(ctx, AppendParams{RoomID: created.Room.ID, ParticipantID: created.Creator.ID, MessageID: "m1", Body: "hello", Kind: model.MessageKind("message")})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.Append(ctx, AppendParams{RoomID: created.Room.ID, ParticipantID: created.Creator.ID, MessageID: "m1", Body: "changed", Kind: model.MessageKind("message")})
	if !errors.Is(err, ErrConflict) || duplicate != (model.Message{}) {
		t.Fatalf("conflicting duplicate=%+v err=%v", duplicate, err)
	}
	retry, err := s.Append(ctx, AppendParams{RoomID: created.Room.ID, ParticipantID: created.Creator.ID, MessageID: "m1", Body: "hello", Kind: model.MessageKind("message")})
	if err != nil || retry != first {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	for i := 2; i <= 999; i++ {
		_, err = s.Append(ctx, AppendParams{RoomID: created.Room.ID, ParticipantID: claimed.Participant.ID, MessageID: "m" + fmtInt(i), Body: "x", Kind: model.MessageKind("message")})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	done, err := s.CloseRoom(ctx, created.Room.ID, claimed.Participant.ID, "done-id")
	if err != nil || done.Sequence != 1000 || done.Kind != "done" {
		t.Fatalf("done=%+v err=%v", done, err)
	}
	if _, err := s.Append(ctx, AppendParams{RoomID: created.Room.ID, ParticipantID: created.Creator.ID, MessageID: "late", Body: "late", Kind: "message"}); !errors.Is(err, ErrRoomClosed) {
		t.Fatalf("late append: %v", err)
	}
	messages, err := s.MessagesAfter(ctx, created.Room.ID, 997, 20)
	if err != nil || len(messages) != 3 || messages[0].Sequence != 998 || messages[2].Kind != "done" {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	if _, err := s.CloseRoom(ctx, created.Room.ID, claimed.Participant.ID, "another"); !errors.Is(err, ErrRoomClosed) {
		t.Fatalf("second done: %v", err)
	}
}

func TestMessageIDReuseAcrossAppendAndCloseConflictsWithoutWrongTransition(t *testing.T) {
	s, _, _ := openTestStore(t)
	created, claimed := createAndClaim(t, s)
	ctx := context.Background()

	if _, err := s.Append(ctx, AppendParams{RoomID: created.Room.ID, ParticipantID: created.Creator.ID, MessageID: "ordinary", Body: "hello", ReplyTo: "prior", Kind: "message"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseRoom(ctx, created.Room.ID, created.Creator.ID, "ordinary"); !errors.Is(err, ErrConflict) {
		t.Fatalf("close with ordinary message ID: %v", err)
	}
	room, err := s.GetRoom(ctx, created.Room.ID)
	if err != nil || room.Status != "active" {
		t.Fatalf("ordinary ID reuse changed room: room=%+v err=%v", room, err)
	}

	if _, err := s.CloseRoom(ctx, created.Room.ID, claimed.Participant.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, AppendParams{RoomID: created.Room.ID, ParticipantID: claimed.Participant.ID, MessageID: "done", Kind: "message"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("append with done message ID: %v", err)
	}
	messages, err := s.MessagesAfter(ctx, created.Room.ID, 0, 10)
	if err != nil || len(messages) != 2 || messages[1].Kind != "done" {
		t.Fatalf("history after done ID reuse: messages=%+v err=%v", messages, err)
	}
}

func TestAppendCannotImpersonateCloseRoomWithSyntheticDone(t *testing.T) {
	s, _, _ := openTestStore(t)
	created, claimed := createAndClaim(t, s)
	ctx := context.Background()
	done := AppendParams{RoomID: created.Room.ID, ParticipantID: claimed.Participant.ID, MessageID: "synthetic-done", Kind: "done"}

	if _, err := s.Append(ctx, done); !errors.Is(err, ErrInvalid) {
		t.Fatalf("append synthetic done: %v", err)
	}
	closed, err := s.CloseRoom(ctx, done.RoomID, done.ParticipantID, done.MessageID)
	if err != nil || closed.Kind != "done" || closed.ID != done.MessageID {
		t.Fatalf("close after rejected synthetic done: message=%+v err=%v", closed, err)
	}
	room, err := s.GetRoom(ctx, done.RoomID)
	if err != nil || room.Status != "done" {
		t.Fatalf("room after close: room=%+v err=%v", room, err)
	}
}

func TestAppendCannotRetryCloseRoomSyntheticDone(t *testing.T) {
	s, _, _ := openTestStore(t)
	created, claimed := createAndClaim(t, s)
	ctx := context.Background()
	done := AppendParams{RoomID: created.Room.ID, ParticipantID: claimed.Participant.ID, MessageID: "synthetic-done", Kind: "done"}

	closed, err := s.CloseRoom(ctx, done.RoomID, done.ParticipantID, done.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, done); !errors.Is(err, ErrInvalid) {
		t.Fatalf("append exact close payload: %v", err)
	}
	retry, err := s.CloseRoom(ctx, done.RoomID, done.ParticipantID, done.MessageID)
	if err != nil || retry != closed {
		t.Fatalf("close retry: message=%+v err=%v", retry, err)
	}
}

func TestExactRetriesCannotAccessExpiredRoomData(t *testing.T) {
	s, clock, _ := openTestStore(t)
	created, claimed := createAndClaim(t, s)
	ctx := context.Background()
	text := AppendParams{RoomID: created.Room.ID, ParticipantID: created.Creator.ID, MessageID: "text", Body: "hello", Kind: "message"}
	if _, err := s.Append(ctx, text); err != nil {
		t.Fatal(err)
	}
	done, err := s.CloseRoom(ctx, created.Room.ID, claimed.Participant.ID, "done")
	if err != nil {
		t.Fatal(err)
	}
	if retry, err := s.CloseRoom(ctx, created.Room.ID, claimed.Participant.ID, done.ID); err != nil || retry != done {
		t.Fatalf("close retry before expiry: message=%+v err=%v", retry, err)
	}

	clock.add(2 * time.Hour)
	if _, err := s.Append(ctx, text); !errors.Is(err, ErrRoomExpired) {
		t.Fatalf("append retry after expiry: %v", err)
	}
	if _, err := s.CloseRoom(ctx, created.Room.ID, claimed.Participant.ID, done.ID); !errors.Is(err, ErrRoomExpired) {
		t.Fatalf("close retry after expiry: %v", err)
	}
}

func TestMessagesSchemaRequiresSenderToParticipateInRoom(t *testing.T) {
	s, _, path := openTestStore(t)
	first, _ := createAndClaim(t, s)
	second, _ := createAndClaim(t, s)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO messages(id,room_id,sequence,sender_id,kind,body,reply_to,created_at) VALUES('cross-room',?,1,?,'message','','',0)`, first.Room.ID, second.Creator.ID)
	if err == nil {
		t.Fatal("schema accepted a sender from another room")
	}
}

func TestEventLimit(t *testing.T) {
	s, _, _ := openTestStore(t)
	created, _ := createAndClaim(t, s)
	for i := 1; i <= 1000; i++ {
		if _, err := s.Append(context.Background(), AppendParams{RoomID: created.Room.ID, ParticipantID: created.Creator.ID, MessageID: fmtInt(i), Body: "x", Kind: "message"}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if _, err := s.Append(context.Background(), AppendParams{RoomID: created.Room.ID, ParticipantID: created.Creator.ID, MessageID: "overflow", Body: "x", Kind: "message"}); !errors.Is(err, ErrEventLimit) {
		t.Fatalf("overflow: %v", err)
	}
}

func TestLazyExpiryAndDeleteExpired(t *testing.T) {
	s, clock, _ := openTestStore(t)
	created, _ := createAndClaim(t, s)
	clock.add(2 * time.Hour)
	if _, err := s.GetRoom(context.Background(), created.Room.ID); !errors.Is(err, ErrRoomExpired) {
		t.Fatalf("get expired: %v", err)
	}
	if _, err := s.Authenticate(context.Background(), created.Room.ID, created.CreatorToken); !errors.Is(err, ErrRoomExpired) {
		t.Fatalf("auth expired: %v", err)
	}
	if _, err := s.MessagesAfter(context.Background(), created.Room.ID, 0, 10); !errors.Is(err, ErrRoomExpired) {
		t.Fatalf("history expired: %v", err)
	}
	deleted, err := s.DeleteExpired(context.Background(), clock.now())
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	if _, err := s.GetRoom(context.Background(), created.Room.ID); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("deleted room: %v", err)
	}
}

func fmtInt(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	b := make([]byte, 0, 4)
	for ; n > 0; n /= 10 {
		b = append(b, digits[n%10])
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}
