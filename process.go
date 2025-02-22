package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Initialize configuration for processor
const (
	maxConcurrentConnections = 1000
	chunkSize                = 1 << 20 // 1MB chunk
	maxFileSize              = 1 << 32 // 4GB
	readTimeout              = 30 * time.Second
	writeTimeout             = 30 * time.Second
	maxRetries               = 3
	heartbeatInterval        = 5 * time.Second
)

type TransferMetadata struct {
	FileID         string         `json:"file_id"`
	FileName       string         `json:"file_name"`
	FileSize       int64          `json:"file_size"`
	ChunkSize      int            `json:"chunk_size"`
	TotalChunks    int            `json:"total_chunks"`
	Checksum       string         `json:"checksum"`
	ChunksReceived map[int]string `json:"chunks_received"`
	StartTime      time.Time      `json:"start_time"`
	LastUpdated    time.Time      `json:"last_updated"`
}

type TransferRequest struct {
	Type     string `json:"type"` // "new" or "resume"
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	FileSize int64  `json:"file_size"`
	Checksum string `json:"checksum"`
}

type TransferResponse struct {
	Status         string `json:"status"`
	FileID         string `json:"file_id"`
	ResumePosition int64  `json:"resume_position"`
	Error          string `json:"error,ommitempty"`
}

type RealFileServer struct {
	activeTransfers sync.Map
	bufferPool      sync.Pool
	tempDir         string
}

func NewRealFileServer(tempDir string) *RealFileServer {
	return &RealFileServer{
		tempDir: tempDir,
		bufferPool: sync.Pool{
			New: func() interface{} {
				return make([]byte, chunkSize)
			},
		},
	}
}

func (fs *RealFileServer) Start(ctx context.Context, serverAddr string, ready chan<- struct{}) error {
	listener, err := net.Listen("tcp", serverAddr)
	if err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	defer listener.Close()

	log.Printf("Server listening on %s", serverAddr)
	ready <- struct{}{} // Signal that server is ready

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("Accept error: %v", err)
				continue
			}
			go fs.handleConnection(ctx, conn)
		}
	}
}

func (fs *RealFileServer) handleConnection(ctx context.Context, conn net.Conn) error {
	defer conn.Close()
	log.Printf("New connection from %s", conn.RemoteAddr())

	// Set initial timeout for reading request
	if err := conn.SetReadDeadline(time.Now().UTC().Add(readTimeout)); err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}

	var req TransferRequest
	if err := readJsonMessage(conn, &req); err != nil {
		return fmt.Errorf("read request: %w", err)
	}

	var err error
	switch req.Type {
	case "new":
		err = fs.handleNewTransfer(ctx, conn, req)
	case "resume":
		err = fs.handleResumeTransfer(ctx, conn, req)
	default:
		return fmt.Errorf("unknown request type: %s", req.Type)
	}

	if err != nil {
		log.Printf("Transfer failed: %v\n", err.Error())
		return err
	}

	log.Printf("Transfer completed successfully")
	return nil
}

func (fs *RealFileServer) handleNewTransfer(ctx context.Context, conn net.Conn, req TransferRequest) error {
	metadata := &TransferMetadata{
		FileID:         req.FileID,
		FileName:       req.FileName,
		FileSize:       req.FileSize,
		ChunkSize:      chunkSize,
		TotalChunks:    int((req.FileSize + int64(chunkSize) - 1) / int64(chunkSize)),
		Checksum:       req.Checksum,
		ChunksReceived: make(map[int]string),
		StartTime:      time.Now().UTC(),
		LastUpdated:    time.Now().UTC(),
	}

	// Store metadata
	fs.activeTransfers.Store(req.FileID, metadata)

	// Create temporary file
	tempPath := filepath.Join(fs.tempDir, req.FileName)
	file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		return fmt.Errorf("create temp file: %v", err)
	}

	defer file.Close()

	// Send response
	res := TransferResponse{
		Status: "ready",
		FileID: req.FileID,
	}
	if err := writeJsonMessage(conn, res); err != nil {
		return fmt.Errorf("send response: %w", err)
	}

	return fs.receiveFileChunks(ctx, conn, file, metadata)
}

func (fs *RealFileServer) handleResumeTransfer(ctx context.Context, conn net.Conn, req TransferRequest) error {
	iMetadata, exists := fs.activeTransfers.Load(req.FileID)
	if !exists {
		res := TransferResponse{
			Status: "not found",
			FileID: req.FileID,
			Error:  "transfer not found",
		}
		// return json.NewEncoder(conn).Encode(res)
		return writeJsonMessage(conn, res)
	}

	metadata := iMetadata.(*TransferMetadata)
	tempPath := filepath.Join(fs.tempDir, req.FileName)
	file, err := os.OpenFile(tempPath, os.O_WRONLY, 0666)
	if err != nil {
		return fmt.Errorf("open temp file: %w", err)
	}
	defer file.Close()

	// Calculate resume position
	resumePosition := fs.findLastContiguousChunk(metadata) * int64(metadata.ChunkSize)

	res := TransferResponse{
		Status:         "resume",
		FileID:         req.FileID,
		ResumePosition: resumePosition,
	}

	if err := writeJsonMessage(conn, res); err != nil {
		return fmt.Errorf("send response: %w", err)
	}

	return fs.receiveFileChunks(ctx, conn, file, metadata)
}

func (fs *RealFileServer) findLastContiguousChunk(metadata *TransferMetadata) int64 {
	lastContiguous := int64(-1)

	// Check each chunk sequentially until we find a gap
	for i := 0; i < metadata.TotalChunks; i++ {
		if _, exists := metadata.ChunksReceived[i]; !exists {
			break
		}
		lastContiguous = int64(i)
	}

	return lastContiguous + 1 // Return next chunk to receive
}

func (fs *RealFileServer) receiveFileChunks(ctx context.Context, conn net.Conn, file *os.File, metadata *TransferMetadata) error {
	buf := fs.bufferPool.Get().([]byte)
	defer fs.bufferPool.Put(buf)

	hasher := sha256.New()
	var retryCount int

	for chunkNum := 0; chunkNum < metadata.TotalChunks; chunkNum++ {
		// Skip already received chunks
		if _, exists := metadata.ChunksReceived[chunkNum]; exists {
			continue
		}

		// Read chunk size
		if err := conn.SetReadDeadline(time.Now().UTC().Add(readTimeout)); err != nil {
			return err
		}

		var chunkSize int32
		if err := binary.Read(conn, binary.LittleEndian, &chunkSize); err != nil {
			if retryCount >= maxRetries {
				return fmt.Errorf("max retries exceeded: %w", err)
			}

			retryCount++
			chunkNum--
			continue
		}

		// Read chunk data
		if err := conn.SetReadDeadline(time.Now().UTC().Add(readTimeout)); err != nil {
			return err
		}

		offset := int64(chunkNum) * int64(chunkSize)
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return fmt.Errorf("write chunk: %w", err)
		}

		n, err := io.ReadFull(conn, buf[:chunkSize])
		if err != nil {
			if retryCount >= maxRetries {
				return fmt.Errorf("max retries exceeded: %w", err)
			}

			retryCount++
			chunkNum--
			continue
		}

		// Calculate chunk checksum
		hasher.Reset()
		hasher.Write(buf[:n])
		chunkChecksum := hex.EncodeToString(hasher.Sum(nil))

		// Write chunk to file
		if _, err := file.Write(buf[:n]); err != nil {
			return fmt.Errorf("write chunk: %w", err)
		}

		// Udpate metadata
		metadata.ChunksReceived[chunkNum] = chunkChecksum
		metadata.LastUpdated = time.Now().UTC()
		fs.activeTransfers.Store(metadata.FileID, metadata)

		// Reset retry count on successful data
		retryCount = 0

		// Send chunk acknowledge
		if err := conn.SetWriteDeadline(time.Now().UTC().Add(writeTimeout)); err != nil {
			return err
		}

		ack := struct {
			ChunkNum int32
		}{
			ChunkNum: int32(chunkNum),
		}

		if err := binary.Write(conn, binary.LittleEndian, ack.ChunkNum); err != nil {
			return fmt.Errorf("send ack: %w", err)
		}
	}

	// Verify complete file checksum
	if err := fs.verifyFileChecksum(metadata); err != nil {
		return fmt.Errorf("check verification: %w", err)
	}

	fs.activeTransfers.Delete(metadata.FileID)
	return nil
}

func (fs *RealFileServer) verifyFileChecksum(metadata *TransferMetadata) error {
	// Rewind file to beginning
	file, err := os.Open(filepath.Join(fs.tempDir, metadata.FileName))
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return fmt.Errorf("copy to hasher: %w", err)
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))
	if checksum != metadata.Checksum {
		return errors.New("file checksum mismatch")
	}

	return nil
}
