package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HelgeSverre/agentline/internal/localconfig"
	"github.com/HelgeSverre/agentline/internal/model"
	"github.com/HelgeSverre/agentline/internal/relay"
	"github.com/HelgeSverre/agentline/internal/store"
)

func TestLifecyclePersistsCredentialsAndCursor(t *testing.T) {
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "relay.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(relay.NewHandler(db, relay.Config{}, time.Now))
	defer server.Close()
	rootA, rootB := t.TempDir(), t.TempDir()

	code, out, stderr := run(t, rootA, "--json", "create", "--server", server.URL, "--name", "alice", "--room-name", "team", "--ttl", "1h")
	if code != 0 {
		t.Fatalf("create: %s", stderr)
	}
	var created map[string]any
	json.Unmarshal([]byte(out), &created)
	invite := created["invite_url"].(string)
	if created["inspect_url"] == "" {
		t.Fatalf("create omitted inspection URL: %s", out)
	}
	if !strings.Contains(out, "participant_token") {
		t.Fatalf("create must return connection information: %s", out)
	}

	code, _, stderr = run(t, rootB, "join", invite, "--name", "bob")
	if code != 0 {
		t.Fatalf("join: %s", stderr)
	}
	code, _, stderr = run(t, rootA, "send", "team", "hello")
	if code != 0 {
		t.Fatalf("send: %s", stderr)
	}
	aliceCredential, err := (localconfig.Store{Root: rootA}).LoadRoom("team")
	if err != nil || aliceCredential.Cursor != 1 {
		t.Fatalf("sender cursor=%d err=%v", aliceCredential.Cursor, err)
	}
	code, out, stderr = run(t, rootB, "--json", "read", "team")
	if code != 0 || !strings.Contains(out, `"body":"hello"`) {
		t.Fatalf("read out=%s err=%s", out, stderr)
	}
	code, _, stderr = run(t, rootA, "send", "team", "again")
	if code != 0 {
		t.Fatalf("second send: %s", stderr)
	}
	code, out, stderr = run(t, rootB, "--json", "wait", "--timeout", "1s", "team")
	if code != 0 || !strings.Contains(out, `"status":"message"`) || !strings.Contains(out, `"body":"again"`) {
		t.Fatalf("wait out=%s err=%s", out, stderr)
	}
	credential, err := (localconfig.Store{Root: rootB}).LoadRoom("team")
	if err != nil || credential.Cursor != 2 {
		t.Fatalf("credential=%+v err=%v", credential, err)
	}
	code, out, stderr = run(t, rootA, "--json", "status", "team")
	if code != 0 || strings.Contains(out+stderr, credential.Token) || strings.Contains(out, "participant_token") {
		t.Fatalf("unsafe status out=%s err=%s", out, stderr)
	}
	code, firstDone, stderr := run(t, rootA, "--json", "done", "--message-id", "stable-done", "team")
	if code != 0 {
		t.Fatalf("done: %s", stderr)
	}
	code, retriedDone, stderr := run(t, rootA, "--json", "done", "--message-id", "stable-done", "team")
	if code != 0 || firstDone != retriedDone || !strings.Contains(firstDone, `"id":"stable-done"`) {
		t.Fatalf("idempotent done: code=%d first=%s retry=%s err=%s", code, firstDone, retriedDone, stderr)
	}
}

func TestJoinAcceptsFlagsBeforeAndAfterExactlyOneInvite(t *testing.T) {
	for _, args := range [][]string{
		{"join", "--name", "bob", "INVITE"},
		{"join", "INVITE", "--name", "bob"},
	} {
		t.Run(strings.Join(args[1:], "_"), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/invites/token/claim" {
					t.Fatalf("path = %q", r.URL.Path)
				}
				fmt.Fprint(w, `{"room":{"id":"r1","name":"room"},"participant":{"id":"p1"},"participant_token":"secret"}`)
			}))
			defer server.Close()
			args[len(args)-1] = strings.ReplaceAll(args[len(args)-1], "INVITE", server.URL+"/join/token")
			if args[1] == "INVITE" {
				args[1] = server.URL + "/join/token"
			}
			if code, _, stderr := run(t, t.TempDir(), args...); code != 0 {
				t.Fatalf("args=%v err=%s", args, stderr)
			}
		})
	}
	if code, _, _ := run(t, t.TempDir(), "join", "https://relay.test/join/one", "https://relay.test/join/two"); code == 0 {
		t.Fatal("join accepted two invites")
	}
}

func TestWaitExplicitOlderAfterDoesNotRegressPersistedCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("after"); got != "2" {
			t.Fatalf("after = %q", got)
		}
		fmt.Fprint(w, `{"status":"message","message":{"id":"m3","sequence":3,"sender_name":"alice","body":"old"}}`)
	}))
	defer server.Close()
	root := t.TempDir()
	credential := modelCredential("room", "room", server.URL, "token")
	credential.Cursor = 10
	if err := (localconfig.Store{Root: root}).SaveRoom(credential); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run(t, root, "wait", "--after", "2", "room"); code != 0 {
		t.Fatal(stderr)
	}
	persisted, err := (localconfig.Store{Root: root}).LoadRoom("room")
	if err != nil || persisted.Cursor != 10 {
		t.Fatalf("cursor=%d err=%v", persisted.Cursor, err)
	}
}

func TestServerStartsAndStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, stderr bytes.Buffer
	code := Run(ctx, []string{"--json", "server", "--listen", "127.0.0.1:0", "--data", filepath.Join(t.TempDir(), "server.db")}, strings.NewReader(""), &out, &stderr, Dependencies{})
	if code != 0 || !strings.Contains(out.String(), `"status":"listening"`) {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
}

func TestMCPRejectsArgumentsWithoutWritingProtocolOutput(t *testing.T) {
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"mcp", "extra"}, strings.NewReader(""), &out, &stderr, Dependencies{})
	if code == 0 || out.Len() != 0 || !strings.Contains(stderr.String(), `unknown command "extra" for "agentline mcp"`) {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), stderr.String())
	}
}

func TestLocalStopOutputAndInvalidSubcommand(t *testing.T) {
	root := t.TempDir()
	code, out, stderr := run(t, root, "local", "stop")
	if code != 0 || out != "Local relay stopped" || stderr != "" {
		t.Fatalf("human: code=%d out=%q err=%q", code, out, stderr)
	}
	code, out, stderr = run(t, root, "--json", "local", "stop")
	if code != 0 || out != `{"status":"stopped"}` || stderr != "" {
		t.Fatalf("json: code=%d out=%q err=%q", code, out, stderr)
	}
	code, out, stderr = run(t, root, "local", "stop", "--json")
	if code != 0 || out != `{"status":"stopped"}` || stderr != "" {
		t.Fatalf("json anywhere: code=%d out=%q err=%q", code, out, stderr)
	}
	for _, args := range [][]string{{"local", "start"}, {"local", "stop", "extra"}} {
		if code, _, stderr := run(t, root, args...); code == 0 || !strings.Contains(stderr, "unknown command") {
			t.Fatalf("args=%v code=%d err=%q", args, code, stderr)
		}
	}
	if code, out, _ := run(t, root, "local"); code != 0 || !strings.Contains(out, "Usage:") {
		t.Fatalf("bare local: code=%d out=%q", code, out)
	}
}

func TestOrdinaryErrorsDoNotPrintCredential(t *testing.T) {
	root := t.TempDir()
	secret := "never-print-this"
	(localconfig.Store{Root: root}).SaveRoom(modelCredential("room", "name", "http://127.0.0.1:1", secret))
	code, out, stderr := run(t, root, "--json", "status", "name")
	if code == 0 || strings.Contains(out+stderr, secret) {
		t.Fatalf("code=%d out=%s err=%s", code, out, stderr)
	}
}

func TestJSONFlagErrorIsOneJSONValue(t *testing.T) {
	code, out, stderr := run(t, t.TempDir(), "--json", "read", "room", "--after", "nope")
	if code == 0 || out != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, stderr)
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(stderr))
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("not JSON: %q: %v", stderr, err)
	}
	if decoder.Decode(&value) == nil {
		t.Fatalf("more than one JSON value: %q", stderr)
	}
	if strings.Contains(stderr, "Usage of") {
		t.Fatalf("flag diagnostic leaked: %q", stderr)
	}
}

func TestInterspersedFlagsAndArgumentValidation(t *testing.T) {
	root := t.TempDir()
	(localconfig.Store{Root: root}).SaveRoom(modelCredential("room", "room", "http://127.0.0.1:1", "token"))
	for _, args := range [][]string{
		{"read", "room", "--after", "12"},
		{"wait", "room", "--timeout", "1ms"},
		{"send", "room", "hello", "--reply-to", "previous"},
	} {
		code, _, stderr := run(t, root, args...)
		if code == 0 || strings.Contains(stderr, "invalid arguments") || strings.Contains(stderr, "flag provided but not defined") {
			t.Fatalf("args=%v code=%d err=%q", args, code, stderr)
		}
	}
	for _, args := range [][]string{
		{"create", "extra"},
		{"create", "--local", "--server", "https://relay.test"},
		{"server", "extra"},
	} {
		if code, _, _ := run(t, t.TempDir(), args...); code == 0 {
			t.Fatalf("args=%v succeeded", args)
		}
	}
}

func TestCreateAcceptsDaysAndRejectsInvalidTTLBeforeNetwork(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["ttl_seconds"] != float64(7*24*60*60) {
			t.Fatalf("ttl_seconds = %v", body["ttl_seconds"])
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"room":{"id":"r1","name":"room"},"participant":{"id":"p1"},"participant_token":"secret","invite_url":"/join/token"}`)
	}))
	defer server.Close()
	if code, _, stderr := run(t, t.TempDir(), "create", "--server", server.URL, "--ttl", "7d"); code != 0 {
		t.Fatalf("exact 7d rejected: %s", stderr)
	}
	for _, ttl := range []string{"0d", "-1d", "8d", "1dd"} {
		code, _, stderr := run(t, t.TempDir(), "create", "--server", "https://relay.test", "--ttl", ttl)
		if code == 0 || !strings.Contains(stderr, "ttl") {
			t.Fatalf("ttl=%q code=%d err=%q", ttl, code, stderr)
		}
	}
}

func TestOmittedRoomRejectsAmbiguousCredentials(t *testing.T) {
	root := t.TempDir()
	config := localconfig.Store{Root: root}
	config.SaveRoom(modelCredential("one", "one", "http://127.0.0.1:1", "token"))
	config.SaveRoom(modelCredential("two", "two", "http://127.0.0.1:1", "token"))
	if code, _, stderr := run(t, root, "status"); code == 0 || !strings.Contains(stderr, "ambiguous") {
		t.Fatalf("code=%d err=%q", code, stderr)
	}
}

func TestRejectsMalformedOriginsAndInvites(t *testing.T) {
	for _, server := range []string{"ftp://relay.test", "https://relay.test/path", "https://user@relay.test"} {
		code, _, _ := run(t, t.TempDir(), "create", "--server", server)
		if code == 0 {
			t.Fatalf("server %q succeeded", server)
		}
	}
	for _, invite := range []string{"ftp://relay.test/join/x", "https://relay.test/join/x?q=1", "https://relay.test/nope/x"} {
		code, _, _ := run(t, t.TempDir(), "join", invite)
		if code == 0 {
			t.Fatalf("invite %q succeeded", invite)
		}
	}
}

func TestSetupPreviewConfirmationRefusalAndJSONNonInteractive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	code, out, stderr := run(t, t.TempDir(), "setup", "claude")
	if code == 0 || !strings.Contains(out, ".claude") || !strings.Contains(stderr, "not confirmed") {
		t.Fatalf("refusal code=%d out=%q err=%q", code, out, stderr)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf("setup mutated after refusal: %v", err)
	}
	code, out, stderr = run(t, t.TempDir(), "--json", "setup", "claude")
	if code != 0 || stderr != "" || !strings.Contains(out, `"applied":false`) {
		t.Fatalf("JSON preview code=%d out=%q err=%q", code, out, stderr)
	}
	code, out, stderr = run(t, t.TempDir(), "--json", "setup", "--yes", "claude")
	if code != 0 || stderr != "" || !strings.Contains(out, `"applied":true`) {
		t.Fatalf("JSON apply code=%d out=%q err=%q", code, out, stderr)
	}
}

func TestSetupSupportsInterspersedRemoveFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if code, _, stderr := run(t, t.TempDir(), "setup", "pi", "--yes"); code != 0 {
		t.Fatal(stderr)
	}
	path := filepath.Join(home, ".agents/skills/agentline/SKILL.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run(t, t.TempDir(), "setup", "--remove", "pi", "--yes"); code != 0 {
		t.Fatal(stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("remove failed: %v", err)
	}
}

func TestRootHelpShowsCommandsAndBareInvocationListsUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"--help"}, {"-h"}, {"help"}} {
		code, out, stderr := run(t, t.TempDir(), args...)
		if code != 0 || stderr != "" {
			t.Fatalf("args=%v code=%d err=%q", args, code, stderr)
		}
		for _, command := range []string{"create", "join", "send", "wait", "done", "status", "list", "mcp", "server", "local", "setup", "doctor"} {
			if !strings.Contains(out, command) {
				t.Fatalf("args=%v help missing %q: %q", args, command, out)
			}
		}
	}
}

func TestPerCommandHelpFlagAndHelpTopicAreEquivalent(t *testing.T) {
	for _, name := range []string{"create", "join", "send", "read", "wait", "done", "status", "list", "mcp", "server", "local", "setup", "doctor"} {
		code, flagOut, flagErr := run(t, t.TempDir(), name, "--help")
		if code != 0 || flagErr != "" || !strings.Contains(flagOut, "Usage:") {
			t.Fatalf("%s --help: code=%d out=%q err=%q", name, code, flagOut, flagErr)
		}
		code, topicOut, topicErr := run(t, t.TempDir(), "help", name)
		if code != 0 || topicErr != "" || topicOut != flagOut {
			t.Fatalf("help %s != %s --help: out=%q flag=%q", name, name, topicOut, flagOut)
		}
	}
}

func TestVersionOutput(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"-v"}} {
		code, out, stderr := run(t, t.TempDir(), args...)
		if code != 0 || stderr != "" || !strings.HasPrefix(out, "agentline ") {
			t.Fatalf("args=%v code=%d out=%q err=%q", args, code, out, stderr)
		}
	}
}

func TestJSONFlagIsGlobalAndListNeverPrintsTokens(t *testing.T) {
	root := t.TempDir()
	config := localconfig.Store{Root: root}
	config.SaveRoom(modelCredential("one", "team", "http://127.0.0.1:1", "secret-one"))
	config.SaveRoom(modelCredential("two", "other", "http://127.0.0.1:1", "secret-two"))

	code, out, stderr := run(t, root, "list")
	if code != 0 || stderr != "" || !strings.Contains(out, "team") || !strings.Contains(out, "other") || !strings.Contains(out, "http://127.0.0.1:1") {
		t.Fatalf("list human: code=%d out=%q err=%q", code, out, stderr)
	}
	if strings.Contains(out, "secret") {
		t.Fatalf("list leaked tokens: %q", out)
	}

	code, out, stderr = run(t, root, "list", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("list json: code=%d err=%q", code, stderr)
	}
	var rooms []map[string]any
	if err := json.Unmarshal([]byte(out), &rooms); err != nil || len(rooms) != 2 {
		t.Fatalf("list json rooms=%v err=%v", rooms, err)
	}
	if strings.Contains(out, "secret") || strings.Contains(out, "token") {
		t.Fatalf("list json leaked credentials: %q", out)
	}

	if code, out, stderr := run(t, t.TempDir(), "--json", "list"); code != 0 || out != "[]" || stderr != "" {
		t.Fatalf("empty list: code=%d out=%q err=%q", code, out, stderr)
	}
}

func TestUsageErrorsSuggestHelp(t *testing.T) {
	code, out, stderr := run(t, t.TempDir(), "frobnicate")
	if code == 0 || out != "" || !strings.Contains(stderr, "unknown command \"frobnicate\"") || !strings.Contains(stderr, "--help") {
		t.Fatalf("unknown: code=%d out=%q err=%q", code, out, stderr)
	}
	code, _, stderr = run(t, t.TempDir(), "read", "--after", "nope")
	if code == 0 || !strings.Contains(stderr, "see 'agentline read --help'") {
		t.Fatalf("flag error: code=%d err=%q", code, stderr)
	}
	code, _, stderr = run(t, t.TempDir(), "send", "--bogus", "hi")
	if code == 0 || !strings.Contains(stderr, "unknown flag") {
		t.Fatalf("unknown flag: code=%d err=%q", code, stderr)
	}
}

func run(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, Dependencies{Config: localconfig.Store{Root: root}})
	return code, strings.TrimSpace(out.String()), strings.TrimSpace(stderr.String())
}

func modelCredential(id, name, server, token string) model.RoomCredential {
	return model.RoomCredential{RoomID: id, RoomName: name, ServerURL: server, Token: token}
}
