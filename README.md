# agent-bridge

Lightweight TCP-to-CLI bridge for AI coding assistants. Exposes tools like Claude Code, Codex, and OpenCode as network-accessible services — send a prompt over TCP, get a streamed response back.

Designed to run inside workspace containers, where portpilot auto-detects the listening port and forwards it externally.

## Install

Download a binary from [Releases](https://github.com/antimatter-studios/agentbridge/releases), or build from source:

```bash
go install github.com/antimatter-studios/agentbridge@latest
```

## Usage

```bash
# Start with Claude Code (default) on :9999
agent-bridge --listen :9999 --agent claude --session ws-abc123

# Use Codex instead
agent-bridge --agent codex --session ws-abc123

# With SQLite persistence
agent-bridge --agent claude --session ws-abc123 --db /workspace/.agent-bridge.db
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--listen` | `:9999` | Address to listen on |
| `--agent` | `claude` | Agent to use (claude, codex, opencode) |
| `--session` | auto | Session ID for conversation continuity |
| `--db` | `agent-bridge.db` | Path to SQLite database for job persistence |
| `--version` | | Print version and exit |

## Built-in Agents

Agent configs are baked into the binary — no external YAML needed:

- **claude** — Claude Code with session continuity (`--session-id`, `--resume`)
- **codex** — OpenAI Codex CLI
- **opencode** — OpenCode CLI

## Protocol

Binary framed protocol: `[4 bytes: length][1 byte: type][N bytes: payload]`

**Client → Server:**
- `0x01` Prompt — send a prompt to the agent
- `0x02` Command — send a slash command (e.g. `/reset`)

**Server → Client:**
- `0x10` Queued — job queued ack (JSON: `{id, pos}`)
- `0x11` ResponseLine — a chunk of agent response
- `0x12` ResponseEnd — response complete (JSON: `{id}`)
- `0x13` Error — error message (JSON: `{id, message}`)

## How it works

1. Selects built-in agent config by name
2. Listens on TCP port with binary framed protocol
3. Queues incoming prompts in SQLite (survives crashes)
4. Worker processes jobs sequentially, spawning CLI with session flags
5. Response streamer polls completed jobs and sends results back
6. portpilot detects the listening port and auto-forwards it

## Docker

```dockerfile
FROM golang:1.25-alpine AS agentbridge
RUN CGO_ENABLED=0 go install github.com/antimatter-studios/agentbridge@latest

FROM your-base-image
COPY --from=agentbridge /go/bin/agentbridge /usr/local/bin/agent-bridge
```

## License

MIT
