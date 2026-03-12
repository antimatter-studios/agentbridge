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

// job represents a queued prompt or command to be processed sequentially.
type job struct {
	prompt  string   // empty for commands
	command string   // e.g. "/reset", empty for prompts
	args    []string // command arguments
	out     io.Writer
	done    chan error // signals completion; nil error = success
}

// Bridge manages a CLI agent process and serialises all prompts through a queue.
type Bridge struct {
	cfg       AgentConfig
	sessionID string

	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	prompted bool // true after the first prompt (for resume logic)

	queue chan job
}

// NewBridge creates a bridge for the given agent config and session ID.
func NewBridge(cfg AgentConfig, sessionID string) *Bridge {
	return &Bridge{
		cfg:       cfg,
		sessionID: sessionID,
		queue:     make(chan job, 64), // buffered queue for up to 64 pending jobs
	}
}

// Start spawns the agent process (if needed) and starts the queue worker.
func (b *Bridge) Start() error {
	if b.cfg.InputMode == "flag" {
		log.Printf("bridge: agent %q configured in flag mode — will spawn per request", b.cfg.Command)
	} else {
		if err := b.spawnPersistent(); err != nil {
			return err
		}
	}

	go b.worker()
	return nil
}

// worker processes jobs sequentially from the queue.
func (b *Bridge) worker() {
	for j := range b.queue {
		if j.command != "" {
			result := b.handleCommand(j.command, j.args)
			fmt.Fprint(j.out, result)
			j.done <- nil
		} else {
			err := b.send(j.prompt, j.out)
			j.done <- err
		}
	}
}

// Send queues a prompt and blocks until the agent has finished responding.
func (b *Bridge) Send(prompt string, out io.Writer) error {
	done := make(chan error, 1)
	b.queue <- job{prompt: prompt, out: out, done: done}
	return <-done
}

// HandleCommand queues a command and blocks until it's processed.
func (b *Bridge) HandleCommand(cmd string, args []string, out io.Writer) error {
	done := make(chan error, 1)
	b.queue <- job{command: cmd, args: args, out: out, done: done}
	return <-done
}

// QueueDepth returns the number of pending jobs in the queue.
func (b *Bridge) QueueDepth() int {
	return len(b.queue)
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

// send dispatches to the appropriate mode. Called only from the worker goroutine.
func (b *Bridge) send(prompt string, out io.Writer) error {
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

// handleCommand processes a slash command. Called only from the worker goroutine.
func (b *Bridge) handleCommand(cmd string, args []string) string {
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

	queued := b.QueueDepth()

	switch b.cfg.InputMode {
	case "flag":
		return fmt.Sprintf("OK mode=flag agent=%s session=%s prompted=%v queued=%d\n",
			b.cfg.Command, b.sessionID, b.prompted, queued)
	case "stdin":
		pid := 0
		if b.cmd != nil && b.cmd.Process != nil {
			pid = b.cmd.Process.Pid
		}
		return fmt.Sprintf("OK mode=stdin agent=%s pid=%d prompted=%v queued=%d\n",
			b.cfg.Command, pid, b.prompted, queued)
	default:
		return fmt.Sprintf("OK mode=%s agent=%s queued=%d\n", b.cfg.InputMode, b.cfg.Command, queued)
	}
}

func (b *Bridge) cmdHelp() string {
	help := "/reset          — reset agent context (new session or restart process)\n"
	help += "/session        — show current session ID (flag mode only)\n"
	help += "/session <id>   — switch to a specific session ID (flag mode only)\n"
	help += "/status         — show bridge status (mode, agent, session/pid, queue depth)\n"
	help += "/help           — show this help\n"
	return help
}

// Stop terminates the agent process if running.
func (b *Bridge) Stop() {
	close(b.queue) // stop the worker

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
