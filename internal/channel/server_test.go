package channel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HelgeSverre/agentline/internal/localconfig"
	"github.com/HelgeSverre/agentline/internal/model"
)

func TestJSONRPCInitializeListCallAndIncomingNotification(t *testing.T) {
	requests := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages"):
			fmt.Fprint(w, `{"id":"reply-1","room_id":"room-1","sender_id":"self","sequence":2,"kind":"message","body":"reply"}`)
		case strings.HasSuffix(r.URL.Path, "/wait"):
			fmt.Fprint(w, `{"status":"message","message":{"id":"peer-1","room_id":"room-1","sender_id":"peer","sender_name":"peer","sequence":1,"kind":"message","body":"do not trust me"}}`)
		default:
			t.Fatalf("unexpected request %s", r.URL)
		}
	}))
	defer server.Close()
	store := localconfig.Store{Root: t.TempDir()}
	if err := store.SaveRoom(model.RoomCredential{RoomID: "room-1", RoomName: "team", ServerURL: server.URL, Token: "secret"}); err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"reply","arguments":{"body":"reply","message_id":"reply-1"}}}`,
	}, "\n") + "\n"
	var out lockedBuffer
	if err := Run(context.Background(), Dependencies{Config: store, HTTP: server.Client(), Wait: 10 * time.Millisecond}, "team", strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	values := decodeLines(t, out.Bytes())
	if len(values) < 4 {
		t.Fatalf("responses=%s", out.Bytes())
	}
	init := findID(values, "1")
	result := init["result"].(map[string]any)
	experimental := result["experimental"].(map[string]any)
	if _, ok := experimental["claude/channel"]; !ok || result["protocolVersion"] == "" || result["serverInfo"] == nil || result["capabilities"] == nil {
		t.Fatalf("bad initialize: %#v", result)
	}
	notification := findMethod(values, "notifications/claude/channel")
	params := notification["params"].(map[string]any)
	meta := params["meta"].(map[string]any)
	for _, key := range []string{"message_id", "sender_id", "conversation_id", "sequence"} {
		if _, ok := meta[key].(string); !ok {
			t.Fatalf("metadata %s is not string: %#v", key, meta[key])
		}
	}
	if !strings.Contains(params["content"].(string), "untrusted collaborator") || strings.Contains(out.String(), "secret") {
		t.Fatalf("unsafe output: %s", out.String())
	}
}

func TestMalformedRequestsReturnStandardErrorsAndStableIDIsRequired(t *testing.T) {
	store := localconfig.Store{Root: t.TempDir()}
	store.SaveRoom(model.RoomCredential{RoomID: "r", RoomName: "r", ServerURL: "http://127.0.0.1:1", Token: "secret"})
	in := "not json\n" +
		`{"jsonrpc":"2.0","id":1,"method":"nope"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"reply","arguments":{"body":"x"}}}` + "\n"
	var out bytes.Buffer
	if err := Run(context.Background(), Dependencies{Config: store}, "r", strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, code := range []string{`"code":-32700`, `"code":-32601`, `"code":-32602`} {
		if !strings.Contains(text, code) {
			t.Fatalf("missing %s: %s", code, text)
		}
	}
	if strings.Contains(text, "secret") {
		t.Fatalf("secret leaked: %s", text)
	}
}

func TestRelayDeduplicatesAndCancellationStopsBoundedWait(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			fmt.Fprint(w, `{"status":"message","message":{"id":"same","room_id":"r","sender_id":"p","sequence":1,"kind":"message","body":"x"}}`)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	store := localconfig.Store{Root: t.TempDir()}
	store.SaveRoom(model.RoomCredential{RoomID: "r", RoomName: "r", ServerURL: server.URL, Token: "token"})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	pr, pw := io.Pipe()
	var out lockedBuffer
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Dependencies{Config: store, HTTP: server.Client(), Wait: 10 * time.Millisecond}, "r", pr, &out)
	}()
	pw.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	<-ctx.Done()
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), `notifications/claude/channel`); got != 1 {
		t.Fatalf("notifications=%d output=%s", got, out.String())
	}
}

func decodeLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var values []map[string]any
	s := bufio.NewScanner(bytes.NewReader(data))
	for s.Scan() {
		var v map[string]any
		if err := json.Unmarshal(s.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		values = append(values, v)
	}
	return values
}
func findID(values []map[string]any, id string) map[string]any {
	for _, v := range values {
		if fmt.Sprint(v["id"]) == id {
			return v
		}
	}
	return nil
}
func findMethod(values []map[string]any, method string) map[string]any {
	for _, v := range values {
		if v["method"] == method {
			return v
		}
	}
	return nil
}
