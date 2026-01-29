package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"piccolod/internal/server"
)

var version = "dev"

func main() {
	// The main function is the entry point. Its only job is to
	// initialize and start the Gin-based server.
	srv, err := server.NewGinServer(server.WithGinVersion(version))
	if err != nil {
		log.Fatalf("FATAL: Failed to initialize server: %v", err)
	}

	// Set up signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Start the server in a goroutine
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start()
	}()

	// Wait for either a signal or an error from the server
	select {
	case sig := <-sigCh:
		log.Printf("INFO: Received signal %v, initiating graceful shutdown...", sig)
	case err := <-errCh:
		if err != nil {
			log.Fatalf("FATAL: Server failed: %v", err)
		}
		return
	}

	// Graceful shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := srv.Stop(ctx); err != nil {
		log.Printf("ERROR: Graceful shutdown failed: %v", err)
		os.Exit(1)
	}

	log.Printf("INFO: Graceful shutdown complete")
}
