package safety

import "github.com/calmcacil/sonarr-remediator/internal/types"

// Re-exports for convenience
type SafetyRule = types.SafetyRule
type Condition = types.Condition
type Action = types.Action

// BuiltinRules generates internal safety rules from config.
// This is called at startup to populate the engine.
func BuiltinRules(config RuleConfig) []SafetyRule {
	var rules []SafetyRule

	if config.RemoveNotCustomFormat.Enabled {
		rules = append(rules, SafetyRule{
			ID:          "remove_not_custom_format",
			Description: "Remove completed downloads that Sonarr determined are not a custom format upgrade",
			Enabled:     true,
			Conditions: []Condition{
				{Field: "queue.status", Operator: "eq", Value: "completed"},
				{Field: "queue.trackedDownloadState", Operator: "eq", Value: "importFailed"},
				{Field: "status_message", Operator: "matches", Value: "(?i)not.*(custom format|an upgrade)"},
				{Field: "age_hours", Operator: "gte", Value: "2"},
				{Field: "currently_importing", Operator: "eq", Value: "false"},
			},
			Action: Action{Type: "remove_from_queue", Params: map[string]string{"blocklist": "false"}},
		})
	}

	if config.RemoveBrokenDownloads.Enabled {
		rules = append(rules, SafetyRule{
			ID:          "remove_broken_downloads",
			Description: "Remove stuck/broken downloads",
			Enabled:     true,
			Conditions: []Condition{
				{Field: "queue.status", Operator: "in", Value: "completed,warning,failed"},
				{Field: "queue.trackedDownloadState", Operator: "neq", Value: "importing"},
			},
			Action: Action{Type: "remove_from_queue", Params: map[string]string{"blocklist": "false"}},
		})
	}

	if config.AutoManualImport.Enabled {
		rules = append(rules, SafetyRule{
			ID:          "auto_manual_import",
			Description: "Automatically recover failed imports with high confidence",
			Enabled:     true,
			Conditions: []Condition{
				{Field: "confidence", Operator: "gte", Value: "95"},
			},
			Action: Action{Type: "manual_import"},
		})
	}

	return rules
}

// RuleConfig provides automation settings for rule generation.
type RuleConfig struct {
	RemoveNotCustomFormat struct {
		Enabled bool
	}
	RemoveBrokenDownloads struct {
		Enabled bool
	}
	AutoManualImport struct {
		Enabled bool
	}
}
