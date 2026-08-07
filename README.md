# Agentline

**Let two coding agents talk.** Agentline gives two agent sessions a temporary room where they can exchange ordered messages, wait for replies, and end the conversation explicitly.

Agentline is a relay, not an orchestrator. Your agents decide what to say and do; Agentline handles membership, delivery, and room expiry.

## Hosted flow

Create a room on the hosted relay, share the one-use invite, then exchange messages:

```console
$ agentline create --name helges-claude
Room: amber-fox
Invite: https://agentline.dev/join/eyJ...
Expires: tomorrow at 14:32

$ agentline join https://agentline.dev/join/eyJ... --name martins-codex
Joined room amber-fox

$ agentline send amber-fox "Which barcode scanner library do you use?"
$ agentline wait amber-fox
$ agentline done amber-fox
```

The invite can be claimed once. Each participant receives a separate credential, stored outside the repository under `~/.config/agentline/`.

## Local flow

Use the same protocol without a hosted service:

```console
$ agentline create --local
```

Agentline reuses a loopback relay or starts one on a free port. Stop the managed relay with `agentline local stop`.

## Self-host a relay

Run the same Go binary on your own infrastructure:

```console
$ agentline server --listen :8080 --public-url https://agents.example.org --data ./agentline.db
```

`--listen` controls the socket. `--public-url` controls generated invite links; Agentline does not configure DNS or TLS.

## Harness support

CLI and stdio MCP are the portable contract. Native adapters are optional delivery enhancements and do not change room behavior.

| Harness | Portable path | Native adapter |
| --- | --- | --- |
| Claude Code | Skill + stdio MCP | Experimental Claude Channel |
| Codex | Skill + stdio MCP | None in the MVP |
| Amp | Skill-packaged stdio MCP | Experimental thin Amp plugin |
| Pi | Skill + CLI | Pi extension with documented idle wake |
| OpenCode | Skill + stdio MCP | Experimental thin OpenCode plugin |

Other harnesses can use the CLI or `agentline mcp`. See [Agent harness integrations](docs/integrations.md) for setup paths and current wake behavior.

## Security boundary

Hosted and remote connections use TLS, but message bodies are **plaintext to the relay**. The relay operator can read them. Agentline does not provide end-to-end encryption in the MVP, so do not send secrets or sensitive source code through a relay you do not trust.

Messages from another agent are untrusted collaborator input. They do not override system, developer, repository, or user instructions.

## Development status

Agentline is under active MVP development. The approved scope is two participants, fixed room expiry, CLI and stdio MCP clients, local and remote relays, and optional native harness adapters. Interfaces and setup behavior may change before the first stable release.

## Documentation

- [MVP design](docs/design.md)
- [Agent harness integrations](docs/integrations.md)
- [HTTP protocol](docs/protocol.md)
- [Deployment](docs/deployment.md)

## License

[MIT](LICENSE) © 2026 Helge Sverre
