package selector

import (
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

func item(id, seriesID, episodeID int, score int, added time.Time) types.QueueItem {
	return types.QueueItem{
		ID:                id,
		SeriesID:          seriesID,
		EpisodeID:         episodeID,
		DownloadID:        "dl-" + itoa(id),
		Title:             "Release-" + itoa(id),
		CustomFormatScore: score,
		Added:             added,
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func planKeys(plans []types.ReconcilePlan) []string {
	out := make([]string, len(plans))
	for i, p := range plans {
		out[i] = p.EpisodeKey()
	}
	return out
}

func TestReconcile_EmptyInput(t *testing.T) {
	plans, unmatched := Reconcile(nil)
	if len(plans) != 0 || len(unmatched) != 0 {
		t.Fatalf("Reconcile(nil) = %d plans, %d unmatched; want 0, 0", len(plans), len(unmatched))
	}
}

func TestReconcile_HighestScoreWinsPerEpisode(t *testing.T) {
	now := time.Now()
	hits := []types.QueueItem{
		item(1, 42, 105, 500, now.Add(-3*time.Hour)),
		item(2, 42, 105, 1000, now.Add(-1*time.Hour)),
		item(3, 42, 105, 800, now.Add(-2*time.Hour)),
	}
	plans, unmatched := Reconcile(hits)
	if len(unmatched) != 0 {
		t.Fatalf("unmatched = %d, want 0", len(unmatched))
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	p := plans[0]
	if p.EpisodeKey() != "42:105" {
		t.Errorf("EpisodeKey = %q, want 42:105", p.EpisodeKey())
	}
	if p.Winner.ID != 2 {
		t.Errorf("Winner = item %d, want item 2 (score 1000)", p.Winner.ID)
	}
	if len(p.Discards) != 2 {
		t.Fatalf("discards = %d, want 2", len(p.Discards))
	}
	got := []int{p.Discards[0].ID, p.Discards[1].ID}
	if got[0] != 1 || got[1] != 3 {
		t.Errorf("discard order = %v, want [1 3] (input order)", got)
	}
}

func TestReconcile_TieBreaksByEarliestAdded(t *testing.T) {
	now := time.Now()
	hits := []types.QueueItem{
		item(1, 42, 105, 900, now.Add(-2*time.Hour)),
		item(2, 42, 105, 900, now.Add(-4*time.Hour)),
	}
	plans, _ := Reconcile(hits)
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	// Equal score: the earlier grab wins; the later one is discarded.
	if plans[0].Winner.ID != 2 {
		t.Errorf("Winner = item %d, want item 2 (added first on tie)", plans[0].Winner.ID)
	}
	if len(plans[0].Discards) != 1 || plans[0].Discards[0].ID != 1 {
		t.Errorf("discards = %+v, want [item 1]", plans[0].Discards)
	}
}

func TestReconcile_SingleHitPerEpisodeHasNoDiscards(t *testing.T) {
	now := time.Now()
	hits := []types.QueueItem{
		item(1, 42, 105, 1000, now.Add(-1*time.Hour)),
		item(2, 43, 106, 800, now.Add(-1*time.Hour)),
	}
	plans, unmatched := Reconcile(hits)
	if len(unmatched) != 0 {
		t.Fatalf("unmatched = %d, want 0", len(unmatched))
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(plans))
	}
	for _, p := range plans {
		if len(p.Discards) != 0 {
			t.Errorf("plan %s has %d discards, want 0", p.EpisodeKey(), len(p.Discards))
		}
	}
	// Deterministic ordering by episode key.
	if planKeys(plans)[0] != "42:105" || planKeys(plans)[1] != "43:106" {
		t.Errorf("plan order = %v, want [42:105 43:106]", planKeys(plans))
	}
}

func TestReconcile_NoEpisodeMatchIsUnmatched(t *testing.T) {
	now := time.Now()
	hits := []types.QueueItem{
		item(1, 42, 0, 1000, now),
		item(2, 42, 105, 1000, now),
	}
	plans, unmatched := Reconcile(hits)
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}
	if len(unmatched) != 1 || unmatched[0].ID != 1 {
		t.Errorf("unmatched = %+v, want [item 1]", unmatched)
	}
}

func TestReconcile_EveryInputAppearsExactlyOnce(t *testing.T) {
	now := time.Now()
	hits := []types.QueueItem{
		item(1, 42, 105, 600, now),
		item(2, 42, 105, 900, now),
		item(3, 42, 0, 700, now), // unmatched
		item(4, 44, 108, 500, now),
	}
	plans, unmatched := Reconcile(hits)

	var accounted []int
	for _, p := range plans {
		accounted = append(accounted, p.Winner.ID)
		for _, d := range p.Discards {
			accounted = append(accounted, d.ID)
		}
	}
	for _, u := range unmatched {
		accounted = append(accounted, u.ID)
	}
	if len(accounted) != len(hits) {
		t.Fatalf("accounted = %v, want all %d hits", accounted, len(hits))
	}
	seen := make(map[int]bool)
	for _, id := range accounted {
		if seen[id] {
			t.Errorf("item %d appears twice", id)
		}
		seen[id] = true
	}
}

func TestIsUpgrade(t *testing.T) {
	tests := []struct {
		name           string
		releaseScore   int
		existingScore  int
		releaseWeight  int
		existingWeight int
		weightsOK      bool
		want           bool
	}{
		{"higher score wins regardless of weights", 1000, 0, 10, 20, true, true},
		{"lower score loses", 0, 1000, 20, 10, true, false},
		{"higher score wins even with unknown weights", 807, 0, 0, 0, false, true},
		{"equal scores: strictly better quality wins", 500, 500, 30, 20, true, true},
		{"equal scores: equal quality loses", 500, 500, 20, 20, true, false},
		{"equal scores: worse quality loses", 500, 500, 10, 20, true, false},
		{"equal scores: unknown weights never upgrade", 500, 500, 0, 0, false, false},
		{"equal scores and both zero never upgrade", 0, 0, 0, 0, true, false},
		{"no file (existing score -1 sentinel) is always an upgrade", 0, -1, 0, 0, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsUpgrade(tc.releaseScore, tc.existingScore, tc.releaseWeight, tc.existingWeight, tc.weightsOK)
			if got != tc.want {
				t.Errorf("IsUpgrade(%d, %d, %d, %d, %t) = %t, want %t",
					tc.releaseScore, tc.existingScore, tc.releaseWeight, tc.existingWeight, tc.weightsOK, got, tc.want)
			}
		})
	}
}
