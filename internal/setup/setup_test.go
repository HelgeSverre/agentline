package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestBuildPlanAllTargetsUsesAbsoluteExecutableAndIsIdempotent(t *testing.T) {
	for _, target := range []string{"claude", "codex", "amp", "pi", "opencode", "mcp"} {
		t.Run(target, func(t *testing.T) {
			home := t.TempDir()
			executable := filepath.Join(home, "bin", "agentline")
			plan, err := BuildPlan(target, home, executable, false)
			if err != nil || len(plan.Changes) == 0 {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
			for _, change := range plan.Changes {
				if strings.Contains(string(change.After), `"agentline"`) && target != "pi" && !strings.Contains(string(change.After), executable) {
					t.Fatalf("config does not contain absolute executable: %s", change.After)
				}
			}
			if err := Apply(plan); err != nil {
				t.Fatal(err)
			}
			again, err := BuildPlan(target, home, executable, false)
			if err != nil || len(again.Changes) != 0 {
				t.Fatalf("second plan=%+v err=%v", again, err)
			}
		})
	}
}

func TestJSONTargetsPreserveUnrelatedSettingsBackupAndRemoveOwnership(t *testing.T) {
	for _, target := range []string{"claude", "opencode", "mcp"} {
		t.Run(target, func(t *testing.T) {
			home := t.TempDir()
			path := configPath(target, home)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			original := []byte(`{"theme":"dark","mcpServers":{"other":{"command":"other"}},"mcp":{"other":{"enabled":true}}}`)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			plan, err := BuildPlan(target, home, "/usr/local/bin/agentline", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(plan); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(path)
			if !strings.Contains(string(data), `"theme": "dark"`) || !strings.Contains(string(data), `"other"`) {
				t.Fatalf("unrelated lost: %s", data)
			}
			if _, err := os.Stat(path + ".agentline.bak"); err != nil {
				t.Fatalf("backup: %v", err)
			}
			remove, err := BuildPlan(target, home, "/usr/local/bin/agentline", true)
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(remove); err != nil {
				t.Fatal(err)
			}
			data, _ = os.ReadFile(path)
			if strings.Contains(string(data), `"agentline"`) || !strings.Contains(string(data), `"other"`) {
				t.Fatalf("bad removal: %s", data)
			}
		})
	}
}

func TestCodexRefusesConflictingOwnedBlockAndPreservesTOML(t *testing.T) {
	home := t.TempDir()
	path := configPath("codex", home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("model = \"gpt\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan("codex", home, "/opt/agentline", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "model = \"gpt\"") || !strings.Contains(string(data), codexBegin) {
		t.Fatalf("bad TOML: %s", data)
	}
	if err := os.WriteFile(path, []byte(codexBegin+"\nbroken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPlan("codex", home, "/opt/agentline", false); err == nil {
		t.Fatal("accepted unterminated owned block")
	}
}

func TestAmpPackagesExactToolsAndRemovalOnlyRemovesSkill(t *testing.T) {
	home := t.TempDir()
	plan, err := BuildPlan("amp", home, "/opt/agentline", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 3 || !strings.HasSuffix(plan.Changes[0].Path, "SKILL.md") || !strings.HasSuffix(plan.Changes[1].Path, "mcp.json") || !strings.HasSuffix(plan.Changes[2].Path, "setup-ownership.json") {
		t.Fatalf("unexpected Amp plan: %+v", plan.Changes)
	}
	text := string(plan.Changes[1].After)
	for _, tool := range MCPTools {
		if !strings.Contains(text, tool) {
			t.Fatalf("missing %s", tool)
		}
	}
	var config map[string]struct {
		IncludeTools []string `json:"includeTools"`
	}
	if json.Unmarshal(plan.Changes[1].After, &config) != nil || len(config["agentline"].IncludeTools) != len(MCPTools) {
		t.Fatalf("unexpected includeTools: %s", text)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	remove, _ := BuildPlan("amp", home, "/opt/agentline", true)
	if err := Apply(remove); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/agents/skills/agentline/SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("skill remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/agents/skills/agentline/mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("mcp remains: %v", err)
	}
}

func TestSkillSourceIsCanonical(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "integrations", "skills", "agentline", "SKILL.md"))
	if err != nil || !bytes.Equal(data, []byte(sharedSkill)) {
		t.Fatalf("canonical skill differs: %v", err)
	}
}

func TestBuildPlanRefusesUnownedArtifacts(t *testing.T) {
	for _, target := range []string{"claude", "opencode"} {
		t.Run(target, func(t *testing.T) {
			home := t.TempDir()
			_, err := BuildPlan(target, home, "/opt/agentline", false)
			if err != nil {
				t.Fatal(err)
			}
			path := ""
			for _, spec := range targetArtifacts(target, home) {
				if spec.unit != "file" {
					path = spec.path
					break
				}
			}
			if path == "" {
				t.Fatal("missing structured artifact")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("mine"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := BuildPlan(target, home, "/opt/agentline", false); err == nil {
				t.Fatal("accepted unowned artifact")
			}
		})
	}
}

func TestManifestPermitsExecutableRelocationAndRejectsOwnedUnitMutation(t *testing.T) {
	for _, target := range []string{"claude", "codex", "amp"} {
		t.Run(target, func(t *testing.T) {
			home := t.TempDir()
			first, err := BuildPlan(target, home, "/old/agentline", false)
			if err != nil || Apply(first) != nil {
				t.Fatalf("install: %v", err)
			}
			manifest := filepath.Join(configRoot(home), "agentline", "setup-ownership.json")
			if info, err := os.Stat(manifest); err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("manifest mode: %v %v", info, err)
			}
			update, err := BuildPlan(target, home, "/new/agentline", false)
			if err != nil || Apply(update) != nil {
				t.Fatalf("relocate: %v", err)
			}
			spec := targetArtifacts(target, home)[len(targetArtifacts(target, home))-1]
			data, _ := os.ReadFile(spec.path)
			if spec.unit == "file" {
				data = append(data, 'x')
				_ = os.WriteFile(spec.path, data, 0o600)
			} else if spec.unit == "codex" {
				_ = os.WriteFile(spec.path, bytes.Replace(data, []byte("/new/agentline"), []byte("/tampered"), 1), 0o600)
			} else {
				_ = os.WriteFile(spec.path, bytes.Replace(data, []byte("/new/agentline"), []byte("/tampered"), 1), 0o600)
			}
			if _, err := BuildPlan(target, home, "/third/agentline", false); err == nil {
				t.Fatal("accepted mutated owned unit")
			}
		})
	}
}

func TestBuildPlanAdoptsUnmanifestedSkill(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".agents/skills/agentline/SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(sharedSkill), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan("pi", home, "/opt/agentline", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(configRoot(home), "agentline", "setup-ownership.json")); err != nil {
		t.Fatalf("ownership manifest: %v", err)
	}
}

func TestConcurrentApplyRefusesStalePlanWithoutLosingSharedSkill(t *testing.T) {
	home := t.TempDir()
	a, _ := BuildPlan("codex", home, "/opt/agentline", false)
	b, _ := BuildPlan("pi", home, "/opt/agentline", false)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, p := range []Plan{a, b} {
		wg.Add(1)
		go func(p Plan) { defer wg.Done(); <-start; errs <- Apply(p) }(p)
	}
	close(start)
	wg.Wait()
	close(errs)
	success, refused := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if strings.Contains(err.Error(), "changed since preview") {
			refused++
		}
	}
	if success != 1 || refused != 1 {
		t.Fatalf("success=%d refused=%d", success, refused)
	}
	if data, err := os.ReadFile(filepath.Join(home, ".agents/skills/agentline/SKILL.md")); err != nil || !bytes.Equal(data, []byte(sharedSkill)) {
		t.Fatalf("shared skill lost: %v", err)
	}
}

func TestApplyFailureRestoresPriorRecoveryBackup(t *testing.T) {
	home := t.TempDir()
	first, _ := BuildPlan("claude", home, "/old/agentline", false)
	if err := Apply(first); err != nil {
		t.Fatal(err)
	}
	path := configPath("claude", home)
	backup := path + ".agentline.bak"
	priorBackup := []byte("prior recovery")
	if err := os.WriteFile(backup, priorBackup, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	update, err := BuildPlan("claude", home, "/new/agentline", false)
	if err != nil {
		t.Fatal(err)
	}
	originalReplace := replace
	failed := false
	replace = func(from, to string) error {
		if to == path && !failed {
			failed = true
			return errors.New("injected target commit failure")
		}
		return originalReplace(from, to)
	}
	t.Cleanup(func() { replace = originalReplace })
	if err := Apply(update); err == nil {
		t.Fatal("apply succeeded despite injected failure")
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, before) {
		t.Fatalf("target changed: %s", got)
	}
	if got, _ := os.ReadFile(backup); !bytes.Equal(got, priorBackup) {
		t.Fatalf("backup changed: %s", got)
	}
}

func TestCodexRejectsUnmarkedAndMalformedRegionsAndReplacesInPlace(t *testing.T) {
	if _, err := editCodex([]byte("[mcp_servers.agentline]\ncommand='mine'\n"), "/opt/agentline", false); err == nil {
		t.Fatal("accepted unowned table")
	}
	for _, malformed := range []string{codexEnd + "\n" + codexBegin, codexBegin + " x\n" + codexEnd, codexBegin + "\n" + codexBegin + "\n" + codexEnd} {
		if _, err := editCodex([]byte(malformed), "/opt/agentline", false); err == nil {
			t.Fatalf("accepted %q", malformed)
		}
	}
	duplicate := codexBegin + "\n[mcp_servers.agentline]\ncommand=\"old\"\n" + codexEnd + "\n[mcp_servers.agentline]\n"
	if _, err := editCodex([]byte(duplicate), "/opt/agentline", false); err == nil {
		t.Fatal("accepted marked and unmarked duplicate table")
	}
	original := "before\n" + codexBegin + "\n[mcp_servers.agentline]\ncommand=\"old\"\n" + codexEnd + "\nafter\n"
	got, err := editCodex([]byte(original), "/opt/agentline", false)
	if err != nil || !strings.HasPrefix(string(got), "before\n"+codexBegin) || !strings.HasSuffix(string(got), "after\n") {
		t.Fatalf("not replaced in place: %q %v", got, err)
	}
}

func TestApplyUsesPrivatePermissions(t *testing.T) {
	home := t.TempDir()
	plan, _ := BuildPlan("claude", home, "/opt/agentline", false)
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	for _, change := range plan.Changes {
		info, err := os.Stat(change.Path)
		want := newFileMode(change.Path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s mode=%v err=%v", change.Path, info.Mode().Perm(), err)
		}
	}
}

func TestDoctorReportsBinaryRelayCredentialsSkillAndRegistration(t *testing.T) {
	home := t.TempDir()
	exe := filepath.Join(home, "agentline")
	if err := os.WriteFile(exe, []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan, _ := BuildPlan("claude", home, exe, false)
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	rooms := filepath.Join(configRoot(home), "agentline", "rooms")
	os.MkdirAll(rooms, 0o700)
	os.WriteFile(filepath.Join(rooms, "room.json"), []byte(`{}`), 0o600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()
	report := Doctor(context.Background(), "claude", home, exe, server.URL)
	if report.Status != "pass" || len(report.Checks) < 5 {
		t.Fatalf("%+v", report)
	}
}

func configPath(target, home string) string {
	switch target {
	case "claude":
		return filepath.Join(home, ".claude.json")
	case "codex":
		return filepath.Join(home, ".codex/config.toml")
	case "opencode":
		return filepath.Join(home, ".config/opencode/opencode.json")
	case "mcp":
		return filepath.Join(home, ".config/agentline/mcp.json")
	default:
		return ""
	}
}
