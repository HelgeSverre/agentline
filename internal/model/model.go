package model

import "time"

type RoomStatus string

type MessageKind string

type Room struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Status          RoomStatus `json:"status"`
	MaxParticipants *int       `json:"max_participants"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
}

type Participant struct {
	ID       string    `json:"id"`
	RoomID   string    `json:"room_id"`
	Name     string    `json:"name"`
	JoinedAt time.Time `json:"joined_at"`
}

type Message struct {
	ID         string      `json:"id"`
	RoomID     string      `json:"room_id"`
	SenderID   string      `json:"sender_id"`
	SenderName string      `json:"sender_name"`
	Body       string      `json:"body"`
	ReplyTo    string      `json:"reply_to,omitempty"`
	To         string      `json:"to,omitempty"`
	Sequence   int64       `json:"sequence"`
	Kind       MessageKind `json:"kind"`
	CreatedAt  time.Time   `json:"created_at"`
}

type RoomCredential struct {
	RoomID        string `json:"room_id"`
	RoomName      string `json:"room_name"`
	ServerURL     string `json:"server_url"`
	ParticipantID string `json:"participant_id"`
	Token         string `json:"token"`
	Cursor        int64  `json:"cursor"`
}
