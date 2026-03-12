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
# Start with default config (Claude Code on :9999)
agent-bridge --listen :9999 --config agent.yaml

# Connect and send a prompt
echo "list the files in the current directory" | nc localhost 9999

# Or interactively
nc localhost 9999
> create a hello world web server in Go
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--listen` | `:9999` | Address to listen on |
| `--config` | `agent.yaml` | Path to agent config YAML |
| `--version` | | Print version and exit |

## Configuration

`agent.yaml` defines how to spawn and communicate with each CLI agent:

```yaml
active: claude

agents:
  claude:
    command: claude
    args: ["--print", "--output-format", "stream-json", "--model", "claude-sonnet-4-6"]
    input_mode: flag      # spawns a new process per prompt
    input_flag: "-p"      # flag that precedes the prompt text
    workdir: /workspace
    env:
      CI: "1"

  codex:
    command: codex
    args: ["--quiet"]
    input_mode: stdin     # persistent process, prompts written to stdin
    workdir: /workspace
```

### Input modes

- **flag** — spawns a new CLI process per prompt, appending the prompt as a CLI argument (e.g. `claude -p "prompt"`). Best for tools with a one-shot mode.
- **stdin** — maintains a persistent CLI process and writes prompts to stdin. Best for interactive/REPL-style tools.

## Protocol

The TCP protocol is newline-delimited text:

1. Client sends a prompt (single line, newline-terminated)
2. Server streams the agent's response line by line
3. Server sends `---END---` to signal response complete
4. Client can send another prompt or disconnect

## How it works

1. Reads `agent.yaml` to determine which CLI agent to use
2. Listens on a TCP port
3. For each connection, reads newline-delimited prompts
4. Spawns (flag mode) or writes to (stdin mode) the CLI agent
5. Streams the agent's stdout back over the TCP connection
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
