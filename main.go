// agent-bridge — TCP-to-CLI bridge for AI coding assistants.
//
// Exposes AI coding assistants (Claude Code, Codex, OpenCode) as
// network-accessible services. All prompts use session IDs for
// conversation continuity. Jobs are persisted in SQLite so nothing
// is lost on crash/restart.
//
// Usage:
//
//	agent-bridge --listen :9999 --config agent.yaml --session ws-abc123
//
// Flags:
//
//	--listen   Address to listen on (default ":9999")
//	--config   Path to agent config YAML (default "agent.yaml")
//	--session  Session ID for conversation continuity
//	--db       Path to SQLite database (default "agent-bridge.db")
//	--version  Print version and exit
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	listen := flag.String("listen", ":9999", "Address to listen on")
	configPath := flag.String("config", "agent.yaml", "Path to agent config YAML")
	sessionID := flag.String("session", "", "Session ID for conversation continuity (default: auto-generated)")
	dbPath := flag.String("db", "agent-bridge.db", "Path to SQLite database")
	showVersion := flag.Bool("version", false, "Print version and exit")

	flag.Parse()

	if *showVersion {
		fmt.Println("agent-bridge", version)
		os.Exit(0)
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	agent := cfg.ActiveAgent()
	log.Printf("agent-bridge: active agent=%q command=%q", cfg.Active, agent.Command)

	// Default session ID if not provided.
	sid := *sessionID
	if sid == "" {
		sid = fmt.Sprintf("ab-%d", os.Getpid())
	}

	store, err := NewStore(*dbPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer store.Close()

	bridge := NewBridge(agent, sid, store)
	if err := bridge.Start(); err != nil {
		log.Fatalf("bridge start: %v", err)
	}
	defer bridge.Stop()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	log.Printf("agent-bridge listening on %s (session=%s, db=%s)", *listen, sid, *dbPath)

	// Accept connections in a loop.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					log.Printf("accept: %v", err)
					continue
				}
			}
			go handleConn(conn, bridge)
		}
	}()

	<-ctx.Done()
	log.Println("agent-bridge: shutting down")
}

// connWriter is a thread-safe writer for a TCP connection.
type connWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (cw *connWriter) Write(p []byte) (int, error) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return cw.conn.Write(p)
}

func (cw *connWriter) writeLine(s string) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	fmt.Fprintln(cw.conn, s)
}

// handleConn manages a TCP connection with async input/output.
// Inbound messages are queued immediately with a QUEUED acknowledgement.
// A per-connection goroutine polls for completed jobs and streams results back.
func handleConn(conn net.Conn, bridge *Bridge) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	log.Printf("connection from %s", remote)

	cw := &connWriter{conn: conn}

	// Track job IDs submitted by this connection for response streaming.
	var pendingMu sync.Mutex
	pendingJobs := make([]int64, 0)

	// Response streamer — polls for completed jobs and sends results back.
	stopStreamer := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-stopStreamer:
				return
			case <-ticker.C:
				pendingMu.Lock()
				if len(pendingJobs) == 0 {
					pendingMu.Unlock()
					continue
				}
				jobID := pendingJobs[0]
				pendingMu.Unlock()

				job, err := bridge.store.GetJob(jobID)
				if err != nil || job == nil {
					continue
				}

				if job.Status == StatusDone || job.Status == StatusError {
					if job.Response != "" {
						cw.Write([]byte(job.Response))
					}
					if job.Status == StatusError && job.Error != "" {
						cw.writeLine(fmt.Sprintf("ERROR: %s", job.Error))
					}
					cw.writeLine("---END---")

					pendingMu.Lock()
					pendingJobs = pendingJobs[1:]
					pendingMu.Unlock()
				}
			}
		}
	}()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var jobID int64
		var err error

		if strings.HasPrefix(line, "/") {
			parts := strings.Fields(line)
			log.Printf("[%s] command: %s", remote, line)
			jobID, err = bridge.EnqueueCommand(parts[0], parts[1:])
		} else {
			log.Printf("[%s] prompt: %s", remote, truncate(line, 80))
			jobID, err = bridge.Enqueue(line)
		}

		if err != nil {
			log.Printf("[%s] enqueue error: %v", remote, err)
			cw.writeLine(fmt.Sprintf("ERROR: %v", err))
			continue
		}

		pendingMu.Lock()
		pendingJobs = append(pendingJobs, jobID)
		pendingMu.Unlock()

		cw.writeLine(fmt.Sprintf("QUEUED id=%d pos=%d", jobID, bridge.QueueDepth()))
	}

	close(stopStreamer)

	if err := scanner.Err(); err != nil {
		log.Printf("[%s] read error: %v", remote, err)
	}
	log.Printf("[%s] disconnected", remote)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
