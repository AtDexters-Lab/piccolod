package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"piccolod/internal/pcv"
	"piccolod/internal/resources/pressure"
	"piccolod/internal/server"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--record-service-exit" {
		if err := recoverAndRecordServiceExit(
			pcv.RecoverPendingControlPlaneThaw,
			taskRecoveryMarkerPath,
			os.Getenv("SERVICE_RESULT"),
			os.Getenv("EXIT_STATUS"),
			os.Getenv("INVOCATION_ID"),
			time.Now(),
		); err != nil {
			log.Printf("ERROR: record service exit: %v", err)
			os.Exit(1)
		}
		return
	}
	processStartedAt := time.Now().UTC()
	if err := pcv.RecoverPendingControlPlaneThaw(); err != nil {
		log.Printf("ERROR: recover pending control-plane thaw before startup: %v", err)
		os.Exit(1)
	}

	previousMarker, recoveryMode, markerErr := loadTaskRecoveryMarker(taskRecoveryMarkerPath)
	if markerErr != nil {
		log.Printf("WARN: task recovery marker is malformed or unreadable; entering recovery mode: %v", markerErr)
		previousMarker = malformedTaskRecoveryMarker("marker_malformed", time.Now())
		if err := writeTaskRecoveryMarker(taskRecoveryMarkerPath, previousMarker); err != nil {
			log.Printf("WARN: normalize malformed task recovery marker: %v", err)
		}
	} else if recoveryMode {
		log.Printf("WARN: task recovery marker found: generation=%d reason=%s suspects=%d global_strike=%d; entering recovery mode",
			previousMarker.Generation, previousMarker.ReasonCode, len(previousMarker.Suspects), previousMarker.GlobalStrike)
	}
	censusCh := make(chan pressure.TaskCensus, 1)
	// Critical task pressure can occur during server construction. Arm the
	// process-level owner before constructors run so a blocked constructor
	// cannot postpone the one-second emergency exit.
	go runTaskEmergencyOwner(censusCh)

	var recoveryStart func(context.Context, *server.GinServer)
	if recoveryMode {
		controller := newTaskRecoveryController(
			taskRecoveryMarkerPath,
			previousMarker,
			os.Getenv("INVOCATION_ID"),
		)
		initialDesired := make([]string, 0, len(previousMarker.Suspects))
		for _, suspect := range previousMarker.Suspects {
			initialDesired = append(initialDesired, suspect.Owner)
		}
		recoveryStart = func(ctx context.Context, srv *server.GinServer) {
			runner := newTaskRecoveryRunner(taskRecoveryRunnerConfig{
				controller:       controller,
				runtime:          ginTaskRecoveryRuntime{server: srv},
				initialDesired:   initialDesired,
				processStartedAt: processStartedAt,
			})
			runner.Run(ctx)
		}
	}

	// The main function is the entry point. Its only job is to
	// initialize and start the Gin-based server.
	srv, err := server.NewGinServer(
		server.WithGinVersion(version),
		server.WithTaskEmergencyOwner(processTaskFatalRequests.RequestCritical, censusCh),
		server.WithTaskRecoveryStart(recoveryStart),
		server.WithUnlockFatalRecovery(func() { requestUnlockChainFatalRecovery() }),
		server.WithTaskGuardDisabled(os.Getenv("PICCOLO_DISABLE_TASK_GUARD") == "1"),
	)
	if err != nil {
		log.Fatalf("FATAL: Failed to initialize server: %v", err)
	}
	if recoveryMode {
		// Seed retained recovery degradation before the access listener can
		// serve its first snapshot. The runner replaces this conservative
		// marker-derived value with its live schedule after desired-owner
		// enumeration establishes per-app suppression.
		srv.SetTaskRecoveryGlobalSuppression(taskRecoveryMarkerHasInitialBackoff(previousMarker))
	}
	attachTaskRestartUnlockContinuity(srv.RestartUnlockContinuity())
	// Construction includes core persistence reacquisition needed to serve the
	// access plane and may run bounded mount/cryptsetup probes. Fence optional
	// child work only after those core prerequisites exist; Start releases this
	// independent latch after the HTTP accept loop is live and Ready is sent.
	pressure.DefaultAdmission.FenceStartup()
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
