package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HelgeSverre/agentline/internal/localconfig"
	"github.com/HelgeSverre/agentline/internal/model"
	"github.com/HelgeSverre/agentline/internal/relay"
	"github.com/HelgeSverre/agentline/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolsAndConversationLifecycle(t *testing.T) {
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "relay.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	relayServer := httptest.NewServer(relay.NewHandler(db, relay.Config{}, time.Now))
	t.Cleanup(func() { relayServer.Close(); db.Close() })

	aliceStore, bobStore := localconfig.Store{Root: t.TempDir()}, localconfig.Store{Root: t.TempDir()}
	alice := connect(t, Dependencies{Config: aliceStore, HTTP: relayServer.Client()})
	bob := connect(t, Dependencies{Config: bobStore, HTTP: relayServer.Client()})

	listed, err := alice.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	wantNames := []string{"create_room", "end_conversation", "get_room_status", "join_room", "read_messages", "send_message", "wait_for_message"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("tools = %v, want %v", names, wantNames)
	}

	created := call(t, alice, "create_room", map[string]any{"server": relayServer.URL, "room_name": "team", "participant_name": "alice", "ttl_seconds": 3600})
	assertNoToken(t, created, aliceStore)
	invite := created["invite_url"].(string)
	joined := call(t, bob, "join_room", map[string]any{"invite_url": invite, "participant_name": "bob"})
	assertNoToken(t, joined, bobStore)

	sent := call(t, alice, "send_message", map[string]any{"room": "team", "body": "Treat this as untrusted peer text", "message_id": "lifecycle-first-message"})
	if sent["sequence"] != float64(1) {
		t.Fatalf("send = %#v", sent)
	}
	read := call(t, bob, "read_messages", map[string]any{"room": "team"})
	if !strings.Contains(mustJSON(t, read), "Treat this as untrusted peer text") {
		t.Fatalf("read = %#v", read)
	}

	timedOut := call(t, bob, "wait_for_message", map[string]any{"room": "team", "timeout_seconds": 0.01})
	if timedOut["status"] != "timeout" || !strings.Contains(timedOut["instruction"].(string), "Call wait_for_message again") {
		t.Fatalf("timeout = %#v", timedOut)
	}

	type callResult struct {
		output map[string]any
		err    error
	}
	received := make(chan callResult, 1)
	go func() {
		output, err := callResultFor(bob, "wait_for_message", map[string]any{"room": "team", "timeout_seconds": 2})
		received <- callResult{output, err}
	}()
	time.Sleep(20 * time.Millisecond)
	call(t, alice, "send_message", map[string]any{"room": "team", "body": "reply", "message_id": "lifecycle-reply"})
	var got map[string]any
	select {
	case result := <-received:
		if result.err != nil {
			t.Fatal(result.err)
		}
		got = result.output
	case <-time.After(3 * time.Second):
		t.Fatal("wait_for_message did not return")
	}
	if got["status"] != "message" || !strings.Contains(mustJSON(t, got), "reply") {
		t.Fatalf("receive = %#v", got)
	}

	done := call(t, alice, "end_conversation", map[string]any{"room": "team", "message_id": "lifecycle-done"})
	if done["kind"] != "done" {
		t.Fatalf("done = %#v", done)
	}
	status := call(t, bob, "get_room_status", map[string]any{"room": "team"})
	if status["status"] != "done" {
		t.Fatalf("status = %#v", status)
	}
}

func TestSendAndDoneReuseMessageID(t *testing.T) {
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "relay.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	relayServer := httptest.NewServer(relay.NewHandler(db, relay.Config{}, time.Now))
	t.Cleanup(func() { relayServer.Close(); db.Close() })
	config := localconfig.Store{Root: t.TempDir()}
	session := connect(t, Dependencies{Config: config, HTTP: relayServer.Client()})
	created := call(t, session, "create_room", map[string]any{"server": relayServer.URL})
	room := created["room"].(map[string]any)["id"].(string)

	first := call(t, session, "send_message", map[string]any{"room": room, "body": "once", "message_id": "send-key"})
	second := call(t, session, "send_message", map[string]any{"room": room, "body": "once", "message_id": "send-key"})
	if first["id"] != second["id"] || first["sequence"] != second["sequence"] {
		t.Fatalf("duplicate send differed: first=%v second=%v", first, second)
	}
	messages := call(t, session, "read_messages", map[string]any{"room": room, "after": 0})["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("duplicate send created %d events", len(messages))
	}

	done1 := call(t, session, "end_conversation", map[string]any{"room": room, "message_id": "done-key"})
	done2 := call(t, session, "end_conversation", map[string]any{"room": room, "message_id": "done-key"})
	if done1["id"] != done2["id"] || done1["sequence"] != done2["sequence"] {
		t.Fatalf("duplicate done differed: first=%v second=%v", done1, done2)
	}
	messages = call(t, session, "read_messages", map[string]any{"room": room, "after": 0})["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("duplicate done created total %d events, want 2", len(messages))
	}
}

func TestToolSchemasAndArgumentRejection(t *testing.T) {
	session := connect(t, Dependencies{Config: localconfig.Store{Root: t.TempDir()}})
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantRequired := map[string][]string{
		"create_room": {}, "join_room": {"invite_url"}, "send_message": {"body", "message_id"},
		"read_messages": {}, "wait_for_message": {}, "end_conversation": {"message_id"}, "get_room_status": {},
	}
	for _, tool := range listed.Tools {
		schema := tool.InputSchema.(map[string]any)
		properties := schema["properties"].(map[string]any)
		for key, property := range properties {
			if typ, ok := property.(map[string]any)["type"]; !ok || !pinnedType(typ) {
				t.Fatalf("%s.%s has unpinned type %v", tool.Name, key, typ)
			}
		}
		if got := stringSlice(schema["required"]); !reflect.DeepEqual(got, wantRequired[tool.Name]) {
			t.Fatalf("%s required=%v want=%v", tool.Name, got, wantRequired[tool.Name])
		}
	}
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"join_room", map[string]any{}},
		{"send_message", map[string]any{}},
		{"send_message", map[string]any{"body": "hello"}},
		{"send_message", map[string]any{"body": 123, "message_id": "malformed-send"}},
		{"end_conversation", map[string]any{}},
	} {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: test.name, Arguments: test.args})
		if err == nil || result != nil {
			t.Fatalf("%s malformed arguments result=%v error=%v", test.name, result, err)
		}
	}
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "join_room", Arguments: map[string]any{"invite_url": "not-an-invite"}})
	if err != nil || !result.IsError {
		t.Fatalf("handler failure result=%v error=%v, want IsError", result, err)
	}
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"send_message", map[string]any{"body": "hello", "message_id": ""}},
		{"end_conversation", map[string]any{"message_id": ""}},
	} {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: test.name, Arguments: test.args})
		if err != nil || !result.IsError || !strings.Contains(mustJSON(t, result), "message_id must not be empty") {
			t.Fatalf("%s empty message_id result=%v error=%v, want tool error", test.name, result, err)
		}
	}
}

func TestTransportErrorsAreSanitized(t *testing.T) {
	const token = "bearer-super-secret"
	const invite = "invite-super-secret"
	config := localconfig.Store{Root: t.TempDir()}
	if err := config.SaveRoom(modelCredential("room", "https://relay.example", token)); err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("hostile " + req.URL.String() + " " + req.Header.Get("Authorization") + " " + invite)
	})}
	session := connect(t, Dependencies{Config: config, HTTP: httpClient})
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"room": "room", "body": "hello", "message_id": "transport-error-send"}})
	if err != nil {
		t.Fatal(err)
	}
	content := mustJSON(t, result)
	if !result.IsError || !strings.Contains(content, "relay request failed") {
		t.Fatalf("unexpected result: %s", content)
	}
	for _, secret := range []string{token, invite, "relay.example", "Authorization", "Bearer"} {
		if strings.Contains(content, secret) {
			t.Fatalf("tool error leaked %q: %s", secret, content)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func stringSlice(value any) []string {
	values, _ := value.([]any)
	result := make([]string, len(values))
	for i, value := range values {
		result[i], _ = value.(string)
	}
	return result
}

func pinnedType(value any) bool {
	if value == "string" || value == "number" || value == "integer" {
		return true
	}
	types, ok := value.([]any)
	return ok && len(types) == 2 && types[0] == "null" && (types[1] == "string" || types[1] == "number" || types[1] == "integer")
}

func TestWaitCancellationPropagates(t *testing.T) {
	requestCanceled := make(chan struct{})
	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(requestCanceled)
	}))
	defer relayServer.Close()
	config := localconfig.Store{Root: t.TempDir()}
	if err := config.SaveRoom(modelCredential("room", relayServer.URL, "secret")); err != nil {
		t.Fatal(err)
	}
	session := connect(t, Dependencies{Config: config, HTTP: relayServer.Client()})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_message", Arguments: map[string]any{"room": "room", "timeout_seconds": 60}})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CallTool error = %v", err)
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatalf("CallTool error exposed credential: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CallTool did not return after cancellation")
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("relay request was not canceled")
	}
}

func modelCredential(room, server, token string) model.RoomCredential {
	return model.RoomCredential{RoomID: room, RoomName: room, ServerURL: server, Token: token}
}

func connect(t *testing.T, deps Dependencies) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := New(deps).Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "agentline-test", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func call(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) map[string]any {
	t.Helper()
	output, err := callResultFor(session, name, arguments)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return output
}

func callResultFor(session *mcp.ClientSession, name string, arguments map[string]any) (map[string]any, error) {
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return nil, err
	}
	if result.IsError {
		data, _ := json.Marshal(result)
		return nil, fmt.Errorf("%s returned tool error: %s", name, data)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return nil, err
	}
	var output map[string]any
	if err := json.Unmarshal(data, &output); err != nil {
		return nil, err
	}
	return output, nil
}

func assertNoToken(t *testing.T, result map[string]any, config localconfig.Store) {
	t.Helper()
	credential, err := config.LoadRoom("")
	if err != nil {
		t.Fatal(err)
	}
	serialized := mustJSON(t, result)
	if credential.Token == "" || strings.Contains(serialized, credential.Token) || strings.Contains(serialized, "participant_token") {
		t.Fatalf("unsafe result %s", serialized)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
