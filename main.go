// agent-bridge — TCP-to-CLI bridge for AI coding assistants.
//
// Exposes AI coding assistants (Claude Code, Codex, OpenCode) as
// network-accessible services. Listens on a TCP port, accepts newline-
// delimited messages, forwards them to the configured CLI agent, and
// streams responses back over the connection.
//
// Input and output are fully async — inbound prompts are queued
// immediately (with QUEUED acknowledgement) while the agent processes
// them one at a time. Responses stream back without blocking input.
//
// Usage:
//
//	agent-bridge --listen :9999 --config agent.yaml
//
// Flags:
//
//	--listen   Address to listen on (default ":9999")
//	--config   Path to agent config YAML (default "agent.yaml")
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
)

var version = "dev"

func main() {
	listen := flag.String("listen", ":9999", "Address to listen on")
	configPath := flag.String("config", "agent.yaml", "Path to agent config YAML")
	sessionID := flag.String("session", "", "Session ID for conversation continuity (default: auto-generated)")
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
	log.Printf("agent-bridge: active agent=%q command=%q input_mode=%q", cfg.Active, agent.Command, agent.InputMode)

	// Default session ID if not provided.
	sid := *sessionID
	if sid == "" {
		sid = fmt.Sprintf("ab-%d", os.Getpid())
	}

	bridge := NewBridge(agent, sid)
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

	log.Printf("agent-bridge listening on %s", *listen)

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
// Multiple goroutines (input reader, output streamer) write to the same
// connection — this serialises their writes.
type connWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (cw *connWriter) Write(p []byte) (int, error) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return cw.conn.Write(p)
}

func (cw *connWriter) WriteString(s string) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	fmt.Fprint(cw.conn, s)
}

func (cw *connWriter) WriteLine(s string) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	fmt.Fprintln(cw.conn, s)
}

// handleConn manages a TCP connection with async input/output.
// Input goroutine: reads lines, queues them immediately, sends QUEUED ack.
// Output: responses stream back via the connWriter as the worker processes jobs.
func handleConn(conn net.Conn, bridge *Bridge) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	log.Printf("connection from %s", remote)

	cw := &connWriter{conn: conn}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB max message

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Control commands — queued like prompts for sequential processing.
		if strings.HasPrefix(line, "/") {
			parts := strings.Fields(line)
			log.Printf("[%s] command: %s", remote, line)

			pos := bridge.QueueDepth()
			cw.WriteLine(fmt.Sprintf("QUEUED pos=%d", pos))

			go func(cmd string, args []string) {
				if err := bridge.HandleCommand(cmd, args, cw); err != nil {
					log.Printf("[%s] command error: %v", remote, err)
					cw.WriteLine(fmt.Sprintf("ERROR: %v", err))
				}
				cw.WriteLine("---END---")
			}(parts[0], parts[1:])
			continue
		}

		log.Printf("[%s] prompt: %s", remote, truncate(line, 80))

		// Acknowledge immediately — don't block on agent response.
		pos := bridge.QueueDepth()
		cw.WriteLine(fmt.Sprintf("QUEUED pos=%d", pos))

		go func(prompt string) {
			if err := bridge.Send(prompt, cw); err != nil {
				log.Printf("[%s] agent error: %v", remote, err)
				cw.WriteLine(fmt.Sprintf("ERROR: %v", err))
			}
			cw.WriteLine("---END---")
		}(line)
	}

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
