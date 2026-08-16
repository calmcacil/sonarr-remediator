// Command sonarr-remediator wires the agent together: configuration loading,
// startup sequence, monitors, the issue-consumer loop, and graceful shutdown
// (SPEC §4, §11).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/detectors"
	"github.com/calmcacil/sonarr-remediator/internal/executor"
	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/monitors"
	"github.com/calmcacil/sonarr-remediator/internal/safety"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// gracefulDrainTimeout bounds how long shutdown waits for in-flight actions
// to complete before aborting them (SPEC §11).
const gracefulDrainTimeout = 30 * time.Second

// version is stamped at build time via -ldflags "-X main.version=..." and
// logged at startup so container logs identify the exact build
// (DOCKER_SPEC.md §3).
var version = "dev"

func main() {
	configPath := flag.String("config", "config.example.yaml", "path to configuration file")
	healthcheck := flag.Bool("healthcheck", false, "validate configuration and Sonarr connectivity, then exit (container healthcheck)")
	flag.Parse()

	// Container healthcheck mode (DOCKER_SPEC.md §4): no monitors start and
	// nothing is mutated. Exit 0 = config valid and Sonarr reachable.
	if *healthcheck {
		os.Exit(runHealthcheck(*configPath))
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("failed to load config", err)
	}

	logger, err := logging.New(cfg.Logging.Level)
	if err != nil {
		fatal("failed to initialize logging", err)
	}
	mainLog := logger.With("component", "main")
	mainLog.Info("starting sonarr recovery agent", "config", *configPath, "dry_run", cfg.DryRun, "version", version)

	client, err := sonarr.New(cfg.Sonarr.URL, cfg.Sonarr.APIKey, cfg.Sonarr.Timeout.Std(), cfg.Sonarr.MaxConcurrency)
	if err != nil {
		mainLog.Error("failed to create sonarr client", "error", err)
		os.Exit(1)
	}

	// Root context: cancelled by the first SIGTERM/SIGINT (SPEC §11).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A second signal causes immediate exit (SPEC §11). The hard-kill
	// listener is armed only after the first signal, so the first signal
	// always triggers the graceful path.
	go func() {
		<-ctx.Done()
		hard := make(chan os.Signal, 1)
		signal.Notify(hard, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(hard)
		<-hard
		os.Exit(1)
	}()

	// Startup sequence (SPEC §4, §5.1): version detection is a hard
	// prerequisite; definitions load failure degrades gracefully.
	if err := client.DetectVersion(ctx); err != nil {
		mainLog.Error("sonarr version detection failed", "error", err)
		os.Exit(1)
	}
	mainLog.Info("detected sonarr version", "version", client.Version)

	if err := client.LoadDefinitions(ctx); err != nil {
		mainLog.Warn("failed to load sonarr quality/language definitions; continuing", "error", err)
	}

	engine := safety.New(cfg, logger)
	retry := executor.NewRetryScheduler(client, cfg, engine, logger)
	exec := executor.New(client, cfg, retry, logger)
	detectorsList := []detectors.Detector{
		detectors.NewStuckDownloadDetector(cfg, logger),
		detectors.NewNotCustomFormatDetector(cfg, logger),
		detectors.NewTorrentErrorDetector(cfg, logger),
		detectors.NewImportRecoveryDetector(cfg, logger),
	}

	issues := make(chan types.Issue, 100)
	queueMon := monitors.NewQueueMonitor(client, cfg, engine, issues, detectorsList, logger)
	healthMon := monitors.NewHealthMonitor(client, cfg, engine, logger)

	// workCtx bounds in-flight actions: it is cancelled only when the
	// graceful drain times out, so actions already in progress complete
	// during shutdown (SPEC §11).
	workCtx, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()

	// Issue consumer: evaluate every emitted issue and execute approved
	// decisions. The engine logs skipped decisions; the executor logs
	// action.taken / action.recommended (SPEC §4, §7).
	var consumers sync.WaitGroup
	consumers.Add(1)
	go func() {
		defer consumers.Done()
		for issue := range issues {
			decision, err := engine.Evaluate(workCtx, issue)
			if err != nil {
				mainLog.Error("safety evaluation failed", "item", issue.QueueItem.CompositeKey(), "error", err)
				continue
			}
			if decision.Approved {
				if err := exec.Execute(workCtx, *decision); err != nil {
					mainLog.Error("action execution failed", "item", issue.QueueItem.CompositeKey(), "action", decision.Action, "error", err)
				}
			}
		}
	}()

	// Startup delay before the first poll cycle (SPEC §8 monitoring.startupDelay).
	select {
	case <-time.After(cfg.Monitoring.StartupDelay.Std()):
	case <-ctx.Done():
	}

	var monitorsWg sync.WaitGroup
	monitorsWg.Add(2)
	go func() {
		defer monitorsWg.Done()
		queueMon.Run(ctx)
	}()
	go func() {
		defer monitorsWg.Done()
		healthMon.Run(ctx)
	}()
	mainLog.Info("monitors started",
		"queue_interval", cfg.Monitoring.QueueInterval.String(),
		"health_interval", cfg.Monitoring.HealthInterval.String())

	// Graceful shutdown (SPEC §11).
	<-ctx.Done()
	mainLog.Info("shutdown signal received, stopping monitors")

	// No new poll cycles: retry timers and monitors are stopped first.
	retry.Stop()
	monitorsWg.Wait()

	// Drain the issue consumer; no new issues arrive once the channel is
	// closed. In-flight actions get gracefulDrainTimeout to complete.
	close(issues)
	consumerDone := make(chan struct{})
	go func() {
		consumers.Wait()
		close(consumerDone)
	}()
	select {
	case <-consumerDone:
	case <-time.After(gracefulDrainTimeout):
		mainLog.Warn("graceful drain timed out, aborting in-flight actions")
		cancelWork()
		<-consumerDone
	}

	// Flush the decision ring buffer to stdout (SPEC §11 step 3).
	for _, dec := range engine.Drain() {
		mainLog.Info("decision flushed",
			"event", "decision.flushed",
			"item", dec.Issue.QueueItem.CompositeKey(),
			"trigger", dec.Issue.Type,
			"action", dec.Action,
			"approved", dec.Approved,
			"dry_run", dec.DryRun,
			"timestamp", dec.Timestamp,
		)
	}

	mainLog.Info("shutdown complete")
}

// runHealthcheck validates the configuration and probes Sonarr, returning
// a process exit code. The probe uses a 4 s internal deadline so it always
// finishes inside Docker's 5 s HEALTHCHECK timeout (DOCKER_SPEC.md §3, §9).
func runHealthcheck(configPath string) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: invalid configuration:", err)
		return 1
	}
	const probeTimeout = 4 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	client, err := sonarr.New(cfg.Sonarr.URL, cfg.Sonarr.APIKey, probeTimeout, 1)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: sonarr client error:", err)
		return 1
	}
	status, err := client.GetSystemStatus(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: sonarr unreachable:", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "healthcheck: ok (sonarr %s)\n", status.Version)
	return 0
}

// fatal logs one structured error line on stdout with component=main and
// exits with status 1. Used for failures before the configured logger exists.
func fatal(msg string, err error) {
	slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error(msg, "component", "main", "error", err)
	os.Exit(1)
}
