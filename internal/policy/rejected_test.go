package policy

import (
	"testing"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
)

func TestEpisodeMismatchVerdict(t *testing.T) {
	for _, tc := range []struct {
		name, reason, verdictType string
		immediate                 bool
	}{
		{"single", "Episode 1x01 was unexpected considering the Example.1x12 folder name", "permanent", true},
		{"multiple", "Episodes 1x01, 2x102 were unexpected considering the Example.Pack folder name", "permanent", true},
		{"temporary", "Episode 1x01 was unexpected considering the Example.1x12 folder name", "temporary", false},
		{"other mismatch", "Episode did not match the expected episode", "permanent", false},
		{"prefixed", "Probe error: Episode 1x01 was unexpected considering the Example folder name", "permanent", false},
		{"truncated", "Episode 1x01 was unexpected considering the Example", "permanent", false},
		{"empty folder", "Episode 1x01 was unexpected considering the  folder name", "permanent", false},
		{"invalid episode", "Episode unknown was unexpected considering the Example folder name", "permanent", false},
		{"wrong plural", "Episodes 1x01 were unexpected considering the Example folder name", "permanent", false},
		{"trailing text", "Episode 1x01 was unexpected considering the Example folder name; retry", "permanent", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := captured(t)["download-002"]
			o.Preview[0].Rejections = []sonarr.Rejection{{Reason: tc.reason, Type: tc.verdictType}}
			c := config.Defaults()
			c.Cleanup.Enabled = true
			d, rejected := RejectedCleanup(c, o)
			if !rejected || d.Kind != "remove_blocked" || d.EpisodeMismatch != tc.immediate || d.Blocklist {
				t.Fatalf("unexpected mismatch cleanup: %+v, rejected=%t", d, rejected)
			}
		})
	}
}

func TestSonarrRejectionCleanupAuthority(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Observation, *config.Config)
		want string
	}{
		{"sample detection", func(o *Observation, c *config.Config) {}, "remove_blocked"},
		{"unknown verdict", func(o *Observation, c *config.Config) { o.Preview[0].Rejections[0].Reason = "Future Sonarr blocker" }, "remove_blocked"},
		{"temporary verdict", func(o *Observation, c *config.Config) { o.Preview[0].Rejections[0].Type = "temporary" }, "remove_blocked"},
		{"missing membership", func(o *Observation, c *config.Config) { o.MembershipComplete = false; o.Intended = nil }, "remove_blocked"},
		{"unknown mapping", func(o *Observation, c *config.Config) { o.Preview[0].Series = nil; o.Preview[0].Episodes = nil }, "remove_blocked"},
		{"disabled", func(o *Observation, c *config.Config) { c.Cleanup.Enabled = false }, "defer"},
		{"importing", func(o *Observation, c *config.Config) { o.Rows[0].TrackedState = "importing" }, "defer"},
		{"competing pending download", func(o *Observation, c *config.Config) {
			o.Queue = append(o.Queue, sonarr.QueueRow{ID: 999, DownloadID: "other", Status: "completed", TrackedState: "importPending", EpisodeID: o.Rows[0].EpisodeID})
		}, "defer"},
		{"no rejection", func(o *Observation, c *config.Config) { o.Preview[0].Rejections = o.Preview[0].Rejections[:0] }, "defer"},
		{"missing rejection field", func(o *Observation, c *config.Config) { o.Preview[0].Rejections = nil }, "defer"},
		{"empty preview", func(o *Observation, c *config.Config) { o.Preview = nil }, "defer"},
		{"wrong download", func(o *Observation, c *config.Config) { o.Preview[0].DownloadID = "other" }, "defer"},
		{"empty verdict", func(o *Observation, c *config.Config) { o.Preview[0].Rejections[0].Reason = " " }, "defer"},
		{"invalid verdict type", func(o *Observation, c *config.Config) { o.Preview[0].Rejections[0].Type = "future" }, "defer"},
		{"excluded series", func(o *Observation, c *config.Config) { c.Scope.ExcludeSeriesIDs = []int{*o.Rows[0].SeriesID} }, "defer"},
		{"excluded client", func(o *Observation, c *config.Config) {
			c.Scope.ExcludeDownloadClients = []string{o.Rows[0].DownloadClient}
		}, "defer"},
		{"unknown restricted series", func(o *Observation, c *config.Config) {
			c.Scope.IncludeSeriesIDs = []int{99}
			o.Rows[0].SeriesID, o.Rows[0].EpisodeID = nil, nil
			o.Preview[0].Series, o.Preview[0].Episodes = nil, nil
		}, "defer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := captured(t)["download-002"]
			c := config.Defaults()
			c.Cleanup.Enabled, c.Cleanup.Blocklist, c.Cleanup.ReplacementSearch = true, true, true
			tc.edit(&o, &c)
			d, _ := RejectedCleanup(c, o)
			if d.Kind != tc.want || d.Blocklist {
				t.Fatalf("got %s blocklist=%t; want %s", d.Kind, d.Blocklist, tc.want)
			}
		})
	}
}
