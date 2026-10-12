package policy

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
)

func ptr[T any](v T) *T { return &v }
func captured(t *testing.T) map[string]Observation {
	t.Helper()
	b, e := os.ReadFile("../../specs/sonarr-remediator/evidence/sonarr-v4.0.20/snapshot.json")
	if e != nil {
		t.Fatal(e)
	}
	var s struct {
		Queue           struct{ Records []sonarr.QueueRow }
		ManualImport    map[string][]sonarr.Preview
		Episodes        []sonarr.Episode
		EpisodeFiles    []sonarr.EpisodeFile
		QualityProfiles []sonarr.Profile
		MediaManagement sonarr.MediaManagement
	}
	if e = json.Unmarshal(b, &s); e != nil {
		t.Fatal(e)
	}
	results := map[string]Observation{}
	for id, p := range s.ManualImport {
		o := Observation{DownloadID: id, Queue: s.Queue.Records, Preview: p, Episodes: map[int]sonarr.Episode{}, Files: map[int]sonarr.EpisodeFile{}, Series: map[int]sonarr.Series{}, Profiles: map[int]sonarr.Profile{}, Propers: s.MediaManagement.Propers, MembershipComplete: true}
		ids := map[int]bool{}
		for _, q := range s.Queue.Records {
			if q.DownloadID == id {
				o.Rows = append(o.Rows, q)
				if q.EpisodeID != nil {
					ids[*q.EpisodeID] = true
				}
			}
		}
		for _, f := range p {
			if f.Series != nil {
				o.Series[f.Series.ID] = *f.Series
			}
			for _, e := range f.Episodes {
				ids[e.ID] = true
			}
		}
		for eid := range ids {
			o.Intended = append(o.Intended, eid)
		}
		sort.Ints(o.Intended)
		for _, e := range s.Episodes {
			o.Episodes[e.ID] = e
		}
		for _, f := range s.EpisodeFiles {
			o.Files[f.ID] = f
		}
		for _, p := range s.QualityProfiles {
			o.Profiles[p.ID] = p
		}
		results[id] = o
	}
	return results
}
func TestCapturedEvidenceCaseMatrix(t *testing.T) {
	c := config.Defaults()
	c.Cleanup.Enabled = true
	expected := map[string]string{"download-001": "import", "download-005": "import", "download-011": "import", "download-003": "remove_redundant", "download-009": "remove_redundant", "download-012": "remove_redundant", "download-014": "remove_redundant", "download-008": "remove_redundant", "download-004": "defer", "download-007": "defer", "download-010": "defer", "download-002": "defer", "download-006": "defer", "download-013": "defer"}
	for id, o := range captured(t) {
		t.Run(id, func(t *testing.T) {
			d := Evaluate(c, o)
			if d.Kind != expected[id] {
				t.Fatalf("got %s: %s; want %s", d.Kind, d.Reason, expected[id])
			}
			if d.Blocklist {
				t.Fatal("redundancy blocklisted")
			}
		})
	}
}
func TestSafetyGatesAndUnusableCatalog(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Observation, *config.Config)
		want string
	}{
		{"sample", func(o *Observation, c *config.Config) {
			o.Preview[0].Rejections = []sonarr.Rejection{{Reason: "Sample", Type: "permanent"}}
		}, "remove_unusable"},
		{"no_audio", func(o *Observation, c *config.Config) {
			o.Preview[0].Rejections = []sonarr.Rejection{{Reason: "No audio tracks detected", Type: "permanent"}}
		}, "remove_unusable"},
		{"indeterminate_sample", func(o *Observation, c *config.Config) {
			o.Preview[0].Rejections = []sonarr.Rejection{{Reason: "Unable to determine if file is a sample", Type: "permanent"}}
		}, "defer"},
		{"excluded_unknown_series", func(o *Observation, c *config.Config) { c.Scope.ExcludeSeriesIDs = []int{o.Preview[0].Series.ID} }, "defer"},
		{"missing_score", func(o *Observation, c *config.Config) { o.Preview[0].Score = nil }, "defer"},
		{"missing_verdict", func(o *Observation, c *config.Config) { o.Preview[0].Rejections = nil }, "defer"},
		{"unknown_preference", func(o *Observation, c *config.Config) { o.Propers = "future" }, "defer"},
		{"pending", func(o *Observation, c *config.Config) { o.Rows[0].TrackedState = "importPending" }, "import"},
		{"importing", func(o *Observation, c *config.Config) { o.Rows[0].TrackedState = "importing" }, "defer"},
		{"imported", func(o *Observation, c *config.Config) { o.Rows[0].TrackedState = "imported" }, "defer"},
		{"downloading_pending", func(o *Observation, c *config.Config) {
			o.Rows[0].Status, o.Rows[0].TrackedState = "downloading", "importPending"
		}, "defer"},
		{"competing", func(o *Observation, c *config.Config) {
			o.Queue = append(o.Queue, sonarr.QueueRow{ID: 999, DownloadID: "other", TrackedState: "downloading", EpisodeID: ptr(o.Intended[0])})
		}, "defer"},
		{"empty_preview", func(o *Observation, c *config.Config) { o.Preview = nil }, "defer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := captured(t)["download-001"]
			c := config.Defaults()
			c.Cleanup.Enabled = true
			c.Cleanup.Blocklist = true
			tc.edit(&o, &c)
			d := Evaluate(c, o)
			if d.Kind != tc.want {
				t.Fatalf("got %s: %s", d.Kind, d.Reason)
			}
		})
	}
}
func TestStrictProfileComparison(t *testing.T) {
	o := captured(t)["download-005"]
	f := o.Preview[0]
	old := o.Files[*o.Episodes[o.Intended[0]].EpisodeFileID]
	p := o.Profiles[f.Series.QualityProfileID]
	if cmp, up, why := Compare(f.Quality, f.Formats, f.Score, old, p, o.Propers); cmp != 1 || !up {
		t.Fatal(why)
	}
	p.UpgradeAllowed = ptr(false)
	if cmp, up, _ := Compare(f.Quality, f.Formats, f.Score, old, p, o.Propers); cmp != 1 || up {
		t.Fatal("disabled upgrade became redundant or importable")
	}
	p.UpgradeAllowed = ptr(true)
	p.CutoffScore = ptr(400)
	if cmp, up, _ := Compare(f.Quality, f.Formats, f.Score, old, p, o.Propers); cmp != 1 || up {
		t.Fatal("format cutoff ignored")
	}
	p.CutoffScore = ptr(10000)
	p.MinUpgradeScore = ptr(1000)
	if cmp, up, _ := Compare(f.Quality, f.Formats, f.Score, old, p, o.Propers); cmp != 1 || up {
		t.Fatal("minimum increment ignored")
	}
	// Real revision sorts ahead of version, as in Sonarr Revision.CompareTo.
	a, b := f.Quality, old.Quality
	a.Revision = &sonarr.Revision{Real: ptr(1), Version: ptr(1), IsRepack: ptr(false)}
	b.Revision = &sonarr.Revision{Real: ptr(0), Version: ptr(3), IsRepack: ptr(false)}
	if RevisionCompare(a, b) != 1 {
		t.Fatal("wrong revision order")
	}
}
func TestFingerprintStableAcrossOrdering(t *testing.T) {
	o := captured(t)["download-004"]
	a := Fingerprint(o)
	o.Preview[0], o.Preview[2] = o.Preview[2], o.Preview[0]
	if Fingerprint(o) != a {
		t.Fatal("response order resets attempts")
	}
	o.Preview[0].Path += "changed"
	if Fingerprint(o) == a {
		t.Fatal("material change not detected")
	}
}

func TestReviewProtectedCompetition(t *testing.T) {
	for _, row := range []sonarr.QueueRow{
		{Status: "downloading", TrackedState: "importBlocked"},
		{Status: "completed", TrackedState: "importPending"},
	} {
		t.Run(row.Status+"/"+row.TrackedState, func(t *testing.T) {
			o := captured(t)["download-001"]
			c := config.Defaults()
			row.ID, row.DownloadID, row.EpisodeID = 999, "competing", ptr(o.Intended[0])
			o.Queue = append(o.Queue, row)
			d := Evaluate(c, o)
			if d.Kind != "defer" || d.Reason != "competing active or unidentified download" {
				t.Fatalf("protected competitor authorized %s: %s", d.Kind, d.Reason)
			}
		})
	}
}
