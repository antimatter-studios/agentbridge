// agent-bridge — TCP-to-CLI bridge for AI coding assistants.
//
// Exposes AI coding assistants (Claude Code, Codex, OpenCode) as
// network-accessible services. Listens on a TCP port, accepts newline-
// delimited messages, forwards them to the configured CLI agent, and
// streams responses back over the connection.
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
	"syscall"
)

var version = "dev"

func main() {
	listen := flag.String("listen", ":9999", "Address to listen on")
	configPath := flag.String("config", "agent.yaml", "Path to agent config YAML")
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

	bridge := NewBridge(agent)
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

// handleConn reads newline-delimited prompts from a TCP connection
// and streams agent responses back.
func handleConn(conn net.Conn, bridge *Bridge) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	log.Printf("connection from %s", remote)

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB max message

	for scanner.Scan() {
		prompt := strings.TrimSpace(scanner.Text())
		if prompt == "" {
			continue
		}

		log.Printf("[%s] prompt: %s", remote, truncate(prompt, 80))

		if err := bridge.Send(prompt, conn); err != nil {
			log.Printf("[%s] agent error: %v", remote, err)
			fmt.Fprintf(conn, "ERROR: %v\n", err)
		}

		// Write a delimiter so the client knows the response is complete.
		fmt.Fprintln(conn, "---END---")
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
