package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

type JsonMessage struct {
	Length uint32
	Data   []byte
}

func writeJsonMessage(conn net.Conn, v interface{}) error {
	// Marshal the data
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}

	// Write length prefix
	if err := binary.Write(conn, binary.LittleEndian, uint32(len(data))); err != nil {
		return fmt.Errorf("write length: %w", err)
	}

	// Write data
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("write data: %w", err)
	}

	return nil
}

func readJsonMessage(conn net.Conn, v interface{}) error {
	// Read length prefix
	var length uint32
	if err := binary.Read(conn, binary.LittleEndian, &length); err != nil {
		return fmt.Errorf("read length: %w", err)
	}

	// Sanity check on length
	if length > 1024*1024 { // 1MB max message size
		return fmt.Errorf("message too large: %d bytes", length)
	}

	// Read exact number of bytes
	data := make([]byte, length)
	if _, err := io.ReadFull(conn, data); err != nil {
		return fmt.Errorf("read data: %w", err)
	}

	// Unmarshal data
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("unmarshal json: %w", err)
	}

	return nil
}
