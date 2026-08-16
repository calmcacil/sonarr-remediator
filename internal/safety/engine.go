// Package safety implements the safety engine (SPEC §7): config-derived
// gates per issue type plus always-enforced global constraints, producing
// approvals or rejections for the executor.
package safety

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// Ring-buffer capacity for decisions flushed at shutdown (SPEC §11).
const ringCapacity = 1000

// Global constraint windows (SPEC §7).
const (
	duplicateWindow  = 5 * time.Minute
	cooldownWindow   = 30 * time.Minute
	stuckMinAgeHours = 2.0
)

// Engine evaluates issues against config-derived automation gates and the
// always-enforced global constraints. It keeps the in-memory state needed for
// duplicate-action, cooldown, and retry checks, plus a bounded ring buffer of
// decisions drained at shutdown.
type Engine struct {
	cfg    *config.Config
	logger *slog.Logger

	sonarrUp atomic.Bool

	mu            sync.Mutex
	activeRetries map[string]bool      // item composite key -> retry scheduled
	activeItems   map[string]time.Time // item key + "|" + action -> last approval
	lastAction    map[string]time.Time // "seriesId:episodeId" -> last action time

	ringMu    sync.Mutex
	ring      []types.Decision
	ringStart int
	ringCount int
}

// New builds a safety engine. A nil logger is replaced with a default info
// logger; the logger is always tagged with component=safety (SPEC §9).
func New(cfg *config.Config, logger *slog.Logger) *Engine {
	if logger == nil {
		logger, _ = logging.New("info")
	}
	logger = logger.With("component", "safety")
	return &Engine{
		cfg:           cfg,
		logger:        logger,
		activeRetries: make(map[string]bool),
		activeItems:   make(map[string]time.Time),
		lastAction:    make(map[string]time.Time),
	}
}

// Evaluate runs the config-derived gates for the issue's automation rule and
// then the always-enforced global constraints. Every evaluated check is
// recorded in the returned decision's Checks with its actual value so dry-run
// logs explain each pass/fail. Approved decisions are NOT logged here — the
// executor emits action.taken / action.recommended. Rejections are logged once
// as event=action.skipped.
func (e *Engine) Evaluate(ctx context.Context, issue types.Issue) (*types.Decision, error) {
	action := issue.ActionTypeFor()
	checks := make([]types.CheckResult, 0, 12)

	// Config-derived gates for this issue type (AND logic, short-circuit).
	for _, c := range e.gatesFor(issue) {
		checks = append(checks, c)
		if !c.Passed {
			return e.reject(issue, action, checks), nil
		}
	}

	// Always-enforced global constraints (SPEC §7).
	for _, c := range e.globalConstraints(issue, action) {
		checks = append(checks, c)
		if !c.Passed {
			return e.reject(issue, action, checks), nil
		}
	}

	// Approved: record the action for duplicate/cooldown tracking.
	now := time.Now()
	e.mu.Lock()
	e.activeItems[itemActionKey(issue.QueueItem, action)] = now
	e.lastAction[cooldownKey(issue.QueueItem)] = now
	e.mu.Unlock()

	dec := &types.Decision{
		Issue:     issue,
		Action:    action,
		Checks:    checks,
		Approved:  true,
		Timestamp: now,
		DryRun:    e.cfg.DryRun,
	}
	e.pushDecision(*dec)
	return dec, nil
}

// SetSonarrUp records the health monitor's latest connectivity result.
// Actions are only approved while Sonarr is reachable (SPEC §7 constraint 3).
func (e *Engine) SetSonarrUp(up bool) {
	e.sonarrUp.Store(up)
}

// SonarrUp reports the latest connectivity result.
func (e *Engine) SonarrUp() bool {
	return e.sonarrUp.Load()
}

// SetRetryActive records whether a retry is scheduled for the given item key.
// Items with an active retry are not eligible for removal or manual import.
func (e *Engine) SetRetryActive(key string, active bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if active {
		e.activeRetries[key] = true
	} else {
		delete(e.activeRetries, key)
	}
}

// Drain returns all buffered decisions in order and clears the buffer. Used
// for the shutdown decision-log flush (SPEC §11).
func (e *Engine) Drain() []types.Decision {
	e.ringMu.Lock()
	defer e.ringMu.Unlock()
	if e.ringCount == 0 {
		return nil
	}
	out := make([]types.Decision, e.ringCount)
	for i := range e.ringCount {
		out[i] = e.ring[(e.ringStart+i)%ringCapacity]
	}
	e.ringStart, e.ringCount = 0, 0
	return out
}

// gatesFor builds the config-derived gates for the issue's automation rule
// (SPEC §3.2, §3.3, §7), in evaluation order.
func (e *Engine) gatesFor(issue types.Issue) []types.CheckResult {
	item := issue.QueueItem
	key := item.CompositeKey()
	switch issue.Type {
	case types.IssueStuckDownload:
		enabled := e.cfg.Automation.RemoveBrokenDownloads.Enabled
		return []types.CheckResult{
			{Check: "rule.enabled", Expected: "true", Actual: strconv.FormatBool(enabled), Passed: enabled},
			{Check: "queue.status", Expected: "completed|warning|failed", Actual: item.Status, Passed: eligibleStatus(item.Status)},
			e.ageCheck(item, stuckMinAgeHours),
			{Check: "queue.trackedDownloadState", Expected: "!= importing", Actual: item.TrackedDownloadState, Passed: item.TrackedDownloadState != "importing"},
			e.manualImportCheck(key),
			e.retryCheck(key),
		}
	case types.IssueNotCustomFormat:
		enabled := e.cfg.Automation.RemoveNotCustomFormat.Enabled
		waitHours := e.cfg.Automation.RemoveNotCustomFormat.WaitHours
		return []types.CheckResult{
			{Check: "rule.enabled", Expected: "true", Actual: strconv.FormatBool(enabled), Passed: enabled},
			{Check: "queue.status", Expected: "completed", Actual: item.Status, Passed: item.Status == "completed"},
			e.ageCheck(item, waitHours),
			{Check: "queue.trackedDownloadState", Expected: "!= importing", Actual: item.TrackedDownloadState, Passed: item.TrackedDownloadState != "importing"},
			e.retryCheck(key),
		}
	case types.IssueTorrentError:
		rule := e.cfg.Automation.RemoveTorrentErrors
		return []types.CheckResult{
			{Check: "rule.enabled", Expected: "true", Actual: strconv.FormatBool(rule.Enabled), Passed: rule.Enabled},
			{Check: "queue.trackedDownloadStatus", Expected: "warning", Actual: item.TrackedDownloadStatus, Passed: item.TrackedDownloadStatus == "warning"},
			{Check: "error_message", Expected: "set", Actual: strconv.FormatBool(item.ErrorMessage != ""), Passed: item.ErrorMessage != ""},
			e.ageCheck(item, rule.WaitHours),
			{Check: "queue.trackedDownloadState", Expected: "!= importing", Actual: item.TrackedDownloadState, Passed: item.TrackedDownloadState != "importing"},
			e.retryCheck(key),
		}
	case types.IssueImportFailed:
		autoImport := e.cfg.Automation.AutoManualImport.Enabled
		retryImports := e.cfg.Automation.RetryImports.Enabled
		return []types.CheckResult{
			{Check: "recovery.possible", Expected: "autoManualImport.enabled OR retryImports.enabled",
				Actual: fmt.Sprintf("autoManualImport=%t retryImports=%t", autoImport, retryImports),
				Passed: autoImport || retryImports},
			e.retryCheck(key),
		}
	case types.IssueReconcile:
		enabled := e.cfg.Automation.Reconcile.Enabled
		return []types.CheckResult{
			{Check: "rule.enabled", Expected: "true", Actual: strconv.FormatBool(enabled), Passed: enabled},
			{Check: "queue.status", Expected: "completed|warning|failed", Actual: item.Status, Passed: eligibleStatus(item.Status)},
			{Check: "queue.trackedDownloadState", Expected: "!= importing", Actual: item.TrackedDownloadState, Passed: item.TrackedDownloadState != "importing"},
			e.retryCheck(key),
		}
	default:
		// No config-derived gates for unknown/other issue types.
		return nil
	}
}

// globalConstraints builds the always-enforced checks (SPEC §7), in order:
// duplicate action, cooldown, Sonarr connectivity, exclusions, state
// eligibility (remove actions only).
func (e *Engine) globalConstraints(issue types.Issue, action types.ActionType) []types.CheckResult {
	item := issue.QueueItem
	now := time.Now()

	// 1. No duplicate actions: same item composite key + same action within
	// 5 minutes is skipped.
	e.mu.Lock()
	actionKey := itemActionKey(item, action)
	lastApproved, seen := e.activeItems[actionKey]
	if seen && now.Sub(lastApproved) >= duplicateWindow {
		delete(e.activeItems, actionKey) // stale; prune opportunistically
		seen = false
	}
	ck := cooldownKey(item)
	lastAct, seenAct := e.lastAction[ck]
	if seenAct && now.Sub(lastAct) >= cooldownWindow {
		delete(e.lastAction, ck)
		seenAct = false
	}
	e.mu.Unlock()

	checks := make([]types.CheckResult, 0, 6)

	if !seen {
		checks = append(checks, types.CheckResult{
			Check: "duplicate.action", Expected: "no action for this item in last 5m", Actual: "none", Passed: true,
		})
	} else {
		elapsed := now.Sub(lastApproved)
		checks = append(checks, types.CheckResult{
			Check: "duplicate.action", Expected: "no action for this item in last 5m",
			Actual: elapsed.Round(time.Second).String() + " ago", Passed: elapsed >= duplicateWindow,
		})
	}

	// 2. Cooldown: at least 30 minutes between actions on the same
	// series:episode pair.
	if !seenAct {
		checks = append(checks, types.CheckResult{
			Check: "cooldown", Expected: ">= 30m since last action on series:episode", Actual: "none", Passed: true,
		})
	} else {
		elapsed := now.Sub(lastAct)
		checks = append(checks, types.CheckResult{
			Check: "cooldown", Expected: ">= 30m since last action on series:episode",
			Actual: elapsed.Round(time.Second).String() + " ago", Passed: elapsed >= cooldownWindow,
		})
	}

	// 3. Sonarr connectivity required.
	up := e.sonarrUp.Load()
	checks = append(checks, types.CheckResult{
		Check: "sonarr.up", Expected: "true", Actual: strconv.FormatBool(up), Passed: up,
	})

	// 4a. Exclusion list: series ID exact match.
	excludedSeries := ""
	for _, id := range e.cfg.Exclusions.SeriesIDs {
		if id == item.SeriesID {
			excludedSeries = strconv.Itoa(id)
			break
		}
	}
	checks = append(checks, types.CheckResult{
		Check: "exclusion.series", Expected: "series not in exclusions",
		Actual: func() string {
			if excludedSeries != "" {
				return "series " + excludedSeries + " excluded"
			}
			return "not excluded"
		}(),
		Passed: excludedSeries == "",
	})

	// 4b. Exclusion list: root path prefix match on the item's output path.
	matchedRoot := ""
	for _, root := range e.cfg.Exclusions.RootPaths {
		if strings.HasPrefix(item.OutputPath, root) {
			matchedRoot = root
			break
		}
	}
	checks = append(checks, types.CheckResult{
		Check: "exclusion.root_path", Expected: "output path has no excluded prefix",
		Actual: func() string {
			if matchedRoot != "" {
				return "matches excluded root " + strconv.Quote(matchedRoot)
			}
			return "none"
		}(),
		Passed: matchedRoot == "",
	})

	// 5. State eligibility for remove actions.
	if action == types.ActionRemoveQueue {
		checks = append(checks, types.CheckResult{
			Check: "state.eligible", Expected: "completed|warning|failed", Actual: item.Status, Passed: eligibleStatus(item.Status),
		})
	}

	return checks
}

// ageCheck builds the age_hours gate: hours since the item was added must be
// at least minHours. A zero Added timestamp skips the gate and records the
// actual as "unknown" (the detector could not compute an age).
func (e *Engine) ageCheck(item types.QueueItem, minHours float64) types.CheckResult {
	expected := ">= " + strconv.FormatFloat(minHours, 'f', -1, 64)
	if item.Added.IsZero() {
		return types.CheckResult{Check: "age_hours", Expected: expected, Actual: "unknown", Passed: true}
	}
	hours := time.Since(item.Added).Hours()
	return types.CheckResult{
		Check: "age_hours", Expected: expected, Actual: fmt.Sprintf("%.1f", hours), Passed: hours >= minHours,
	}
}

// retryCheck builds the retry.scheduled gate: no active retry for the item.
func (e *Engine) retryCheck(key string) types.CheckResult {
	e.mu.Lock()
	active := e.activeRetries[key]
	e.mu.Unlock()
	return types.CheckResult{
		Check: "retry.scheduled", Expected: "false", Actual: strconv.FormatBool(active), Passed: !active,
	}
}

// manualImportCheck builds the manual_import.scheduled gate (SPEC §3.2): a
// manual import approved for this item within the duplicate-action window
// means the import may still be running — a removal must not race it. The
// check is explicit in the decision log even though the series:episode
// cooldown would eventually block the same pair.
func (e *Engine) manualImportCheck(key string) types.CheckResult {
	actionKey := key + "|" + string(types.ActionManualImport)
	e.mu.Lock()
	last, seen := e.activeItems[actionKey]
	if seen && time.Since(last) >= duplicateWindow {
		delete(e.activeItems, actionKey) // stale; prune opportunistically
		seen = false
	}
	e.mu.Unlock()
	if !seen {
		return types.CheckResult{
			Check: "manual_import.scheduled", Expected: "no manual import in last 5m", Actual: "none", Passed: true,
		}
	}
	return types.CheckResult{
		Check: "manual_import.scheduled", Expected: "no manual import in last 5m",
		Actual: time.Since(last).Round(time.Second).String() + " ago", Passed: false,
	}
}

// reject builds the rejected decision, records it in the ring buffer, and
// emits the single action.skipped log line (SPEC §9).
func (e *Engine) reject(issue types.Issue, action types.ActionType, checks []types.CheckResult) *types.Decision {
	dec := &types.Decision{
		Issue:     issue,
		Action:    action,
		Checks:    checks,
		Approved:  false,
		Reason:    firstFailure(checks),
		Timestamp: time.Now(),
		DryRun:    e.cfg.DryRun,
	}
	e.pushDecision(*dec)
	e.logSkipped(*dec)
	return dec
}

// firstFailure returns a description of the first failing check, used as the
// decision reason (SPEC §5.4).
func firstFailure(checks []types.CheckResult) string {
	for _, c := range checks {
		if !c.Passed {
			return fmt.Sprintf("%s: expected %s, got %s", c.Check, c.Expected, c.Actual)
		}
	}
	return ""
}

// logSkipped emits the action.skipped info line with the full decision fields.
func (e *Engine) logSkipped(dec types.Decision) {
	item := dec.Issue.QueueItem
	decisionID := dec.Issue.ID
	if decisionID == "" {
		decisionID = "dec_" + item.CompositeKey()
	}
	e.logger.Info(
		fmt.Sprintf("Skipped %s for queue item %d: %s", dec.Action, item.ID, dec.Reason),
		"event", "action.skipped",
		"decision_id", decisionID,
		"item", map[string]any{
			"key":     item.CompositeKey(),
			"id":      item.ID,
			"title":   item.SeriesTitle,
			"series":  item.SeriesTitle,
			"episode": item.EpisodeTitle,
		},
		"trigger", string(dec.Issue.Type),
		"checks", checksToLog(dec.Checks),
		"action", string(dec.Action),
		"reason", dec.Reason,
		"dry_run", dec.DryRun,
	)
}

// checksToLog renders checks as maps so the JSON logger emits lowercase keys
// matching the SPEC §7 decision log format.
func checksToLog(checks []types.CheckResult) []map[string]any {
	out := make([]map[string]any, 0, len(checks))
	for _, c := range checks {
		out = append(out, map[string]any{
			"check":    c.Check,
			"expected": c.Expected,
			"actual":   c.Actual,
			"passed":   c.Passed,
		})
	}
	return out
}

// pushDecision appends a decision to the bounded ring buffer, dropping the
// oldest entry when full.
func (e *Engine) pushDecision(d types.Decision) {
	e.ringMu.Lock()
	defer e.ringMu.Unlock()
	if e.ring == nil {
		e.ring = make([]types.Decision, ringCapacity)
	}
	if e.ringCount == ringCapacity {
		e.ring[e.ringStart] = d
		e.ringStart = (e.ringStart + 1) % ringCapacity
		return
	}
	e.ring[(e.ringStart+e.ringCount)%ringCapacity] = d
	e.ringCount++
}

// itemActionKey is the duplicate-action tracking key: item composite key plus
// the action, so the same item may act again under a different action.
func itemActionKey(item types.QueueItem, action types.ActionType) string {
	return item.CompositeKey() + "|" + string(action)
}

// cooldownKey is the series:episode pair key used for the cooldown constraint.
// Unknown-series items (seriesId and episodeId both 0) share no meaningful
// pair, so their download ID becomes the bucket: otherwise every such item
// would serialize on the same "0:0" cooldown.
func cooldownKey(item types.QueueItem) string {
	if item.SeriesID == 0 && item.EpisodeID == 0 && item.DownloadID != "" {
		return "dl:" + item.DownloadID
	}
	return strconv.Itoa(item.SeriesID) + ":" + strconv.Itoa(item.EpisodeID)
}

// eligibleStatus reports whether a queue status is eligible for action
// (SPEC §3.2: completed, warning, or failed).
func eligibleStatus(status string) bool {
	return status == "completed" || status == "warning" || status == "failed"
}
