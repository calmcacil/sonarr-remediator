package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/control"
	"github.com/calmcacil/sonarr-remediator/internal/service"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/state"
)

var version = "0.2.0"

func main() {
	if e := execute(os.Args[1:], os.Stdout, os.Stderr); e != nil {
		var exit *exitError
		if errors.As(e, &exit) {
			os.Exit(exit.code)
		}
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func execute(args []string, out, errOut io.Writer) error {
	command := "run"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		command = args[0]
		args = args[1:]
	}
	if command == "version" {
		fmt.Fprintln(out, version)
		return nil
	}
	if command == "trigger" || command == "triggers" {
		return manualCommand(command, args, out, errOut)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(errOut)
	path := fs.String("state", "/state/remediator.db", "local state store")
	jsonOut := fs.Bool("json", false, "JSON output")
	limit := fs.Int("limit", 100, "maximum result count")
	download := fs.String("download-id", "", "filter actions by Sonarr download ID")
	conf := fs.String("config", "", "YAML configuration file")
	// Permit the documented show <id> --json ordering.
	showID := ""
	if command == "show" && len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		showID = args[0]
		args = args[1:]
	}
	if e := fs.Parse(args); e != nil {
		return e
	}
	if command == "run" {
		if fs.NArg() != 0 {
			return errors.New("run takes no positional arguments")
		}
		c, e := config.Load(*conf, os.Environ())
		if e != nil {
			return e
		}
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "state" {
				c.StatePath = *path
			}
		})
		var level slog.Level
		if e = level.UnmarshalText([]byte(c.Logging.Level)); e != nil {
			return e
		}
		opts := &slog.HandlerOptions{Level: level}
		var handler slog.Handler = slog.NewJSONHandler(errOut, opts)
		if c.Logging.Format == "text" {
			handler = slog.NewTextHandler(errOut, opts)
		}
		log := slog.New(handler)
		store, e := state.Open(c.StatePath, c.Identity(), false)
		if e != nil {
			return e
		}
		stopped := make(chan struct{})
		defer func() { store.Close(); close(stopped) }()
		client, e := sonarr.New(c.Sonarr.URL, c.Sonarr.APIKey, c.Sonarr.Timeout, c.DryRun)
		if e != nil {
			return e
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		// On signal, context cancels outstanding HTTP immediately; the hard deadline
		// bounds storage shutdown even if a local filesystem stalls.
		go func() {
			select {
			case <-ctx.Done():
				select {
				case <-stopped:
				case <-time.After(25 * time.Second):
					os.Exit(1)
				}
			case <-stopped:
			}
		}()
		observer := service.New(c, client, store, log)
		controller, controlErr := control.Start(ctx, c.StatePath, c.Identity(), observer.Request)
		if controlErr != nil {
			store.Data.Status.ManualControl = "unavailable: " + controlErr.Error()
			log.Warn("manual control unavailable", "reason", controlErr.Error())
		} else {
			store.Data.Status.ManualControl = "available"
			defer controller.Close()
		}
		return observer.Run(ctx)
	}
	switch command {
	case "status", "issues", "show", "actions", "healthcheck":
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	if *limit <= 0 || *limit > 10000 {
		return errors.New("limit must be between 1 and 10000")
	}
	if command == "show" && showID == "" && fs.NArg() == 1 {
		showID = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	store, e := state.Open(*path, "", true)
	if e != nil {
		return e
	}
	defer store.Close()
	data := &store.Data
	if command == "healthcheck" {
		if !data.Status.Healthy(time.Now()) {
			return errors.New("unhealthy: local readiness, Sonarr reads, or scan freshness")
		}
		return nil
	}
	data.AggregateStatus(time.Now())
	var result any
	switch command {
	case "status":
		result = data.Status
	case "issues":
		rows := data.SortedIssues(false)
		if len(rows) > *limit {
			rows = rows[:*limit]
		}
		result = rows
	case "show":
		if showID == "" {
			return errors.New("show requires a Sonarr download ID")
		}
		i := data.Issues[showID]
		if i == nil {
			return errors.New("unknown Sonarr download ID")
		}
		result = i
	case "actions":
		rows := []state.Attempt{}
		for j := len(data.Attempts) - 1; j >= 0 && len(rows) < *limit; j-- {
			a := data.Attempts[j]
			if *download == "" || a.DownloadID == *download {
				rows = append(rows, a)
			}
		}
		result = rows
	}
	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	switch v := result.(type) {
	case state.Status:
		fmt.Fprintf(out, "Mode: dry-run=%t recovery=%t cleanup=%t blocklist=%t replacement-search=%t\nLocal ready: %t | Sonarr: %s (%s)\nLast complete scan: %s\nBudget: %d/%d per rolling hour | Unresolved outcomes: %d\nIssue counts: %v\n", v.DryRun, v.Recovery, v.Cleanup, v.Blocklist, v.ReplacementSearch, v.Ready, v.Connectivity, v.Version, v.CompleteScan.Format(time.RFC3339), v.BudgetUsed, v.HourLimit, v.Unresolved, v.IssueCounts)
		if v.ReadFailure != "" {
			fmt.Fprintln(out, "Required read failure:", v.ReadFailure)
		}
		fmt.Fprintln(out, "Manual control:", v.ManualControl)
	case []*state.Issue:
		for _, i := range v {
			fmt.Fprintf(out, "%s  %s  %s\n", i.DownloadID, i.State, i.Reason)
		}
	case *state.Issue:
		fmt.Fprintf(out, "Download: %s\nState: %s\nEpisodes: %v\nObserved eligible: %s\nDecision: %s\nReason: %s\n", v.DownloadID, v.State, v.Decision.EpisodeIDs, v.Elapsed, v.Decision.Kind, v.Reason)
	case []state.Attempt:
		for _, a := range v {
			fmt.Fprintf(out, "%d  %s  %s  %s  %s  %s\n", a.ID, a.Started.Format(time.RFC3339), a.DownloadID, a.Kind, a.Outcome, a.Reason)
		}
	}
	return nil
}
