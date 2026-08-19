package setup

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/HelgeSverre/agentline/integrations"
)

// ChannelServerName is the MCP server key the Claude Channel adapter is
// registered under. It is deliberately distinct from the "agentline" key used
// by the portable MCP server so both can be registered at once.
const ChannelServerName = "agentline-channel"

// nativeAdapter describes a target's experimental idle-push adapter: the
// artifact setup writes and the launch instructions the user still needs.
type nativeAdapter struct {
	// path is the installed artifact, relative to the user's home directory.
	path string
	// unit is the ownership unit; "file" for plugins, "json:KEY:ENTRY" for the
	// Claude Channel's MCP registration.
	unit string
	// description labels the change in the setup preview.
	description string
	// warning is shown whenever the adapter is installed.
	warning string
	// source is the plugin template; empty for the Claude Channel.
	source string
}

func nativeAdapterFor(target string) (nativeAdapter, bool) {
	switch target {
	case "claude":
		return nativeAdapter{
			path:        ".claude.json",
			unit:        "json:mcpServers:" + ChannelServerName,
			description: "register the experimental Claude Channel adapter",
			warning: "Claude Channels are a research preview and custom channels are not on the approved allowlist. Start Claude with 'claude --dangerously-load-development-channels server:" + ChannelServerName +
				"'. That flag skips the allowlist only; the channelsEnabled organization policy still applies.",
		}, true
	case "pi":
		return nativeAdapter{
			path:        ".pi/agent/extensions/agentline/index.ts",
			unit:        "file",
			description: "install the experimental Pi extension",
			warning:     "Bind a room by starting Pi with 'pi --agentline-room ROOM'; without that flag the extension only registers its tools. Idle wake through pi.sendUserMessage() is verified but experimental; the bounded 'agentline wait' loop remains the guaranteed path.",
			source:      integrations.PiPlugin,
		}, true
	case "amp":
		return nativeAdapter{
			path:        ".config/amp/plugins/agentline/index.ts",
			unit:        "file",
			description: "install the experimental Amp plugin",
			warning:     "The Amp plugin appends to an explicitly bound thread with steer:true. Amp does not guarantee that an asynchronous append starts inference on an idle thread, so idle wake is experimental; bind a room with agentline_bind_room.",
			source:      integrations.AmpPlugin,
		}, true
	case "opencode":
		return nativeAdapter{
			path:        ".config/opencode/plugins/agentline.ts",
			unit:        "file",
			description: "install the experimental OpenCode plugin",
			warning:     "Bind a room by calling the agentline_bind_room tool inside an OpenCode session. Received messages are stored as session context; waking an idle session additionally requires AGENTLINE_OPENCODE_EXPERIMENTAL_PROMPT=1, which starts a billed turn without a human present. The bounded MCP wait loop remains the guaranteed path.",
			source:      integrations.OpenCodePlugin,
		}, true
	}
	return nativeAdapter{}, false
}

// nativeArtifact returns the ownership spec for a target's native adapter.
func nativeArtifact(target, home string) (artifactSpec, bool) {
	adapter, ok := nativeAdapterFor(target)
	if !ok {
		return artifactSpec{}, false
	}
	return artifactSpec{filepath.Join(home, filepath.FromSlash(adapter.path)), adapter.unit}, true
}

// addNativePlugin plans the installation or removal of a file-based native
// adapter. The Claude Channel is not one: its registration is merged into the
// same ~/.claude.json edit as the portable MCP server, so it is handled by
// BuildPlan directly.
func addNativePlugin(plan *Plan, target, home, executable string, remove bool) error {
	adapter, ok := nativeAdapterFor(target)
	if !ok || adapter.source == "" {
		return nil
	}
	path := filepath.Join(home, filepath.FromSlash(adapter.path))
	change, err := ownedFileChange(path, adapter.description, []byte(renderPlugin(adapter.source, executable)), remove)
	if err != nil {
		return err
	}
	if change != nil {
		plan.Changes = append(plan.Changes, *change)
	}
	return nil
}

// renderPlugin substitutes the agentline binary path into a plugin template.
// The path is JSON-encoded, which is also a valid TypeScript string literal.
func renderPlugin(source, executable string) string {
	quoted, _ := json.Marshal(executable)
	return strings.ReplaceAll(source, integrations.ExecutablePlaceholder, string(quoted))
}

// editChannelJSON adds or removes the Claude Channel entry in ~/.claude.json
// without disturbing the sibling "agentline" MCP registration.
func editChannelJSON(before []byte, executable string, remove bool) ([]byte, error) {
	return editJSONEntry("claude", "mcpServers", ChannelServerName,
		map[string]any{"command": executable, "args": []any{"channel"}}, before, remove)
}
