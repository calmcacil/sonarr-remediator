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
}

// New builds the executor.
func New(client *sonarr.Client, cfg *config.Config, retry *RetryScheduler, logger *slog.Logger) *Executor {
	return &Executor{
		client:     client,
		cfg:        cfg,
		retry:      retry,
		logger:     logger.With("component", "executor"),
		baseLogger: logger,
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
// release (SPEC §3.2, §3.3). Torrent-client error items (§3.9) use the
// extended flow: after the removal the release is blocklisted through
// POST /api/v3/history/failed/{id} (the only working blocklist path for
// qBit-bridge clients) and a replacement search is triggered either by
// Sonarr's own redownload (the history/failed command does this when
// AutoRedownloadFailed is on) or by an explicit EpisodeSearch fallback.
func (e *Executor) removeQueue(ctx context.Context, decision types.Decision) error {
	item := decision.Issue.QueueItem
	id := strconv.Itoa(item.ID)

	// Specialized flows own their dry-run messaging and dispatch before the
	// generic short-circuit.
	if decision.Issue.Type == types.IssueUnknownSeries {
		return e.resolveUnknownSeries(ctx, decision, id)
	}
	if decision.Issue.Type == types.IssueTorrentError {
		return e.removeTorrentError(ctx, decision, id)
	}

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

// resolveUnknownSeries handles a queue item whose series Sonarr does not
// know (SPEC §3.10): the manual-import preview anchored to the tracked
// download resolves the real series and episodes, so the item is imported
// through the ManualImport command (proven by the queue poll); only when the
// preview finds nothing — or fails — is the item removed as a fallback. The
// preview is read-only and is also performed in dry-run so the
// recommendation names the exact outcome.
func (e *Executor) resolveUnknownSeries(ctx context.Context, decision types.Decision, id string) error {
	item := decision.Issue.QueueItem
	attrs := e.decisionAttrs(decision, "action.taken", "Removed queue item "+id)

	files, err := e.client.ManualImportPreview(ctx, item.DownloadID)
	if err != nil {
		return e.removeUnknownSeriesFallback(ctx, decision, id, fmt.Sprintf("manual-import preview failed: %v", err))
	}
	file := recovery.SelectPreviewMatched(files)
	if file == nil {
		return e.removeUnknownSeriesFallback(ctx, decision, id, "manual-import preview found no series match")
	}

	episodeIDs := make([]int, 0, len(file.Episodes))
	for _, ep := range file.Episodes {
		episodeIDs = append(episodeIDs, ep.ID)
	}
	seriesID := item.SeriesID
	if seriesID == 0 {
		ep, err := e.client.GetEpisode(ctx, episodeIDs[0])
		if err != nil {
			return e.removeUnknownSeriesFallback(ctx, decision, id, "could not resolve the matched episode's series")
		}
		seriesID = ep.SeriesID
	}
	langs := file.Languages
	if len(langs) == 0 {
		langs = []types.LanguageModel{{Name: "Unknown"}}
	}
	cmd := types.ManualImportCommand{
		Name:       "ManualImport",
		ImportMode: "auto",
		Files: []types.ManualImportCommandFile{{
			Path:       file.Path,
			SeriesID:   seriesID,
			EpisodeIDs: episodeIDs,
			Quality:    file.Quality,
			Languages:  langs,
			DownloadID: item.DownloadID,
		}},
	}

	if decision.DryRun {
		msg := fmt.Sprintf("Would have imported %s for episodes %v (unknown-series manual import)", file.Path, episodeIDs)
		e.logger.Info(msg, e.decisionAttrs(decision, "action.recommended", msg)...)
		return nil
	}

	ok, err := recovery.SubmitAndWait(ctx, e.client, cmd, item, e.logger)
	if err != nil {
		e.logger.Error("unknown-series manual import command failed",
			append(attrs, "candidate_path", file.Path, "error", err)...)
		return fmt.Errorf("executor: unknown-series import for queue item %d: %w", item.ID, err)
	}
	if !ok {
		e.logger.Info("unknown-series import did not clear the queue item; no mutation reported",
			append(attrs, "candidate_path", file.Path)...)
		return nil
	}
	e.logger.Info("auto-imported "+file.Path+" (unknown-series manual import)",
		append(attrs, "candidate_path", file.Path, "episodes", episodeIDs, "action", string(types.ActionManualImport))...)
	return nil
}

// removeUnknownSeriesFallback logs the fallback removal (recommended in
// dry-run, executed live) when the manual-import resolution cannot proceed.
func (e *Executor) removeUnknownSeriesFallback(ctx context.Context, decision types.Decision, id, reason string) error {
	msg := "Would have removed queue item " + id + " (" + reason + ")"
	if decision.DryRun {
		e.logger.Info(msg, e.decisionAttrs(decision, "action.recommended", msg)...)
		return nil
	}
	attrs := e.decisionAttrs(decision, "action.taken", "Removed queue item "+id)
	if err := e.client.RemoveQueueItem(ctx, decision.Issue.QueueItem.ID, false); err != nil {
		msg := "Failed to remove queue item " + id
		e.logger.Error(msg,
			append(e.decisionAttrs(decision, "action.error", msg), "error", err)...)
		return fmt.Errorf("executor: remove queue item %d: %w", decision.Issue.QueueItem.ID, err)
	}
	e.logger.Info("Removed queue item "+id+" ("+reason+")", attrs...)
	return nil
}

// removeTorrentError removes a torrent-client error item and, per the
// removeTorrentErrors rule, blocklists the release and triggers a replacement
// search (SPEC §3.9). Order matters: the removal happens first so the
// history/failed command cannot import the dying item, and the blocklist
// happens before any fallback search so the same release is not re-grabbed.
func (e *Executor) removeTorrentError(ctx context.Context, decision types.Decision, id string) error {
	item := decision.Issue.QueueItem
	rule := e.cfg.Automation.RemoveTorrentErrors
	attrs := e.decisionAttrs(decision, "action.taken", "Removed queue item "+id)

	if decision.DryRun {
		msg := "Would have removed queue item " + id +
			" (torrent client error: blocklist=" + strconv.FormatBool(rule.BlocklistRelease) +
			", redownload=" + strconv.FormatBool(rule.Redownload) + ")"
		e.logger.Info(msg, e.decisionAttrs(decision, "action.recommended", msg)...)
		return nil
	}

	if err := e.client.RemoveQueueItem(ctx, item.ID, false); err != nil {
		msg := "Failed to remove queue item " + id
		e.logger.Error(msg,
			append(e.decisionAttrs(decision, "action.error", msg), "error", err)...)
		return fmt.Errorf("executor: remove queue item %d: %w", item.ID, err)
	}
	e.logger.Info("Removed queue item "+id, attrs...)

	blocklisted := false
	if rule.BlocklistRelease {
		h, err := e.client.FindGrabbedHistory(ctx, item.SeriesID, item.EpisodeID, item.Title)
		if err != nil {
			e.logger.Warn("failed to look up grabbed history for blocklist",
				"decision_id", decisionID(decision),
				"item", item.CompositeKey(),
				"error", err)
		} else if h == nil {
			e.logger.Info("no grabbed history found for release; skipping blocklist",
				"decision_id", decisionID(decision),
				"item", item.CompositeKey())
		} else {
			if err := e.client.MarkHistoryFailed(ctx, h.ID); err != nil {
				e.logger.Error("failed to mark grabbed history as failed",
					append(attrs, "history_id", h.ID, "error", err)...)
			} else {
				blocklisted = true
				e.logger.Info("blocklisted release via history "+strconv.Itoa(h.ID),
					append(attrs, "history_id", h.ID, "release_title", item.Title)...)
			}
		}
	}

	// Sonarr's history/failed command already triggers the redownload when
	// AutoRedownloadFailed is enabled; an explicit EpisodeSearch is the
	// fallback when nothing was blocklisted (or blocklisting is off).
	if rule.Redownload && !blocklisted {
		if err := e.client.EpisodeSearch(ctx, []int{item.EpisodeID}); err != nil {
			e.logger.Error("failed to trigger episode search",
				append(attrs, "error", err)...)
			return fmt.Errorf("executor: episode search for queue item %d: %w", item.ID, err)
		}
		e.logger.Info("triggered episode search for replacement",
			append(attrs, "episode_id", item.EpisodeID)...)
	}
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

	if err := recovery.Recover(ctx, e.client, e.cfg, item, e.baseLogger); err != nil {
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
