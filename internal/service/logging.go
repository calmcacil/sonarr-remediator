package service

import (
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/state"
)

const summaryInterval = 30 * time.Minute

type summaryState struct {
	last            time.Time
	mutations       map[string]int
	outcomes        map[string]int
	recommendations int
}

func (s *Service) logMutation(attempt *state.Attempt) {
	if s.summary.mutations == nil {
		s.summary.mutations = map[string]int{}
	}
	s.summary.mutations[attempt.Kind]++
	s.Log.Info("mutation attempted", "download_id", attempt.DownloadID, "episodes", attempt.Decision.EpisodeIDs, "action", attempt.Kind, "blocklist", attempt.Decision.Blocklist, "outcome", attempt.Outcome)
}

func (s *Service) collectSummaryOutcomes(previous map[int64]string) {
	if s.request != nil && !s.request.Execute {
		return
	}
	for _, attempt := range s.Store.Data.Attempts {
		if attempt.DryRun || previous[attempt.ID] == attempt.Outcome || attempt.Outcome == "intent" {
			continue
		}
		if s.summary.outcomes == nil {
			s.summary.outcomes = map[string]int{}
		}
		s.summary.outcomes[attempt.Outcome]++
		previous[attempt.ID] = attempt.Outcome
	}
}

func (s *Service) logSummary() {
	now := s.Now()
	trigger := "interval"
	if len(s.summary.mutations) != 0 {
		trigger = "action"
	} else if s.summary.last.IsZero() {
		trigger = "startup"
	} else if now.Sub(s.summary.last) < summaryInterval {
		return
	}
	counts := map[string]int{}
	active, unresolved := 0, 0
	for _, issue := range s.Store.Data.Issues {
		if issue.State != "resolved" {
			counts[issue.State]++
			active++
		}
	}
	for _, attempt := range s.Store.Data.Attempts {
		if attempt.Pending() {
			unresolved++
		}
	}
	mutations, outcomes := s.summary.mutations, s.summary.outcomes
	if mutations == nil {
		mutations = map[string]int{}
	}
	if outcomes == nil {
		outcomes = map[string]int{}
	}
	s.Log.Info("service summary", "trigger", trigger, "dry_run", s.Config.DryRun,
		"connectivity", s.Store.Data.Status.Connectivity, "complete_scan", s.Store.Data.Status.CompleteScan,
		"active_issues", active, "issue_counts", counts, "unresolved_actions", unresolved,
		"mutation_attempts", mutations, "action_outcomes", outcomes,
		"dry_run_recommendations", s.summary.recommendations,
		"mutations_last_hour", s.Store.Data.Budget(now), "hour_limit", s.Config.Limits.MutationsPerHour)
	s.summary = summaryState{last: now}
}
