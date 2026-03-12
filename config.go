package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level agent-bridge configuration.
type Config struct {
	Active string                 `yaml:"active"`
	Agents map[string]AgentConfig `yaml:"agents"`
}

// AgentConfig defines how to spawn and communicate with a single CLI agent.
type AgentConfig struct {
	Command    string            `yaml:"command"`
	Args       []string          `yaml:"args"`
	InputMode  string            `yaml:"input_mode"`  // "stdin" or "flag"
	InputFlag  string            `yaml:"input_flag"`   // used when input_mode is "flag"
	SessionFlag string           `yaml:"session_flag"` // flag for session ID (e.g. "--session-id")
	ResumeFlag string            `yaml:"resume_flag"`  // flag to resume a session (e.g. "--resume")
	Workdir    string            `yaml:"workdir"`
	Env        map[string]string `yaml:"env"`
}

// LoadConfig reads and parses the YAML config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Active == "" {
		return nil, fmt.Errorf("config: 'active' agent not specified")
	}
	if _, ok := cfg.Agents[cfg.Active]; !ok {
		return nil, fmt.Errorf("config: active agent %q not found in agents", cfg.Active)
	}

	return &cfg, nil
}

// ActiveAgent returns the configuration for the currently active agent.
func (c *Config) ActiveAgent() AgentConfig {
	return c.Agents[c.Active]
}
