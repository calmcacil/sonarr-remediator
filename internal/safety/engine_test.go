package safety

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

func TestMain(m *testing.M) {
	logging.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	os.Exit(m.Run())
}

// newTestEngine is a helper to create an Engine with fewer arguments.
func newTestEngine(rules []SafetyRule, excludedSeries []int, excludedPaths []string, dryRun bool) *Engine {
	return New(rules, excludedSeries, excludedPaths, dryRun)
}

// defaultMatchingIssue returns an Issue that passes all global constraints and can match a basic rule.
func defaultMatchingIssue() types.Issue {
	return types.Issue{
		QueueItem: types.QueueItem{
			ID:                    1,
			SeriesID:              1,
			EpisodeID:             10,
			DownloadID:            "dl-001",
			Status:                "completed",
			TrackedDownloadState:  "importPending",
			TrackedDownloadStatus: "ok",
			OutputPath:            "/downloads/show",
			Added:                 time.Now().Add(-3 * time.Hour),
		},
		Details: map[string]any{},
	}
}

// simpleRule creates a simple enabled rule with one condition.
func simpleRule(id, field, operator, value string) SafetyRule {
	return SafetyRule{
		ID:          id,
		Description: "test rule " + id,
		Enabled:     true,
		Conditions:  []Condition{{Field: field, Operator: operator, Value: value}},
		Action:      Action{Type: "remove_from_queue", Params: map[string]string{}},
	}
}

// ─── 1. Duplicate action cooldown ──────────────────────────────────────

func TestEngine_DuplicateActionCooldown(t *testing.T) {
	engine := newTestEngine(
		[]SafetyRule{simpleRule("r1", "queue.status", "eq", "completed")},
		nil, nil, false,
	)
	ctx := context.Background()
	issue := defaultMatchingIssue()

	// First call — should produce a decision
	dec, err := engine.Evaluate(ctx, issue)
	if err != nil {
		t.Fatalf("first Evaluate: unexpected error: %v", err)
	}
	if dec == nil {
		t.Fatal("first Evaluate: expected decision, got nil")
	}

	// Second call with the same item — duplicate cooldown (<5 min)
	dec, err = engine.Evaluate(ctx, issue)
	if err == nil {
		t.Fatal("second Evaluate: expected error for duplicate cooldown, got nil")
	}
	if dec != nil {
		t.Fatal("second Evaluate: expected nil decision, got non-nil")
	}
}

// ─── 2. Series/episode cooldown ────────────────────────────────────────

func TestEngine_SeriesEpisodeCooldown(t *testing.T) {
	engine := newTestEngine(
		[]SafetyRule{simpleRule("r1", "queue.status", "eq", "completed")},
		nil, nil, false,
	)
	ctx := context.Background()

	issue1 := defaultMatchingIssue() // seriesID=1, episodeID=10, downloadID="dl-001"
	dec, err := engine.Evaluate(ctx, issue1)
	if err != nil {
		t.Fatalf("first Evaluate: unexpected error: %v", err)
	}
	if dec == nil {
		t.Fatal("first Evaluate: expected decision, got nil")
	}

	// Second call: same series+episode, different downloadID -> composite key differs,
	// but series/ep cooldown (<30 min) triggers.
	issue2 := defaultMatchingIssue()
	issue2.QueueItem.DownloadID = "dl-002" // different download

	dec, err = engine.Evaluate(ctx, issue2)
	if err != nil {
		t.Fatalf("second Evaluate: unexpected error: %v", err)
	}
	if dec != nil {
		t.Fatal("second Evaluate: expected nil decision (cooldown), got non-nil")
	}
}

// ─── 3. Excluded series ────────────────────────────────────────────────

func TestEngine_ExcludedSeries(t *testing.T) {
	// SeriesID 1 is excluded
	engine := newTestEngine(
		[]SafetyRule{simpleRule("r1", "queue.status", "eq", "completed")},
		[]int{1}, nil, false,
	)
	ctx := context.Background()
	issue := defaultMatchingIssue() // seriesID=1

	dec, err := engine.Evaluate(ctx, issue)
	if err != nil {
		t.Fatalf("Evaluate: unexpected error: %v", err)
	}
	if dec != nil {
		t.Fatal("expected nil decision for excluded series")
	}
}

// ─── 4. Excluded paths ─────────────────────────────────────────────────

func TestEngine_ExcludedPaths(t *testing.T) {
	engine := newTestEngine(
		[]SafetyRule{simpleRule("r1", "queue.status", "eq", "completed")},
		nil, []string{"/downloads/show"}, false,
	)
	ctx := context.Background()
	issue := defaultMatchingIssue() // OutputPath="/downloads/show"

	dec, err := engine.Evaluate(ctx, issue)
	if err != nil {
		t.Fatalf("Evaluate: unexpected error: %v", err)
	}
	if dec != nil {
		t.Fatal("expected nil decision for excluded path")
	}
}

// ─── 5. Ineligible states ──────────────────────────────────────────────

func TestEngine_IneligibleStates(t *testing.T) {
	engine := newTestEngine(
		[]SafetyRule{simpleRule("r1", "queue.status", "eq", "completed")},
		nil, nil, false,
	)
	ctx := context.Background()

	ineligible := []string{"queued", "paused", "downloading", ""}
	for _, status := range ineligible {
		t.Run(status, func(t *testing.T) {
			issue := defaultMatchingIssue()
			issue.QueueItem.Status = status

			dec, err := engine.Evaluate(ctx, issue)
			if err != nil {
				t.Fatalf("Evaluate: unexpected error: %v", err)
			}
			if dec != nil {
				t.Errorf("expected nil decision for status %q, got non-nil", status)
			}
		})
	}
}

// ─── 6. Rule matching ──────────────────────────────────────────────────

func TestEngine_RuleMatching(t *testing.T) {
	rule := SafetyRule{
		ID:          "test-rule",
		Description: "Matches when status is completed",
		Enabled:     true,
		Conditions: []Condition{
			{Field: "queue.status", Operator: "eq", Value: "completed"},
			{Field: "queue.trackedDownloadState", Operator: "eq", Value: "importFailed"},
			{Field: "currently_importing", Operator: "eq", Value: "false"},
		},
		Action: Action{Type: "remove_from_queue"},
	}
	engine := newTestEngine([]SafetyRule{rule}, nil, nil, false)
	ctx := context.Background()

	issue := defaultMatchingIssue()
	issue.QueueItem.TrackedDownloadState = "importFailed"

	dec, err := engine.Evaluate(ctx, issue)
	if err != nil {
		t.Fatalf("Evaluate: unexpected error: %v", err)
	}
	if dec == nil {
		t.Fatal("expected approved decision for matching rule")
	}
	if !dec.Approved {
		t.Error("expected decision.Approved to be true")
	}
	if dec.Rule.ID != "test-rule" {
		t.Errorf("expected rule ID 'test-rule', got %q", dec.Rule.ID)
	}
	if dec.Action.Type != "remove_from_queue" {
		t.Errorf("expected action type 'remove_from_queue', got %q", dec.Action.Type)
	}
}

// ─── 7. Rule non-matching ──────────────────────────────────────────────

func TestEngine_RuleNonMatching(t *testing.T) {
	// The rule expects status "failed", but the issue has "completed"
	engine := newTestEngine(
		[]SafetyRule{simpleRule("r1", "queue.status", "eq", "failed")},
		nil, nil, false,
	)
	ctx := context.Background()
	issue := defaultMatchingIssue() // status = "completed"

	dec, err := engine.Evaluate(ctx, issue)
	if err != nil {
		t.Fatalf("Evaluate: unexpected error: %v", err)
	}
	if dec != nil {
		t.Fatal("expected nil decision when rule conditions do not match")
	}
}

// ─── 8. Multiple rules — first matching wins ────────────────────────────

func TestEngine_MultipleRulesFirstWins(t *testing.T) {
	rule1 := simpleRule("first", "queue.status", "eq", "completed")
	rule2 := simpleRule("second", "queue.status", "eq", "warning")

	engine := newTestEngine([]SafetyRule{rule1, rule2}, nil, nil, false)
	ctx := context.Background()
	issue := defaultMatchingIssue() // status = "completed"

	dec, err := engine.Evaluate(ctx, issue)
	if err != nil {
		t.Fatalf("Evaluate: unexpected error: %v", err)
	}
	if dec == nil {
		t.Fatal("expected a decision")
	}
	if dec.Rule.ID != "first" {
		t.Errorf("expected first matching rule 'first', got %q", dec.Rule.ID)
	}
}

// ─── 9. Disabled rules are skipped ──────────────────────────────────────

func TestEngine_DisabledRulesSkipped(t *testing.T) {
	disabledRule := simpleRule("disabled-rule", "queue.status", "eq", "completed")
	disabledRule.Enabled = false

	enabledRule := simpleRule("enabled-rule", "queue.status", "eq", "warning")

	// issue has status "completed" — only the disabled rule matches, but it's skipped
	engine := newTestEngine([]SafetyRule{disabledRule, enabledRule}, nil, nil, false)
	ctx := context.Background()
	issue := defaultMatchingIssue() // status = "completed"

	dec, err := engine.Evaluate(ctx, issue)
	if err != nil {
		t.Fatalf("Evaluate: unexpected error: %v", err)
	}
	if dec != nil {
		t.Fatalf("expected nil decision (enabled rule doesn't match), got rule %q", dec.Rule.ID)
	}
}

// ─── 10. Condition operators ────────────────────────────────────────────

func TestEngine_ConditionOperators(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name         string
		field        string
		operator     string
		value        string
		issueMod     func(types.Issue) types.Issue
		wantDecision bool
	}{
		{
			name:     "eq passes",
			field:    "queue.status",
			operator: "eq",
			value:    "completed",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				return i
			},
			wantDecision: true,
		},
		{
			name:     "eq fails",
			field:    "queue.status",
			operator: "eq",
			value:    "failed",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				return i
			},
			wantDecision: false,
		},
		{
			name:     "neq passes",
			field:    "queue.status",
			operator: "neq",
			value:    "failed",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				return i
			},
			wantDecision: true,
		},
		{
			name:     "neq fails",
			field:    "queue.status",
			operator: "neq",
			value:    "completed",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				return i
			},
			wantDecision: false,
		},
		{
			name:     "gt passes",
			field:    "confidence",
			operator: "gt",
			value:    "90",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				i.Details = map[string]any{"confidence": 95}
				return i
			},
			wantDecision: true,
		},
		{
			name:     "gt fails",
			field:    "confidence",
			operator: "gt",
			value:    "90",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				i.Details = map[string]any{"confidence": 85}
				return i
			},
			wantDecision: false,
		},
		{
			name:     "gte passes (equal)",
			field:    "confidence",
			operator: "gte",
			value:    "95",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				i.Details = map[string]any{"confidence": 95}
				return i
			},
			wantDecision: true,
		},
		{
			name:     "gte passes (greater)",
			field:    "confidence",
			operator: "gte",
			value:    "90",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				i.Details = map[string]any{"confidence": 95}
				return i
			},
			wantDecision: true,
		},
		{
			name:     "gte fails",
			field:    "confidence",
			operator: "gte",
			value:    "95",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				i.Details = map[string]any{"confidence": 80}
				return i
			},
			wantDecision: false,
		},
		{
			name:     "lt passes",
			field:    "confidence",
			operator: "lt",
			value:    "90",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				i.Details = map[string]any{"confidence": 80}
				return i
			},
			wantDecision: true,
		},
		{
			name:     "lt fails",
			field:    "confidence",
			operator: "lt",
			value:    "90",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				i.Details = map[string]any{"confidence": 95}
				return i
			},
			wantDecision: false,
		},
		{
			name:     "lte passes (equal)",
			field:    "confidence",
			operator: "lte",
			value:    "95",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				i.Details = map[string]any{"confidence": 95}
				return i
			},
			wantDecision: true,
		},
		{
			name:     "lte passes (less)",
			field:    "confidence",
			operator: "lte",
			value:    "95",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				i.Details = map[string]any{"confidence": 80}
				return i
			},
			wantDecision: true,
		},
		{
			name:     "lte fails",
			field:    "confidence",
			operator: "lte",
			value:    "80",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				i.Details = map[string]any{"confidence": 95}
				return i
			},
			wantDecision: false,
		},
		{
			name:     "in passes",
			field:    "queue.status",
			operator: "in",
			value:    "completed,warning,failed",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				return i
			},
			wantDecision: true,
		},
		{
			name:     "in fails",
			field:    "queue.status",
			operator: "in",
			value:    "completed,warning,failed",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "queued"
				return i
			},
			wantDecision: false,
		},
		{
			name:     "matches passes",
			field:    "queue.status",
			operator: "matches",
			value:    "compl.*",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				return i
			},
			wantDecision: true,
		},
		{
			name:     "matches fails",
			field:    "queue.status",
			operator: "matches",
			value:    "fail.*",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				return i
			},
			wantDecision: false,
		},
		{
			name:     "unknown operator returns false",
			field:    "queue.status",
			operator: "unknown",
			value:    "completed",
			issueMod: func(i types.Issue) types.Issue {
				i.QueueItem.Status = "completed"
				return i
			},
			wantDecision: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := simpleRule("op-test", tt.field, tt.operator, tt.value)
			engine := newTestEngine([]SafetyRule{rule}, nil, nil, false)
			base := defaultMatchingIssue()
			issue := tt.issueMod(base)

			dec, err := engine.Evaluate(ctx, issue)
			if err != nil {
				t.Fatalf("Evaluate: unexpected error: %v", err)
			}
			if tt.wantDecision && dec == nil {
				t.Error("expected a decision, got nil")
			}
			if !tt.wantDecision && dec != nil {
				t.Errorf("expected no decision, but got rule %q", dec.Rule.ID)
			}
		})
	}
}

// ─── 11. Field resolution ──────────────────────────────────────────────

func TestEngine_FieldResolution(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name  string
		field string
		setup func() types.Issue
	}{
		{
			name:  "queue.status",
			field: "queue.status",
			setup: func() types.Issue {
				i := defaultMatchingIssue()
				i.QueueItem.Status = "completed"
				return i
			},
		},
		{
			name:  "queue.trackedDownloadState",
			field: "queue.trackedDownloadState",
			setup: func() types.Issue {
				i := defaultMatchingIssue()
				i.QueueItem.TrackedDownloadState = "importFailed"
				return i
			},
		},
		{
			name:  "queue.trackedDownloadStatus",
			field: "queue.trackedDownloadStatus",
			setup: func() types.Issue {
				i := defaultMatchingIssue()
				i.QueueItem.TrackedDownloadStatus = "error"
				return i
			},
		},
		{
			name:  "status_message",
			field: "status_message",
			setup: func() types.Issue {
				i := defaultMatchingIssue()
				i.QueueItem.StatusMessages = []types.StatusMessage{
					{Messages: []string{"Not a custom format upgrade"}},
				}
				return i
			},
		},
		{
			name:  "age_hours",
			field: "age_hours",
			setup: func() types.Issue {
				i := defaultMatchingIssue()
				i.QueueItem.Added = time.Now().Add(-3 * time.Hour)
				return i
			},
		},
		{
			name:  "currently_importing true",
			field: "currently_importing",
			setup: func() types.Issue {
				i := defaultMatchingIssue()
				i.QueueItem.TrackedDownloadState = "importing"
				return i
			},
		},
		{
			name:  "currently_importing false",
			field: "currently_importing",
			setup: func() types.Issue {
				i := defaultMatchingIssue()
				i.QueueItem.TrackedDownloadState = "importFailed"
				return i
			},
		},
		{
			name:  "confidence",
			field: "confidence",
			setup: func() types.Issue {
				i := defaultMatchingIssue()
				i.Details = map[string]any{"confidence": 95}
				return i
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := tt.setup()

			var cond Condition
			switch tt.field {
			case "age_hours", "confidence":
				cond = Condition{Field: tt.field, Operator: "gte", Value: "0"}
			case "currently_importing":
				val := "false"
				if issue.QueueItem.TrackedDownloadState == "importing" {
					val = "true"
				}
				cond = Condition{Field: tt.field, Operator: "eq", Value: val}
			default:
				cond = Condition{Field: tt.field, Operator: "matches", Value: ".*"}
			}

			rule := SafetyRule{
				ID: "field-test", Enabled: true,
				Conditions: []Condition{cond},
				Action:     Action{Type: "remove_from_queue"},
			}
			engine := newTestEngine([]SafetyRule{rule}, nil, nil, false)
			dec, err := engine.Evaluate(ctx, issue)
			if err != nil {
				t.Fatalf("Evaluate: unexpected error: %v", err)
			}
			if dec == nil {
				t.Errorf("expected decision for field %q", tt.field)
			}
		})
	}
}

// ─── 12. Decision log ──────────────────────────────────────────────────

func TestEngine_DecisionLog(t *testing.T) {
	engine := newTestEngine(
		[]SafetyRule{simpleRule("r1", "queue.status", "eq", "completed")},
		nil, nil, false,
	)
	ctx := context.Background()

	// Evaluate two different issues (different download IDs) to generate decisions
	issue1 := defaultMatchingIssue()
	dec1, err := engine.Evaluate(ctx, issue1)
	if err != nil {
		t.Fatalf("first Evaluate: %v", err)
	}
	if dec1 == nil {
		t.Fatal("first Evaluate: expected decision")
	}

	issue2 := defaultMatchingIssue()
	issue2.QueueItem.DownloadID = "dl-002"
	issue2.QueueItem.ID = 2
	issue2.QueueItem.EpisodeID = 20
	dec2, err := engine.Evaluate(ctx, issue2)
	if err != nil {
		t.Fatalf("second Evaluate: %v", err)
	}
	if dec2 == nil {
		t.Fatal("second Evaluate: expected decision")
	}

	// RecentDecisions(0) — empty
	recent := engine.RecentDecisions(0)
	if len(recent) != 0 {
		t.Errorf("RecentDecisions(0): expected 0, got %d", len(recent))
	}

	// RecentDecisions(1) — last decision only
	recent = engine.RecentDecisions(1)
	if len(recent) != 1 {
		t.Errorf("RecentDecisions(1): expected 1, got %d", len(recent))
	}
	if recent[0].Issue.QueueItem.ID != 2 {
		t.Errorf("expected last decision to have QueueItem.ID=2, got %d", recent[0].Issue.QueueItem.ID)
	}

	// RecentDecisions(2) — both decisions
	recent = engine.RecentDecisions(2)
	if len(recent) != 2 {
		t.Errorf("RecentDecisions(2): expected 2, got %d", len(recent))
	}

	// RecentDecisions(10) — larger than log size, returns all
	recent = engine.RecentDecisions(10)
	if len(recent) != 2 {
		t.Errorf("RecentDecisions(10): expected 2, got %d", len(recent))
	}
}

// ─── 13. DryRun flag ───────────────────────────────────────────────────

func TestEngine_DryRun(t *testing.T) {
	// Engine with DryRun = true
	dryEngine := newTestEngine(
		[]SafetyRule{simpleRule("r1", "queue.status", "eq", "completed")},
		nil, nil, true,
	)
	ctx := context.Background()

	if !dryEngine.IsDryRun() {
		t.Fatal("expected IsDryRun() to be true")
	}

	issue := defaultMatchingIssue()
	dec, err := dryEngine.Evaluate(ctx, issue)
	if err != nil {
		t.Fatalf("Evaluate: unexpected error: %v", err)
	}
	if dec == nil {
		t.Fatal("expected a decision")
	}
	if !dec.DryRun {
		t.Error("expected decision.DryRun to be true")
	}

	// Engine with DryRun = false
	liveEngine := newTestEngine(
		[]SafetyRule{simpleRule("r1", "queue.status", "eq", "completed")},
		nil, nil, false,
	)
	if liveEngine.IsDryRun() {
		t.Fatal("expected IsDryRun() to be false")
	}

	dec, err = liveEngine.Evaluate(ctx, issue)
	if err != nil {
		t.Fatalf("Evaluate: unexpected error: %v", err)
	}
	if dec == nil {
		t.Fatal("expected a decision")
	}
	if dec.DryRun {
		t.Error("expected decision.DryRun to be false")
	}
}

// ─── Helper: ensure decision conditions are populated ───────────────────

func TestEngine_DecisionConditionsPopulated(t *testing.T) {
	rule := SafetyRule{
		ID: "cond-test", Enabled: true,
		Conditions: []Condition{
			{Field: "queue.status", Operator: "eq", Value: "completed"},
		},
		Action: Action{Type: "log_only"},
	}
	engine := newTestEngine([]SafetyRule{rule}, nil, nil, false)
	ctx := context.Background()
	issue := defaultMatchingIssue()

	dec, err := engine.Evaluate(ctx, issue)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if dec == nil {
		t.Fatal("expected decision")
	}
	if len(dec.Conditions) != 1 {
		t.Fatalf("expected 1 condition result, got %d", len(dec.Conditions))
	}
	cr := dec.Conditions[0]
	if cr.Field != "queue.status" {
		t.Errorf("expected field queue.status, got %q", cr.Field)
	}
	if cr.Operator != "eq" {
		t.Errorf("expected operator eq, got %q", cr.Operator)
	}
	if cr.Expected != "completed" {
		t.Errorf("expected value completed, got %q", cr.Expected)
	}
	if cr.Actual != "completed" {
		t.Errorf("expected actual completed, got %q", cr.Actual)
	}
	if !cr.Passed {
		t.Error("expected condition to pass")
	}
}
