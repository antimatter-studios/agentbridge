package main

import "fmt"

// AgentConfig defines how to spawn and communicate with a single CLI agent.
// All agents use session-based conversation — each prompt spawns a CLI
// invocation with session continuation flags.
type AgentConfig struct {
	Command     string
	Args        []string
	InputFlag   string // flag that precedes the prompt (e.g. "-p")
	SessionFlag string // flag for session ID (e.g. "--session-id")
	ResumeFlag  string // flag to resume a session (e.g. "--resume")
	Workdir     string
	Env         map[string]string
}

// Config is the top-level agent-bridge configuration.
type Config struct {
	Active string
	Agents map[string]AgentConfig
}

// builtinAgents are the known agent configurations baked into the binary.
var builtinAgents = map[string]AgentConfig{
	"claude": {
		Command: "claude",
		Args: []string{
			"--print",
			"--output-format", "stream-json",
			"--model", "claude-sonnet-4-6",
		},
		InputFlag:   "-p",
		SessionFlag: "--session-id",
		ResumeFlag:  "--resume",
		Workdir:     "/workspace",
		Env:         map[string]string{"CI": "1"},
	},
	"codex": {
		Command:   "codex",
		Args:      []string{"--quiet"},
		InputFlag: "",
		Workdir:   "/workspace",
		Env:       map[string]string{},
	},
	"opencode": {
		Command:   "opencode",
		Args:      []string{},
		InputFlag: "",
		Workdir:   "/workspace",
		Env:       map[string]string{},
	},
}

// LoadBuiltinConfig returns a Config with baked-in agents, selecting the given one as active.
func LoadBuiltinConfig(active string) (*Config, error) {
	if _, ok := builtinAgents[active]; !ok {
		known := make([]string, 0, len(builtinAgents))
		for k := range builtinAgents {
			known = append(known, k)
		}
		return nil, fmt.Errorf("unknown agent %q (known: %v)", active, known)
	}

	return &Config{
		Active: active,
		Agents: builtinAgents,
	}, nil
}

// ActiveAgent returns the configuration for the currently active agent.
func (c *Config) ActiveAgent() AgentConfig {
	return c.Agents[c.Active]
}
