package store

import (
	"context"
	"errors"
	"time"

	"github.com/HelgeSverre/agentline/internal/model"
)

var (
	ErrRoomNotFound   = errors.New("room not found")
	ErrRoomExpired    = errors.New("room expired")
	ErrRoomClosed     = errors.New("room closed")
	ErrInviteInvalid  = errors.New("invite invalid")
	ErrInviteClaimed  = errors.New("invite already claimed")
	ErrRoomFull       = errors.New("room full")
	ErrUnauthorized   = errors.New("unauthorized")
	ErrInspectInvalid = errors.New("inspect capability invalid")
	ErrEventLimit     = errors.New("room event limit reached")
	ErrConflict       = errors.New("conflict")
	ErrInvalid        = errors.New("invalid")
)

type CreateRoomParams struct {
	Name, CreatorName string
	TTL               time.Duration
	MaxParticipants   *int
}

type CreatedRoom struct {
	Room                      model.Room
	Creator                   model.Participant
	CreatorToken, InviteToken string
	InspectToken              string
}

type ClaimResult struct {
	Room             model.Room
	Participant      model.Participant
	ParticipantToken string
}

type AppendParams struct {
	RoomID, ParticipantID, MessageID, Body, ReplyTo, To string
	Kind                                                model.MessageKind
}

type VisibleMessages struct {
	Messages []model.Message
	Cursor   int64
}

type Store interface {
	CreateRoom(context.Context, CreateRoomParams) (CreatedRoom, error)
	ClaimInvite(context.Context, string, string) (ClaimResult, error)
	Authenticate(context.Context, string, string) (model.Participant, error)
	Inspect(context.Context, string) (model.Room, []model.Message, error)
	GetRoom(context.Context, string) (model.Room, error)
	Participants(context.Context, string) ([]model.Participant, error)
	Append(context.Context, AppendParams) (model.Message, error)
	MessagesAfter(context.Context, string, string, int64, int, bool) (VisibleMessages, error)
	CloseRoom(context.Context, string, string, string) (model.Message, error)
	DeleteExpired(context.Context, time.Time) (int64, error)
	Ping(context.Context) error
	Close() error
}
