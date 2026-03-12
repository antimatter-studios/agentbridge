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

// HandleCommand processes a slash command and returns the response text.
// Commands are agent-aware — behavior varies by input mode and agent type.
func (b *Bridge) HandleCommand(cmd string, args []string) string {
	switch cmd {
	case "/reset":
		return b.cmdReset(args)
	case "/session":
		return b.cmdSession(args)
	case "/status":
		return b.cmdStatus()
	case "/help":
		return b.cmdHelp()
	default:
		return fmt.Sprintf("ERROR: unknown command %q (try /help)\n", cmd)
	}
}

func (b *Bridge) cmdReset(args []string) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.cfg.InputMode {
	case "flag":
		// Flag mode: generate a new session ID so next prompt starts fresh.
		id := ""
		if len(args) > 0 {
			id = args[0]
		}
		if id == "" {
			id = fmt.Sprintf("ab-%d-%d", os.Getpid(), time.Now().Unix())
		}
		b.sessionID = id
		b.prompted = false
		return fmt.Sprintf("OK session=%s\n", id)

	case "stdin":
		// Stdin mode: kill the persistent process and respawn it.
		if b.cmd != nil && b.cmd.Process != nil {
			b.stdin.Close()
			b.cmd.Process.Kill()
			b.cmd.Wait()
			b.cmd = nil
		}
		b.prompted = false
		// Respawn without holding the lock (unlock, spawn, relock).
		b.mu.Unlock()
		err := b.spawnPersistent()
		b.mu.Lock()
		if err != nil {
			return fmt.Sprintf("ERROR: respawn failed: %v\n", err)
		}
		return "OK process restarted\n"

	default:
		return "ERROR: unknown input mode\n"
	}
}

func (b *Bridge) cmdSession(args []string) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cfg.InputMode != "flag" {
		return fmt.Sprintf("OK mode=%s (session IDs only apply to flag mode)\n", b.cfg.InputMode)
	}

	if len(args) == 0 {
		return fmt.Sprintf("OK session=%s\n", b.sessionID)
	}

	b.sessionID = args[0]
	b.prompted = false
	return fmt.Sprintf("OK session=%s\n", b.sessionID)
}

func (b *Bridge) cmdStatus() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.cfg.InputMode {
	case "flag":
		return fmt.Sprintf("OK mode=flag agent=%s session=%s prompted=%v\n",
			b.cfg.Command, b.sessionID, b.prompted)
	case "stdin":
		pid := 0
		if b.cmd != nil && b.cmd.Process != nil {
			pid = b.cmd.Process.Pid
		}
		return fmt.Sprintf("OK mode=stdin agent=%s pid=%d prompted=%v\n",
			b.cfg.Command, pid, b.prompted)
	default:
		return fmt.Sprintf("OK mode=%s agent=%s\n", b.cfg.InputMode, b.cfg.Command)
	}
}

func (b *Bridge) cmdHelp() string {
	help := "/reset          — reset agent context (new session or restart process)\n"
	help += "/session        — show current session ID (flag mode only)\n"
	help += "/session <id>   — switch to a specific session ID (flag mode only)\n"
	help += "/status         — show bridge status (mode, agent, session/pid)\n"
	help += "/help           — show this help\n"
	return help
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
