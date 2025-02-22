package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"
)

// Generate a test file with random content
func generateTestFile(filename string, size int64) (string, error) {
	file, err := os.Create(filename)
	if err != nil {
		return "", fmt.Errorf("create file: %w", err)
	}
	defer file.Close()

	// Create a buffer for random data
	buf := make([]byte, 32*1024) // 32KB buffer for writing
	remaining := size

	// Calculate hash while writing
	hasher := sha256.New()

	for remaining > 0 {
		n := int64(len(buf))
		if remaining < n {
			n = remaining
		}

		// Generate random data
		_, err := rand.Read(buf[:n])
		if err != nil {
			return "", fmt.Errorf("generate random data: %w", err)
		}

		// Write to file and hash
		_, err = file.Write(buf[:n])
		if err != nil {
			return "", fmt.Errorf("write to file: %w", err)
		}
		hasher.Write(buf[:n])

		remaining -= n
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// Client-side implementation
func sendTempFile(ctx context.Context, filepath string, serverAddr string) error {
	file, err := os.Open(filepath)
	if err != nil {
		return err
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return err
	}

	// Calculate file checksum
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return err
	}
	checksum := hex.EncodeToString(hasher.Sum(nil))

	// Generate unique file ID
	fileID := fmt.Sprintf("%s-%d", checksum[:8], time.Now().UnixNano())

	var attempt int
	for attempt < maxRetries {
		if err := attemptTransfer(ctx, file, fileInfo, fileID, checksum, serverAddr); err != nil {
			log.Printf("Transfer attempt %d failed: %v", attempt+1, err)
			attempt++
			time.Sleep(time.Second * time.Duration(attempt))
			continue
		}
		return nil
	}

	return fmt.Errorf("failed to transfer file after %d attempts", maxRetries)
}

func attemptTransfer(ctx context.Context, file *os.File, fileInfo os.FileInfo, fileID, checksum, serverAddr string) error {
	conn, err := net.Dial("tcp", serverAddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Send transfer request
	request := TransferRequest{
		Type:     "new",
		FileID:   fileID,
		FileName: fileInfo.Name(),
		FileSize: fileInfo.Size(),
		Checksum: checksum,
	}

	if err := writeJsonMessage(conn, request); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	// Read response
	var response TransferResponse
	if err := readJsonMessage(conn, &response); err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if response.Status != "ready" && response.Status != "resume" {
		return fmt.Errorf("unexpected server response: %s", response.Status)
	}

	// Send file chunks
	buf := make([]byte, chunkSize)
	offset := response.ResumePosition

	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("file seek failed: %w", err)
	}

	var sent int64

	for {
		n, err := file.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Send chunk size
		if err := binary.Write(conn, binary.LittleEndian, int32(n)); err != nil {
			return fmt.Errorf("binary failed: %w", err)
		}

		// Send chunk data
		written, err := conn.Write(buf[:n])
		if err != nil {
			return fmt.Errorf("write to connection: %w", err)
		}

		// Wait for acknowledgment
		if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return fmt.Errorf("set read deadline: %w", err)
		}
		var ackChunkNum int32
		if err := binary.Read(conn, binary.LittleEndian, &ackChunkNum); err != nil {
			return fmt.Errorf("sender ack to connection: %w", err)
		}
		sent += int64(written)
		log.Printf("Progress: %.2f%%", float64(sent)/float64(fileInfo.Size())*100)
	}

	return nil
}
