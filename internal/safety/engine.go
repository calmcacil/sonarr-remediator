package safety

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/metrics"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// Engine evaluates safety rules and produces decisions.
type Engine struct {
	rules          []SafetyRule
	activeItems    map[string]time.Time // composite key -> active action time
	lastAction     map[string]time.Time // seriesId:episodeId -> last action time
	mu             sync.Mutex
	dryRun         bool
	excludedSeries map[int]bool
	excludedPaths  []string // prefixes
	decisionsLog   []types.Decision
}

// New creates a safety Engine.
func New(rules []SafetyRule, excludedSeries []int, excludedPaths []string, dryRun bool) *Engine {
	excl := make(map[int]bool)
	for _, id := range excludedSeries {
		excl[id] = true
	}
	return &Engine{
		rules:          rules,
		activeItems:    make(map[string]time.Time),
		lastAction:     make(map[string]time.Time),
		dryRun:         dryRun,
		excludedSeries: excl,
		excludedPaths:  excludedPaths,
	}
}

// Evaluate runs all applicable rules against an issue and returns a decision.
func (e *Engine) Evaluate(ctx context.Context, issue types.Issue) (*types.Decision, error) {
	key := CompositeKey(issue.QueueItem)
	epKey := SeriesEpKey(issue.QueueItem)

	e.mu.Lock()
	defer e.mu.Unlock()

	// Global constraint 1: No duplicate actions within 5 minutes
	if t, ok := e.activeItems[key]; ok && time.Since(t) < 5*time.Minute {
		return nil, fmt.Errorf("duplicate action within cooldown for %s", key)
	}

	// Global constraint 2: Cooldown between series/episode actions
	if t, ok := e.lastAction[epKey]; ok && time.Since(t) < 30*time.Minute {
		logging.Logger.Debug("cooldown active", "key", epKey, "since", time.Since(t))
		return nil, nil // not an error, just skip
	}

	// Global constraint 5: Exclusion list
	if e.excludedSeries[issue.QueueItem.SeriesID] {
		logging.Logger.Debug("series excluded", "seriesId", issue.QueueItem.SeriesID)
		return nil, nil
	}
	for _, prefix := range e.excludedPaths {
		if strings.HasPrefix(issue.QueueItem.OutputPath, prefix) {
			logging.Logger.Debug("path excluded", "path", issue.QueueItem.OutputPath, "prefix", prefix)
			return nil, nil
		}
	}

	// Global constraint 6: State eligibility
	if !isEligibleState(issue.QueueItem.Status) {
		return nil, nil
	}

	// Match against applicable rules
	for _, rule := range e.rules {
		if !rule.Enabled {
			continue
		}

		results, allPassed := e.evaluateConditions(rule.Conditions, issue)
		metrics.DecisionsEvaluated.WithLabelValues(rule.ID, fmt.Sprintf("%t", allPassed)).Inc()

		if allPassed {
			decision := &types.Decision{
				Issue:      issue,
				Rule:       rule,
				Action:     rule.Action,
				Conditions: results,
				Approved:   true,
				Timestamp:  time.Now(),
				DryRun:     e.dryRun,
			}
			e.activeItems[key] = time.Now()
			e.lastAction[epKey] = time.Now()
			e.decisionsLog = append(e.decisionsLog, *decision)

			// Trim log to last 1000 entries
			if len(e.decisionsLog) > 1000 {
				e.decisionsLog = e.decisionsLog[len(e.decisionsLog)-1000:]
			}

			return decision, nil
		}
	}

	return nil, nil
}

func (e *Engine) evaluateConditions(conditions []Condition, issue types.Issue) ([]types.ConditionResult, bool) {
	var results []types.ConditionResult
	allPassed := true

	for _, cond := range conditions {
		actual := e.resolveField(cond.Field, issue)
		passed := e.evaluateCondition(cond, actual)

		results = append(results, types.ConditionResult{
			Field:    cond.Field,
			Operator: cond.Operator,
			Expected: cond.Value,
			Actual:   actual,
			Passed:   passed,
		})

		if !passed {
			allPassed = false
			break // short-circuit on first failure (AND logic)
		}
	}

	return results, allPassed
}

func (e *Engine) resolveField(field string, issue types.Issue) string {
	item := issue.QueueItem
	switch field {
	case "queue.status":
		return item.Status
	case "queue.trackedDownloadState":
		return item.TrackedDownloadState
	case "queue.trackedDownloadStatus":
		return item.TrackedDownloadStatus
	case "status_message":
		var msgs []string
		for _, sm := range item.StatusMessages {
			msgs = append(msgs, sm.Messages...)
		}
		return strings.Join(msgs, "; ")
	case "age_hours":
		return fmt.Sprintf("%.1f", time.Since(item.Added).Hours())
	case "currently_importing":
		if item.TrackedDownloadState == "importing" {
			return "true"
		}
		return "false"
	case "confidence":
		// match confidence field
		if v, ok := issue.Details["confidence"]; ok {
			return fmt.Sprintf("%v", v)
		}
		return "0"
	default:
		return ""
	}
}

func (e *Engine) evaluateCondition(cond Condition, actual string) bool {
	switch cond.Operator {
	case "eq":
		return actual == cond.Value
	case "neq":
		return actual != cond.Value
	case "gt":
		av, _ := strconv.ParseFloat(actual, 64)
		ev, _ := strconv.ParseFloat(cond.Value, 64)
		return av > ev
	case "gte":
		av, _ := strconv.ParseFloat(actual, 64)
		ev, _ := strconv.ParseFloat(cond.Value, 64)
		return av >= ev
	case "lt":
		av, _ := strconv.ParseFloat(actual, 64)
		ev, _ := strconv.ParseFloat(cond.Value, 64)
		return av < ev
	case "lte":
		av, _ := strconv.ParseFloat(actual, 64)
		ev, _ := strconv.ParseFloat(cond.Value, 64)
		return av <= ev
	case "in":
		for _, v := range strings.Split(cond.Value, ",") {
			if strings.TrimSpace(v) == actual {
				return true
			}
		}
		return false
	case "matches":
		re, err := regexp.Compile(cond.Value)
		if err != nil {
			return false
		}
		return re.MatchString(actual)
	default:
		return false
	}
}

func isEligibleState(status string) bool {
	return status == "completed" || status == "warning" || status == "failed"
}

// RecentDecisions returns the last N decisions.
func (e *Engine) RecentDecisions(n int) []types.Decision {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n >= len(e.decisionsLog) {
		result := make([]types.Decision, len(e.decisionsLog))
		copy(result, e.decisionsLog)
		return result
	}
	start := len(e.decisionsLog) - n
	result := make([]types.Decision, n)
	copy(result, e.decisionsLog[start:])
	return result
}

// IsDryRun returns whether dry run is enabled.
func (e *Engine) IsDryRun() bool {
	return e.dryRun
}
