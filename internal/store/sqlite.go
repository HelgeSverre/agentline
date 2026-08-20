package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/HelgeSverre/agentline/internal/model"
	"github.com/HelgeSverre/agentline/internal/securetoken"
	_ "modernc.org/sqlite"
)

const maxEvents = 1000

type sqliteStore struct {
	db  *sql.DB
	now func() time.Time
}

func OpenSQLite(path string, now func() time.Time) (Store, error) {
	if now == nil {
		now = time.Now
	}
	dsn := "file:" + url.PathEscape(path) + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	s := &sqliteStore{db: db, now: now}
	if err := s.createSchema(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// createSchema declares the schema. Relay data is disposable, so there is no
// upgrade path from an older layout: rooms expire within days, and a relay
// carrying a database from an earlier schema is started against a fresh data
// directory rather than converted.
func (s *sqliteStore) createSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
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
CREATE INDEX IF NOT EXISTS rooms_expiry ON rooms(expires_at);`)
	if err != nil {
		return fmt.Errorf("create sqlite schema: %w", err)
	}
	return nil
}

func (s *sqliteStore) CreateRoom(ctx context.Context, p CreateRoomParams) (CreatedRoom, error) {
	if p.TTL <= 0 || p.MaxParticipants != nil && *p.MaxParticipants <= 0 {
		return CreatedRoom{}, ErrInvalid
	}
	roomID, err := securetoken.New(16)
	if err != nil {
		return CreatedRoom{}, err
	}
	participantID, err := securetoken.New(16)
	if err != nil {
		return CreatedRoom{}, err
	}
	inviteID, err := securetoken.New(16)
	if err != nil {
		return CreatedRoom{}, err
	}
	creatorToken, err := securetoken.New(32)
	if err != nil {
		return CreatedRoom{}, err
	}
	inviteToken, err := securetoken.New(32)
	if err != nil {
		return CreatedRoom{}, err
	}
	inspectToken, err := securetoken.New(32)
	if err != nil {
		return CreatedRoom{}, err
	}
	now := s.now().UTC()
	room := model.Room{ID: roomID, Name: p.Name, Status: "waiting_for_peer", MaxParticipants: p.MaxParticipants, CreatedAt: now, ExpiresAt: now.Add(p.TTL)}
	creator := model.Participant{ID: participantID, RoomID: roomID, Name: p.CreatorName, JoinedAt: now}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreatedRoom{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO rooms(id,public_name,max_participants,status,created_at,expires_at) VALUES(?,?,?,?,?,?)`, room.ID, room.Name, room.MaxParticipants, room.Status, nanos(room.CreatedAt), nanos(room.ExpiresAt)); err != nil {
		return CreatedRoom{}, fmt.Errorf("insert room: %w", err)
	}
	creatorHash := securetoken.Hash(creatorToken)
	if _, err = tx.ExecContext(ctx, `INSERT INTO participants(id,room_id,name,token_hash,joined_at) VALUES(?,?,?,?,?)`, creator.ID, creator.RoomID, creator.Name, creatorHash[:], nanos(creator.JoinedAt)); err != nil {
		return CreatedRoom{}, fmt.Errorf("insert creator: %w", err)
	}
	inviteHash := securetoken.Hash(inviteToken)
	if _, err = tx.ExecContext(ctx, `INSERT INTO invites(id,room_id,token_hash) VALUES(?,?,?)`, inviteID, room.ID, inviteHash[:]); err != nil {
		return CreatedRoom{}, fmt.Errorf("insert invite: %w", err)
	}
	inspectHash := securetoken.Hash(inspectToken)
	if _, err = tx.ExecContext(ctx, `INSERT INTO inspectors(room_id,token_hash) VALUES(?,?)`, room.ID, inspectHash[:]); err != nil {
		return CreatedRoom{}, fmt.Errorf("insert inspector: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return CreatedRoom{}, fmt.Errorf("commit room: %w", err)
	}
	return CreatedRoom{Room: room, Creator: creator, CreatorToken: creatorToken, InviteToken: inviteToken, InspectToken: inspectToken}, nil
}

func (s *sqliteStore) ClaimInvite(ctx context.Context, rawToken, name string) (ClaimResult, error) {
	hash := securetoken.Hash(rawToken)
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ClaimResult{}, err
	}
	defer tx.Rollback()
	var roomID string
	err = tx.QueryRowContext(ctx, `SELECT room_id FROM invites WHERE token_hash=?`, hash[:]).Scan(&roomID)
	if errors.Is(err, sql.ErrNoRows) {
		return ClaimResult{}, ErrInviteInvalid
	}
	if err != nil {
		return ClaimResult{}, err
	}
	room, err := getRoom(ctx, tx, roomID)
	if err != nil {
		return ClaimResult{}, err
	}
	if err := activeAt(room, now); err != nil {
		return ClaimResult{}, err
	}
	if room.Status == "done" {
		return ClaimResult{}, ErrRoomClosed
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM participants WHERE room_id=?`, roomID).Scan(&count); err != nil {
		return ClaimResult{}, err
	}
	if room.MaxParticipants != nil && count >= *room.MaxParticipants {
		return ClaimResult{}, ErrRoomFull
	}
	participantToken, err := securetoken.New(32)
	if err != nil {
		return ClaimResult{}, err
	}
	participantID, err := securetoken.New(16)
	if err != nil {
		return ClaimResult{}, err
	}
	participant := model.Participant{ID: participantID, RoomID: roomID, Name: name, JoinedAt: now}
	tokenHash := securetoken.Hash(participantToken)
	if _, err = tx.ExecContext(ctx, `INSERT INTO participants(id,room_id,name,token_hash,joined_at) VALUES(?,?,?,?,?)`, participant.ID, roomID, name, tokenHash[:], nanos(now)); err != nil {
		return ClaimResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE rooms SET status='active' WHERE id=?`, roomID); err != nil {
		return ClaimResult{}, err
	}
	room.Status = "active"
	if err = tx.Commit(); err != nil {
		return ClaimResult{}, err
	}
	return ClaimResult{Room: room, Participant: participant, ParticipantToken: participantToken}, nil
}

func (s *sqliteStore) Authenticate(ctx context.Context, roomID, rawToken string) (model.Participant, error) {
	room, err := s.GetRoom(ctx, roomID)
	if err != nil {
		return model.Participant{}, err
	}
	hash := securetoken.Hash(rawToken)
	var p model.Participant
	var joined int64
	err = s.db.QueryRowContext(ctx, `SELECT id,room_id,name,joined_at FROM participants WHERE room_id=? AND token_hash=?`, room.ID, hash[:]).Scan(&p.ID, &p.RoomID, &p.Name, &joined)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Participant{}, ErrUnauthorized
	}
	if err != nil {
		return model.Participant{}, err
	}
	p.JoinedAt = fromNanos(joined)
	return p, nil
}

func (s *sqliteStore) Inspect(ctx context.Context, rawToken string) (model.Room, []model.Message, error) {
	hash := securetoken.Hash(rawToken)
	var roomID string
	if err := s.db.QueryRowContext(ctx, `SELECT room_id FROM inspectors WHERE token_hash=?`, hash[:]).Scan(&roomID); errors.Is(err, sql.ErrNoRows) {
		return model.Room{}, nil, ErrInspectInvalid
	} else if err != nil {
		return model.Room{}, nil, err
	}
	room, err := s.GetRoom(ctx, roomID)
	if err != nil {
		return model.Room{}, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.id,m.room_id,m.sender_id,p.name,m.body,m.reply_to,COALESCE(m.recipient_id,''),m.sequence,m.kind,m.created_at
		FROM messages m JOIN participants p ON p.id=m.sender_id
		WHERE m.room_id=? ORDER BY m.sequence LIMIT ?`, room.ID, maxEvents)
	if err != nil {
		return model.Room{}, nil, err
	}
	defer rows.Close()
	messages := make([]model.Message, 0)
	for rows.Next() {
		var message model.Message
		var created int64
		if err := rows.Scan(&message.ID, &message.RoomID, &message.SenderID, &message.SenderName, &message.Body, &message.ReplyTo, &message.To, &message.Sequence, &message.Kind, &created); err != nil {
			return model.Room{}, nil, err
		}
		message.CreatedAt = fromNanos(created)
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return model.Room{}, nil, err
	}
	return room, messages, nil
}

func (s *sqliteStore) GetRoom(ctx context.Context, roomID string) (model.Room, error) {
	room, err := getRoom(ctx, s.db, roomID)
	if err != nil {
		return model.Room{}, err
	}
	if err := activeAt(room, s.now()); err != nil {
		return model.Room{}, err
	}
	return room, nil
}

func (s *sqliteStore) Participants(ctx context.Context, roomID string) ([]model.Participant, error) {
	if _, err := s.GetRoom(ctx, roomID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,room_id,name,joined_at FROM participants WHERE room_id=? ORDER BY joined_at,id`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	participants := []model.Participant{}
	for rows.Next() {
		var participant model.Participant
		var joined int64
		if err := rows.Scan(&participant.ID, &participant.RoomID, &participant.Name, &joined); err != nil {
			return nil, err
		}
		participant.JoinedAt = fromNanos(joined)
		participants = append(participants, participant)
	}
	return participants, rows.Err()
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getRoom(ctx context.Context, q queryer, roomID string) (model.Room, error) {
	var room model.Room
	var created, expires int64
	var maxParticipants sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT id,public_name,status,max_participants,created_at,expires_at FROM rooms WHERE id=?`, roomID).Scan(&room.ID, &room.Name, &room.Status, &maxParticipants, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Room{}, ErrRoomNotFound
	}
	if err != nil {
		return model.Room{}, err
	}
	if maxParticipants.Valid {
		max := int(maxParticipants.Int64)
		room.MaxParticipants = &max
	}
	room.CreatedAt, room.ExpiresAt = fromNanos(created), fromNanos(expires)
	return room, nil
}

func activeAt(room model.Room, now time.Time) error {
	if !now.Before(room.ExpiresAt) {
		return ErrRoomExpired
	}
	return nil
}

func (s *sqliteStore) Append(ctx context.Context, p AppendParams) (model.Message, error) {
	if p.Kind != "message" {
		return model.Message{}, ErrInvalid
	}
	return s.append(ctx, p, false)
}

func (s *sqliteStore) append(ctx context.Context, p AppendParams, closeRoom bool) (model.Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, err
	}
	defer tx.Rollback()
	room, err := getRoom(ctx, tx, p.RoomID)
	if err != nil {
		return model.Message{}, err
	}
	if err = activeAt(room, s.now()); err != nil {
		return model.Message{}, err
	}
	if existing, found, err := findMessage(ctx, tx, p.RoomID, p.MessageID); err != nil {
		return model.Message{}, err
	} else if found {
		if sameMessage(existing, p) {
			return existing, nil
		}
		return model.Message{}, ErrConflict
	}
	if room.Status == "done" {
		return model.Message{}, ErrRoomClosed
	}
	var senderName string
	if err = tx.QueryRowContext(ctx, `SELECT name FROM participants WHERE id=? AND room_id=?`, p.ParticipantID, p.RoomID).Scan(&senderName); errors.Is(err, sql.ErrNoRows) {
		return model.Message{}, ErrUnauthorized
	} else if err != nil {
		return model.Message{}, err
	}
	if p.To != "" {
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT 1 FROM participants WHERE room_id=? AND id=?`, p.RoomID, p.To).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return model.Message{}, ErrInvalid
		} else if err != nil {
			return model.Message{}, err
		}
	}
	var sequence int64
	if err = tx.QueryRowContext(ctx, `SELECT next_sequence FROM rooms WHERE id=?`, p.RoomID).Scan(&sequence); err != nil {
		return model.Message{}, err
	}
	if sequence > maxEvents {
		return model.Message{}, ErrEventLimit
	}
	now := s.now().UTC()
	message := model.Message{ID: p.MessageID, RoomID: p.RoomID, SenderID: p.ParticipantID, SenderName: senderName, Body: p.Body, ReplyTo: p.ReplyTo, To: p.To, Sequence: sequence, Kind: p.Kind, CreatedAt: now}
	if closeRoom {
		message.To = ""
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,room_id,sequence,sender_id,recipient_id,kind,body,reply_to,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, message.ID, message.RoomID, sequence, message.SenderID, nullIfEmpty(message.To), message.Kind, message.Body, message.ReplyTo, nanos(now)); err != nil {
		return model.Message{}, err
	}
	if closeRoom {
		if _, err = tx.ExecContext(ctx, `UPDATE rooms SET next_sequence=next_sequence+1,status='done',ended_at=?,ended_by=? WHERE id=?`, nanos(now), p.ParticipantID, p.RoomID); err != nil {
			return model.Message{}, err
		}
	} else if _, err = tx.ExecContext(ctx, `UPDATE rooms SET next_sequence=next_sequence+1 WHERE id=?`, p.RoomID); err != nil {
		return model.Message{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Message{}, err
	}
	return message, nil
}

func sameMessage(existing model.Message, retry AppendParams) bool {
	return existing.RoomID == retry.RoomID &&
		existing.SenderID == retry.ParticipantID &&
		existing.Kind == retry.Kind &&
		existing.Body == retry.Body &&
		existing.ReplyTo == retry.ReplyTo &&
		existing.To == retry.To
}

// findMessage looks an idempotency key up within its room. Keys are chosen by
// clients, so scoping matters: two rooms picking the same key are two unrelated
// messages, not a retry of one.
func findMessage(ctx context.Context, q queryer, roomID, id string) (model.Message, bool, error) {
	var m model.Message
	var created int64
	err := q.QueryRowContext(ctx, `SELECT m.id,m.room_id,m.sender_id,p.name,m.body,m.reply_to,COALESCE(m.recipient_id,''),m.sequence,m.kind,m.created_at FROM messages m JOIN participants p ON p.id=m.sender_id WHERE m.room_id=? AND m.id=?`, roomID, id).Scan(&m.ID, &m.RoomID, &m.SenderID, &m.SenderName, &m.Body, &m.ReplyTo, &m.To, &m.Sequence, &m.Kind, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Message{}, false, nil
	}
	if err != nil {
		return model.Message{}, false, err
	}
	m.CreatedAt = fromNanos(created)
	return m, true, nil
}

func (s *sqliteStore) MessagesAfter(ctx context.Context, roomID, participantID string, sequence int64, limit int, skipSelf bool) (VisibleMessages, error) {
	if _, err := s.GetRoom(ctx, roomID); err != nil {
		return VisibleMessages{}, err
	}
	var participantExists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM participants WHERE id=? AND room_id=?`, participantID, roomID).Scan(&participantExists); errors.Is(err, sql.ErrNoRows) {
		return VisibleMessages{}, ErrUnauthorized
	} else if err != nil {
		return VisibleMessages{}, err
	}
	result := VisibleMessages{Messages: []model.Message{}, Cursor: sequence}
	if limit <= 0 {
		return result, nil
	}
	const pageSize = 100
	for result.Cursor < maxEvents && len(result.Messages) < limit {
		rows, err := s.db.QueryContext(ctx, `SELECT m.id,m.room_id,m.sender_id,p.name,m.body,m.reply_to,COALESCE(m.recipient_id,''),m.sequence,m.kind,m.created_at,
			(m.kind='done' OR m.recipient_id IS NULL OR m.sender_id=? OR m.recipient_id=?)
			FROM messages m JOIN participants p ON p.id=m.sender_id
			WHERE m.room_id=? AND m.sequence>? ORDER BY m.sequence LIMIT ?`, participantID, participantID, roomID, result.Cursor, pageSize)
		if err != nil {
			return VisibleMessages{}, err
		}
		examined := 0
		for rows.Next() {
			var m model.Message
			var created int64
			var visible bool
			if err := rows.Scan(&m.ID, &m.RoomID, &m.SenderID, &m.SenderName, &m.Body, &m.ReplyTo, &m.To, &m.Sequence, &m.Kind, &created, &visible); err != nil {
				rows.Close()
				return VisibleMessages{}, err
			}
			examined++
			result.Cursor = m.Sequence
			if !visible || (skipSelf && m.Kind == "message" && m.SenderID == participantID) {
				continue
			}
			m.CreatedAt = fromNanos(created)
			result.Messages = append(result.Messages, m)
			if len(result.Messages) == limit {
				break
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return VisibleMessages{}, err
		}
		if err := rows.Close(); err != nil {
			return VisibleMessages{}, err
		}
		if examined < pageSize {
			break
		}
	}
	return result, nil
}

func (s *sqliteStore) CloseRoom(ctx context.Context, roomID, participantID, messageID string) (model.Message, error) {
	return s.append(ctx, AppendParams{RoomID: roomID, ParticipantID: participantID, MessageID: messageID, Kind: "done"}, true)
}

func (s *sqliteStore) DeleteExpired(ctx context.Context, at time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM rooms WHERE expires_at<=?`, nanos(at))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *sqliteStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *sqliteStore) Close() error                   { return s.db.Close() }
func nanos(t time.Time) int64                         { return t.UnixNano() }
func fromNanos(n int64) time.Time                     { return time.Unix(0, n).UTC() }
func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
