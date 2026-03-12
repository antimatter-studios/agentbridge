package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Bridge manages a persistent CLI agent process and pipes messages to/from it.
type Bridge struct {
	cfg       AgentConfig
	sessionID string

	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	prompted  bool // true after the first prompt (for resume logic)
}

// NewBridge creates a bridge for the given agent config and session ID.
func NewBridge(cfg AgentConfig, sessionID string) *Bridge {
	return &Bridge{cfg: cfg, sessionID: sessionID}
}

// Start spawns the agent process in persistent/interactive mode.
// For "stdin" input_mode, the process stays alive and accepts prompts on stdin.
// For "flag" input_mode, each prompt spawns a new process invocation.
func (b *Bridge) Start() error {
	if b.cfg.InputMode == "flag" {
		// Flag mode doesn't need a persistent process.
		log.Printf("bridge: agent %q configured in flag mode — will spawn per request", b.cfg.Command)
		return nil
	}

	return b.spawnPersistent()
}

func (b *Bridge) spawnPersistent() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	cmd := exec.Command(b.cfg.Command, b.cfg.Args...)
	cmd.Dir = b.cfg.Workdir
	cmd.Env = buildEnv(b.cfg.Env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}

	b.cmd = cmd
	b.stdin = stdin
	b.stdout = stdout

	log.Printf("bridge: started persistent agent %q (pid %d)", b.cfg.Command, cmd.Process.Pid)
	return nil
}

// Send sends a prompt to the agent and streams the response to the writer.
// For flag mode, it spawns a new process per prompt.
// For stdin mode, it writes to the persistent process.
func (b *Bridge) Send(prompt string, out io.Writer) error {
	if b.cfg.InputMode == "flag" {
		return b.sendFlag(prompt, out)
	}
	return b.sendStdin(prompt, out)
}

func (b *Bridge) sendFlag(prompt string, out io.Writer) error {
	args := make([]string, len(b.cfg.Args))
	copy(args, b.cfg.Args)

	// Add session ID if configured.
	if b.cfg.SessionFlag != "" && b.sessionID != "" {
		args = append(args, b.cfg.SessionFlag, b.sessionID)
	}

	// Add resume flag on subsequent prompts (session already exists).
	if b.prompted && b.cfg.ResumeFlag != "" {
		args = append(args, b.cfg.ResumeFlag)
	}

	if b.cfg.InputFlag != "" {
		args = append(args, b.cfg.InputFlag, prompt)
	} else {
		args = append(args, prompt)
	}

	cmd := exec.Command(b.cfg.Command, args...)
	cmd.Dir = b.cfg.Workdir
	cmd.Env = buildEnv(b.cfg.Env)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}

	log.Printf("bridge: spawned %q session=%q resume=%v prompt=(%d chars)",
		b.cfg.Command, b.sessionID, b.prompted, len(prompt))

	// Stream output line by line.
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB line buffer
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(out, line)
	}

	b.prompted = true
	return cmd.Wait()
}

func (b *Bridge) sendStdin(prompt string, out io.Writer) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.stdin == nil {
		return fmt.Errorf("agent process not running")
	}

	// Write the prompt followed by a newline.
	if _, err := fmt.Fprintln(b.stdin, prompt); err != nil {
		return fmt.Errorf("write to agent: %w", err)
	}

	// Read response lines until we get an empty line or EOF.
	scanner := bufio.NewScanner(b.stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(out, line)
		// Empty line signals end of response for interactive mode.
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	return scanner.Err()
}

// SessionID returns the current session ID.
func (b *Bridge) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

// HasPrompted returns whether at least one prompt has been sent.
func (b *Bridge) HasPrompted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.prompted
}

// ResetSession switches to a new session ID. If id is empty, generates one.
// Returns the new session ID.
func (b *Bridge) ResetSession(id string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id == "" {
		id = fmt.Sprintf("ab-%d-%d", os.Getpid(), time.Now().Unix())
	}
	b.sessionID = id
	b.prompted = false
	return id
}

// Stop terminates the agent process if running.
func (b *Bridge) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cmd != nil && b.cmd.Process != nil {
		log.Printf("bridge: stopping agent (pid %d)", b.cmd.Process.Pid)
		b.stdin.Close()
		b.cmd.Process.Kill()
		b.cmd.Wait()
		b.cmd = nil
	}
}

// buildEnv merges the current environment with agent-specific overrides.
func buildEnv(overrides map[string]string) []string {
	env := os.Environ()
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}
