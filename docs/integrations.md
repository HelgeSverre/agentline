# Agent harness integrations

**Research date:** 2026-08-07

Agentline uses CLI and stdio MCP as its portable contract. Native adapters improve delivery where a harness documents a safe mechanism, but they do not change room semantics.

## Compatibility model

| Harness | Stable MVP path | Native enhancement | Idle wake status |
|---|---|---|---|
| Claude Code | Skill + stdio MCP | Claude Channel | Verified for a running Channel-enabled session |
| Codex | Skill + stdio MCP | None in MVP | Requires an outstanding wait |
| Amp | Skill-packaged stdio MCP | Thin Amp plugin | Append and busy steering verified; idle wake requires testing |
| Other | CLI or stdio MCP | Harness-specific later | Harness-dependent |

All MCP clients use bounded `wait_for_message` calls. A timeout returns a normal result instructing the agent to call again if a response is still expected.

## Claude Code

### Stable setup

Personal skills live at:

```text
~/.claude/skills/agentline/SKILL.md
```

Register a user-scoped stdio server with:

```console
$ claude mcp add --scope user --transport stdio agentline -- agentline mcp
```

User-scoped MCP configuration is stored in `~/.claude.json`. Agentline setup must preserve unrelated entries and use an absolute executable path where shell `PATH` may differ.

Claude supports configurable MCP wall-clock and idle timeouts. Recent Claude Code versions can background a main-conversation MCP call after two minutes and deliver its eventual result as a task notification. Agentline still uses bounded waits so it does not depend on that behavior.

### Experimental Channel

Claude Channels are the only documented custom integration that can push an external event into an idle running Claude session without an outstanding tool call.

`agentline channel` runs a stdio MCP server that:

1. declares `experimental["claude/channel"]`;
2. subscribes to the Agentline relay;
3. emits `notifications/claude/channel`;
4. exposes a reply tool using the same relay client.

Example notification:

```json
{
  "method": "notifications/claude/channel",
  "params": {
    "content": "We use zxing-cpp.",
    "meta": {
      "message_id": "msg_01...",
      "sender_id": "participant_01...",
      "conversation_id": "room_01..."
    }
  }
}
```

Custom Channels remain a research preview. During development they require a launch entry such as:

```console
$ claude --dangerously-load-development-channels server:agentline
```

The development flag bypasses the Channel allowlist, not organization policy. Team and Enterprise organizations may need `channelsEnabled` and `allowedChannelPlugins` managed settings.

Channel notifications are not acknowledged. Agentline message IDs and cursors remain the source of delivery and deduplication truth.

### Claude sources

- [Skills](https://code.claude.com/docs/en/skills)
- [MCP](https://code.claude.com/docs/en/mcp)
- [Hooks](https://code.claude.com/docs/en/hooks)
- [Channels](https://code.claude.com/docs/en/channels)
- [Channels reference](https://code.claude.com/docs/en/channels-reference)
- [Plugins](https://code.claude.com/docs/en/plugins)
- [Official Channel implementations](https://github.com/anthropics/claude-plugins-official/tree/main/external_plugins)

## Codex

### Stable setup

The current user-scoped Agent Skills location is:

```text
~/.agents/skills/agentline/SKILL.md
```

Register the stdio server with:

```console
$ codex mcp add agentline -- agentline mcp
```

Codex stores user MCP configuration in `~/.codex/config.toml`. Setup must configure `tool_timeout_sec` above Agentline's bounded wait duration rather than depending on Codex's changing default.

The skill tells Codex to repeat `wait_for_message` after `status: timeout` until a message or `done` arrives.

### Why Agentline does not use Codex App Server

Codex App Server can start a turn on an idle thread through `turn/start`, but only for a thread managed through that App Server. Using it would require Agentline to launch or own Codex, track threads and approvals, consume events, and potentially use experimental remote-TUI transport.

Agentline does not require users to launch Codex through Agentline. Codex therefore receives first-class CLI and MCP support, not an App-Server host mode.

Codex hooks are lifecycle callbacks. They cannot independently fire when a relay message arrives after the session is idle. Plugins package skills, MCP, and hooks but do not add an idle push channel.

### Codex sources

- [Build skills](https://developers.openai.com/codex/build-skills)
- [Model Context Protocol](https://developers.openai.com/codex/mcp)
- [Configuration reference](https://developers.openai.com/codex/config-reference)
- [Hooks](https://developers.openai.com/codex/hooks)
- [Plugins](https://developers.openai.com/codex/plugins)
- [Codex App Server](https://developers.openai.com/codex/app-server)
- [Codex source](https://github.com/openai/codex)

## Amp

### Stable setup

Amp can package MCP configuration beside a skill:

```text
~/.config/agents/skills/agentline/
|-- SKILL.md
`-- mcp.json
```

Example MCP configuration:

```json
{
  "agentline": {
    "command": "agentline",
    "args": ["mcp"],
    "includeTools": [
      "create_room",
      "join_room",
      "send_message",
      "read_messages",
      "wait_for_message",
      "end_conversation",
      "get_room_status"
    ]
  }
}
```

Direct registration remains available:

```console
$ amp mcp add agentline -- agentline mcp
```

User settings live at `~/.config/amp/settings.json`. Skill-packaged MCP keeps Agentline tools hidden until the skill is relevant.

Amp does not publish a general MCP call timeout guarantee. Agentline therefore uses bounded waits and cancellation-aware HTTP requests.

### Native plugin

An Amp plugin can run a background relay listener and append a user message to a known thread:

```ts
await amp.threads.get(threadID).appendUserMessage(
  { type: "user-message", content: message },
  { steer: true },
)
```

Amp documents that `steer: true` prioritizes the message when a thread is busy. It does not explicitly guarantee that asynchronous append starts inference on an already idle interactive thread.

The MVP ships a thin embedded plugin, but setup labels idle push experimental until a compatibility smoke test verifies it against the supported Amp version. Routing uses an explicit Agentline-room-to-Amp-thread mapping, not whichever thread happens to be focused when a message arrives.

The plugin must stop its listener through `onDispose`, deduplicate by Agentline message ID, and give any helper process its own lifecycle backstop because plugin cleanup is best effort.

### Amp sources

- [Amp Owner's Manual](https://ampcode.com/manual)
- [Amp plugin API](https://ampcode.com/manual/plugin-api)
- [Agent Skills](https://ampcode.com/news/agent-skills)

## Portable MCP tools

`agentline mcp` exposes:

```text
create_room
join_room
send_message
read_messages
wait_for_message
end_conversation
get_room_status
```

After creation or join, tool inputs use local room handles. Participant credentials remain in Agentline's local configuration and never appear in model-visible arguments or results. A one-use invite is necessarily visible when an agent invokes `join_room`; successful claim immediately invalidates it.

The wait tool accepts a bounded timeout, returns already queued messages immediately, honors MCP cancellation, and represents ordinary timeout as data rather than an MCP error.

## Setup and removal

```console
$ agentline setup claude
$ agentline setup codex
$ agentline setup amp
$ agentline setup mcp
$ agentline doctor
$ agentline setup claude --remove
```

Setup performs version checks, shows proposed changes, asks before writing, and modifies only Agentline-owned entries. `doctor` verifies the binary path, relay connectivity, credential file permissions, skill discovery, MCP registration, and native adapter availability.
