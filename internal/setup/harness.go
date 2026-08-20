package setup

import "path/filepath"

// harness locates the files setup owns for one target. Planning, ownership
// tracking, and doctor all read these paths, so they live in one table: a path
// that differed between them would leave setup writing one file while ownership
// tracked another.
type harness struct {
	// skill is the Agent Skill path, relative to the user's home directory.
	// Empty for targets that take no skill.
	skill string
	// packagedMCP is an MCP configuration file installed beside the skill,
	// which Amp uses to keep Agentline's tools hidden until the skill applies.
	packagedMCP bool
	// config is the harness configuration file Agentline registers itself in,
	// relative to home. Empty for targets registered by hand.
	config string
	// configUnit names the part of that file Agentline owns: a JSON object key,
	// or the delimited block in Codex's TOML.
	configUnit string
}

var harnesses = map[string]harness{
	"claude": {
		skill:      ".claude/skills/agentline/SKILL.md",
		config:     ".claude.json",
		configUnit: "json:mcpServers",
	},
	"codex": {
		skill:      ".agents/skills/agentline/SKILL.md",
		config:     ".codex/config.toml",
		configUnit: "codex",
	},
	"opencode": {
		skill:      ".agents/skills/agentline/SKILL.md",
		config:     ".config/opencode/opencode.json",
		configUnit: "json:mcp",
	},
	"pi": {
		// Pi has no MCP client, so the skill drives its CLI directly.
		skill: ".agents/skills/agentline/SKILL.md",
	},
	"amp": {
		skill:       ".config/agents/skills/agentline/SKILL.md",
		packagedMCP: true,
	},
	"mcp": {
		// A portable snippet for harnesses Agentline cannot configure itself.
		config:     ".config/agentline/mcp.json",
		configUnit: "json:mcpServers",
	},
}

// skillPath and configPath resolve a harness's files against a home directory,
// returning "" where the target has none.
func (h harness) skillPath(home string) string {
	if h.skill == "" {
		return ""
	}
	return filepath.Join(home, filepath.FromSlash(h.skill))
}

func (h harness) configPath(home string) string {
	if h.config == "" {
		return ""
	}
	return filepath.Join(home, filepath.FromSlash(h.config))
}

// packagedMCPPath is the MCP configuration installed next to the skill.
func (h harness) packagedMCPPath(home string) string {
	if !h.packagedMCP {
		return ""
	}
	return filepath.Join(filepath.Dir(h.skillPath(home)), "mcp.json")
}
