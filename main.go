package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"os"
	"path/filepath"
)

const (
	serverAddr = ":3000"
)

// func main() {
// Demo example processing large file
// go func() {
// 	time.Sleep(4 * time.Second)
// 	sendFile(33 * 1024)
// }()
// server := &FileServer{}
// server.start()
// }

func main() {
	// Create temporary directories
	serverTemp, err := os.MkdirTemp(".", "server")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(serverTemp)

	clientTemp, err := os.MkdirTemp(".", "client")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(clientTemp)

	// Generate test file
	testFile := filepath.Join(clientTemp, "testfile.dat")
	testFileSize := int64(100 * 1024 * 1024) // 100MB test file

	log.Printf("Generating test file of size: %d bytes", testFileSize)
	checksum, err := generateTestFile(testFile, testFileSize)
	if err != nil {
		log.Fatal(err)
	}
	defer os.Remove(testFile)
	log.Printf("Generated test file: %s with checksum: %s", testFile, checksum)

	// Start server
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := NewRealFileServer(serverTemp)
	serverReady := make(chan struct{})

	go func() {
		if err := server.Start(ctx, serverAddr, serverReady); err != nil {
			log.Printf("Server error: %v", err)
		}
	}()

	// Wait for server to be ready
	<-serverReady

	// Start file transfer
	log.Printf("Starting file transfer...")
	if err := sendTempFile(ctx, testFile, serverAddr); err != nil {
		log.Fatal(err)
	}

	// Verify transfer
	serverFile := filepath.Join(serverTemp, filepath.Base(testFile))
	if _, err := os.Stat(serverFile); err != nil {
		log.Fatal("Transfer verification failed: file not found on server")
	}

	// Calculate received file checksum
	file, err := os.Open(serverFile)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		log.Fatal(err)
	}
	receivedChecksum := hex.EncodeToString(hasher.Sum(nil))

	if receivedChecksum != checksum {
		log.Fatal("Transfer verification failed: checksum mismatch")
	}

	log.Printf("Transfer completed and verified successfully")
	log.Printf("Original checksum: %s", checksum)
	log.Printf("Received checksum: %s", receivedChecksum)
}
