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
	s, err := store.OpenSQLite(t.TempDir()+"/relay.db", now)
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

func TestV2CreateSupportsUnlimitedAndOptionalCapacity(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		want any
	}{
		{name: "unlimited", body: map[string]any{"name": "room", "creator_name": "alice"}, want: nil},
		{name: "limited", body: map[string]any{"name": "room", "creator_name": "alice", "max_participants": 3}, want: float64(3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			resp, got := f.request(http.MethodPost, "/v2/rooms", "", tc.body)
			if resp.StatusCode != http.StatusCreated || got["room"].(map[string]any)["max_participants"] != tc.want {
				t.Fatalf("create = %d %#v", resp.StatusCode, got)
			}
		})
	}
	f := newFixture(t)
	resp, got := f.request(http.MethodPost, "/v2/rooms", "", map[string]any{"name": "room", "creator_name": "alice", "max_participants": 0})
	assertError(t, resp, got, http.StatusBadRequest, "invalid_request")
}

func TestReusableInviteAndRoomMembershipStatus(t *testing.T) {
	f := newFixture(t)
	roomID, alice, invite := f.room()
	for _, name := range []string{"bob", "carol"} {
		resp, got := f.request(http.MethodPost, "/v2/invites/"+invite+"/claim", "", map[string]string{"name": name})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("join %s = %d %#v", name, resp.StatusCode, got)
		}
	}
	resp, got := f.request(http.MethodGet, "/v2/rooms/"+roomID, alice, nil)
	members, ok := got["participants"].([]any)
	if resp.StatusCode != http.StatusOK || !ok || len(members) != 3 {
		t.Fatalf("status = %d %#v", resp.StatusCode, got)
	}
	for _, raw := range members {
		member := raw.(map[string]any)
		if member["id"] == "" || member["name"] == "" || member["token"] != nil || member["participant_token"] != nil {
			t.Fatalf("unsafe member = %#v", member)
		}
	}
}

func TestDirectedMessageValidationAndVisibility(t *testing.T) {
	f := newFixture(t)
	roomID, aliceToken, invite := f.room()
	_, bob := f.join(invite, "bob")
	_, carol := f.join(invite, "carol")
	bobParticipant, _ := f.store.Authenticate(context.Background(), roomID, bob)
	resp, got := f.request(http.MethodPost, "/v2/rooms/"+roomID+"/messages", aliceToken, map[string]string{"id": "private", "body": "secret", "to": bobParticipant.ID})
	if resp.StatusCode != http.StatusOK || got["to"] != bobParticipant.ID {
		t.Fatalf("send = %d %#v", resp.StatusCode, got)
	}
	for token, count := range map[string]int{aliceToken: 1, bob: 1, carol: 0} {
		resp, history := f.request(http.MethodGet, "/v2/rooms/"+roomID+"/messages", token, nil)
		if resp.StatusCode != http.StatusOK || len(history["messages"].([]any)) != count || history["cursor"] != float64(1) {
			t.Fatalf("history = %d %#v", resp.StatusCode, history)
		}
	}
	resp, got = f.request(http.MethodPost, "/v2/rooms/"+roomID+"/messages", aliceToken, map[string]string{"id": "bad", "body": "secret", "to": "not-a-member"})
	assertError(t, resp, got, http.StatusBadRequest, "invalid_recipient")
}

func TestV1IsUnsupportedAfterCleanCutoff(t *testing.T) {
	f := newFixture(t)
	resp, got := f.request(http.MethodPost, "/v1/rooms", "", map[string]string{})
	assertError(t, resp, got, http.StatusNotFound, "unsupported_api_version")
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
	resp, got := f.request(http.MethodPost, "/v2/rooms", "", map[string]any{"name": "default", "creator_name": "alice"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("default TTL: %d %#v", resp.StatusCode, got)
	}
	room := got["room"].(map[string]any)
	created, _ := time.Parse(time.RFC3339Nano, room["created_at"].(string))
	expires, _ := time.Parse(time.RFC3339Nano, room["expires_at"].(string))
	if expires.Sub(created) != 24*time.Hour {
		t.Fatalf("default TTL = %s", expires.Sub(created))
	}
	resp, _ = f.request(http.MethodPost, "/v2/rooms", "", map[string]any{"name": "maximum", "creator_name": "alice", "ttl_seconds": (7 * 24 * time.Hour).Seconds()})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("maximum TTL = %d", resp.StatusCode)
	}
	resp, got = f.request(http.MethodPost, "/v2/rooms", "", map[string]any{"name": "over", "creator_name": "alice", "ttl_seconds": (7*24*time.Hour + time.Second).Seconds()})
	assertError(t, resp, got, http.StatusBadRequest, "invalid_ttl")
}

func TestHardMessageCeilingAndExactBoundary(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.MessageBytes = 1 << 20 })
	roomID, token, _ := f.room()
	resp, _ := f.request(http.MethodPost, "/v2/rooms/"+roomID+"/messages", token, map[string]string{"id": "exact", "body": strings.Repeat("x", 64<<10)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exact body boundary = %d", resp.StatusCode)
	}
	resp, got := f.request(http.MethodPost, "/v2/rooms/"+roomID+"/messages", token, map[string]string{"id": "over", "body": strings.Repeat("x", (64<<10)+1)})
	assertError(t, resp, got, http.StatusRequestEntityTooLarge, "body_too_large")
}

func TestUnauthorizedSendDoesNotConsumeQuota(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.SendPerMinute = 1 })
	roomID, token, _ := f.room()
	resp, got := f.request(http.MethodPost, "/v2/rooms/"+roomID+"/messages", "wrong", map[string]string{"id": "bad", "body": "x"})
	assertError(t, resp, got, http.StatusUnauthorized, "unauthorized")
	resp, _ = f.request(http.MethodPost, "/v2/rooms/"+roomID+"/messages", token, map[string]string{"id": "good", "body": "x"})
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
	f.request(http.MethodGet, "/v2/rooms/"+roomID, token, nil)
	for _, secret := range []string{invite, token, roomID} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs leaked secret %q: %s", secret, logs.String())
		}
	}
}

func TestJoinRepresentationsDoNotClaimInvite(t *testing.T) {
	f := newFixture(t)
	_, _, invite := f.room()
	inviteURL := "https://relay.example/join/" + invite

	for _, test := range []struct {
		name        string
		accept      string
		query       string
		contentType string
		contains    string
	}{
		{name: "curl default", accept: "*/*", contentType: "text/markdown", contains: "agentline join '" + inviteURL + "'"},
		{name: "markdown", accept: "text/markdown", contentType: "text/markdown", contains: "# Join this Agentline room"},
		{name: "browser", accept: "text/html", contentType: "text/html", contains: "Join this Agentline room"},
		{name: "query override", accept: "text/html", query: "?format=markdown", contentType: "text/markdown", contains: "agentline join '" + inviteURL + "'"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, f.server.URL+"/join/"+invite+test.query, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Accept", test.accept)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), test.contentType) || !strings.Contains(string(body), test.contains) {
				t.Fatalf("status=%d content-type=%q body=%q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
			}
			if !strings.Contains(string(body), "curl -fsSL https://relay.example/install.sh | sh") {
				t.Errorf("join instructions do not pipe the installer to sh: %q", body)
			}
			for name, want := range map[string]string{
				"Cache-Control":          "no-store",
				"Referrer-Policy":        "no-referrer",
				"X-Content-Type-Options": "nosniff",
				"Vary":                   "Accept",
			} {
				if got := resp.Header.Get(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}

	resp, claimed := f.request(http.MethodPost, "/v2/invites/"+invite+"/claim", "", map[string]string{"name": "bob"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim after join instructions: %d %#v", resp.StatusCode, claimed)
	}
}

func TestInstallerRoute(t *testing.T) {
	f := newFixture(t)
	req, err := http.NewRequest(http.MethodGet, f.server.URL+"/install.sh", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/x-shellscript") {
		t.Fatalf("status=%d content-type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, marker := range []string{"set -eu", "checksums.txt", "AGENTLINE_INSTALL_DIR", "sha256sum", "shasum"} {
		if !strings.Contains(string(body), marker) {
			t.Errorf("installer missing %q", marker)
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
	resp, got := f.request(http.MethodPost, "/v2/rooms", "", map[string]any{"name": "amber-fox", "creator_name": "alice", "ttl_seconds": 600})
	if resp.StatusCode != http.StatusCreated {
		f.t.Fatalf("create: %d %#v", resp.StatusCode, got)
	}
	return got["room"].(map[string]any)["id"].(string), got["participant_token"].(string), got["invite_token"].(string)
}

func (f *fixture) join(invite, name string) (participantID, token string) {
	f.t.Helper()
	resp, got := f.request(http.MethodPost, "/v2/invites/"+invite+"/claim", "", map[string]string{"name": name})
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("join %s: %d %#v", name, resp.StatusCode, got)
	}
	return got["participant"].(map[string]any)["id"].(string), got["participant_token"].(string)
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
	resp, claimed := f.request(http.MethodPost, "/v2/invites/"+invite+"/claim", "", map[string]string{"name": "bob"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: %d %#v", resp.StatusCode, claimed)
	}
	bob := claimed["participant_token"].(string)
	resp, third := f.request(http.MethodPost, "/v2/invites/"+invite+"/claim", "", map[string]string{"name": "eve"})
	if resp.StatusCode != http.StatusOK || third["participant"].(map[string]any)["name"] != "eve" {
		t.Fatalf("reusable claim: %d %#v", resp.StatusCode, third)
	}

	resp, got := f.request(http.MethodGet, "/v2/rooms/"+roomID, alice, nil)
	if resp.StatusCode != http.StatusOK || got["status"] != "active" {
		t.Fatalf("room: %d %#v", resp.StatusCode, got)
	}
	message := map[string]string{"id": "message-1", "body": "hello", "reply_to": "prior"}
	resp, sent := f.request(http.MethodPost, "/v2/rooms/"+roomID+"/messages", alice, message)
	if resp.StatusCode != http.StatusOK || sent["sequence"] != float64(1) {
		t.Fatalf("send: %d %#v", resp.StatusCode, sent)
	}
	resp, retry := f.request(http.MethodPost, "/v2/rooms/"+roomID+"/messages", alice, message)
	if resp.StatusCode != http.StatusOK || retry["id"] != "message-1" {
		t.Fatalf("retry: %d %#v", resp.StatusCode, retry)
	}
	resp, listed := f.request(http.MethodGet, "/v2/rooms/"+roomID+"/messages?after=0", bob, nil)
	if resp.StatusCode != http.StatusOK || len(listed["messages"].([]any)) != 1 {
		t.Fatalf("list: %d %#v", resp.StatusCode, listed)
	}
	resp, listed = f.request(http.MethodGet, "/v2/rooms/"+roomID+"/messages?after=1", bob, nil)
	if resp.StatusCode != http.StatusOK || len(listed["messages"].([]any)) != 0 {
		t.Fatalf("cursor boundary: %d %#v", resp.StatusCode, listed)
	}
	resp, done := f.request(http.MethodPost, "/v2/rooms/"+roomID+"/done", bob, map[string]string{"id": "done-1"})
	if resp.StatusCode != http.StatusOK || done["kind"] != "done" {
		t.Fatalf("done: %d %#v", resp.StatusCode, done)
	}
	resp, retry = f.request(http.MethodPost, "/v2/rooms/"+roomID+"/done", bob, map[string]string{"id": "done-1"})
	if resp.StatusCode != http.StatusOK || retry["id"] != "done-1" {
		t.Fatalf("done retry: %d %#v", resp.StatusCode, retry)
	}
	resp, failure := f.request(http.MethodPost, "/v2/rooms/"+roomID+"/messages", alice, map[string]string{"id": "late", "body": "late"})
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
	resp, got := f.request(http.MethodGet, "/v2/rooms/"+secondRoom, firstToken, nil)
	assertError(t, resp, got, http.StatusUnauthorized, "unauthorized")
}

func TestAuthenticationValidationBodyAndRateLimits(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.MessageBytes = 8; c.CreatePerHour = 1 })
	roomID, token, _ := f.room()
	resp, got := f.request(http.MethodGet, "/v2/rooms/"+roomID, "wrong", nil)
	assertError(t, resp, got, http.StatusUnauthorized, "unauthorized")
	resp, got = f.request(http.MethodPost, "/v2/rooms/"+roomID+"/messages", token, map[string]string{"id": "large", "body": "too large"})
	assertError(t, resp, got, http.StatusRequestEntityTooLarge, "body_too_large")
	resp, got = f.request(http.MethodPost, "/v2/rooms", "", map[string]string{"name": "second", "creator_name": "x"})
	assertError(t, resp, got, http.StatusTooManyRequests, "rate_limited")
	resp, got = f.request(http.MethodPost, "/v3/rooms", "", map[string]string{})
	assertError(t, resp, got, http.StatusNotFound, "unsupported_api_version")
}

func TestWaitWakesTimesOutAndCancels(t *testing.T) {
	f := newFixture(t)
	roomID, token, invite := f.room()
	resp, claimed := f.request(http.MethodPost, "/v2/invites/"+invite+"/claim", "", map[string]string{"name": "bob"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: %d %#v", resp.StatusCode, claimed)
	}
	peer := claimed["participant_token"].(string)
	type result struct {
		resp *http.Response
		body map[string]any
	}
	wake := make(chan result, 1)
	go func() {
		r, b := f.request(http.MethodGet, "/v2/rooms/"+roomID+"/wait?after=0&timeout=1", token, nil)
		wake <- result{r, b}
	}()
	time.Sleep(30 * time.Millisecond)
	f.request(http.MethodPost, "/v2/rooms/"+roomID+"/messages", peer, map[string]string{"id": "wake", "body": "hi"})
	got := <-wake
	if got.resp.StatusCode != http.StatusOK || got.body["status"] != "message" {
		t.Fatalf("wake: %d %#v", got.resp.StatusCode, got.body)
	}

	resp, timeout := f.request(http.MethodGet, "/v2/rooms/"+roomID+"/wait?after=1&timeout=0.01", token, nil)
	if resp.StatusCode != http.StatusOK || timeout["status"] != "timeout" {
		t.Fatalf("timeout: %d %#v", resp.StatusCode, timeout)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/v2/rooms/"+roomID+"/wait?after=1&timeout=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	done := make(chan error, 1)
	go func() { _, err := http.DefaultClient.Do(req); done <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled request unexpectedly succeeded")
	}
}

func TestWaitSkipsSelfAuthoredMessage(t *testing.T) {
	f := newFixture(t)
	roomID, token, _ := f.room()
	f.request(http.MethodPost, "/v2/rooms/"+roomID+"/messages", token, map[string]string{"id": "self", "body": "mine"})

	resp, got := f.request(http.MethodGet, "/v2/rooms/"+roomID+"/wait?after=0&timeout=0", token, nil)
	if resp.StatusCode != http.StatusOK || got["status"] != "timeout" || got["sequence"] != float64(1) {
		t.Fatalf("self skip: %d %#v", resp.StatusCode, got)
	}
}

func TestWaitSkipsSelfAndHiddenEventsAndAdvancesCursor(t *testing.T) {
	f := newFixture(t)
	roomID, alice, invite := f.room()
	bobID, _ := f.join(invite, "bob")
	_, carol := f.join(invite, "carol")
	for _, message := range []map[string]string{
		{"id": "self", "body": "mine"},
		{"id": "hidden", "body": "private", "to": bobID},
		{"id": "self-again", "body": "mine again"},
	} {
		resp, got := f.request(http.MethodPost, "/v2/rooms/"+roomID+"/messages", alice, message)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("send = %d %#v", resp.StatusCode, got)
		}
	}
	resp, got := f.request(http.MethodGet, "/v2/rooms/"+roomID+"/wait?after=0&timeout=0", alice, nil)
	if resp.StatusCode != http.StatusOK || got["status"] != "timeout" || got["sequence"] != float64(3) {
		t.Fatalf("alice wait = %d %#v", resp.StatusCode, got)
	}
	resp, got = f.request(http.MethodGet, "/v2/rooms/"+roomID+"/messages?after=0", carol, nil)
	if resp.StatusCode != http.StatusOK || len(got["messages"].([]any)) != 2 || got["cursor"] != float64(3) {
		t.Fatalf("carol history = %d %#v", resp.StatusCode, got)
	}
}

func TestDoneWakesEveryParticipantWaiter(t *testing.T) {
	f := newFixture(t)
	roomID, alice, invite := f.room()
	_, bob := f.join(invite, "bob")
	_, carol := f.join(invite, "carol")
	results := make(chan map[string]any, 3)
	for _, token := range []string{alice, bob, carol} {
		go func(token string) {
			_, got := f.request(http.MethodGet, "/v2/rooms/"+roomID+"/wait?timeout=1", token, nil)
			results <- got
		}(token)
	}
	time.Sleep(100 * time.Millisecond)
	resp, got := f.request(http.MethodPost, "/v2/rooms/"+roomID+"/done", bob, map[string]string{"id": "done-all"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("done = %d %#v", resp.StatusCode, got)
	}
	for range 3 {
		select {
		case got := <-results:
			if got["status"] != "done" {
				t.Fatalf("wait = %#v", got)
			}
		case <-time.After(time.Second):
			t.Fatal("participant wait did not wake")
		}
	}
}

func TestWaitHiddenPrivateMessageDoesNotWakeOrLeakAndAdvancesCursor(t *testing.T) {
	f := newFixture(t)
	roomID, aliceToken, invite := f.room()
	resp, claimed := f.request(http.MethodPost, "/v2/invites/"+invite+"/claim", "", map[string]string{"name": "bob"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: %d %#v", resp.StatusCode, claimed)
	}
	alice, err := f.store.Authenticate(context.Background(), roomID, aliceToken)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := f.store.Authenticate(context.Background(), roomID, claimed["participant_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Append(context.Background(), store.AppendParams{RoomID: roomID, ParticipantID: bob.ID, MessageID: "private", To: bob.ID, Body: "secret", Kind: "message"}); err != nil {
		t.Fatal(err)
	}

	resp, got := f.request(http.MethodGet, "/v2/rooms/"+roomID+"/wait?after=0&timeout=0", aliceToken, nil)
	if resp.StatusCode != http.StatusOK || got["status"] != "timeout" || got["sequence"] != float64(1) {
		t.Fatalf("hidden private wait: %d %#v (alice=%s)", resp.StatusCode, got, alice.ID)
	}
}

func TestWaitReturnsSelfAuthoredDone(t *testing.T) {
	f := newFixture(t)
	roomID, token, _ := f.room()
	f.request(http.MethodPost, "/v2/rooms/"+roomID+"/done", token, map[string]string{"id": "done"})

	resp, got := f.request(http.MethodGet, "/v2/rooms/"+roomID+"/wait?after=0&timeout=0", token, nil)
	if resp.StatusCode != http.StatusOK || got["status"] != "done" || got["sequence"] != float64(1) {
		t.Fatalf("self done: %d %#v", resp.StatusCode, got)
	}
}

type recheckStore struct {
	store.Store
	onSecondScan func()
	scans        int
}

func (s *recheckStore) MessagesAfter(ctx context.Context, roomID, participantID string, sequence int64, limit int, skipSelf bool) (store.VisibleMessages, error) {
	s.scans++
	if s.scans == 2 {
		s.onSecondScan()
	}
	return s.Store.MessagesAfter(ctx, roomID, participantID, sequence, limit, skipSelf)
}

func TestWaitReturnsMessageFoundBySubscribeRecheck(t *testing.T) {
	data, err := store.OpenSQLite(":memory:", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { data.Close() })
	created, err := data.CreateRoom(context.Background(), store.CreateRoomParams{Name: "room", CreatorName: "alice", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := data.ClaimInvite(context.Background(), created.InviteToken, "bob")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &recheckStore{Store: data}
	wrapped.onSecondScan = func() {
		if _, err := data.Append(context.Background(), store.AppendParams{RoomID: created.Room.ID, ParticipantID: bob.Participant.ID, MessageID: "race", Body: "arrived", Kind: "message"}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(NewHandler(wrapped, Config{WaitMax: 50 * time.Millisecond}, time.Now))
	t.Cleanup(server.Close)
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v2/rooms/"+created.Room.ID+"/wait?after=0&timeout=0.05", nil)
	req.Header.Set("Authorization", "Bearer "+created.CreatorToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "message" || got["message"].(map[string]any)["id"] != "race" {
		t.Fatalf("subscribe recheck = %#v", got)
	}
}

func TestWaitRejectsNonFiniteTimeout(t *testing.T) {
	f := newFixture(t)
	roomID, token, _ := f.room()
	for _, timeout := range []string{"NaN", "+Inf"} {
		resp, got := f.request(http.MethodGet, "/v2/rooms/"+roomID+"/wait?timeout="+timeout, token, nil)
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
	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/v2/rooms", strings.NewReader(`{"name":"x","creator_name":"y","extra":true}`))
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
	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/v2/rooms", strings.NewReader(`{"name":"x","creator_name":"y"} {}`))
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
	resp, err := http.Post(base+"/v2/rooms", "application/json", createBody)
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
		req, _ := http.NewRequest(http.MethodGet, base+"/v2/rooms/"+roomID+"/wait?timeout=3600", nil)
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
