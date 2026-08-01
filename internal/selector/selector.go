// Package selector implements episode-level reconciliation of targeted
// releases (SPEC §3.2, release context): when several releases that would
// otherwise be discarded target the same episode, the one with the highest
// custom-format score is kept for import and the rest are marked for
// discard. It is a pure planning package — it performs no API calls and no
// mutations; callers decide how to execute a plan.
package selector

import (
	"sort"
	"strconv"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// Reconcile groups targeted releases by matched episode and selects one
// import winner per episode: the release with the highest custom-format
// score. Ties resolve to the earliest-added release, then to input order.
// The remaining releases of the episode are returned as the plan's discards.
//
// Releases without an episode match (EpisodeID == 0) cannot be grouped and
// are returned in unmatched for the caller's existing per-item flow. Every
// input release appears exactly once: as a winner, in some plan's discards,
// or in unmatched.
//
// Output is deterministic: plans are sorted by episode key.
func Reconcile(hits []types.QueueItem) (plans []types.ReconcilePlan, unmatched []types.QueueItem) {
	byEpisode := make(map[string][]types.QueueItem)
	for _, it := range hits {
		if it.EpisodeID == 0 {
			unmatched = append(unmatched, it)
			continue
		}
		key := strconv.Itoa(it.SeriesID) + ":" + strconv.Itoa(it.EpisodeID)
		byEpisode[key] = append(byEpisode[key], it)
	}

	keys := make([]string, 0, len(byEpisode))
	for k := range byEpisode {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		group := byEpisode[key]
		winner := best(group)
		var discards []types.QueueItem
		for _, it := range group {
			if it.DownloadID != winner.DownloadID {
				discards = append(discards, it)
			}
		}
		plans = append(plans, types.ReconcilePlan{
			SeriesID:  winner.SeriesID,
			EpisodeID: winner.EpisodeID,
			Winner:    winner,
			Discards:  discards,
		})
	}
	return plans, unmatched
}

// best returns the highest-scoring release of the group, breaking ties by
// earliest Added and then by input order.
func best(group []types.QueueItem) types.QueueItem {
	best := group[0]
	for _, it := range group[1:] {
		if it.CustomFormatScore > best.CustomFormatScore {
			best = it
			continue
		}
		if it.CustomFormatScore == best.CustomFormatScore && it.Added.Before(best.Added) {
			best = it
		}
	}
	return best
}

// IsUpgrade reports whether a release should replace the existing episode
// file (SPEC §3.2): a strictly higher custom-format score is an upgrade; on
// equal scores the release must be strictly better in quality. weightsOK
// reports whether both quality weights were resolved — without both, equal
// scores never prove an upgrade, mirroring the recovery pre-import check's
// conservative fallback.
func IsUpgrade(releaseScore, existingScore int, releaseWeight, existingWeight int, weightsOK bool) bool {
	if releaseScore != existingScore {
		return releaseScore > existingScore
	}
	return weightsOK && releaseWeight > existingWeight
}
