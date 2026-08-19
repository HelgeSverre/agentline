package setup

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HelgeSverre/agentline/internal/client"
	"github.com/HelgeSverre/agentline/internal/model"
)

// TestPluginsAreValidTypeScript parses each rendered plugin with Node's type
// stripper. It catches syntax breakage and unsubstituted templates, which is
// otherwise invisible: nothing in the Go build compiles these files.
func TestPluginsAreValidTypeScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping plugin syntax check")
	}
	dir := t.TempDir()
	for _, target := range []string{"amp", "pi", "opencode"} {
		t.Run(target, func(t *testing.T) {
			adapter, ok := nativeAdapterFor(target)
			if !ok || adapter.source == "" {
				t.Fatalf("%s has no plugin source", target)
			}
			path := filepath.Join(dir, target+".ts")
			if err := os.WriteFile(path, []byte(renderPlugin(adapter.source, "/usr/local/bin/agentline")), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(node, "--experimental-strip-types", "--check", path).CombinedOutput()
			if err != nil {
				t.Fatalf("%s plugin does not parse: %v\n%s", target, err, out)
			}
		})
	}
}

// TestPluginsBindThroughDocumentedHostAPIs guards the defect that made the Pi
// and OpenCode plugins inert: each read its room from a config path the host
// never populates, so the listener could not start and nothing failed loudly.
// Every binding must come from an API the host actually documents.
func TestPluginsBindThroughDocumentedHostAPIs(t *testing.T) {
	required := map[string][]string{
		// Pi's ExtensionContext has no config field; a registered CLI flag is
		// how an extension reads its own settings.
		"pi": {`pi.registerFlag("agentline-room"`, `pi.getFlag("agentline-room")`, `ctx.isIdle()`},
		// OpenCode's Session carries no user fields and local plugins get no
		// options, so binding goes through a tool whose context has sessionID.
		"opencode": {`agentline_bind_room: tool(`, `context.sessionID`},
		// Amp's configuration store is async, so binding goes through the tool
		// whose context carries the PluginThread to append to.
		"amp": {`agentline_bind_room`, `ctx.thread.id`, `amp.threads.get(threadID)`},
	}
	forbidden := map[string][]string{
		"pi":       {`ctx?.config?.agentline`, `pi.isStreaming`},
		"opencode": {`info?.agentline`},
		// Property access on the async configuration store always yields
		// undefined, and the thread must come from the tool context rather than
		// from an argument the model would have to guess.
		"amp": {`amp.configuration?.agentline`, `required: ["room", "thread"]`},
	}
	for target, fragments := range required {
		adapter, _ := nativeAdapterFor(target)
		for _, fragment := range fragments {
			if !strings.Contains(adapter.source, fragment) {
				t.Errorf("%s plugin lost its documented binding %q", target, fragment)
			}
		}
		for _, fragment := range forbidden[target] {
			if strings.Contains(adapter.source, fragment) {
				t.Errorf("%s plugin uses %q, which the host never populates", target, fragment)
			}
		}
	}
}

// TestPluginCLIContract pins the exact `agentline --json wait` fields every
// plugin reads. The plugins are not compiled by the Go build, so a change to
// this output shape would otherwise break them silently.
func TestPluginCLIContract(t *testing.T) {
	for _, target := range []string{"amp", "pi", "opencode"} {
		adapter, _ := nativeAdapterFor(target)
		source := adapter.source
		for _, fragment := range []string{
			`"--json"`,           // every invocation asks for machine-readable output
			`"wait", room`,       // positional room handle
			`"--timeout", "60s"`, // bounded waits only
			`result.status === "message"`,
			`result.status === "done"`,
			`message?.id`,
			`message.body`,
		} {
			if !strings.Contains(source, fragment) {
				t.Errorf("%s plugin no longer contains %s", target, fragment)
			}
		}
	}

	// The fields above must exist in the CLI's own wait payload.
	var payload map[string]json.RawMessage
	encoded, err := json.Marshal(client.WaitResult{Status: "message", Message: &model.Message{ID: "m1", Body: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"status", "message"} {
		if _, ok := payload[field]; !ok {
			t.Fatalf("agentline --json wait no longer emits %q: %s", field, encoded)
		}
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(payload["message"], &message); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "body"} {
		if _, ok := message[field]; !ok {
			t.Fatalf("agentline --json wait message no longer emits %q: %s", field, payload["message"])
		}
	}
}
