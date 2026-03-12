package main

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Wire protocol: [4 bytes: payload length][1 byte: message type][N bytes: payload]
// All integers are big-endian. Maximum payload size is 16MB.

const maxPayloadSize = 16 * 1024 * 1024 // 16MB

// Message types — client to server.
const (
	MsgPrompt  byte = 0x01 // Send a prompt to the agent
	MsgCommand byte = 0x02 // Send a slash command (e.g. /reset)
)

// Message types — server to client.
const (
	MsgQueued       byte = 0x10 // Job queued acknowledgement (payload: JSON {id, pos})
	MsgResponseLine byte = 0x11 // A line of agent response
	MsgResponseEnd  byte = 0x12 // Response complete for a job (payload: JSON {id})
	MsgError        byte = 0x13 // Error message
)

// Message represents a framed protocol message.
type Message struct {
	Type    byte
	Payload []byte
}

// WriteMessage writes a framed message to the writer.
func WriteMessage(w io.Writer, msg Message) error {
	length := uint32(len(msg.Payload))
	if length > maxPayloadSize {
		return fmt.Errorf("payload too large: %d > %d", length, maxPayloadSize)
	}

	// Write length (4 bytes, big-endian).
	if err := binary.Write(w, binary.BigEndian, length); err != nil {
		return fmt.Errorf("write length: %w", err)
	}

	// Write type (1 byte).
	if _, err := w.Write([]byte{msg.Type}); err != nil {
		return fmt.Errorf("write type: %w", err)
	}

	// Write payload.
	if length > 0 {
		if _, err := w.Write(msg.Payload); err != nil {
			return fmt.Errorf("write payload: %w", err)
		}
	}

	return nil
}

// ReadMessage reads a framed message from the reader.
func ReadMessage(r io.Reader) (Message, error) {
	// Read length (4 bytes, big-endian).
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return Message{}, fmt.Errorf("read length: %w", err)
	}

	if length > maxPayloadSize {
		return Message{}, fmt.Errorf("payload too large: %d > %d", length, maxPayloadSize)
	}

	// Read type (1 byte).
	typeBuf := make([]byte, 1)
	if _, err := io.ReadFull(r, typeBuf); err != nil {
		return Message{}, fmt.Errorf("read type: %w", err)
	}

	// Read payload.
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Message{}, fmt.Errorf("read payload: %w", err)
		}
	}

	return Message{Type: typeBuf[0], Payload: payload}, nil
}
