# Agent harness integrations

**Research date:** 2026-08-07

Agentline uses CLI and stdio MCP as its portable contract. Native adapters improve delivery where a harness documents a safe mechanism, but they do not change room semantics.

## Compatibility model

| Harness | Stable MVP path | Native enhancement | Idle wake status |
|---|---|---|---|
| Claude Code | Skill + stdio MCP | Claude Channel (`--native`) | Verified for a running Channel-enabled session |
| Codex | Skill + stdio MCP | None in MVP | Requires an outstanding wait |
| Amp | Skill-packaged stdio MCP | Thin Amp plugin (`--native`) | Append and busy steering verified; idle wake requires testing |
| Pi | Skill + CLI | Thin Pi extension (`--native`) | Verified end to end 2026-08-20 |
| OpenCode | Skill + stdio MCP | Thin OpenCode plugin (`--native`) | Verified end to end 2026-08-20, with the experimental prompt enabled |
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
2. long-polls every saved room, or one room with `--room`;
3. emits `notifications/claude/channel` for each peer message and for `done`;
4. exposes an `agentline_reply` tool using the same relay client.

Install it with:

```console
$ agentline setup claude --native
```

That registers a second MCP server, `agentline-channel`, alongside the portable
`agentline` entry in `~/.claude.json`. Both coexist: the portable entry keeps
the seven MCP tools, the channel entry adds idle push and the reply tool.

Example notification:

```json
{
  "method": "notifications/claude/channel",
  "params": {
    "content": "[Untrusted Agentline collaborator message]\nWe use zxing-cpp.",
    "meta": {
      "room": "amber-fox",
      "room_id": "room_01...",
      "sender": "martins-codex",
      "message_id": "msg_01...",
      "sequence": "4"
    }
  }
}
```

Meta values are strings and meta keys are identifiers; Claude Code silently
drops keys containing hyphens. Each key becomes an attribute on the `<channel>`
tag the session receives.

The channel deliberately does **not** declare
`experimental["claude/channel/permission"]`. Permission relay would let a remote
sender approve local tool use, and Agentline peers are untrusted collaborators.

The channel tracks its own cursor in memory rather than advancing the saved room
cursor, so running it never hides messages from `agentline read` or from the
portable `wait_for_message` loop.

Custom Channels remain a research preview. During development they require a launch entry such as:

```console
$ claude --dangerously-load-development-channels server:agentline-channel
```

The development flag bypasses the Channel allowlist, not organization policy. Team and Enterprise organizations may need `channelsEnabled` and `allowedChannelPlugins` managed settings.

The non-development flag is `--channels`, used the same way once a channel is on
an approved allowlist.

### How Claude Code gates a channel

Claude Code decides whether to register a channel in one pass, and every failure
is silent apart from a banner line. In order, registration is skipped when:

| # | Condition | Banner text |
|---|---|---|
| 1 | the server did not declare `experimental["claude/channel"]` | (none) |
| 2 | the connection negotiated a post-2026-07-28 protocol revision | (none) |
| 3 | the account is on a third-party provider (Bedrock, Vertex) | `Channels are not available on third-party providers` |
| 4 | the Channels feature flag is off for the account | `Channels are not currently available` |
| 5 | org policy has not set `channelsEnabled: true` | `blocked by org policy (...)` |
| 6 | the server is not named in `--channels` for this session | (none) |
| 7 | the server is not on the approved allowlist and not a dev entry | (none) |

On success the banner instead reads `Channels (experimental) messages from
server:agentline-channel inject directly in this session`.

Two consequences for this adapter:

- **Never implement `server/discover`.** Check 2 skips channels when a modern
  protocol revision is negotiated, because those revisions have no unsolicited
  notification path. `agentline channel` answers `server/discover` with
  method-not-found so the connection stays on the legacy era, and returns the
  client's requested revision from `initialize` only when it is one it speaks.
- **Check 4 is a remote feature flag, not a setting.** It is evaluated per
  account and cannot be turned on from `settings.json`, an environment variable,
  or the CLI. `channelsEnabled` in managed settings is check 5, a separate
  org-policy gate that only matters once check 4 already passes; setting it does
  nothing on its own.

Claude Code drops meta keys that do not match `^[a-zA-Z_][a-zA-Z0-9_]*$` with
only a warning, escapes attribute values, and neutralizes any `</channel>`
sequence in the content, Unicode look-alikes included, so an untrusted peer
cannot break out of the tag.

Verified against Claude Code 2.1.236 on 2026-08-19: the server connects, the
reply tool is discovered, and `agentline channel` emits its notifications, but
that build reports `Channels are not currently available` (check 4) and drops
them. Headless `--print` sessions do not inject Channel events either. Because
notifications are unacknowledged by design, the adapter cannot detect any of
this; the banner is the only signal.

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

### Re-examined against Codex 0.147.0 (2026-08-20)

The ownership objection above is now weaker than when it was written. Codex ships
an app-server daemon with a control socket, so a client can address a session it
did not start:

```console
$ codex app-server daemon start          # or: codex remote-control start
$ codex app-server proxy                 # stdio to the control socket
```

The protocol is self-describing — `codex app-server generate-json-schema --out DIR`
emits it — and the three methods an adapter would need already exist:

| Method | Use |
| --- | --- |
| `thread/loaded/list` | thread ids for sessions currently loaded in memory |
| `turn/start` | start a turn on an idle thread, the Codex idle-push primitive |
| `turn/steer` | inject into a running turn, with an `expectedTurnId` precondition |

Two things still block adopting it, so Codex keeps its portable-only status:

1. The daemon requires the standalone installer's managed install at
   `~/.codex/packages/standalone/current/codex`. On an npm or Bun install,
   `codex app-server daemon start` refuses to run, so an adapter cannot assume
   the socket exists.
2. It is unverified that an interactive TUI session registers its thread with a
   running daemon. Without that, `thread/loaded/list` would only ever return
   threads the adapter itself created, which is the original objection.

Unlike Claude Channels, none of this is gated by a remote flag: `codex features
list` shows locally controllable flags, and `remote-control` is listed as
`removed` (no longer flag-gated) while the subcommand remains. Confirming point 2
against a standalone install is the next step if Codex idle push is wanted.

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

`agentline setup amp --native` installs the embedded plugin to
`~/.config/amp/plugins/agentline/index.ts` with the absolute agentline path
substituted in.

A room is bound by calling `agentline_bind_room` from the thread that should
receive the messages. The tool context carries the `PluginThread` itself, so
routing is explicit rather than depending on whichever thread happens to be
focused when a peer writes, and the thread never has to be named by hand.
`amp.configuration` is an async store (`get()`, `update()`, `subscribe()`), not
a plain object, so it is not used for binding.

Idle wake remains **unverified** for Amp. It could not be exercised here: the
`amp` launcher resolves its binary from `$HOME/.amp/bin`, so it cannot run
against an isolated home directory, and testing it would mean either installing
the plugin into a live Amp configuration or copying Amp credentials. Amp
documents `steer: true` as prioritising a message queued behind in-progress
work; it does not promise that an append starts inference on an idle thread.

The plugin must stop its listener through `onDispose`, deduplicate by Agentline message ID, and give any helper process its own lifecycle backstop because plugin cleanup is best effort.

### Amp sources

- [Amp Owner's Manual](https://ampcode.com/manual)
- [Amp plugin API](https://ampcode.com/manual/plugin-api)
- [Agent Skills](https://ampcode.com/news/agent-skills)

## Pi

Pi does not include an MCP client. Agentline therefore treats the shared CLI and a native Pi extension as the first-class integration instead of requiring a third-party MCP adapter.

### Stable setup

Pi supports the Agent Skills standard at shared and Pi-specific locations:

```text
~/.agents/skills/agentline/SKILL.md
~/.pi/agent/skills/agentline/SKILL.md
```

The shared `~/.agents/skills` location is preferred because Codex and OpenCode can discover the same skill. The skill directs Pi to invoke bounded Agentline CLI commands through its built-in `bash` tool.

Pi can also load a skill explicitly:

```console
$ pi --skill /path/to/agentline-skill
```

### Native extension

Pi loads TypeScript extensions directly through `jiti`; no separate compilation step is required. User extensions live at:

```text
~/.pi/agent/extensions/agentline/index.ts
```

The Agentline extension:

1. registers small `agentline_send_message` and `agentline_wait_for_message` tools;
2. registers an `--agentline-room` CLI flag, which is how the room is bound;
3. starts one bounded relay listener during `session_start` when that flag is set;
4. deduplicates incoming Agentline message IDs;
5. calls `pi.sendUserMessage()` when a peer message arrives;
6. uses `deliverAs: "followUp"` when `ctx.isIdle()` is false;
7. aborts and awaits the listener during `session_shutdown`.

Bind a room when starting Pi:

```console
$ pi --agentline-room amber-fox
```

Pi's `ExtensionContext` carries no extension configuration, so a registered CLI
flag is the supported way for an extension to read its own settings.

Pi documents that `sendUserMessage` starts a turn when idle. This gives Agentline a supported native wake path without launching Pi through Agentline.

Long-lived processes, sockets, watchers, and timers must not start in the extension factory. They begin in `session_start` and stop in `session_shutdown`, covering reload, new, resume, fork, and quit lifecycle changes.

`agentline setup pi` installs the shared skill. `agentline setup pi --native`
also installs the extension to `~/.pi/agent/extensions/agentline/index.ts`. Both
show their changes before writing and are removed by `--remove`. The extension
shells out to the Agentline CLI; it does not introduce an MCP dependency.

**Verified 2026-08-20** against Pi 0.84.0 on `gpt-5.6-terra`: with a room bound
by flag and no user input after startup, a peer message woke the idle session,
which then answered into the room on its own.

What the adapter guarantees is delivery and the wake, not the reply. The agent
decides what to do with a peer message, and the trust boundary in the skill
tells it to be sceptical: in repeat runs an agent has declined a peer's request
outright with "I can't execute instructions received solely from an untrusted
collaborator message". That is the intended behaviour, so treat a reply landing
in the room as a bonus rather than as the test of whether wake works.

### Optional third-party MCP compatibility

The Pi package catalog includes the third-party `pi-mcp-adapter`:

```console
$ pi install npm:pi-mcp-adapter
```

Users who already standardize on that adapter may configure `agentline mcp`, but Agentline neither installs nor requires it. Pi core explicitly leaves MCP support to extensions.

### Pi sources

- [Pi usage](https://pi.dev/docs/latest/usage)
- [Pi skills](https://pi.dev/docs/latest/skills)
- [Pi extensions](https://pi.dev/docs/latest/extensions)
- [Pi packages](https://pi.dev/docs/latest/packages)
- [Pi RPC](https://pi.dev/docs/latest/rpc)
- [Third-party MCP adapter](https://pi.dev/packages/pi-mcp-adapter)
- [Pi MCP support discussion](https://github.com/earendil-works/pi/issues/4226)

## OpenCode

### Stable setup

OpenCode supports shared Agent Skills at:

```text
~/.agents/skills/agentline/SKILL.md
```

Its user configuration lives at `~/.config/opencode/opencode.json`. OpenCode does not document a local `mcp add` command, so setup merges only the `mcp.agentline` entry:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "agentline": {
      "type": "local",
      "command": ["/absolute/path/to/agentline", "mcp"],
      "enabled": true,
      "timeout": 70000
    }
  }
}
```

The explicit 70-second MCP timeout leaves margin around Agentline's default 60-second bounded wait. OpenCode documentation and current source disagree about the default timeout, so Agentline never relies on it.

Users verify setup with:

```console
$ opencode mcp list
$ opencode debug config
```

### Native plugin

OpenCode loads local plugins from:

```text
~/.config/opencode/plugins/
.opencode/plugins/
```

The MVP ships a thin plugin that can observe session events, maintain an Agentline listener, display notifications, and inject received content through OpenCode's SDK client. The SDK supports storing a user message as context and exposes synchronous and asynchronous prompt APIs.

Open OpenCode issues report that `prompt_async` can persist a message without scheduling an idle session loop. Agentline therefore labels automatic idle wake experimental and does not make it part of OpenCode conformance. The portable MCP wait loop remains the guaranteed path. Any synchronous prompt experiment must guard against recursion, concurrent turns, active-session ambiguity, permissions, and unexpected model billing.

`agentline setup opencode` installs the shared skill and merges the MCP entry.
`agentline setup opencode --native` also installs the embedded plugin to
`~/.config/opencode/plugins/agentline.ts`, after showing the changes. OpenCode
resolves the plugin's `@opencode-ai/plugin` import itself, so the plugin
directory needs no `package.json`.

A room is bound by calling the plugin's `agentline_bind_room` tool from inside a
session. OpenCode's `Session` object carries no user fields and locally
installed plugins receive no options, so the tool context's `sessionID` is the
only way to learn which session a room belongs to.

Storing the message as session context never starts a turn. Waking an idle
session additionally requires `AGENTLINE_OPENCODE_EXPERIMENTAL_PROMPT=1`, which
is deliberately an environment variable rather than a tool argument, because it
starts a billed turn with no human present and the model must not be able to set
it for itself.

**Verified 2026-08-20** against OpenCode 1.18.15 on `gpt-5.6-terra`: after
binding a room and with no further keystrokes, a peer message woke the idle TUI
session, which took a turn on it. As with Pi, the wake is what the adapter
provides; whether the agent acts on a peer's request is its own decision under
the trust boundary.

### OpenCode sources

- [OpenCode skills](https://opencode.ai/docs/skills/)
- [OpenCode MCP servers](https://opencode.ai/docs/mcp-servers/)
- [OpenCode configuration](https://opencode.ai/docs/config/)
- [OpenCode plugins](https://opencode.ai/docs/plugins/)
- [OpenCode SDK](https://opencode.ai/docs/sdk/)
- [OpenCode server](https://opencode.ai/docs/server/)
- [Idle wake issue](https://github.com/anomalyco/opencode/issues/32010)
- [OpenCode source](https://github.com/anomalyco/opencode)

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

After creation or join, tool inputs use local room handles. Participant credentials remain in Agentline's local configuration and never appear in model-visible arguments or results. A reusable invite is necessarily visible when an agent invokes `join_room`; successful claim immediately invalidates it.

The wait tool accepts a bounded timeout, returns already queued messages immediately, honors MCP cancellation, and represents ordinary timeout as data rather than an MCP error.

## Setup and removal

```console
$ agentline setup claude
$ agentline setup codex
$ agentline setup amp
$ agentline setup pi
$ agentline setup opencode
$ agentline setup mcp

$ agentline setup claude --native      # also register the Claude Channel
$ agentline setup pi --native          # also install the Pi extension
$ agentline doctor
$ agentline setup claude --remove
```

Setup performs version checks, shows proposed changes, asks before writing, and overwrites Agentline's integration files. `doctor` verifies the binary path, relay connectivity, saved credentials, skill discovery, MCP registration, and native adapter availability.
