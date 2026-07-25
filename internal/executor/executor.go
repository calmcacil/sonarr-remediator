package executor

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/metrics"
	"github.com/calmcacil/sonarr-remediator/internal/notifications"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// Executor performs approved actions.
type Executor struct {
	sonarrClient *sonarr.Client
	notifier     *notifications.Notifier
	dryRun       bool
	cleanupCfg   *CleanupConfig
}

// New creates an Executor.
func New(client *sonarr.Client, notifier *notifications.Notifier, dryRun bool, cleanupCfg *CleanupConfig) *Executor {
	return &Executor{
		sonarrClient: client,
		notifier:     notifier,
		dryRun:       dryRun,
		cleanupCfg:   cleanupCfg,
	}
}

// Execute performs the action described in a decision.
func (e *Executor) Execute(ctx context.Context, decision types.Decision) error {
	if e.dryRun {
		logging.Logger.Info("DRY RUN: would execute", "action", decision.Action.Type, "item", decision.Issue.QueueItem.ID)
		return nil
	}

	switch decision.Action.Type {
	case "remove_from_queue":
		return e.removeFromQueue(ctx, decision)
	case "manual_import":
		return e.manualImport(ctx, decision)
	case "cleanup":
		return e.cleanup(ctx, decision)
	case "retry":
		return e.retry(ctx, decision)
	case "blocklist":
		blocklistParams := decision.Action.Params
		blocklistParams["blocklist"] = "true"
		decision.Action.Params = blocklistParams
		return e.removeFromQueue(ctx, decision)
	default:
		return fmt.Errorf("unknown action type: %s", decision.Action.Type)
	}
}

func (e *Executor) removeFromQueue(ctx context.Context, decision types.Decision) error {
	item := decision.Issue.QueueItem
	blocklist := decision.Action.Params["blocklist"] == "true"

	err := e.sonarrClient.RemoveQueueItem(ctx, item.ID, blocklist)
	if err != nil {
		logging.Logger.Error("failed to remove queue item", "id", item.ID, "error", err)
		return err
	}

	reason := "not_custom_format"
	if decision.Issue.Type == types.IssueStuckDownload {
		reason = "stuck_download"
	}
	metrics.DownloadsRemoved.WithLabelValues(reason).Inc()
	logging.Logger.Info("removed queue item", "id", item.ID, "title", item.SeriesTitle, "blocklist", blocklist)
	return nil
}

func (e *Executor) manualImport(ctx context.Context, decision types.Decision) error {
	logging.Logger.Info("manual import action — handled by recovery engine")
	return nil
}

func (e *Executor) cleanup(ctx context.Context, decision types.Decision) error {
	item := decision.Issue.QueueItem
	if item.OutputPath == "" {
		logging.Logger.Info("cleanup: no output path", "item", item.ID)
		return nil
	}

	// Run targeted cleanup on the item's parent directory
	if e.cleanupCfg != nil {
		parentDir := filepath.Dir(item.OutputPath)
		RunCleanup(ctx, []string{parentDir}, *e.cleanupCfg, e.dryRun)
	}

	action := "cleanup"
	if v, ok := decision.Issue.Details["action"]; ok {
		action = fmt.Sprintf("%v", v)
	}
	metrics.CleanupActionsTotal.WithLabelValues(action).Inc()
	logging.Logger.Info("cleanup action executed", "action", action, "path", item.OutputPath)
	return nil
}

func (e *Executor) retry(ctx context.Context, decision types.Decision) error {
	metrics.RetriesTotal.WithLabelValues("attempted").Inc()
	logging.Logger.Info("retry import", "item", decision.Issue.QueueItem.ID)
	return nil
}
