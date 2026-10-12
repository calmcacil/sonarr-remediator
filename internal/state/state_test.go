package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/policy"
)

func TestOwnershipReadOnlyAndCrashProtection(t *testing.T) {
	p := filepath.Join(t.TempDir(), "private", "state.db")
	s, e := Open(p, "http://sonarr", false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Open(p, "http://sonarr", false); e == nil {
		t.Fatal("duplicate observer accepted")
	}
	now := time.Now()
	s.Data.Attempts = []Attempt{{ID: 1, DownloadID: "download", Started: now, Outcome: "intent", Decision: policy.Decision{EpisodeIDs: []int{1}}}}
	if e = s.Save(); e != nil {
		t.Fatal(e)
	}
	q, e := Open(p, "", true)
	if e != nil {
		t.Fatal(e)
	}
	if len(q.Data.Attempts) != 1 || q.Data.Budget(now) != 1 {
		t.Fatal("query cannot inspect live state")
	}
	q.Close()
	s.Close()
	s, e = Open(p, "http://sonarr", false)
	if e != nil {
		t.Fatal(e)
	}
	if s.Data.Attempts[0].Outcome != "uncertain" {
		t.Fatal("crash intent became retryable")
	}
	s.Close()
	if _, e = Open(p, "http://different", false); e == nil {
		t.Fatal("foreign instance accepted")
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, e = Open(missing, "", true); e == nil {
		t.Fatal("missing query succeeded")
	}
	if _, e = os.Stat(missing); !os.IsNotExist(e) {
		t.Fatal("query created state")
	}
}
func TestPrunePreservesSafetyAndDryRunBudget(t *testing.T) {
	now := time.Now()
	d := Data{Issues: map[string]*Issue{"active": {State: "exhausted"}}, Operations: map[string]*Operation{}}
	d.Attempts = []Attempt{{ID: 1, DownloadID: "unknown", Started: now.Add(-1000 * time.Hour), Outcome: "uncertain"}, {ID: 2, DownloadID: "active", Started: now.Add(-1000 * time.Hour), Outcome: "failed"}, {ID: 3, Started: now, Outcome: "recommended", DryRun: true}, {ID: 4, Started: now, Outcome: "failed"}}
	d.Prune(now, 720*time.Hour, 30*time.Minute)
	if len(d.Attempts) != 4 || d.Budget(now) != 1 {
		t.Fatal("safety state pruned or dry-run consumed budget")
	}
}
