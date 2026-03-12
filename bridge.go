package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Bridge manages a CLI agent process with a persistent SQLite-backed job queue.
// All prompts use session IDs for conversation continuity — no one-shot mode.
type Bridge struct {
	cfg       AgentConfig
	sessionID string
	store     *Store

	mu       sync.Mutex
	prompted bool // true after the first prompt (for resume logic)

	queue chan struct{} // signals that new work is available
}

// NewBridge creates a bridge with a persistent store.
func NewBridge(cfg AgentConfig, sessionID string, store *Store) *Bridge {
	return &Bridge{
		cfg:       cfg,
		sessionID: sessionID,
		store:     store,
		queue:     make(chan struct{}, 64),
	}
}

// Start recovers any interrupted jobs and starts the queue worker.
func (b *Bridge) Start() error {
	// Recover jobs that were processing when we crashed.
	recovered, err := b.store.RecoverProcessing()
	if err != nil {
		return fmt.Errorf("recover jobs: %w", err)
	}
	if recovered > 0 {
		log.Printf("bridge: recovered %d interrupted jobs", recovered)
	}

	// Check if there are pending jobs from a previous run.
	pending, err := b.store.PendingCount()
	if err != nil {
		return fmt.Errorf("count pending: %w", err)
	}
	if pending > 0 {
		log.Printf("bridge: %d pending jobs from previous run", pending)
		// Signal the worker for each pending job.
		for range pending {
			b.queue <- struct{}{}
		}
	}

	go b.worker()
	return nil
}

// Enqueue adds a prompt to the persistent queue and returns its job ID.
func (b *Bridge) Enqueue(prompt string) (int64, error) {
	id, err := b.store.Enqueue("prompt", prompt, "")
	if err != nil {
		return 0, err
	}
	b.queue <- struct{}{} // wake the worker
	return id, nil
}

// EnqueueCommand adds a command to the persistent queue and returns its job ID.
func (b *Bridge) EnqueueCommand(cmd string, args []string) (int64, error) {
	argsJSON, _ := json.Marshal(args)
	id, err := b.store.Enqueue("command", cmd, string(argsJSON))
	if err != nil {
		return 0, err
	}
	b.queue <- struct{}{} // wake the worker
	return id, nil
}

// QueueDepth returns the number of pending jobs.
func (b *Bridge) QueueDepth() int {
	count, _ := b.store.PendingCount()
	return count
}

// worker processes jobs sequentially from the persistent queue.
func (b *Bridge) worker() {
	for range b.queue {
		j, err := b.store.NextPending()
		if err != nil {
			log.Printf("bridge: worker error fetching job: %v", err)
			continue
		}
		if j == nil {
			continue // spurious wake
		}

		log.Printf("bridge: processing job #%d type=%s", j.ID, j.Type)

		switch j.Type {
		case "prompt":
			if err := b.processPrompt(j); err != nil {
				log.Printf("bridge: job #%d failed: %v", j.ID, err)
				b.store.Fail(j.ID, err.Error())
			} else {
				b.store.Complete(j.ID)
			}
		case "command":
			b.processCommand(j)
			b.store.Complete(j.ID)
		}
	}
}

// processPrompt runs the agent CLI with session continuation.
func (b *Bridge) processPrompt(j *StoredJob) error {
	args := make([]string, len(b.cfg.Args))
	copy(args, b.cfg.Args)

	// Add session ID.
	if b.cfg.SessionFlag != "" && b.sessionID != "" {
		args = append(args, b.cfg.SessionFlag, b.sessionID)
	}

	// Resume on subsequent prompts.
	b.mu.Lock()
	shouldResume := b.prompted
	b.mu.Unlock()

	if shouldResume && b.cfg.ResumeFlag != "" {
		args = append(args, b.cfg.ResumeFlag)
	}

	// Add the prompt.
	if b.cfg.InputFlag != "" {
		args = append(args, b.cfg.InputFlag, j.Payload)
	} else {
		args = append(args, j.Payload)
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

	log.Printf("bridge: spawned %q session=%q resume=%v job=#%d",
		b.cfg.Command, b.sessionID, shouldResume, j.ID)

	// Stream output and persist it.
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text() + "\n"
		b.store.AppendResponse(j.ID, line)
		// Notify connected clients via the output broadcast (added later).
	}

	b.mu.Lock()
	b.prompted = true
	b.mu.Unlock()

	return cmd.Wait()
}

// processCommand handles a slash command.
func (b *Bridge) processCommand(j *StoredJob) {
	var args []string
	if j.Args != "" {
		json.Unmarshal([]byte(j.Args), &args)
	}

	result := b.handleCommand(j.Payload, args)
	b.store.AppendResponse(j.ID, result)
}

// handleCommand dispatches slash commands.
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
}

func (b *Bridge) cmdSession(args []string) string {
	b.mu.Lock()
	defer b.mu.Unlock()

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
	return fmt.Sprintf("OK agent=%s session=%s prompted=%v queued=%d\n",
		b.cfg.Command, b.sessionID, b.prompted, queued)
}

func (b *Bridge) cmdHelp() string {
	help := "/reset          — reset session (new ID, fresh context)\n"
	help += "/session        — show current session ID\n"
	help += "/session <id>   — switch to a specific session ID\n"
	help += "/status         — show bridge status\n"
	help += "/help           — show this help\n"
	return help
}

// Stop closes the queue channel. Pending jobs persist in SQLite for next startup.
func (b *Bridge) Stop() {
	close(b.queue)
}

// buildEnv merges the current environment with agent-specific overrides.
func buildEnv(overrides map[string]string) []string {
	env := os.Environ()
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}

// responseWriter wraps a store to implement io.Writer, persisting all output.
type responseWriter struct {
	store *Store
	jobID int64
}

func (rw *responseWriter) Write(p []byte) (int, error) {
	if err := rw.store.AppendResponse(rw.jobID, string(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

var _ io.Writer = (*responseWriter)(nil)
