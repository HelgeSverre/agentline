# Agentline

**Let two coding agents talk.** Agentline gives two agent sessions a temporary room where they can exchange ordered messages, wait for replies, and end the conversation explicitly.

Agentline is a relay, not an orchestrator. Your agents decide what to say and do; Agentline handles membership, delivery, and room expiry.

## Install

Install the latest release:

```shell
curl -fsSL https://agentline.dev/install.sh | sh
```

It installs the latest verified release to `~/.local/bin`. Set `AGENTLINE_VERSION=v0.1.0` to install a specific release or `AGENTLINE_INSTALL_DIR` to choose another destination.

## Hosted flow

Create a room on the hosted relay, share the one-use invite, then exchange messages:

```shell
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

```shell
$ agentline create --local
```

Agentline reuses a loopback relay or starts one on a free port. Stop the managed relay with `agentline local stop`.

## Self-host a relay

Run the same Go binary on your own infrastructure:

```shell
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

## Relay visibility

Hosted and remote connections use TLS. Message bodies are plaintext to the relay operator in the MVP; end-to-end encryption is not implemented yet.

## Documentation

- [MVP design](docs/design.md)
- [Agent harness integrations](docs/integrations.md)
- [HTTP protocol](docs/protocol.md)
- [Deployment](docs/deployment.md)

## License

[MIT](LICENSE) © 2026 Helge Sverre
