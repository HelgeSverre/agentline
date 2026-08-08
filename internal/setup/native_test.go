package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeAssetsAreCanonicalCompleteAndSafelyEmbedExecutable(t *testing.T) {
	exe := `/Applications/Agent Line "quoted"/agentline`
	for _, tc := range []struct {
		target, suffix string
		required       []string
		forbidden      []string
	}{
		{"pi", ".pi/agent/extensions/agentline/index.ts", []string{"session_start", "session_shutdown", "sendUserMessage", `deliverAs: "followUp"`, "AbortController", "message_id", "spawn(AGENTLINE", "60000", "seen"}, nil},
		{"amp", ".config/amp/plugins/agentline.ts", []string{"registerTool", "configuration", "onDispose", "appendUserMessage", "steer: true", "thread", "room", "AbortController", "message_id", "spawn(AGENTLINE", "60000", "seen"}, []string{"activeThread", "focusedThread"}},
		{"opencode", ".config/opencode/plugins/agentline.ts", []string{"event", "session", "room", "experimental", "prompt", "AbortController", "message_id", "spawn(AGENTLINE", "60000", "seen"}, nil},
	} {
		t.Run(tc.target, func(t *testing.T) {
			home := t.TempDir()
			plan, err := BuildPlan(tc.target, home, exe, false)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, filepath.FromSlash(tc.suffix))
			var asset []byte
			for _, c := range plan.Changes {
				if c.Path == path {
					asset = c.After
				}
			}
			if len(asset) == 0 {
				t.Fatalf("missing native asset at %s", path)
			}
			for _, required := range tc.required {
				if !bytes.Contains(asset, []byte(required)) {
					t.Errorf("asset missing %q", required)
				}
			}
			for _, forbidden := range tc.forbidden {
				if bytes.Contains(asset, []byte(forbidden)) {
					t.Errorf("asset contains forbidden routing %q", forbidden)
				}
			}
			if !strings.Contains(string(asset), `const AGENTLINE = "/Applications/Agent Line \"quoted\"/agentline"`) {
				t.Fatalf("executable is not safely quoted: %s", asset)
			}
			canonical, err := os.ReadFile(filepath.Join("..", "..", "integrations", tc.target, "agentline.ts"))
			if err != nil || !bytes.Equal(asset, renderNativeAsset(canonical, exe)) {
				t.Fatalf("installed bytes differ from canonical rendering: %v", err)
			}
			if err := Apply(plan); err != nil {
				t.Fatal(err)
			}
			remove, err := BuildPlan(tc.target, home, exe, true)
			if err != nil || Apply(remove) != nil {
				t.Fatalf("remove: %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("native asset remains: %v", err)
			}
		})
	}
}

func TestNativeExperimentalWarningsDoNotMislabelPortableMCP(t *testing.T) {
	for _, target := range []string{"amp", "opencode"} {
		plan, err := BuildPlan(target, t.TempDir(), "/opt/agentline", false)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.ToLower(strings.Join(plan.Warnings, " "))
		if !strings.Contains(joined, "native idle wake") || !strings.Contains(joined, "experimental") || strings.Contains(joined, "mcp is experimental") {
			t.Fatalf("%s warnings: %v", target, plan.Warnings)
		}
	}
	if plan, _ := BuildPlan("pi", t.TempDir(), "/opt/agentline", false); strings.Contains(strings.ToLower(strings.Join(plan.Warnings, " ")), "experimental") {
		t.Fatalf("Pi native wake incorrectly experimental: %v", plan.Warnings)
	}
}
