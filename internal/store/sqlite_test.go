package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
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

func intPtr(n int) *int { return &n }

func createAndClaim(t *testing.T, s Store) (CreatedRoom, ClaimResult) {
	t.Helper()
	created, err := s.CreateRoom(context.Background(), CreateRoomParams{Name: "room", CreatorName: "alice", TTL: time.Hour, MaxParticipants: intPtr(2)})
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
	created, err := s.CreateRoom(context.Background(), CreateRoomParams{Name: "alpha", CreatorName: "alice", TTL: 2 * time.Hour, MaxParticipants: intPtr(2)})
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

func TestCreateRoomSupportsUnlimitedAndPositiveCapacity(t *testing.T) {
	s, _, path := openTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		max  *int
		want any
		err  error
	}{
		{name: "unlimited", want: nil},
		{name: "capped", max: intPtr(3), want: int64(3)},
		{name: "zero", max: intPtr(0), err: ErrInvalid},
		{name: "negative", max: intPtr(-1), err: ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created, err := s.CreateRoom(ctx, CreateRoomParams{Name: tc.name, CreatorName: "alice", TTL: time.Hour, MaxParticipants: tc.max})
			if !errors.Is(err, tc.err) {
				t.Fatalf("CreateRoom() error = %v, want %v", err, tc.err)
			}
			if tc.err != nil {
				return
			}
			if created.Room.MaxParticipants == nil != (tc.want == nil) {
				t.Fatalf("room capacity = %v, want %v", created.Room.MaxParticipants, tc.want)
			}
			if tc.want == nil {
				encoded, err := json.Marshal(created.Room)
				if err != nil || !strings.Contains(string(encoded), `"max_participants":null`) {
					t.Fatalf("room JSON = %s, err = %v", encoded, err)
				}
			}
		})
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var unlimited sql.NullInt64
	if err := db.QueryRow(`SELECT max_participants FROM rooms WHERE public_name='unlimited'`).Scan(&unlimited); err != nil || unlimited.Valid {
		t.Fatalf("unlimited capacity = %v, err = %v", unlimited, err)
	}
	var capped int
	if err := db.QueryRow(`SELECT max_participants FROM rooms WHERE public_name='capped'`).Scan(&capped); err != nil || capped != 3 {
		t.Fatalf("capped capacity = %d, err = %v", capped, err)
	}
	if _, err := db.Exec(`INSERT INTO rooms(id,public_name,max_participants,status,created_at,expires_at) VALUES('large','crowd',1000000,'waiting_for_peer',0,1)`); err != nil {
		t.Fatalf("schema rejected large positive capacity: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO rooms(id,public_name,max_participants,status,created_at,expires_at) VALUES('zero','crowd',0,'waiting_for_peer',0,1)`); err == nil {
		t.Fatal("schema accepted zero capacity")
	}
}

func TestSchemaIsCleanMultiParticipantCutoff(t *testing.T) {
	_, _, path := openTestStore(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	columns := func(table string) map[string]bool {
		rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]bool{}
		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, dataType string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
				t.Fatal(err)
			}
			got[name] = notNull != 0
		}
		return got
	}

	invites := columns("invites")
	if _, ok := invites["claimed_at"]; ok {
		t.Fatal("invites retains claimed_at")
	}
	if _, ok := invites["claimed_by"]; ok {
		t.Fatal("invites retains claimed_by")
	}
	messages := columns("messages")
	if notNull, ok := messages["recipient_id"]; !ok || notNull {
		t.Fatalf("messages.recipient_id exists=%v not_null=%v", ok, notNull)
	}
	rooms := columns("rooms")
	if rooms["max_participants"] {
		t.Fatal("rooms.max_participants is not nullable")
	}
}

func TestInviteClaimIsAtomicOneUseAndCredentialsAuthenticate(t *testing.T) {
	s, _, _ := openTestStore(t)
	created, err := s.CreateRoom(context.Background(), CreateRoomParams{Name: "alpha", CreatorName: "alice", TTL: time.Hour, MaxParticipants: intPtr(2)})
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
	success, full := 0, 0
	for range 2 {
		result := <-results
		if result.err == nil {
			success++
			winner = result.claim
		} else if errors.Is(result.err, ErrRoomFull) {
			full++
		} else {
			t.Fatalf("claim error: %v", result.err)
		}
	}
	if success != 1 || full != 1 {
		t.Fatalf("success=%d full=%d", success, full)
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
	if _, err := s.ClaimInvite(context.Background(), created.InviteToken, "third"); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("second claim: %v", err)
	}
	clock.add(30 * time.Minute)
	room, err := s.GetRoom(context.Background(), created.Room.ID)
	if err != nil || !room.ExpiresAt.Equal(created.Room.ExpiresAt) {
		t.Fatalf("expiry changed: %+v %v", room, err)
	}
	if claimed.Room.MaxParticipants == nil || *claimed.Room.MaxParticipants != 2 {
		t.Fatal("capacity changed")
	}
}

func TestInviteClaimRejectsRoomAtCapacity(t *testing.T) {
	s, _, _ := openTestStore(t)
	created, err := s.CreateRoom(context.Background(), CreateRoomParams{Name: "solo", CreatorName: "alice", TTL: time.Hour, MaxParticipants: intPtr(1)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimInvite(context.Background(), created.InviteToken, "bob"); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("claim full room: %v", err)
	}
}

func TestReusableInviteCreatesDistinctParticipants(t *testing.T) {
	s, _, _ := openTestStore(t)
	ctx := context.Background()
	created, err := s.CreateRoom(ctx, CreateRoomParams{Name: "room", CreatorName: "alice", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ClaimInvite(ctx, created.InviteToken, "guest")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimInvite(ctx, created.InviteToken, "guest")
	if err != nil {
		t.Fatal(err)
	}
	if first.Participant.ID == second.Participant.ID || first.ParticipantToken == second.ParticipantToken {
		t.Fatalf("reused identity or token: first=%+v second=%+v", first, second)
	}
}

func TestParticipantsListsRoomMembersDirectly(t *testing.T) {
	s, _, _ := openTestStore(t)
	ctx := context.Background()
	created, err := s.CreateRoom(ctx, CreateRoomParams{Name: "room", CreatorName: "alice", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimInvite(ctx, created.InviteToken, "bob")
	if err != nil {
		t.Fatal(err)
	}

	participants, err := s.Participants(ctx, created.Room.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Participant{created.Creator, claimed.Participant}
	slices.SortFunc(want, func(a, b model.Participant) int {
		if joined := a.JoinedAt.Compare(b.JoinedAt); joined != 0 {
			return joined
		}
		return strings.Compare(a.ID, b.ID)
	})
	if len(participants) != len(want) {
		t.Fatalf("participants=%+v", participants)
	}
	for i := range want {
		if participants[i] != want[i] {
			t.Fatalf("participant[%d]=%+v, want %+v", i, participants[i], want[i])
		}
	}
}

func TestUnlimitedRoomAcceptsSeveralParticipants(t *testing.T) {
	s, _, _ := openTestStore(t)
	ctx := context.Background()
	created, err := s.CreateRoom(ctx, CreateRoomParams{Name: "room", CreatorName: "alice", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := s.ClaimInvite(ctx, created.InviteToken, "guest"); err != nil {
			t.Fatal(err)
		}
	}
	participants, err := s.Participants(ctx, created.Room.ID)
	if err != nil || len(participants) != 6 {
		t.Fatalf("participants=%+v err=%v", participants, err)
	}
}

func TestConcurrentClaimsStopExactlyAtCapacity(t *testing.T) {
	s, _, _ := openTestStore(t)
	ctx := context.Background()
	created, err := s.CreateRoom(ctx, CreateRoomParams{Name: "room", CreatorName: "alice", TTL: time.Hour, MaxParticipants: intPtr(3)})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 8)
	for range 8 {
		go func() {
			<-start
			_, err := s.ClaimInvite(ctx, created.InviteToken, "guest")
			results <- err
		}()
	}
	close(start)
	successes, full := 0, 0
	for range 8 {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, ErrRoomFull):
			full++
		default:
			t.Fatalf("claim error: %v", err)
		}
	}
	if successes != 2 || full != 6 {
		t.Fatalf("successes=%d full=%d", successes, full)
	}
	participants, err := s.Participants(ctx, created.Room.ID)
	if err != nil || len(participants) != 3 {
		t.Fatalf("participants=%+v err=%v", participants, err)
	}
}

func TestReusableInviteRejectsFullExpiredAndCompletedRooms(t *testing.T) {
	t.Run("full", func(t *testing.T) {
		s, _, _ := openTestStore(t)
		created, err := s.CreateRoom(context.Background(), CreateRoomParams{Name: "room", CreatorName: "alice", TTL: time.Hour, MaxParticipants: intPtr(1)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimInvite(context.Background(), created.InviteToken, "guest"); !errors.Is(err, ErrRoomFull) {
			t.Fatalf("claim error=%v", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		s, clock, _ := openTestStore(t)
		created, err := s.CreateRoom(context.Background(), CreateRoomParams{Name: "room", CreatorName: "alice", TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		clock.add(time.Hour)
		if _, err := s.ClaimInvite(context.Background(), created.InviteToken, "guest"); !errors.Is(err, ErrRoomExpired) {
			t.Fatalf("claim error=%v", err)
		}
	})
	t.Run("completed", func(t *testing.T) {
		s, _, _ := openTestStore(t)
		ctx := context.Background()
		created, err := s.CreateRoom(ctx, CreateRoomParams{Name: "room", CreatorName: "alice", TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CloseRoom(ctx, created.Room.ID, created.Creator.ID, "done"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimInvite(ctx, created.InviteToken, "guest"); !errors.Is(err, ErrRoomClosed) {
			t.Fatalf("claim error=%v", err)
		}
	})
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

func TestAppendBroadcastAndPrivateRecipient(t *testing.T) {
	s, _, path := openTestStore(t)
	ctx := context.Background()
	created, err := s.CreateRoom(ctx, CreateRoomParams{Name: "room", CreatorName: "alice", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.ClaimInvite(ctx, created.InviteToken, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimInvite(ctx, created.InviteToken, "carol"); err != nil {
		t.Fatal(err)
	}

	broadcast, err := s.Append(ctx, AppendParams{RoomID: created.Room.ID, ParticipantID: created.Creator.ID, MessageID: "broadcast", Body: "hello", Kind: "message"})
	if err != nil || broadcast.To != "" {
		t.Fatalf("broadcast=%+v err=%v", broadcast, err)
	}
	private, err := s.Append(ctx, AppendParams{RoomID: created.Room.ID, ParticipantID: created.Creator.ID, MessageID: "private", To: bob.Participant.ID, Body: "secret", Kind: "message"})
	if err != nil || private.To != bob.Participant.ID {
		t.Fatalf("private=%+v err=%v", private, err)
	}
	messages, err := s.MessagesAfter(ctx, created.Room.ID, 0, 10)
	if err != nil || len(messages) != 2 || messages[0].To != "" || messages[1].To != bob.Participant.ID {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	if _, err := s.CloseRoom(ctx, created.Room.ID, created.Creator.ID, "done"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, id := range []string{"broadcast", "done"} {
		var recipient sql.NullString
		if err := db.QueryRow(`SELECT recipient_id FROM messages WHERE id=?`, id).Scan(&recipient); err != nil || recipient.Valid {
			t.Fatalf("recipient for %q=%v err=%v", id, recipient, err)
		}
	}
}

func TestAppendRejectsUnknownAndCrossRoomRecipient(t *testing.T) {
	s, _, _ := openTestStore(t)
	ctx := context.Background()
	first, err := s.CreateRoom(ctx, CreateRoomParams{Name: "first", CreatorName: "alice", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateRoom(ctx, CreateRoomParams{Name: "second", CreatorName: "mallory", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	for _, recipient := range []string{"unknown", second.Creator.ID} {
		if _, err := s.Append(ctx, AppendParams{RoomID: first.Room.ID, ParticipantID: first.Creator.ID, MessageID: "rejected-" + recipient, To: recipient, Body: "secret", Kind: "message"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("recipient %q: %v", recipient, err)
		}
	}
	accepted, err := s.Append(ctx, AppendParams{RoomID: first.Room.ID, ParticipantID: first.Creator.ID, MessageID: "accepted", Body: "hello", Kind: "message"})
	if err != nil || accepted.Sequence != 1 {
		t.Fatalf("accepted=%+v err=%v", accepted, err)
	}
	messages, err := s.MessagesAfter(ctx, first.Room.ID, 0, 10)
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
}

func TestAppendIdempotencyIncludesRecipient(t *testing.T) {
	s, _, _ := openTestStore(t)
	created, claimed := createAndClaim(t, s)
	ctx := context.Background()
	p := AppendParams{RoomID: created.Room.ID, ParticipantID: created.Creator.ID, MessageID: "private", To: claimed.Participant.ID, Body: "secret", Kind: "message"}
	first, err := s.Append(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if retry, err := s.Append(ctx, p); err != nil || retry != first {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	p.To = ""
	if _, err := s.Append(ctx, p); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed recipient: %v", err)
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
