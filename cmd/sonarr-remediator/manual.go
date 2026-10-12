package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/control"
	"github.com/calmcacil/sonarr-remediator/internal/state"
)

type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("trigger exit %d", e.code) }

type downloadIDs []string

func (v *downloadIDs) String() string { return fmt.Sprint([]string(*v)) }
func (v *downloadIDs) Set(id string) error {
	if id == "" {
		return errors.New("download IDs must not be empty")
	}
	*v = append(*v, id)
	return nil
}

func manualCommand(command string, args []string, out, errOut io.Writer) error {
	err := manualCommandRun(command, args, out, errOut)
	var exit *exitError
	if err != nil && !errors.As(err, &exit) && (slices.Contains(args, "--json") || slices.Contains(args, "--json=true")) {
		report := control.Failure(control.Request{}, 1, err.Error())
		if command == "triggers" {
			report.Mode = "history"
		}
		if encodeErr := json.NewEncoder(out).Encode(report); encodeErr != nil {
			return encodeErr
		}
		return &exitError{code: 1}
	}
	return err
}

func manualCommandRun(command string, args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(errOut)
	path := fs.String("state", "/state/remediator.db", "local observer state store")
	jsonOut := fs.Bool("json", false, "versioned JSON output")
	if command == "triggers" {
		requestID := fs.String("request-id", "", "inspect one live request receipt")
		limit := fs.Int("limit", 100, "maximum recent receipts")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 || *limit < 1 || *limit > 10000 {
			return errors.New("triggers requires no positional arguments and a limit between 1 and 10000")
		}
		store, err := state.Open(*path, "", true)
		if err != nil {
			return err
		}
		defer store.Close()
		rows := []*state.Receipt{}
		for id, receipt := range store.Data.Triggers {
			if *requestID != "" && id != *requestID {
				continue
			}
			view := *receipt
			view.Report = store.Data.ReceiptReport(receipt)
			rows = append(rows, &view)
		}
		if *requestID != "" && len(rows) == 0 {
			return errors.New("unknown manual request identifier")
		}
		sort.Slice(rows, func(a, b int) bool {
			if rows[a].Accepted.Equal(rows[b].Accepted) {
				return rows[a].Request.ID < rows[b].Request.ID
			}
			return rows[a].Accepted.After(rows[b].Accepted)
		})
		if len(rows) > *limit {
			rows = rows[:*limit]
		}
		if *jsonOut {
			return json.NewEncoder(out).Encode(struct {
				Version  int              `json:"schema_version"`
				Receipts []*state.Receipt `json:"receipts"`
			}{control.Version, rows})
		}
		for _, receipt := range rows {
			fmt.Fprintf(out, "Request: %s | %s | accepted %s | attempts %v\n", receipt.Request.ID, receipt.Status, receipt.Accepted.Format(time.RFC3339), receipt.AttemptIDs)
			if err := printReport(out, receipt.Report, false); err != nil {
				return err
			}
		}
		return nil
	}
	dry := fs.Bool("dry-run", true, "preview only (default)")
	live := fs.Bool("execute", false, "permit live work for this request")
	override := fs.Bool("override-wait", false, "skip initial recovery/cleanup waits and episode cooldowns")
	timeout := fs.Duration("timeout", control.MaxTimeout, "end-to-end timeout (maximum 2m)")
	var ids downloadIDs
	fs.Var(&ids, "download-id", "exact Sonarr download ID; repeat to select more")
	if err := fs.Parse(args); err != nil {
		return err
	}
	drySeen, liveSeen := false, false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "dry-run" {
			drySeen = true
		}
		if f.Name == "execute" {
			liveSeen = true
		}
	})
	if fs.NArg() != 0 || !*dry || (drySeen && liveSeen) || *timeout <= 0 || *timeout > control.MaxTimeout {
		return errors.New("invalid trigger options: use --execute for live work; --dry-run and --execute conflict; timeout must be in (0, 2m]")
	}
	req, err := control.NewRequest(*live, *override, ids, *timeout)
	if err != nil {
		return err
	}
	store, err := state.Open(*path, "", true)
	if err != nil {
		return err
	}
	identity := store.Data.Status.Identity
	store.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT)
	defer cancel()
	ctx, deadlineCancel := context.WithDeadline(ctx, req.Expires)
	defer deadlineCancel()
	if req.Execute {
		fmt.Fprintf(errOut, "Manual request ID: %s (inspect with triggers --request-id %s)\n", req.ID, req.ID)
	}
	report := control.Call(ctx, *path, identity, req)
	if ctx.Err() == context.Canceled {
		report.ExitCode = 130
		if req.Execute {
			report.Error = "interrupted; execution may have reached the observer or Sonarr; inspect triggers before another execution"
		} else {
			report.Error = "preview interrupted"
		}
	}
	if err := printReport(out, report, *jsonOut); err != nil {
		if req.Execute && (report.ExitCode == 0 || report.ExitCode == 3) {
			fmt.Fprintf(errOut, "Result output failed; inspect triggers --request-id %s before another execution\n", req.ID)
			return &exitError{code: 3}
		}
		return err
	}
	if report.ExitCode != 0 {
		return &exitError{code: report.ExitCode}
	}
	return nil
}

func printReport(writer io.Writer, report control.Report, jsonOut bool) error {
	if jsonOut {
		return json.NewEncoder(writer).Encode(report)
	}
	var body strings.Builder
	out := &body
	fmt.Fprintf(out, "Mode: %s | background dry-run=%t | override-wait=%t | request=%s\nSelection: %v | queue rows=%d | shared downloads=%d\nPermissions: recovery=%t cleanup=%t blocklist=%t replacement-search=%t\nBudget available: %d (cycle=%d; rolling hour=%d/%d)\n", report.Mode, report.BackgroundDryRun, report.OverrideWait, report.RequestID, report.Downloads, report.QueueRows, report.SharedDownloads, report.Permissions.Recovery, report.Permissions.Cleanup, report.Permissions.Blocklist, report.Permissions.ReplacementSearch, report.Budget.Available, report.Budget.PerCycle, report.Budget.HourUsed, report.Budget.HourLimit)
	for _, item := range report.Items {
		fmt.Fprintf(out, "%s  %s  action=%s episodes=%v preview-files=%d membership-complete=%t\n  Wait: %s observed=%s remaining=%s bypassed=%t\n  %s\n", item.DownloadID, item.Disposition, item.Action, item.Episodes, item.PreviewFiles, item.MembershipComplete, item.Wait, item.Observed, item.Remaining, item.WaitBypassed, item.Reason)
		if item.ClientDeletion {
			fmt.Fprintf(out, "  Removal uses the download client and can delete downloaded data; blocklist=%t replacement-search=%t\n", item.Blocklist, item.ReplacementSearch)
		}
		for _, effect := range item.Effects {
			fmt.Fprintf(out, "  Attempt %d: %s %s acknowledged=%t removal-verified=%t blocklist-verified=%t\n", effect.AttemptID, effect.Action, effect.Outcome, effect.Acknowledged, effect.RemovalVerified, effect.BlocklistVerified)
		}
	}
	if report.Error != "" {
		fmt.Fprintln(out, "Error:", report.Error)
	}
	fmt.Fprintf(out, "Exit: %d (acknowledgement does not prove external success)\n", report.ExitCode)
	_, err := io.WriteString(writer, body.String())
	return err
}
