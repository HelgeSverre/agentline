package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/HelgeSverre/agentline/internal/localconfig"
	"github.com/gofrs/flock"
)

const (
	codexBegin = "# BEGIN AGENTLINE (managed by agentline setup)"
	codexEnd   = "# END AGENTLINE"
)

type Change struct {
	Path, Description string
	Before, After     []byte
}
type Plan struct {
	Target   string   `json:"target"`
	Changes  []Change `json:"changes"`
	Warnings []string `json:"warnings,omitempty"`
	lockPath string
}

type ownershipManifest struct {
	Artifacts map[string]ownedArtifact `json:"artifacts"`
}
type ownedArtifact struct {
	Path, Unit, Digest string
}
type Check struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}
type Report struct {
	Status string  `json:"status"`
	Checks []Check `json:"checks"`
}

func BuildPlan(target, home, executable string, remove bool) (Plan, error) {
	if home == "" {
		return Plan{}, errors.New("home directory is required")
	}
	if !filepath.IsAbs(executable) {
		return Plan{}, errors.New("agentline executable path must be absolute")
	}
	root, err := setupRoot(home)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Target: target, lockPath: filepath.Join(root, "setup.lock")}
	skillPath := ""
	switch target {
	case "claude":
		skillPath = filepath.Join(home, ".claude/skills/agentline/SKILL.md")
	case "codex", "pi", "opencode":
		skillPath = filepath.Join(home, ".agents/skills/agentline/SKILL.md")
	case "amp":
		skillPath = filepath.Join(home, ".config/agents/skills/agentline/SKILL.md")
	case "mcp":
	default:
		return Plan{}, fmt.Errorf("unknown setup target %q", target)
	}
	if skillPath != "" {
		change, err := ownedFileChange(skillPath, "install Agentline skill", []byte(sharedSkill), remove)
		if err != nil {
			return Plan{}, err
		}
		if change != nil {
			plan.Changes = append(plan.Changes, *change)
		}
		if target == "amp" {
			mcpPath := filepath.Join(filepath.Dir(skillPath), "mcp.json")
			change, err := ownedFileChange(mcpPath, "install Agentline skill MCP configuration", ampMCP(executable), remove)
			if err != nil {
				return Plan{}, err
			}
			if change != nil {
				plan.Changes = append(plan.Changes, *change)
			}
		}
	}
	var path string
	switch target {
	case "claude":
		path = filepath.Join(home, ".claude.json")
	case "codex":
		path = filepath.Join(home, ".codex/config.toml")
	case "opencode":
		path = filepath.Join(home, ".config/opencode/opencode.json")
	case "mcp":
		path = filepath.Join(home, ".config/agentline/mcp.json")
	case "amp", "pi":
		if err := addOwnershipChanges(&plan, target, home, remove); err != nil {
			return Plan{}, err
		}
		return plan, nil
	}
	before, err := readOptional(path)
	if err != nil {
		return Plan{}, err
	}
	var after []byte
	if target == "codex" {
		after, err = editCodex(before, executable, remove)
	} else {
		after, err = editJSON(target, before, executable, remove)
	}
	if err != nil {
		return Plan{}, err
	}
	if !bytes.Equal(before, after) {
		description := "update Agentline MCP registration"
		if target == "mcp" {
			description = "write portable MCP snippet (manual registration required)"
		}
		plan.Changes = append(plan.Changes, Change{path, description, before, after})
	}
	if err := addOwnershipChanges(&plan, target, home, remove); err != nil {
		return Plan{}, err
	}
	for _, c := range plan.Changes {
		if len(c.Before) > 0 {
			plan.Warnings = append(plan.Warnings, "Backups are private (0600) but may contain source configuration secrets.")
			break
		}
	}
	return plan, nil
}

func ownedFileChange(path, description string, installed []byte, remove bool) (*Change, error) {
	before, err := readOptional(path)
	if err != nil {
		return nil, err
	}
	after := installed
	if remove {
		after = nil
	}
	if bytes.Equal(before, after) {
		return nil, nil
	}
	return &Change{path, description, before, after}, nil
}

func editJSON(target string, before []byte, executable string, remove bool) ([]byte, error) {
	if remove && len(bytes.TrimSpace(before)) == 0 {
		return nil, nil
	}
	root := map[string]any{}
	if len(bytes.TrimSpace(before)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(before))
		decoder.UseNumber()
		if err := decoder.Decode(&root); err != nil {
			return nil, fmt.Errorf("decode %s config: %w", target, err)
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("decode %s config: trailing data", target)
		}
	}
	key := "mcpServers"
	if target == "opencode" {
		key = "mcp"
	}
	m, ok := root[key].(map[string]any)
	if !ok {
		if root[key] != nil {
			return nil, fmt.Errorf("%s config %q must be an object", target, key)
		}
		m = map[string]any{}
	}
	want := jsonEntry(target, executable)
	if remove {
		delete(m, "agentline")
	} else {
		m["agentline"] = want
	}
	if len(m) == 0 {
		if _, existed := root[key]; !existed {
			delete(root, key)
		} else {
			root[key] = m
		}
	} else {
		root[key] = m
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func jsonEntry(target, executable string) map[string]any {
	if target == "opencode" {
		return map[string]any{"type": "local", "command": []any{executable, "mcp"}, "enabled": true, "timeout": json.Number("70000")}
	}
	return map[string]any{"command": executable, "args": []any{"mcp"}}
}

func editCodex(before []byte, executable string, remove bool) ([]byte, error) {
	text := string(before)
	lines := strings.SplitAfter(text, "\n")
	start, end := -1, -1
	offset := 0
	for _, line := range lines {
		plain := strings.TrimSuffix(line, "\n")
		plain = strings.TrimSuffix(plain, "\r")
		switch plain {
		case codexBegin:
			if start >= 0 {
				return nil, errors.New("ambiguous Agentline-owned block in Codex config")
			}
			start = offset
		case codexEnd:
			if end >= 0 || start < 0 {
				return nil, errors.New("ambiguous Agentline-owned block in Codex config")
			}
			end = offset + len(line)
		}
		offset += len(line)
	}
	if (start >= 0) != (end >= 0) {
		return nil, errors.New("ambiguous Agentline-owned block in Codex config")
	}
	tableCount, insideCount := 0, 0
	for offset, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(strings.TrimSuffix(line, "\r")) == "[mcp_servers.agentline]" {
			tableCount++
			lineOffset := len(strings.Join(strings.Split(text, "\n")[:offset], "\n"))
			if offset > 0 {
				lineOffset++
			}
			if start >= 0 && lineOffset > start && lineOffset < end {
				insideCount++
			}
		}
	}
	if start < 0 && tableCount > 0 {
		return nil, errors.New("refusing to modify unowned [mcp_servers.agentline] table")
	}
	if start >= 0 && (insideCount != 1 || tableCount != 1) {
		return nil, errors.New("managed Codex block must contain exactly one [mcp_servers.agentline] table and none may exist outside it")
	}
	oldStart, oldEnd := start, end
	if start >= 0 {
		text = text[:start] + text[end:]
	}
	if remove {
		return []byte(text), nil
	}
	quoted, _ := json.Marshal(executable)
	block := fmt.Sprintf("%s\n[mcp_servers.agentline]\ncommand = %s\nargs = [\"mcp\"]\ntool_timeout_sec = 70\n%s\n", codexBegin, quoted, codexEnd)
	if oldStart >= 0 {
		return []byte(string(before)[:oldStart] + block + string(before)[oldEnd:]), nil
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return []byte(text + block), nil
}

func hasCodexTable(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(strings.TrimSuffix(line, "\r")) == "[mcp_servers.agentline]" {
			return true
		}
	}
	return false
}

func ampMCP(executable string) []byte {
	b, _ := json.MarshalIndent(map[string]any{"agentline": map[string]any{"command": executable, "args": []string{"mcp"}, "includeTools": MCPTools}}, "", "  ")
	return append(b, '\n')
}

type artifactSpec struct{ path, unit string }

func addOwnershipChanges(plan *Plan, target, home string, remove bool) error {
	root := filepath.Dir(plan.lockPath)
	manifestPath := filepath.Join(root, "setup-ownership.json")
	manifestBefore, err := readOptional(manifestPath)
	if err != nil {
		return err
	}
	m := ownershipManifest{Artifacts: map[string]ownedArtifact{}}
	if len(manifestBefore) > 0 {
		if err := json.Unmarshal(manifestBefore, &m); err != nil {
			return fmt.Errorf("decode setup ownership manifest: %w", err)
		}
		if m.Artifacts == nil {
			m.Artifacts = map[string]ownedArtifact{}
		}
	}
	specs := targetArtifacts(target, home)
	for _, spec := range specs {
		key := target + ":" + spec.unit + ":" + spec.path
		current, err := ownedUnit(spec, nil)
		if err != nil {
			return err
		}
		record, recorded := m.Artifacts[key]
		if recorded {
			if record.Path != spec.path || record.Unit != spec.unit || record.Digest != digest(current) {
				return fmt.Errorf("refusing to modify %s: Agentline-owned unit changed since setup", spec.path)
			}
		} else if len(current) > 0 && spec.unit != "file" && !artifactOwnedByAnother(m, spec, digest(current)) {
			return fmt.Errorf("refusing to modify unmanifested artifact in %s", spec.path)
		}
		if remove {
			delete(m.Artifacts, key)
			if spec.unit == "file" && artifactHasAnotherOwner(m, spec) {
				removeChange(plan, spec.path)
			}
			continue
		}
		desired, err := ownedUnit(spec, plannedAfter(plan, spec.path))
		if err != nil {
			return err
		}
		m.Artifacts[key] = ownedArtifact{Path: spec.path, Unit: spec.unit, Digest: digest(desired)}
	}
	manifestAfter, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	manifestAfter = append(manifestAfter, '\n')
	if len(m.Artifacts) == 0 {
		manifestAfter = nil
	}
	if !bytes.Equal(manifestBefore, manifestAfter) {
		plan.Changes = append(plan.Changes, Change{Path: manifestPath, Description: "update private Agentline setup ownership manifest", Before: manifestBefore, After: manifestAfter})
	}
	return nil
}

func targetArtifacts(target, home string) []artifactSpec {
	var specs []artifactSpec
	var skill string
	switch target {
	case "claude":
		skill = filepath.Join(home, ".claude/skills/agentline/SKILL.md")
	case "codex", "pi", "opencode":
		skill = filepath.Join(home, ".agents/skills/agentline/SKILL.md")
	case "amp":
		skill = filepath.Join(home, ".config/agents/skills/agentline/SKILL.md")
	}
	if skill != "" {
		specs = append(specs, artifactSpec{skill, "file"})
	}
	switch target {
	case "claude":
		specs = append(specs, artifactSpec{filepath.Join(home, ".claude.json"), "json:mcpServers"})
	case "codex":
		specs = append(specs, artifactSpec{filepath.Join(home, ".codex/config.toml"), "codex"})
	case "opencode":
		specs = append(specs, artifactSpec{filepath.Join(home, ".config/opencode/opencode.json"), "json:mcp"})
	case "mcp":
		specs = append(specs, artifactSpec{filepath.Join(home, ".config/agentline/mcp.json"), "json:mcpServers"})
	case "amp":
		specs = append(specs, artifactSpec{filepath.Join(home, ".config/agents/skills/agentline/mcp.json"), "file"})
	}
	return specs
}

func plannedAfter(plan *Plan, path string) []byte {
	for _, c := range plan.Changes {
		if c.Path == path {
			return c.After
		}
	}
	b, _ := readOptional(path)
	return b
}

func ownedUnit(spec artifactSpec, content []byte) ([]byte, error) {
	if content == nil {
		var err error
		content, err = readOptional(spec.path)
		if err != nil {
			return nil, err
		}
	}
	if spec.unit == "file" {
		return content, nil
	}
	if spec.unit == "codex" {
		text := string(content)
		start := strings.Index(text, codexBegin)
		end := strings.Index(text, codexEnd)
		if start < 0 && end < 0 {
			return nil, nil
		}
		if start < 0 || end < start {
			return nil, errors.New("ambiguous Agentline-owned block in Codex config")
		}
		return []byte(text[start : end+len(codexEnd)]), nil
	}
	var root map[string]any
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	key := strings.TrimPrefix(spec.unit, "json:")
	entries, _ := root[key].(map[string]any)
	entry, ok := entries["agentline"]
	if !ok {
		return nil, nil
	}
	return json.Marshal(entry)
}

func digest(data []byte) string { sum := sha256.Sum256(data); return fmt.Sprintf("%x", sum[:]) }
func artifactOwnedByAnother(m ownershipManifest, spec artifactSpec, d string) bool {
	for _, r := range m.Artifacts {
		if r.Path == spec.path && r.Unit == spec.unit && r.Digest == d {
			return true
		}
	}
	return false
}
func artifactHasAnotherOwner(m ownershipManifest, spec artifactSpec) bool {
	for _, r := range m.Artifacts {
		if r.Path == spec.path && r.Unit == spec.unit {
			return true
		}
	}
	return false
}
func removeChange(plan *Plan, path string) {
	for i := range plan.Changes {
		if plan.Changes[i].Path == path {
			plan.Changes = append(plan.Changes[:i], plan.Changes[i+1:]...)
			return
		}
	}
}

func setupRoot(home string) (string, error) {
	actual, err := os.UserHomeDir()
	if err == nil && home == actual {
		return localconfig.DefaultRoot()
	}
	return filepath.Join(configRoot(home), "agentline"), nil
}

func Apply(plan Plan) error {
	lock := flock.New(plan.lockPath)
	if err := os.MkdirAll(filepath.Dir(plan.lockPath), 0o700); err != nil {
		return err
	}
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("lock setup: %w", err)
	}
	defer lock.Unlock()
	// Validate every snapshot before creating any visible output.
	for _, change := range plan.Changes {
		current, err := readOptional(change.Path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, change.Before) {
			return fmt.Errorf("%s changed since preview", change.Path)
		}
	}
	staged := make([]string, len(plan.Changes))
	stagedBackups := make([]string, len(plan.Changes))
	backupBefore := make([][]byte, len(plan.Changes))
	backupExisted := make([]bool, len(plan.Changes))
	for i, change := range plan.Changes {
		if len(change.After) > 0 {
			path, err := stageFile(change.Path, change.After)
			if err != nil {
				cleanupStages(staged)
				cleanupStages(stagedBackups)
				return err
			}
			staged[i] = path
		}
		if len(change.Before) > 0 {
			var err error
			backupBefore[i], err = readOptional(change.Path + ".agentline.bak")
			if err != nil {
				cleanupStages(staged)
				cleanupStages(stagedBackups)
				return err
			}
			_, statErr := os.Stat(change.Path + ".agentline.bak")
			backupExisted[i] = statErr == nil
			backup, stageErr := stageFile(change.Path+".agentline.bak", change.Before)
			if stageErr != nil {
				cleanupStages(staged)
				cleanupStages(stagedBackups)
				return stageErr
			}
			stagedBackups[i] = backup
		}
	}
	if err := validateSnapshots(plan); err != nil {
		cleanupStages(staged)
		cleanupStages(stagedBackups)
		return err
	}
	// Commit private backups only after every output and backup has staged.
	backupsCommitted := 0
	for i, change := range plan.Changes {
		if stagedBackups[i] != "" {
			if err := replace(stagedBackups[i], change.Path+".agentline.bak"); err != nil {
				cleanupStages(staged)
				cleanupStages(stagedBackups)
				return errors.Join(err, restoreBackups(plan, backupBefore, backupExisted, backupsCommitted))
			}
			stagedBackups[i] = ""
			backupsCommitted = i + 1
		}
	}
	committed := 0
	for i, change := range plan.Changes {
		if err := validateSnapshotsFrom(plan, i); err != nil {
			cleanupStages(staged)
			return errors.Join(err, rollbackTargets(plan, committed), restoreBackups(plan, backupBefore, backupExisted, len(plan.Changes)))
		}
		var err error
		if len(change.After) == 0 {
			err = os.Remove(change.Path)
		} else {
			err = replace(staged[i], change.Path)
			staged[i] = ""
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupStages(staged)
			return errors.Join(err, rollbackTargets(plan, committed), restoreBackups(plan, backupBefore, backupExisted, len(plan.Changes)))
		}
		committed++
	}
	return nil
}

var replace = replaceFile

func validateSnapshots(plan Plan) error {
	return validateSnapshotsFrom(plan, 0)
}
func validateSnapshotsFrom(plan Plan, start int) error {
	for _, change := range plan.Changes[start:] {
		current, err := readOptional(change.Path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, change.Before) {
			return fmt.Errorf("%s changed since preview", change.Path)
		}
	}
	return nil
}

func rollbackTargets(plan Plan, committed int) error {
	var errs []error
	for j := committed - 1; j >= 0; j-- {
		c := plan.Changes[j]
		if len(c.Before) == 0 {
			if err := os.Remove(c.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		} else if err := atomicWrite(c.Path, c.Before); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func restoreBackups(plan Plan, before [][]byte, existed []bool, through int) error {
	var errs []error
	for i := through - 1; i >= 0; i-- {
		if len(plan.Changes[i].Before) == 0 {
			continue
		}
		path := plan.Changes[i].Path + ".agentline.bak"
		if !existed[i] {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		} else if err := atomicWrite(path, before[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func cleanupStages(paths []string) {
	for _, path := range paths {
		if path != "" {
			_ = os.Remove(path)
		}
	}
}

func stageFile(path string, data []byte) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".agentline-*")
	if err != nil {
		return "", err
	}
	temp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(temp)
		return "", err
	}
	return temp, nil
}

func atomicWrite(path string, data []byte) error {
	temp, err := stageFile(path, data)
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	return replace(temp, path)
}

func readOptional(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func HarnessVersionWarning(target string) string {
	if target == "mcp" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, target, "--version").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("Could not verify %s version (%v); setup can still be applied.", target, err)
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return fmt.Sprintf("Could not verify %s version (empty --version output); setup can still be applied.", target)
	}
	return ""
}

func Doctor(ctx context.Context, target, home, executable, relayURL string) Report {
	r := Report{Status: "pass"}
	add := func(name, status, message string) {
		r.Checks = append(r.Checks, Check{name, status, message})
		if status == "fail" {
			r.Status = "fail"
		} else if status == "warn" && r.Status == "pass" {
			r.Status = "warn"
		}
	}
	if filepath.IsAbs(executable) {
		if info, err := os.Stat(executable); err == nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0) {
			add("executable", "pass", executable)
		} else {
			add("executable", "fail", "binary does not exist or is not executable")
		}
	} else {
		add("executable", "fail", "binary path is not absolute")
	}
	parsed, parseErr := url.ParseRequestURI(relayURL)
	var resp *http.Response
	var err error
	if parseErr == nil && parsed.IsAbs() && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(relayURL, "/")+"/healthz", nil)
		if reqErr == nil {
			resp, err = (&http.Client{Timeout: 5 * time.Second}).Do(req)
		} else {
			err = reqErr
		}
	} else {
		err = fmt.Errorf("malformed relay URL")
	}
	if err == nil && resp.StatusCode == http.StatusOK {
		resp.Body.Close()
		add("relay", "pass", relayURL)
	} else {
		if resp != nil {
			resp.Body.Close()
		}
		add("relay", "fail", "relay health check failed")
	}
	root, rootErr := setupRoot(home)
	rooms := filepath.Join(root, "rooms")
	entries, err := os.ReadDir(rooms)
	if rootErr != nil {
		add("credentials", "fail", rootErr.Error())
	} else if errors.Is(err, os.ErrNotExist) || len(entries) == 0 {
		add("credentials", "warn", "no room credentials found")
	} else if err != nil {
		add("credentials", "fail", err.Error())
	} else {
		count := 0
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			count++
		}
		add("credentials", "pass", fmt.Sprintf("%d saved room credential(s)", count))
	}
	targets := []string{target}
	if target == "all" {
		targets = []string{"claude", "codex", "amp", "pi", "opencode", "mcp"}
	}
	sort.Strings(targets)
	for _, t := range targets {
		p, err := BuildPlan(t, home, executable, false)
		if err != nil {
			add(t, "fail", err.Error())
			continue
		}
		skillNeeded := t != "mcp"
		skillMissing, registrationMissing := false, false
		for _, c := range p.Changes {
			if strings.HasSuffix(c.Path, "SKILL.md") {
				skillMissing = true
			} else {
				registrationMissing = true
			}
		}
		if skillNeeded {
			if skillMissing {
				add(t+" skill", "fail", "Agentline skill is missing or outdated")
			} else {
				add(t+" skill", "pass", "skill discovered")
			}
		}
		if t != "pi" {
			if registrationMissing {
				label := "MCP registration"
				if t == "mcp" {
					label = "portable MCP snippet"
				}
				add(t+" MCP", "fail", label+" is missing or outdated")
			} else {
				message := "MCP registered"
				if t == "mcp" {
					message = "portable MCP snippet exists; manual registration is still required"
				}
				add(t+" MCP", "pass", message)
			}
		}
	}
	return r
}

func configRoot(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming")
	default:
		return filepath.Join(home, ".config")
	}
}
