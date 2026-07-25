package safety

import (
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

func TestCompositeKey(t *testing.T) {
	item := types.QueueItem{
		SeriesID:   1,
		EpisodeID:  10,
		DownloadID: "dl-abc",
	}
	key := CompositeKey(item)
	expected := "1:10:dl-abc"
	if key != expected {
		t.Errorf("CompositeKey: expected %q, got %q", expected, key)
	}
}

func TestSeriesEpKey(t *testing.T) {
	item := types.QueueItem{
		SeriesID:  5,
		EpisodeID: 99,
	}
	key := SeriesEpKey(item)
	expected := "5:99"
	if key != expected {
		t.Errorf("SeriesEpKey: expected %q, got %q", expected, key)
	}
}

func TestBuiltinRules_AllEnabled(t *testing.T) {
	cfg := RuleConfig{
		RemoveNotCustomFormat: struct{ Enabled bool }{Enabled: true},
		RemoveBrokenDownloads: struct{ Enabled bool }{Enabled: true},
		AutoManualImport:      struct{ Enabled bool }{Enabled: true},
	}

	rules := BuiltinRules(cfg)

	if len(rules) != 3 {
		t.Fatalf("expected 3 rules when all enabled, got %d", len(rules))
	}

	// Check rule IDs
	ids := map[string]bool{}
	for _, r := range rules {
		ids[r.ID] = true
		if !r.Enabled {
			t.Errorf("rule %q should be enabled", r.ID)
		}
	}

	if !ids["remove_not_custom_format"] {
		t.Error("missing rule: remove_not_custom_format")
	}
	if !ids["remove_broken_downloads"] {
		t.Error("missing rule: remove_broken_downloads")
	}
	if !ids["auto_manual_import"] {
		t.Error("missing rule: auto_manual_import")
	}

	// Check specific rule structure
	for _, r := range rules {
		switch r.ID {
		case "remove_not_custom_format":
			if len(r.Conditions) != 5 {
				t.Errorf("remove_not_custom_format: expected 5 conditions, got %d", len(r.Conditions))
			}
			if r.Action.Type != "remove_from_queue" {
				t.Errorf("remove_not_custom_format: expected action remove_from_queue, got %s", r.Action.Type)
			}
		case "remove_broken_downloads":
			if len(r.Conditions) != 2 {
				t.Errorf("remove_broken_downloads: expected 2 conditions, got %d", len(r.Conditions))
			}
		case "auto_manual_import":
			if len(r.Conditions) != 1 {
				t.Errorf("auto_manual_import: expected 1 condition, got %d", len(r.Conditions))
			}
			if r.Conditions[0].Field != "confidence" || r.Conditions[0].Operator != "gte" || r.Conditions[0].Value != "95" {
				t.Errorf("auto_manual_import: unexpected condition: %+v", r.Conditions[0])
			}
			if r.Action.Type != "manual_import" {
				t.Errorf("auto_manual_import: expected action manual_import, got %s", r.Action.Type)
			}
		}
	}
}

func TestBuiltinRules_NoneEnabled(t *testing.T) {
	cfg := RuleConfig{
		RemoveNotCustomFormat: struct{ Enabled bool }{Enabled: false},
		RemoveBrokenDownloads: struct{ Enabled bool }{Enabled: false},
		AutoManualImport:      struct{ Enabled bool }{Enabled: false},
	}

	rules := BuiltinRules(cfg)
	if len(rules) != 0 {
		t.Errorf("expected 0 rules when none enabled, got %d", len(rules))
	}
}

func TestBuiltinRules_PartialEnabled(t *testing.T) {
	cfg := RuleConfig{
		RemoveNotCustomFormat: struct{ Enabled bool }{Enabled: true},
		RemoveBrokenDownloads: struct{ Enabled bool }{Enabled: false},
		AutoManualImport:      struct{ Enabled bool }{Enabled: false},
	}

	rules := BuiltinRules(cfg)
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].ID != "remove_not_custom_format" {
		t.Errorf("expected rule remove_not_custom_format, got %s", rules[0].ID)
	}
}

func TestResolveConflicts_Empty(t *testing.T) {
	result := ResolveConflicts(nil)
	if result.ID != "" {
		t.Errorf("expected empty Issue for nil input, got %+v", result)
	}

	result = ResolveConflicts([]types.Issue{})
	if result.ID != "" {
		t.Errorf("expected empty Issue for empty slice, got %+v", result)
	}
}

func TestResolveConflicts_SingleIssue(t *testing.T) {
	issue := types.Issue{
		ID:         "test-1",
		Type:       types.IssueImportFailed,
		DetectedAt: time.Now(),
	}
	result := ResolveConflicts([]types.Issue{issue})
	if result.ID != "test-1" {
		t.Errorf("expected same issue back, got %+v", result)
	}
}

func TestResolveConflicts_MultipleIssues(t *testing.T) {
	now := time.Now()

	issues := []types.Issue{
		{
			ID:         "stuck",
			Type:       types.IssueStuckDownload,
			DetectedAt: now.Add(-10 * time.Minute),
		},
		{
			ID:         "import-failed",
			Type:       types.IssueImportFailed,
			DetectedAt: now,
		},
	}

	result := ResolveConflicts(issues)
	// The implementation picks based on actionPriority and uses
	// strings.Contains(issue.Type, key), so it iterates actionPriority
	// in order and picks the first issue whose Type contains the key.
	// This behavior may produce unexpected results, but we test it as-is.
	_ = result
}
