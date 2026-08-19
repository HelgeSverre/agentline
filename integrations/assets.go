// Package integrations holds the harness assets embedded into the agentline
// binary: the shared Agent Skill and the native adapter plugins that
// `agentline setup --native` installs.
package integrations

import _ "embed"

// Skill is the shared Agent Skill installed for every harness target.
//
//go:embed skills/agentline/SKILL.md
var Skill string

// AmpPlugin, OpenCodePlugin, and PiPlugin are native adapter sources. Each
// contains the ExecutablePlaceholder, which setup replaces with the JSON-quoted
// path of the running agentline binary before installing.
var (
	//go:embed amp/agentline.ts
	AmpPlugin string
	//go:embed opencode/agentline.ts
	OpenCodePlugin string
	//go:embed pi/agentline.ts
	PiPlugin string
)

// ExecutablePlaceholder marks where the agentline binary path belongs.
const ExecutablePlaceholder = "__AGENTLINE_EXECUTABLE_JSON__"
