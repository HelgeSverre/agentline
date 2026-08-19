package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HelgeSverre/agentline/integrations"
)

func TestNativeAdapterInstallIsIdempotentAndRemovable(t *testing.T) {
	for _, target := range []string{"claude", "amp", "pi", "opencode"} {
		t.Run(target, func(t *testing.T) {
			home := t.TempDir()
			exe := filepath.Join(home, "bin", "agentline")
			spec, ok := nativeArtifact(target, home)
			if !ok {
				t.Fatalf("%s has no native adapter", target)
			}

			plan, err := BuildPlan(target, home, exe, Options{Native: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Warnings) == 0 {
				t.Fatal("installing an experimental adapter must warn")
			}
			if err := Apply(plan); err != nil {
				t.Fatal(err)
			}
			installed, err := ownedUnit(spec, nil)
			if err != nil || len(installed) == 0 {
				t.Fatalf("adapter unit is empty after install: %v", err)
			}
			if !strings.Contains(string(installed), exe) {
				t.Fatalf("adapter does not reference the executable: %s", installed)
			}
			if strings.Contains(string(installed), integrations.ExecutablePlaceholder) {
				t.Fatalf("adapter still contains the unsubstituted placeholder: %s", installed)
			}

			again, err := BuildPlan(target, home, exe, Options{Native: true})
			if err != nil || len(again.Changes) != 0 {
				t.Fatalf("second native plan is not empty: %+v %v", again.Changes, err)
			}

			// A routine re-run must leave the installed adapter alone.
			routine, err := BuildPlan(target, home, exe, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(routine); err != nil {
				t.Fatal(err)
			}
			if unit, err := ownedUnit(spec, nil); err != nil || len(unit) == 0 {
				t.Fatalf("a routine setup run uninstalled the adapter: %v", err)
			}

			remove, err := BuildPlan(target, home, exe, Options{Remove: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(remove); err != nil {
				t.Fatal(err)
			}
			if unit, err := ownedUnit(spec, nil); err != nil || len(unit) != 0 {
				t.Fatalf("adapter survived removal: %s %v", unit, err)
			}
		})
	}
}

// The Channel registers a second MCP server beside the portable one; neither
// entry may displace the other.
func TestClaudeChannelCoexistsWithPortableMCPEntry(t *testing.T) {
	home := t.TempDir()
	exe := filepath.Join(home, "bin", "agentline")
	plan, err := BuildPlan("claude", home, exe, Options{Native: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	portable, channel := config.MCPServers["agentline"], config.MCPServers[ChannelServerName]
	if portable.Command != exe || len(portable.Args) != 1 || portable.Args[0] != "mcp" {
		t.Fatalf("portable entry = %+v", portable)
	}
	if channel.Command != exe || len(channel.Args) != 1 || channel.Args[0] != "channel" {
		t.Fatalf("channel entry = %+v", channel)
	}

	// Removing only the channel must keep the portable registration.
	after, err := editChannelJSON(data, exe, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), `"agentline"`) || strings.Contains(string(after), ChannelServerName) {
		t.Fatalf("channel removal damaged the portable entry: %s", after)
	}
}

func TestNativeAdapterRejectedForTargetsWithoutOne(t *testing.T) {
	for _, target := range []string{"codex", "mcp"} {
		if _, err := BuildPlan(target, t.TempDir(), "/opt/agentline", Options{Native: true}); err == nil {
			t.Fatalf("%s accepted --native", target)
		}
	}
}

func TestDoctorReportsAnInstalledNativeAdapter(t *testing.T) {
	home := t.TempDir()
	exe := filepath.Join(home, "agentline")
	if err := os.WriteFile(exe, []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	var checks []Check
	add := func(name, status, message string) { checks = append(checks, Check{name, status, message}) }

	addNativeCheck(add, "pi", home, exe)
	if len(checks) != 0 {
		t.Fatalf("an uninstalled adapter must not be reported: %+v", checks)
	}

	plan, err := BuildPlan("pi", home, exe, Options{Native: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	addNativeCheck(add, "pi", home, exe)
	if len(checks) != 1 || checks[0].Status != "pass" {
		t.Fatalf("checks = %+v", checks)
	}

	// An adapter left behind by an older binary is reported as outdated.
	spec, _ := nativeArtifact("pi", home)
	if err := os.WriteFile(spec.path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	checks = nil
	addNativeCheck(add, "pi", home, exe)
	if len(checks) != 1 || checks[0].Status != "warn" {
		t.Fatalf("checks = %+v", checks)
	}
}
