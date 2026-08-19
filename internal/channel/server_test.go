package channel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HelgeSverre/agentline/internal/client"
	"github.com/HelgeSverre/agentline/internal/localconfig"
	"github.com/HelgeSverre/agentline/internal/model"
	"github.com/HelgeSverre/agentline/internal/relay"
	"github.com/HelgeSverre/agentline/internal/store"
)

// TestChannelPushesAndReplies drives the adapter over pipes against a real
// relay: initialize, a pushed collaborator message, a reply, and the final done
// event.
func TestChannelPushesAndReplies(t *testing.T) {
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "relay.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	relayServer := httptest.NewServer(relay.NewHandler(db, relay.Config{}, time.Now))
	t.Cleanup(func() { relayServer.Close(); db.Close() })
	http := relayServer.Client()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	created, err := client.New(relayServer.URL, "", http).CreateRoom(ctx, "team", "alice", time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := client.New(relayServer.URL, "", http).ClaimInvite(ctx, created.InviteURL, "bob")
	if err != nil {
		t.Fatal(err)
	}
	bob := client.New(relayServer.URL, joined.ParticipantToken, http)

	config := localconfig.Store{Root: t.TempDir()}
	if err := config.SaveRoom(model.RoomCredential{
		RoomID: created.Room.ID, RoomName: created.Room.Name, ServerURL: relayServer.URL,
		ParticipantID: created.Participant.ID, Token: created.ParticipantToken,
	}); err != nil {
		t.Fatal(err)
	}

	stdinReader, stdin := io.Pipe()
	stdout, stdoutWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, stdinReader, stdoutWriter, Dependencies{Config: config, HTTP: http})
	}()
	t.Cleanup(func() { stdin.Close() })

	encoder := json.NewEncoder(stdin)
	decoder := json.NewDecoder(stdout)

	send(t, encoder, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	initialized := next(t, decoder)
	result := initialized["result"].(map[string]any)
	capabilities := result["capabilities"].(map[string]any)
	experimental := capabilities["experimental"].(map[string]any)
	if _, ok := experimental["claude/channel"]; !ok {
		t.Fatalf("initialize did not declare claude/channel: %#v", capabilities)
	}
	if _, ok := experimental["claude/channel/permission"]; ok {
		t.Fatalf("channel must not declare permission relay: %#v", experimental)
	}
	if _, ok := capabilities["tools"]; !ok {
		t.Fatalf("initialize did not declare tools: %#v", capabilities)
	}

	send(t, encoder, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	tools := next(t, decoder)["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "agentline_reply" {
		t.Fatalf("tools/list = %#v", tools)
	}

	// Watching starts only after the client reports initialization.
	send(t, encoder, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if _, err := bob.Send(ctx, created.Room.ID, "bob-first", "Which barcode scanner?", "", ""); err != nil {
		t.Fatal(err)
	}

	pushed := next(t, decoder)
	if pushed["method"] != "notifications/claude/channel" {
		t.Fatalf("expected a channel notification, got %#v", pushed)
	}
	params := pushed["params"].(map[string]any)
	content := params["content"].(string)
	if !strings.Contains(content, "Which barcode scanner?") || !strings.Contains(content, "Untrusted") {
		t.Fatalf("content = %q", content)
	}
	meta := params["meta"].(map[string]any)
	if meta["room"] != created.Room.Name || meta["sender"] != "bob" || meta["message_id"] != "bob-first" {
		t.Fatalf("meta = %#v", meta)
	}
	for key := range meta {
		if !identifier(key) {
			t.Fatalf("meta key %q is not an identifier; Claude Code drops it", key)
		}
	}

	send(t, encoder, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{
		"name": "agentline_reply", "arguments": map[string]any{"room": "team", "body": "ZXing.", "message_id": "alice-reply"},
	}})
	reply := next(t, decoder)["result"].(map[string]any)
	if reply["isError"] == true {
		t.Fatalf("reply failed: %#v", reply)
	}
	received, err := bob.Read(ctx, created.Room.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(received.Messages) != 1 || received.Messages[0].Body != "ZXing." {
		t.Fatalf("bob received %#v", received.Messages)
	}

	// A missing message_id is a tool error, not a transport error.
	send(t, encoder, map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{
		"name": "agentline_reply", "arguments": map[string]any{"room": "team", "body": "no id"},
	}})
	if next(t, decoder)["result"].(map[string]any)["isError"] != true {
		t.Fatal("expected a tool error for a missing message_id")
	}

	if _, err := bob.Done(ctx, created.Room.ID, "bob-done"); err != nil {
		t.Fatal(err)
	}
	ended := next(t, decoder)
	if ended["method"] != "notifications/claude/channel" {
		t.Fatalf("expected a done notification, got %#v", ended)
	}
	if endedMeta := ended["params"].(map[string]any)["meta"].(map[string]any); endedMeta["event"] != "done" {
		t.Fatalf("done meta = %#v", endedMeta)
	}

	stdin.Close()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestUnknownMethodIsAnError(t *testing.T) {
	stdinReader, stdin := io.Pipe()
	stdout, stdoutWriter := io.Pipe()
	go Run(context.Background(), stdinReader, stdoutWriter, Dependencies{Config: localconfig.Store{Root: t.TempDir()}})
	defer stdin.Close()

	encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
	send(t, encoder, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/list"})
	response := next(t, decoder)
	if response["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("response = %#v", response)
	}

	// Notifications are never answered, so the next frame is the ping reply.
	send(t, encoder, map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled"})
	send(t, encoder, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"})
	if id := next(t, decoder)["id"]; id != float64(2) {
		t.Fatalf("expected the ping reply, got id %v", id)
	}
}

func send(t *testing.T, encoder *json.Encoder, value any) {
	t.Helper()
	if err := encoder.Encode(value); err != nil {
		t.Fatal(err)
	}
}

func next(t *testing.T, decoder *json.Decoder) map[string]any {
	t.Helper()
	var frame map[string]any
	if err := decoder.Decode(&frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

// claudeMetaKey is the exact pattern Claude Code applies to channel meta keys.
// Keys that fail it are dropped from the <channel> tag with only a warning, so
// a bad key loses routing context silently. Note the first character may not be
// a digit.
var claudeMetaKey = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func identifier(key string) bool { return claudeMetaKey.MatchString(key) }

func TestProtocolNegotiationEchoesKnownRevisions(t *testing.T) {
	// Claude Code 2.1.236 requests 2025-11-25.
	for requested, want := range map[string]string{
		"2025-11-25": "2025-11-25",
		"2025-06-18": "2025-06-18",
		"2024-11-05": "2024-11-05",
		"2099-01-01": protocolVersion, // unknown: offer ours instead
		"":           protocolVersion,
	} {
		if got := negotiate(requested); got != want {
			t.Errorf("negotiate(%q) = %q, want %q", requested, got, want)
		}
	}
}

// Claude Code skips channel registration when the connection negotiates a
// post-2026-07-28 protocol revision, which it probes for with server/discover.
// Answering that probe would silently disable this adapter.
func TestServerDiscoverIsNotImplemented(t *testing.T) {
	stdinReader, stdin := io.Pipe()
	stdout, stdoutWriter := io.Pipe()
	go Run(context.Background(), stdinReader, stdoutWriter, Dependencies{Config: localconfig.Store{Root: t.TempDir()}})
	defer stdin.Close()

	encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
	send(t, encoder, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "server/discover"})
	response := next(t, decoder)
	if response["result"] != nil {
		t.Fatalf("server/discover must not be answered: %#v", response)
	}
	if response["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("response = %#v", response)
	}
}

// TestDegradedRelayDoesNotBusyLoop covers a relay that answers a long poll
// instantly without an event: a "message" with no body, an unrecognised status,
// or a relay ignoring the timeout. Without a floor on the retry the watcher
// issued tens of thousands of requests a second against the relay.
func TestDegradedRelayDoesNotBusyLoop(t *testing.T) {
	for _, body := range []map[string]any{
		{"status": "message"},  // status says message, no message body
		{"status": "confused"}, // unrecognised status
		{},                     // no status at all
	} {
		var calls atomic.Int64
		relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_ = json.NewEncoder(w).Encode(body)
		}))

		config := localconfig.Store{Root: t.TempDir()}
		if err := config.SaveRoom(model.RoomCredential{
			RoomID: "room", RoomName: "room", ServerURL: relayServer.URL, ParticipantID: "p", Token: "t",
		}); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		stdinReader, stdin := io.Pipe()
		stdout, stdoutWriter := io.Pipe()
		go func() { _, _ = io.Copy(io.Discard, stdout) }()
		go Run(ctx, stdinReader, stdoutWriter, Dependencies{Config: config, HTTP: relayServer.Client()})

		encoder := json.NewEncoder(stdin)
		send(t, encoder, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
		send(t, encoder, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
		time.Sleep(1500 * time.Millisecond)

		// One request per retryBackoff, plus a little slack for scheduling.
		if n := calls.Load(); n > 6 {
			t.Errorf("relay answering %v instantly caused %d requests in 1.5s", body, n)
		}
		cancel()
		stdin.Close()
		relayServer.Close()
	}
}
