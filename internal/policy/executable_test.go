package policy

import (
	"testing"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
)

func TestExecutableWarningAuthority(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Observation, *config.Config)
		want bool
	}{
		{"empty preview", func(o *Observation, c *config.Config) {}, true},
		{"unmatched preview", func(o *Observation, c *config.Config) {
			o.Preview = []sonarr.Preview{{ID: 1, Path: "/downloads/file.exe", DownloadID: o.DownloadID}}
		}, true},
		{"no warning status", func(o *Observation, c *config.Config) { o.Rows[0].TrackedStatus = "ok" }, false},
		{"no status", func(o *Observation, c *config.Config) { o.Rows[0].TrackedStatus = "" }, false},
		{"generic warning", func(o *Observation, c *config.Config) { o.Rows[0].StatusMessages[0].Messages[0] = "Import failed" }, false},
		{"case differs", func(o *Observation, c *config.Config) {
			o.Rows[0].StatusMessages[0].Messages[0] = "Caution: found executable file"
		}, false},
		{"extra text", func(o *Observation, c *config.Config) { o.Rows[0].StatusMessages[0].Messages[0] += ": file.exe" }, false},
		{"title only", func(o *Observation, c *config.Config) {
			o.Rows[0].StatusMessages[0].Title = "Caution: Found executable file"
			o.Rows[0].StatusMessages[0].Messages = nil
		}, false},
		{"filename only", func(o *Observation, c *config.Config) {
			o.Rows[0].StatusMessages = nil
			o.Rows[0].OutputPath = "/downloads/file.exe"
		}, false},
		{"disabled", func(o *Observation, c *config.Config) { c.Cleanup.Enabled = false }, false},
		{"importing", func(o *Observation, c *config.Config) { o.Rows[0].TrackedState = "importing" }, false},
		{"unknown restricted series", func(o *Observation, c *config.Config) {
			c.Scope.IncludeSeriesIDs = []int{10}
			o.Rows[0].SeriesID = nil
		}, false},
		{"competing download", func(o *Observation, c *config.Config) {
			o.Queue = append(o.Queue, sonarr.QueueRow{ID: 999, DownloadID: "other", Status: "downloading", EpisodeID: o.Rows[0].EpisodeID})
		}, false},
		{"unidentified competing download", func(o *Observation, c *config.Config) {
			o.Queue = append(o.Queue, sonarr.QueueRow{ID: 999, DownloadID: "other", Status: "downloading"})
		}, false},
		{"excluded preview series", func(o *Observation, c *config.Config) {
			c.Scope.ExcludeSeriesIDs = []int{99}
			o.Preview = []sonarr.Preview{{ID: 1, Path: "/downloads/file.exe", DownloadID: o.DownloadID, Series: &sonarr.Series{ID: 99}}}
		}, false},
		{"invalid preview identity", func(o *Observation, c *config.Config) {
			o.Preview = []sonarr.Preview{{Path: "/downloads/file.exe", DownloadID: o.DownloadID}}
		}, false},
		{"wrong preview download", func(o *Observation, c *config.Config) {
			o.Preview = []sonarr.Preview{{ID: 1, Path: "/downloads/file.exe", DownloadID: "other"}}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := captured(t)["download-002"]
			o.Preview = nil
			o.Rows[0].TrackedStatus = "warning"
			o.Rows[0].StatusMessages = []sonarr.StatusMessage{{Messages: []string{"Caution: Found executable file"}}}
			c := config.Defaults()
			c.Cleanup.Enabled = true
			tc.edit(&o, &c)
			d, _ := RejectedCleanup(c, o)
			if (d.Kind == "remove_blocked" && d.ExecutableWarning) != tc.want || d.Blocklist {
				t.Fatalf("unexpected executable cleanup: %+v", d)
			}
		})
	}
}

func TestExecutableWarningFingerprint(t *testing.T) {
	o := captured(t)["download-002"]
	o.Preview = nil
	o.Rows[0].TrackedStatus = "warning"
	o.Rows[0].StatusMessages = []sonarr.StatusMessage{
		{Title: "release", Messages: []string{"Caution: Found executable file", "other"}},
		{Title: "second", Messages: []string{"another"}},
	}
	fingerprint, identity := Fingerprint(o), QueueIdentity(o.Rows)
	o.Rows[0].StatusMessages[0].Messages[0], o.Rows[0].StatusMessages[0].Messages[1] = o.Rows[0].StatusMessages[0].Messages[1], o.Rows[0].StatusMessages[0].Messages[0]
	o.Rows[0].StatusMessages[0], o.Rows[0].StatusMessages[1] = o.Rows[0].StatusMessages[1], o.Rows[0].StatusMessages[0]
	if Fingerprint(o) != fingerprint {
		t.Fatal("warning response ordering changed fingerprint")
	}
	o.Rows[0].StatusMessages[1].Messages = []string{"other"}
	if Fingerprint(o) == fingerprint || QueueIdentity(o.Rows) != identity {
		t.Fatal("warning change must invalidate the plan without changing queue identity")
	}
	o.Rows[0].StatusMessages[1].Messages = []string{"Caution: Found executable file"}
	fingerprint = Fingerprint(o)
	o.Rows[0].TrackedStatus = "ok"
	if Fingerprint(o) == fingerprint {
		t.Fatal("warning status change did not invalidate fingerprint")
	}
}
