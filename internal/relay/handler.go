package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HelgeSverre/agentline/internal/model"
	"github.com/HelgeSverre/agentline/internal/store"
	"github.com/HelgeSverre/agentline/web"
)

type Config struct {
	PublicURL                    string
	MaxTTL, WaitMax              time.Duration
	MessageBytes                 int64
	CreatePerHour, SendPerMinute int
	TrustProxy                   bool
}

const (
	defaultRoomTTL = 24 * time.Hour
	maxRoomTTL     = 7 * 24 * time.Hour
	maxWait        = 60 * time.Second
	maxMessageSize = int64(64 << 10)
)

type waitGroup struct {
	ch   chan struct{}
	refs int
}

type handler struct {
	store       store.Store
	config      Config
	now         func() time.Time
	createLimit *limiter
	sendLimit   *limiter
	waitsMu     sync.Mutex
	waits       map[string]*waitGroup
}

func NewHandler(data store.Store, config Config, now func() time.Time) http.Handler {
	if now == nil {
		now = time.Now
	}
	if config.MaxTTL <= 0 {
		config.MaxTTL = maxRoomTTL
	} else if config.MaxTTL > maxRoomTTL {
		config.MaxTTL = maxRoomTTL
	}
	if config.WaitMax <= 0 {
		config.WaitMax = maxWait
	} else if config.WaitMax > maxWait {
		config.WaitMax = maxWait
	}
	if config.MessageBytes <= 0 {
		config.MessageBytes = maxMessageSize
	} else if config.MessageBytes > maxMessageSize {
		config.MessageBytes = maxMessageSize
	}
	if config.CreatePerHour <= 0 {
		config.CreatePerHour = 20
	}
	if config.SendPerMinute <= 0 {
		config.SendPerMinute = 120
	}
	h := &handler{store: data, config: config, now: now, createLimit: newLimiter(time.Hour, config.CreatePerHour, now), sendLimit: newLimiter(time.Minute, config.SendPerMinute, now), waits: make(map[string]*waitGroup)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.index)
	mux.HandleFunc("GET /llms.txt", h.llms)
	mux.HandleFunc("GET /install.sh", h.install)
	mux.HandleFunc("GET /join/{token}", h.join)
	mux.HandleFunc("GET /inspect/{token}", h.inspectPage)
	mux.HandleFunc("GET /inspect/{token}/events", h.inspectEvents)
	mux.HandleFunc("GET /assets/agentline-inspect.css", h.inspectCSS)
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("POST /api/rooms", h.createRoom)
	mux.HandleFunc("POST /api/invites/{token}/claim", h.claimInvite)
	mux.HandleFunc("GET /api/rooms/{id}", h.getRoom)
	mux.HandleFunc("POST /api/rooms/{id}/messages", h.sendMessage)
	mux.HandleFunc("GET /api/rooms/{id}/messages", h.messages)
	mux.HandleFunc("GET /api/rooms/{id}/wait", h.wait)
	mux.HandleFunc("POST /api/rooms/{id}/done", h.done)
	return h.logRequests(mux)
}

func (h *handler) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		pattern := r.Pattern
		if pattern == "" {
			pattern = "unmatched"
		}
		log.Printf("relay request method=%s route=%q", r.Method, pattern)
	})
}

func (h *handler) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(web.IndexHTML)
}

func (h *handler) llms(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(web.LLMSTXT)
}

func (h *handler) install(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(web.InstallSH)
}

func (h *handler) join(w http.ResponseWriter, r *http.Request) {
	base := strings.TrimRight(h.config.PublicURL, "/")
	inviteURL := base + "/join/" + r.PathValue("token")
	joinCommand := "agentline join " + shellQuote(inviteURL)
	installCommand := "curl -fsSL " + base + "/install.sh | sh"

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Vary", "Accept")

	if r.URL.Query().Get("format") != "markdown" && strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Join Agentline</title><style>body{max-width:44rem;margin:4rem auto;padding:0 1.25rem;background:#0a0a0a;color:#e4e4e4;font:1rem/1.6 system-ui,sans-serif}h1{font-size:2.5rem}p{color:#aaa}pre{overflow:auto;padding:1rem;border:1px solid #333;background:#111;color:#eee}code{font-family:ui-monospace,monospace}a{color:#d47b66}</style></head><body><main><h1>Join this Agentline room</h1><p>Install Agentline if needed:</p><pre><code>%s</code></pre><p>Then join:</p><pre><code>%s</code></pre></main></body></html>`, html.EscapeString(installCommand), html.EscapeString(joinCommand))
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	fmt.Fprintf(w, "# Join this Agentline room\n\nInstall Agentline if needed:\n\n```sh\n%s\n```\n\nThen join:\n\n```sh\n%s\n```\n", installCommand, joinCommand)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Ping(r.Context()); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) createRoom(w http.ResponseWriter, r *http.Request) {
	if !h.createLimit.allow(clientIP(r, h.config.TrustProxy)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests.")
		return
	}
	var in struct {
		Name            string  `json:"name"`
		CreatorName     string  `json:"creator_name"`
		TTLSeconds      float64 `json:"ttl_seconds"`
		MaxParticipants *int    `json:"max_participants"`
	}
	if !decodeJSON(w, r, 4096, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.CreatorName) == "" || in.TTLSeconds < 0 || in.MaxParticipants != nil && *in.MaxParticipants <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request is invalid.")
		return
	}
	if in.TTLSeconds > h.config.MaxTTL.Seconds() {
		writeError(w, http.StatusBadRequest, "invalid_ttl", "The room lifetime exceeds the server limit.")
		return
	}
	ttl := defaultRoomTTL
	if in.TTLSeconds > 0 {
		ttl = time.Duration(in.TTLSeconds * float64(time.Second))
	} else {
		ttl = defaultRoomTTL
		if ttl > h.config.MaxTTL {
			ttl = h.config.MaxTTL
		}
	}
	created, err := h.store.CreateRoom(r.Context(), store.CreateRoomParams{Name: in.Name, CreatorName: in.CreatorName, TTL: ttl, MaxParticipants: in.MaxParticipants})
	if err != nil {
		h.fail(w, err)
		return
	}
	base := strings.TrimRight(h.config.PublicURL, "/")
	writeJSON(w, http.StatusCreated, map[string]any{"room": created.Room, "participant": created.Creator, "participant_token": created.CreatorToken, "invite_token": created.InviteToken, "invite_url": base + "/join/" + created.InviteToken, "inspect_url": base + "/inspect/" + created.InspectToken})
}

func (h *handler) claimInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, 4096, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request is invalid.")
		return
	}
	claimed, err := h.store.ClaimInvite(r.Context(), r.PathValue("token"), in.Name)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.notify(claimed.Room.ID)
	writeJSON(w, http.StatusOK, map[string]any{"room": claimed.Room, "participant": claimed.Participant, "participant_token": claimed.ParticipantToken})
}

func (h *handler) getRoom(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authorize(w, r); !ok {
		return
	}
	room, err := h.store.GetRoom(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	participants, err := h.store.Participants(r.Context(), room.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	room.Participants = participants
	writeJSON(w, http.StatusOK, room)
}

func (h *handler) sendMessage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authorize(w, r)
	if !ok {
		return
	}
	if !h.sendLimit.allow(clientIP(r, h.config.TrustProxy)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests.")
		return
	}
	var in struct {
		ID      string `json:"id"`
		Body    string `json:"body"`
		ReplyTo string `json:"reply_to"`
		To      string `json:"to"`
	}
	if !decodeJSON(w, r, h.config.MessageBytes+4096, &in) {
		return
	}
	if in.ID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request is invalid.")
		return
	}
	if int64(len(in.Body)) > h.config.MessageBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "The message body is too large.")
		return
	}
	m, err := h.store.Append(r.Context(), store.AppendParams{RoomID: r.PathValue("id"), ParticipantID: p.ID, MessageID: in.ID, Body: in.Body, ReplyTo: in.ReplyTo, To: in.To, Kind: "message"})
	if err != nil {
		if in.To != "" && errors.Is(err, store.ErrInvalid) {
			writeError(w, http.StatusBadRequest, "invalid_recipient", "The recipient is not a participant in this room.")
			return
		}
		h.fail(w, err)
		return
	}
	h.notify(m.RoomID)
	writeJSON(w, http.StatusOK, m)
}

func (h *handler) messages(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authorize(w, r)
	if !ok {
		return
	}
	after, ok := queryInt(w, r, "after", 0)
	if !ok {
		return
	}
	visible, err := h.store.MessagesAfter(r.Context(), r.PathValue("id"), p.ID, after, 1000, false)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": visible.Messages, "cursor": visible.Cursor})
}

func (h *handler) done(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authorize(w, r)
	if !ok {
		return
	}
	var in struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, 4096, &in) {
		return
	}
	if in.ID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request is invalid.")
		return
	}
	m, err := h.store.CloseRoom(r.Context(), r.PathValue("id"), p.ID, in.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.notify(m.RoomID)
	writeJSON(w, http.StatusOK, m)
}

func (h *handler) wait(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authorize(w, r)
	if !ok {
		return
	}
	scanAfter, ok := queryInt(w, r, "after", 0)
	if !ok {
		return
	}
	seconds, err := strconv.ParseFloat(defaultString(r.URL.Query().Get("timeout"), strconv.FormatFloat(h.config.WaitMax.Seconds(), 'f', -1, 64)), 64)
	if err != nil || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request is invalid.")
		return
	}
	if seconds > h.config.WaitMax.Seconds() {
		seconds = h.config.WaitMax.Seconds()
	}
	duration := time.Duration(seconds * float64(time.Second))
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	for {
		visible, err := h.store.MessagesAfter(r.Context(), r.PathValue("id"), p.ID, scanAfter, 1, true)
		if err != nil {
			h.fail(w, err)
			return
		}
		scanAfter = visible.Cursor
		if len(visible.Messages) > 0 {
			m := visible.Messages[0]
			if m.Kind == "done" {
				writeJSON(w, http.StatusOK, map[string]any{"status": "done", "ended_by": m.SenderName, "sequence": m.Sequence})
			} else {
				writeJSON(w, http.StatusOK, map[string]any{"status": "message", "message": m})
			}
			return
		}
		room, err := h.store.GetRoom(r.Context(), r.PathValue("id"))
		if err != nil {
			h.fail(w, err)
			return
		}
		if room.Status == "done" {
			writeJSON(w, http.StatusOK, map[string]any{"status": "done"})
			return
		}
		ch, unsubscribe := h.subscribe(room.ID)
		// Recheck after subscribing so an append between the query and subscription cannot be missed.
		visible, err = h.store.MessagesAfter(r.Context(), room.ID, p.ID, scanAfter, 1, true)
		if err != nil {
			unsubscribe()
			h.fail(w, err)
			return
		}
		scanAfter = visible.Cursor
		if len(visible.Messages) > 0 {
			m := visible.Messages[0]
			unsubscribe()
			if m.Kind == "done" {
				writeJSON(w, http.StatusOK, map[string]any{"status": "done", "ended_by": m.SenderName, "sequence": m.Sequence})
			} else {
				writeJSON(w, http.StatusOK, map[string]any{"status": "message", "message": m})
			}
			return
		}
		select {
		case <-ch:
			unsubscribe()
		case <-deadline.C:
			unsubscribe()
			writeJSON(w, http.StatusOK, map[string]any{"status": "timeout", "room": room.Name, "sequence": scanAfter, "instruction": "No message arrived. Call wait_for_message again if a response is still expected."})
			return
		case <-r.Context().Done():
			unsubscribe()
			return
		}
	}
}

func (h *handler) authorize(w http.ResponseWriter, r *http.Request) (model.Participant, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")) == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "A valid participant credential is required.")
		return model.Participant{}, false
	}
	p, err := h.store.Authenticate(r.Context(), r.PathValue("id"), strings.TrimPrefix(header, "Bearer "))
	if err != nil {
		h.fail(w, err)
		return model.Participant{}, false
	}
	return p, true
}

func (h *handler) subscribe(room string) (<-chan struct{}, func()) {
	h.waitsMu.Lock()
	group := h.waits[room]
	if group == nil {
		group = &waitGroup{ch: make(chan struct{})}
		h.waits[room] = group
	}
	group.refs++
	h.waitsMu.Unlock()
	var once sync.Once
	return group.ch, func() {
		once.Do(func() {
			h.waitsMu.Lock()
			defer h.waitsMu.Unlock()
			group.refs--
			if h.waits[room] == group && group.refs == 0 {
				delete(h.waits, room)
			}
		})
	}
}
func (h *handler) notify(room string) {
	h.waitsMu.Lock()
	defer h.waitsMu.Unlock()
	if group := h.waits[room]; group != nil {
		close(group.ch)
		delete(h.waits, room)
	}
}

func (h *handler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized", "A valid participant credential is required.")
	case errors.Is(err, store.ErrInspectInvalid):
		writeError(w, http.StatusNotFound, "inspect_not_found", "The inspection link was not found.")
	case errors.Is(err, store.ErrInviteInvalid):
		writeError(w, http.StatusNotFound, "invite_invalid", "This invite is invalid.")
	case errors.Is(err, store.ErrRoomNotFound):
		writeError(w, http.StatusNotFound, "room_not_found", "The room was not found.")
	case errors.Is(err, store.ErrRoomExpired):
		writeError(w, http.StatusGone, "room_expired", "The room has expired.")
	case errors.Is(err, store.ErrRoomClosed):
		writeError(w, http.StatusConflict, "room_closed", "The room is closed.")
	case errors.Is(err, store.ErrRoomFull):
		writeError(w, http.StatusConflict, "room_full", "The room is full.")
	case errors.Is(err, store.ErrEventLimit):
		writeError(w, http.StatusConflict, "event_limit", "The room event limit has been reached.")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "The request conflicts with an existing event.")
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", "The request is invalid.")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "The relay could not complete the request.")
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "The request body is too large.")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", "The JSON body is invalid.")
		}
		return false
	}
	if decoder.Decode(&struct{}{}) != nil {
		return true
	}
	writeError(w, http.StatusBadRequest, "invalid_json", "The JSON body is invalid.")
	return false
}

func queryInt(w http.ResponseWriter, r *http.Request, name string, fallback int64) (int64, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request is invalid.")
		return 0, false
	}
	return n, true
}
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); forwarded != "" {
			return forwarded
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
