// agent-bridge — TCP-to-CLI bridge for AI coding assistants.
//
// Exposes AI coding assistants (Claude Code, Codex, OpenCode) as
// network-accessible services using a binary framed protocol. All
// prompts use session IDs for conversation continuity. Jobs are
// persisted in SQLite so nothing is lost on crash/restart.
//
// Wire protocol: [4 bytes: length][1 byte: type][N bytes: payload]
//
// Usage:
//
//	agent-bridge --listen :9999 --agent claude --session ws-abc123
package main

import (
	"bufio"
	"context"
	"encoding/json"
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
	agentName := flag.String("agent", "claude", "Agent to use (claude, codex, opencode)")
	sessionID := flag.String("session", "", "Session ID for conversation continuity (default: auto-generated)")
	dbPath := flag.String("db", "agent-bridge.db", "Path to SQLite database")
	showVersion := flag.Bool("version", false, "Print version and exit")

	flag.Parse()

	if *showVersion {
		fmt.Println("agent-bridge", version)
		os.Exit(0)
	}

	cfg, err := LoadBuiltinConfig(*agentName)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	agent := cfg.ActiveAgent()
	log.Printf("agent-bridge: active agent=%q command=%q", cfg.Active, agent.Command)

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

// protoWriter sends binary-framed messages to a TCP connection (thread-safe).
type protoWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (pw *protoWriter) send(msgType byte, payload []byte) error {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	return WriteMessage(pw.conn, Message{Type: msgType, Payload: payload})
}

func (pw *protoWriter) sendJSON(msgType byte, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return pw.send(msgType, data)
}

// queuedAck is the QUEUED acknowledgement payload.
type queuedAck struct {
	ID       int64 `json:"id"`
	Position int   `json:"pos"`
}

// responseEnd is the end-of-response payload.
type responseEnd struct {
	ID int64 `json:"id"`
}

// errorPayload is the error message payload.
type errorPayload struct {
	ID      int64  `json:"id,omitempty"`
	Message string `json:"message"`
}

// handleConn manages a TCP connection using the binary framed protocol.
func handleConn(conn net.Conn, bridge *Bridge) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	log.Printf("connection from %s", remote)

	pw := &protoWriter{conn: conn}
	reader := bufio.NewReader(conn)

	// Track pending jobs for this connection.
	var pendingMu sync.Mutex
	pendingJobs := make([]int64, 0)

	// Response streamer — polls for completed jobs and sends results.
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
					// Send response content.
					if job.Response != "" {
						pw.send(MsgResponseLine, []byte(job.Response))
					}
					if job.Status == StatusError && job.Error != "" {
						pw.sendJSON(MsgError, errorPayload{ID: jobID, Message: job.Error})
					}
					// Signal response complete.
					pw.sendJSON(MsgResponseEnd, responseEnd{ID: jobID})

					pendingMu.Lock()
					pendingJobs = pendingJobs[1:]
					pendingMu.Unlock()
				}
			}
		}
	}()

	// Read binary-framed messages.
	for {
		msg, err := ReadMessage(reader)
		if err != nil {
			if err.Error() != "read length: EOF" {
				log.Printf("[%s] read error: %v", remote, err)
			}
			break
		}

		var jobID int64

		switch msg.Type {
		case MsgPrompt:
			prompt := string(msg.Payload)
			log.Printf("[%s] prompt: %s", remote, truncate(prompt, 80))
			jobID, err = bridge.Enqueue(prompt)

		case MsgCommand:
			cmdLine := string(msg.Payload)
			parts := strings.Fields(cmdLine)
			if len(parts) == 0 {
				continue
			}
			log.Printf("[%s] command: %s", remote, cmdLine)
			jobID, err = bridge.EnqueueCommand(parts[0], parts[1:])

		default:
			log.Printf("[%s] unknown message type: 0x%02x", remote, msg.Type)
			pw.sendJSON(MsgError, errorPayload{Message: fmt.Sprintf("unknown message type: 0x%02x", msg.Type)})
			continue
		}

		if err != nil {
			log.Printf("[%s] enqueue error: %v", remote, err)
			pw.sendJSON(MsgError, errorPayload{Message: err.Error()})
			continue
		}

		pendingMu.Lock()
		pendingJobs = append(pendingJobs, jobID)
		pendingMu.Unlock()

		pw.sendJSON(MsgQueued, queuedAck{ID: jobID, Position: bridge.QueueDepth()})
	}

	close(stopStreamer)
	log.Printf("[%s] disconnected", remote)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
