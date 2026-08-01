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
	"github.com/calmcacil/sonarr-remediator/internal/selector"
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
	case types.ActionReconcile:
		return e.reconcile(ctx, decision)
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

// reconcile executes one approved episode-reconciliation plan (SPEC §3.2):
// the plan winner is imported when it upgrades the episode's existing file,
// otherwise it is removed; every discard is removed. All removals pass
// removeFromClient=true so Sonarr tells the download client to delete the
// files. Discards are removed regardless of the winner's outcome — the plan
// has resolved them. Dry-run logs the intended actions and touches nothing.
func (e *Executor) reconcile(ctx context.Context, decision types.Decision) error {
	plan, ok := decision.Issue.Details[types.DetailsReconcilePlan].(types.ReconcilePlan)
	if !ok {
		return fmt.Errorf("executor: reconcile decision %s missing %s details",
			decisionID(decision), types.DetailsReconcilePlan)
	}
	winner := plan.Winner
	id := strconv.Itoa(winner.ID)

	upgrade, err := e.reconcileUpgrade(ctx, winner)
	if err != nil {
		return fmt.Errorf("executor: upgrade check for queue item %d: %w", winner.ID, err)
	}

	if decision.DryRun {
		want := "import"
		if !upgrade {
			want = "remove"
		}
		msg := fmt.Sprintf("Would have %s queue item %s (episode reconciliation winner) and removed %d discarded release(s)",
			want, id, len(plan.Discards))
		e.logger.Info(msg, e.reconcileAttrs(decision, "action.recommended", msg, plan, upgrade)...)
		return nil
	}

	var firstErr error
	if upgrade {
		imported, err := recovery.ReconcileImport(ctx, e.client, winner, e.baseLogger)
		if err != nil {
			firstErr = err
			msg := "Failed to import reconciliation winner " + id
			e.logger.Error(msg, append(e.reconcileAttrs(decision, "action.error", msg, plan, upgrade), "error", err)...)
		} else if imported {
			msg := "Imported reconciliation winner " + id
			e.logger.Info(msg, e.reconcileAttrs(decision, "action.taken", msg, plan, upgrade)...)
		} else {
			msg := "No importable candidate for reconciliation winner " + id + "; left in queue"
			e.logger.Info(msg, append(e.reconcileAttrs(decision, "action.skipped", msg, plan, upgrade),
				"reason", "no file matched in Sonarr preview or import rejected")...)
		}
	} else {
		if err := e.client.RemoveQueueItem(ctx, winner.ID, true); err != nil {
			firstErr = err
			msg := "Failed to remove non-upgrade winner " + id
			e.logger.Error(msg, append(e.reconcileAttrs(decision, "action.error", msg, plan, upgrade), "error", err)...)
		} else {
			msg := "Removed non-upgrade winner " + id
			e.logger.Info(msg, e.reconcileAttrs(decision, "action.taken", msg, plan, upgrade)...)
		}
	}

	// Discards are resolved by the plan regardless of the winner's outcome.
	for _, d := range plan.Discards {
		did := strconv.Itoa(d.ID)
		if err := e.client.RemoveQueueItem(ctx, d.ID, true); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			msg := "Failed to remove discarded release " + did
			e.logger.Error(msg, append(e.decisionAttrs(decision, "action.error", msg), "error", err)...)
			continue
		}
		msg := "Removed discarded release " + did
		e.logger.Info(msg, e.decisionAttrs(decision, "action.taken", msg)...)
	}
	return firstErr
}

// reconcileUpgrade reports whether the plan winner upgrades the episode's
// existing file (SPEC §3.2): an episode with no file is always upgradeable;
// otherwise the release's custom-format score must beat the existing file's
// score, with quality as the tie-breaker (selector.IsUpgrade).
func (e *Executor) reconcileUpgrade(ctx context.Context, release types.QueueItem) (bool, error) {
	ef, err := e.client.GetEpisodeFileForEpisode(ctx, release.EpisodeID)
	if err != nil {
		return false, err
	}
	if ef == nil {
		return true, nil // no existing file: any matched release imports
	}
	rw, rok := e.client.QualityWeightByID(release.Quality.Quality.ID)
	ew, eok := e.client.QualityWeightByName(string(ef.Quality))
	return selector.IsUpgrade(release.CustomFormatScore, ef.CustomFormatScore, rw, ew, rok && eok), nil
}

// reconcileAttrs renders the SPEC §7 decision record for a reconcile action:
// the item group from decisionAttrs plus the plan's episode key, the winner's
// upgrade decision, and the discard list.
func (e *Executor) reconcileAttrs(decision types.Decision, event, message string, plan types.ReconcilePlan, upgrade bool) []any {
	attrs := e.decisionAttrs(decision, event, message)
	attrs = append(attrs,
		"episode_key", plan.EpisodeKey(),
		"upgrade", upgrade,
	)
	if len(plan.Discards) > 0 {
		attrs = append(attrs, "discards", reconcileDiscardLogs(plan.Discards))
	}
	return attrs
}

// reconcileDiscardLog is the JSON shape of one discarded release in the
// SPEC §7 decision log. slog marshals KindAny values with encoding/json, so
// json tags control the emitted keys.
type reconcileDiscardLog struct {
	ID      int    `json:"id"`
	Release string `json:"release"`
	Score   int    `json:"score"`
}

// reconcileDiscardLogs converts a plan's discards to the decision-log shape.
func reconcileDiscardLogs(discards []types.QueueItem) []reconcileDiscardLog {
	out := make([]reconcileDiscardLog, 0, len(discards))
	for _, d := range discards {
		out = append(out, reconcileDiscardLog{ID: d.ID, Release: d.Title, Score: d.CustomFormatScore})
	}
	return out
}

// decisionAttrs renders the SPEC §7 decision record as slog attributes.
// The item group carries release identity and, when the issuing detector
// enriched the details (stuck-download release context, SPEC §3.2), the
// matched-episode fields as well.
func (e *Executor) decisionAttrs(decision types.Decision, event, message string) []any {
	item := decision.Issue.QueueItem
	itemGroup := []slog.Attr{
		slog.String("key", item.CompositeKey()),
		slog.Int("id", item.ID),
		slog.String("title", item.SeriesTitle),
		slog.String("series", item.SeriesTitle),
		slog.String("episode", item.EpisodeTitle),
		slog.String("release", item.Title),
		slog.String("release_id", item.DownloadID),
		slog.Int("custom_format_score", item.CustomFormatScore),
		slog.Any("custom_formats", item.CustomFormatNames()),
	}
	for _, k := range []string{"episode_match", "episode_has_file", "existing_quality"} {
		if v, ok := decision.Issue.Details[k]; ok {
			itemGroup = append(itemGroup, slog.Any(k, v))
		}
	}
	return []any{
		"event", event,
		"decision_id", decisionID(decision),
		"item", slog.GroupValue(itemGroup...),
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
