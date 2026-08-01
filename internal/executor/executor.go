// Package executor performs approved recovery actions and schedules import
// retries (SPEC §3.6, §5.5).
package executor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/recovery"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// Executor performs approved recovery actions (SPEC §5.5).
type Executor struct {
	client     *sonarr.Client
	cfg        *config.Config
	retry      *RetryScheduler
	logger     *slog.Logger // component=executor, used for executor's own logs
	baseLogger *slog.Logger // unscoped, handed to recovery.Recover (which scopes it)
	translator *sonarr.PathTranslator
	roots      []string
}

// New builds the executor. Download roots default to cfg.Paths.DownloadRoots;
// when empty they are discovered once from Sonarr's download client
// configuration (a warning is logged and empty roots are used on failure).
func New(client *sonarr.Client, cfg *config.Config, retry *RetryScheduler, logger *slog.Logger) *Executor {
	roots := cfg.Paths.DownloadRoots
	if len(roots) == 0 {
		discovered, err := client.DownloadRoots(context.Background())
		if err != nil {
			logger.Warn("failed to discover download roots, proceeding without roots", "error", err)
		} else {
			roots = discovered
		}
	}

	return &Executor{
		client:     client,
		cfg:        cfg,
		retry:      retry,
		logger:     logger.With("component", "executor"),
		baseLogger: logger,
		translator: sonarr.NewPathTranslator(cfg.Paths.AgentRoot, cfg.Paths.SonarrRoot),
		roots:      roots,
	}
}

// Execute performs one approved decision, logging the full action record
// (SPEC §7, §9). Unapproved decisions are logged as warnings and skipped; the
// safety engine has already logged the action.skipped event.
func (e *Executor) Execute(ctx context.Context, decision types.Decision) error {
	if !decision.Approved {
		e.logger.Warn("action not approved, skipping",
			"decision_id", decisionID(decision),
			"item", decision.Issue.QueueItem.CompositeKey(),
			"trigger", string(decision.Issue.Type),
			"action", string(decision.Action),
			"reason", decision.Reason,
		)
		return nil
	}

	switch decision.Action {
	case types.ActionLogOnly:
		msg := "No action required for queue item " + strconv.Itoa(decision.Issue.QueueItem.ID)
		e.logger.Info(msg, e.decisionAttrs(decision, "", msg)...)
		return nil
	case types.ActionRemoveQueue:
		return e.removeQueue(ctx, decision)
	case types.ActionManualImport:
		return e.manualImport(ctx, decision)
	case types.ActionRetry:
		return e.retryImport(ctx, decision)
	default:
		return fmt.Errorf("executor: unknown action %q", decision.Action)
	}
}

// removeQueue removes the queue item via Sonarr, optionally blocklisting the
// release (SPEC §3.2, §3.3).
func (e *Executor) removeQueue(ctx context.Context, decision types.Decision) error {
	item := decision.Issue.QueueItem
	id := strconv.Itoa(item.ID)

	if decision.DryRun {
		msg := "Would have removed queue item " + id
		e.logger.Info(msg, e.decisionAttrs(decision, "action.recommended", msg)...)
		return nil
	}

	blocklist := e.cfg.Automation.RemoveBrokenDownloads.BlocklistRelease
	if decision.Issue.Type == types.IssueNotCustomFormat {
		blocklist = e.cfg.Automation.RemoveNotCustomFormat.BlocklistRelease
	}

	if err := e.client.RemoveQueueItem(ctx, item.ID, blocklist); err != nil {
		msg := "Failed to remove queue item " + id
		e.logger.Error(msg,
			append(e.decisionAttrs(decision, "action.error", msg), "error", err)...)
		return fmt.Errorf("executor: remove queue item %d: %w", item.ID, err)
	}

	msg := "Removed queue item " + id
	e.logger.Info(msg, e.decisionAttrs(decision, "action.taken", msg)...)
	return nil
}

// manualImport either schedules retries (when the failure is retryable) or
// runs full import recovery (SPEC §3.4, §3.6).
func (e *Executor) manualImport(ctx context.Context, decision types.Decision) error {
	item := decision.Issue.QueueItem
	id := strconv.Itoa(item.ID)

	if decision.DryRun {
		msg := "Would have attempted manual import for queue item " + id
		e.logger.Info(msg, e.decisionAttrs(decision, "action.recommended", msg)...)
		return nil
	}

	history := decision.Issue.RelatedHistory
	if e.retry != nil && e.cfg.Automation.RetryImports.Enabled && IsRetryableError(e.cfg, item, history) {
		if err := e.retry.Schedule(ctx, item, history); err != nil {
			return fmt.Errorf("executor: schedule retries for queue item %d: %w", item.ID, err)
		}
		msg := "Scheduled import retries for queue item " + id
		e.logger.Info(msg, e.decisionAttrs(decision, "action.taken", msg)...)
		return nil
	}

	if err := recovery.Recover(ctx, e.client, e.cfg, e.translator, e.roots, item, history, e.baseLogger); err != nil {
		return fmt.Errorf("executor: import recovery for queue item %d: %w", item.ID, err)
	}
	return nil
}

// retryImport delegates a retry action to the retry scheduler (SPEC §5.5).
func (e *Executor) retryImport(ctx context.Context, decision types.Decision) error {
	item := decision.Issue.QueueItem
	id := strconv.Itoa(item.ID)

	if decision.DryRun {
		msg := "Would have scheduled import retries for queue item " + id
		e.logger.Info(msg, e.decisionAttrs(decision, "action.recommended", msg)...)
		return nil
	}

	if e.retry == nil {
		e.logger.Warn("retry scheduler unavailable, skipping retry action",
			"decision_id", decisionID(decision),
			"item", item.CompositeKey())
		return nil
	}

	if err := e.retry.Schedule(ctx, item, decision.Issue.RelatedHistory); err != nil {
		return fmt.Errorf("executor: schedule retries for queue item %d: %w", item.ID, err)
	}
	msg := "Scheduled import retries for queue item " + id
	e.logger.Info(msg, e.decisionAttrs(decision, "action.taken", msg)...)
	return nil
}

// decisionAttrs renders the SPEC §7 decision record as slog attributes.
func (e *Executor) decisionAttrs(decision types.Decision, event, message string) []any {
	item := decision.Issue.QueueItem
	return []any{
		"event", event,
		"decision_id", decisionID(decision),
		"item", slog.GroupValue(
			slog.String("key", item.CompositeKey()),
			slog.Int("id", item.ID),
			slog.String("title", item.SeriesTitle),
			slog.String("series", item.SeriesTitle),
			slog.String("episode", item.EpisodeTitle),
		),
		"trigger", string(decision.Issue.Type),
		"checks", checksToLog(decision.Checks),
		"action", string(decision.Action),
		"message", message,
		"dry_run", decision.DryRun,
	}
}

// checkLog is the JSON shape of one safety check in the SPEC §7 decision log.
// slog marshals KindAny values with encoding/json, so json tags (not slog
// group keys) control the emitted keys.
type checkLog struct {
	Check    string `json:"check"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Passed   bool   `json:"passed"`
}

// checksToLog converts safety check results to the SPEC §7 decision log shape.
func checksToLog(checks []types.CheckResult) []checkLog {
	out := make([]checkLog, 0, len(checks))
	for _, c := range checks {
		out = append(out, checkLog{Check: c.Check, Expected: c.Expected, Actual: c.Actual, Passed: c.Passed})
	}
	return out
}

// decisionID derives a stable per-decision identifier from the item key,
// action, and timestamp (SPEC §7).
func decisionID(decision types.Decision) string {
	sum := sha256.Sum256([]byte(
		decision.Issue.QueueItem.CompositeKey() + ":" + string(decision.Action) + ":" +
			strconv.FormatInt(decision.Timestamp.UnixNano(), 10),
	))
	return fmt.Sprintf("dec_%x", sum[:6])
}
