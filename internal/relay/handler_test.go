package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HelgeSverre/agentline/internal/store"
)

type fixture struct {
	t      *testing.T
	store  store.Store
	server *httptest.Server
}

func newFixture(t *testing.T, mutate ...func(*Config)) *fixture {
	t.Helper()
	now := time.Now
	s, err := store.OpenSQLite(":memory:", now)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{PublicURL: "https://relay.example", MaxTTL: 7 * 24 * time.Hour, WaitMax: time.Second, MessageBytes: 64 << 10, CreatePerHour: 20, SendPerMinute: 120}
	for _, fn := range mutate {
		fn(&cfg)
	}
	ts := httptest.NewServer(NewHandler(s, cfg, now))
	t.Cleanup(func() { ts.Close(); s.Close() })
	return &fixture{t: t, store: s, server: ts}
}

func TestRootRouteIsExact(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/missing", "/v1", "/join", "/healthz/extra"} {
		resp, _ := f.request(http.MethodGet, path, "", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestTTLDefaultAndHardMaximum(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.MaxTTL = 30 * 24 * time.Hour })
	resp, got := f.request(http.MethodPost, "/v1/rooms", "", map[string]any{"name": "default", "creator_name": "alice"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("default TTL: %d %#v", resp.StatusCode, got)
	}
	room := got["room"].(map[string]any)
	created, _ := time.Parse(time.RFC3339Nano, room["created_at"].(string))
	expires, _ := time.Parse(time.RFC3339Nano, room["expires_at"].(string))
	if expires.Sub(created) != 24*time.Hour {
		t.Fatalf("default TTL = %s", expires.Sub(created))
	}
	resp, _ = f.request(http.MethodPost, "/v1/rooms", "", map[string]any{"name": "maximum", "creator_name": "alice", "ttl_seconds": (7 * 24 * time.Hour).Seconds()})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("maximum TTL = %d", resp.StatusCode)
	}
	resp, got = f.request(http.MethodPost, "/v1/rooms", "", map[string]any{"name": "over", "creator_name": "alice", "ttl_seconds": (7*24*time.Hour + time.Second).Seconds()})
	assertError(t, resp, got, http.StatusBadRequest, "invalid_ttl")
}

func TestHardMessageCeilingAndExactBoundary(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.MessageBytes = 1 << 20 })
	roomID, token, _ := f.room()
	resp, _ := f.request(http.MethodPost, "/v1/rooms/"+roomID+"/messages", token, map[string]string{"id": "exact", "body": strings.Repeat("x", 64<<10)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exact body boundary = %d", resp.StatusCode)
	}
	resp, got := f.request(http.MethodPost, "/v1/rooms/"+roomID+"/messages", token, map[string]string{"id": "over", "body": strings.Repeat("x", (64<<10)+1)})
	assertError(t, resp, got, http.StatusRequestEntityTooLarge, "body_too_large")
}

func TestUnauthorizedSendDoesNotConsumeQuota(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.SendPerMinute = 1 })
	roomID, token, _ := f.room()
	resp, got := f.request(http.MethodPost, "/v1/rooms/"+roomID+"/messages", "wrong", map[string]string{"id": "bad", "body": "x"})
	assertError(t, resp, got, http.StatusUnauthorized, "unauthorized")
	resp, _ = f.request(http.MethodPost, "/v1/rooms/"+roomID+"/messages", token, map[string]string{"id": "good", "body": "x"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated send after rejected bearer = %d", resp.StatusCode)
	}
}

func TestLogsDoNotContainSecrets(t *testing.T) {
	f := newFixture(t)
	roomID, token, invite := f.room()
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })
	f.request(http.MethodGet, "/join/"+invite, "", nil)
	f.request(http.MethodGet, "/v1/rooms/"+roomID, token, nil)
	for _, secret := range []string{invite, token, roomID} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs leaked secret %q: %s", secret, logs.String())
		}
	}
}

func (f *fixture) request(method, path, token string, body any) (*http.Response, map[string]any) {
	f.t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		f.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	var decoded map[string]any
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			f.t.Fatal(err)
		}
		resp.Body.Close()
	}
	return resp, decoded
}

func (f *fixture) room() (roomID, token, invite string) {
	f.t.Helper()
	resp, got := f.request(http.MethodPost, "/v1/rooms", "", map[string]any{"name": "amber-fox", "creator_name": "alice", "ttl_seconds": 600})
	if resp.StatusCode != http.StatusCreated {
		f.t.Fatalf("create: %d %#v", resp.StatusCode, got)
	}
	return got["room"].(map[string]any)["id"].(string), got["participant_token"].(string), got["invite_token"].(string)
}

func TestRoomLifecycleAndEndpoints(t *testing.T) {
	f := newFixture(t)
	roomID, alice, invite := f.room()

	resp, page := f.request(http.MethodGet, "/", "", nil)
	if resp.StatusCode != http.StatusOK || page != nil {
		t.Fatalf("website: %d", resp.StatusCode)
	}
	resp, _ = f.request(http.MethodGet, "/join/"+invite, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("join page: %d", resp.StatusCode)
	}
	resp, claimed := f.request(http.MethodPost, "/v1/invites/"+invite+"/claim", "", map[string]string{"name": "bob"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: %d %#v", resp.StatusCode, claimed)
	}
	bob := claimed["participant_token"].(string)
	resp, failure := f.request(http.MethodPost, "/v1/invites/"+invite+"/claim", "", map[string]string{"name": "eve"})
	assertError(t, resp, failure, http.StatusConflict, "invite_already_claimed")

	resp, got := f.request(http.MethodGet, "/v1/rooms/"+roomID, alice, nil)
	if resp.StatusCode != http.StatusOK || got["status"] != "active" {
		t.Fatalf("room: %d %#v", resp.StatusCode, got)
	}
	message := map[string]string{"id": "message-1", "body": "hello", "reply_to": "prior"}
	resp, sent := f.request(http.MethodPost, "/v1/rooms/"+roomID+"/messages", alice, message)
	if resp.StatusCode != http.StatusOK || sent["sequence"] != float64(1) {
		t.Fatalf("send: %d %#v", resp.StatusCode, sent)
	}
	resp, retry := f.request(http.MethodPost, "/v1/rooms/"+roomID+"/messages", alice, message)
	if resp.StatusCode != http.StatusOK || retry["id"] != "message-1" {
		t.Fatalf("retry: %d %#v", resp.StatusCode, retry)
	}
	resp, listed := f.request(http.MethodGet, "/v1/rooms/"+roomID+"/messages?after=0", bob, nil)
	if resp.StatusCode != http.StatusOK || len(listed["messages"].([]any)) != 1 {
		t.Fatalf("list: %d %#v", resp.StatusCode, listed)
	}
	resp, listed = f.request(http.MethodGet, "/v1/rooms/"+roomID+"/messages?after=1", bob, nil)
	if resp.StatusCode != http.StatusOK || len(listed["messages"].([]any)) != 0 {
		t.Fatalf("cursor boundary: %d %#v", resp.StatusCode, listed)
	}
	resp, done := f.request(http.MethodPost, "/v1/rooms/"+roomID+"/done", bob, map[string]string{"id": "done-1"})
	if resp.StatusCode != http.StatusOK || done["kind"] != "done" {
		t.Fatalf("done: %d %#v", resp.StatusCode, done)
	}
	resp, retry = f.request(http.MethodPost, "/v1/rooms/"+roomID+"/done", bob, map[string]string{"id": "done-1"})
	if resp.StatusCode != http.StatusOK || retry["id"] != "done-1" {
		t.Fatalf("done retry: %d %#v", resp.StatusCode, retry)
	}
	resp, failure = f.request(http.MethodPost, "/v1/rooms/"+roomID+"/messages", alice, map[string]string{"id": "late", "body": "late"})
	assertError(t, resp, failure, http.StatusConflict, "room_closed")

	resp, health := f.request(http.MethodGet, "/healthz", "", nil)
	if resp.StatusCode != http.StatusOK || health["status"] != "ok" {
		t.Fatalf("health: %d %#v", resp.StatusCode, health)
	}
}

func TestBearerCannotCrossRooms(t *testing.T) {
	f := newFixture(t)
	_, firstToken, _ := f.room()
	secondRoom, _, _ := f.room()
	resp, got := f.request(http.MethodGet, "/v1/rooms/"+secondRoom, firstToken, nil)
	assertError(t, resp, got, http.StatusUnauthorized, "unauthorized")
}

func TestAuthenticationValidationBodyAndRateLimits(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.MessageBytes = 8; c.CreatePerHour = 1 })
	roomID, token, _ := f.room()
	resp, got := f.request(http.MethodGet, "/v1/rooms/"+roomID, "wrong", nil)
	assertError(t, resp, got, http.StatusUnauthorized, "unauthorized")
	resp, got = f.request(http.MethodPost, "/v1/rooms/"+roomID+"/messages", token, map[string]string{"id": "large", "body": "too large"})
	assertError(t, resp, got, http.StatusRequestEntityTooLarge, "body_too_large")
	resp, got = f.request(http.MethodPost, "/v1/rooms", "", map[string]string{"name": "second", "creator_name": "x"})
	assertError(t, resp, got, http.StatusTooManyRequests, "rate_limited")
	resp, got = f.request(http.MethodPost, "/v2/rooms", "", map[string]string{})
	assertError(t, resp, got, http.StatusNotFound, "unsupported_api_version")
}

func TestWaitWakesTimesOutAndCancels(t *testing.T) {
	f := newFixture(t)
	roomID, token, _ := f.room()
	type result struct {
		resp *http.Response
		body map[string]any
	}
	wake := make(chan result, 1)
	go func() {
		r, b := f.request(http.MethodGet, "/v1/rooms/"+roomID+"/wait?after=0&timeout=1", token, nil)
		wake <- result{r, b}
	}()
	time.Sleep(30 * time.Millisecond)
	f.request(http.MethodPost, "/v1/rooms/"+roomID+"/messages", token, map[string]string{"id": "wake", "body": "hi"})
	got := <-wake
	if got.resp.StatusCode != http.StatusOK || got.body["status"] != "message" {
		t.Fatalf("wake: %d %#v", got.resp.StatusCode, got.body)
	}

	resp, timeout := f.request(http.MethodGet, "/v1/rooms/"+roomID+"/wait?after=1&timeout=0.01", token, nil)
	if resp.StatusCode != http.StatusOK || timeout["status"] != "timeout" {
		t.Fatalf("timeout: %d %#v", resp.StatusCode, timeout)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/v1/rooms/"+roomID+"/wait?after=1&timeout=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	done := make(chan error, 1)
	go func() { _, err := http.DefaultClient.Do(req); done <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled request unexpectedly succeeded")
	}
}

func TestWaitRejectsNonFiniteTimeout(t *testing.T) {
	f := newFixture(t)
	roomID, token, _ := f.room()
	for _, timeout := range []string{"NaN", "+Inf"} {
		resp, got := f.request(http.MethodGet, "/v1/rooms/"+roomID+"/wait?timeout="+timeout, token, nil)
		assertError(t, resp, got, http.StatusBadRequest, "invalid_request")
	}
}

func TestServeGracefulShutdownClosesStore(t *testing.T) {
	s, err := store.OpenSQLite(":memory:", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, NewHandler(s, Config{}, time.Now), s) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(context.Background()); err == nil {
		t.Fatal("store remained open")
	}
}

func assertError(t *testing.T, resp *http.Response, body map[string]any, status int, code string) {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status %d, want %d: %#v", resp.StatusCode, status, body)
	}
	errBody, ok := body["error"].(map[string]any)
	if !ok || errBody["code"] != code {
		t.Fatalf("error %#v, want %q", body, code)
	}
}

func TestClientIPDefaultsToRemoteAddrAndProxyTrustIsExplicit(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.4:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := clientIP(r, false); got != "192.0.2.4" {
		t.Fatalf("default IP = %q", got)
	}
	if got := clientIP(r, true); got != "203.0.113.9" {
		t.Fatalf("trusted proxy IP = %q", got)
	}
}

func TestStrictJSONAndMethods(t *testing.T) {
	f := newFixture(t)
	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/v1/rooms", strings.NewReader(`{"name":"x","creator_name":"y","extra":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("strict JSON status = %d", resp.StatusCode)
	}
	resp, got := f.request(http.MethodDelete, "/healthz", "", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d %#v", resp.StatusCode, got)
	}
}

func TestTrailingJSONIsRejected(t *testing.T) {
	f := newFixture(t)
	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/v1/rooms", strings.NewReader(`{"name":"x","creator_name":"y"} {}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("trailing JSON status = %d", resp.StatusCode)
	}
}

func TestWaitRegistryUnsubscribeDoesNotDeleteNewGeneration(t *testing.T) {
	h := &handler{waits: make(map[string]*waitGroup)}
	old, unsubscribeOld := h.subscribe("room")
	h.notify("room")
	select {
	case <-old:
	default:
		t.Fatal("old generation was not notified")
	}
	_, unsubscribeNew := h.subscribe("room")
	unsubscribeOld()
	h.waitsMu.Lock()
	_, exists := h.waits["room"]
	h.waitsMu.Unlock()
	if !exists {
		t.Fatal("old unsubscribe deleted newer generation")
	}
	unsubscribeNew()
	h.waitsMu.Lock()
	_, exists = h.waits["room"]
	h.waitsMu.Unlock()
	if exists {
		t.Fatal("last waiter leaked")
	}
}

func TestStableStoreErrorMappings(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{store.ErrUnauthorized, http.StatusUnauthorized, "unauthorized"},
		{store.ErrInviteClaimed, http.StatusConflict, "invite_already_claimed"},
		{store.ErrInviteInvalid, http.StatusNotFound, "invite_invalid"},
		{store.ErrRoomNotFound, http.StatusNotFound, "room_not_found"},
		{store.ErrRoomExpired, http.StatusGone, "room_expired"},
		{store.ErrRoomClosed, http.StatusConflict, "room_closed"},
		{store.ErrRoomFull, http.StatusConflict, "room_full"},
		{store.ErrEventLimit, http.StatusConflict, "event_limit"},
		{store.ErrConflict, http.StatusConflict, "conflict"},
		{store.ErrInvalid, http.StatusBadRequest, "invalid_request"},
		{errors.New("database unavailable"), http.StatusInternalServerError, "internal_error"},
	}
	h := &handler{}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			h.fail(recorder, tt.err)
			var body map[string]any
			if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			assertError(t, recorder.Result(), body, tt.status, tt.code)
		})
	}
}

func TestServeCancellationReleasesActiveWaitBeforeClosingStore(t *testing.T) {
	s, err := store.OpenSQLite(":memory:", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, NewHandler(s, Config{WaitMax: time.Hour}, time.Now), s) }()
	base := "http://" + ln.Addr().String()
	createBody := strings.NewReader(`{"name":"room","creator_name":"alice"}`)
	resp, err := http.Post(base+"/v1/rooms", "application/json", createBody)
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	roomID := created["room"].(map[string]any)["id"].(string)
	token := created["participant_token"].(string)
	waitStarted := make(chan struct{})
	waitDone := make(chan error, 1)
	go func() {
		close(waitStarted)
		req, _ := http.NewRequest(http.MethodGet, base+"/v1/rooms/"+roomID+"/wait?timeout=3600", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		_, err := http.DefaultClient.Do(req)
		waitDone <- err
	}()
	<-waitStarted
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not promptly release active wait")
	}
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("active wait remained blocked")
	}
}
