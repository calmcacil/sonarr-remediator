package state

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
)

func TestCrashIntentChild(t *testing.T) {
	stage := os.Getenv("SRA_TEST_CRASH_STAGE")
	if stage == "" {
		return
	}
	store, e := Open(os.Getenv("SRA_TEST_STATE"), os.Getenv("SRA_TEST_URL"), false)
	if e != nil {
		os.Exit(2)
	}
	store.Data.Attempts = append(store.Data.Attempts, Attempt{ID: 1, DownloadID: "crash-download", Kind: "import", Started: time.Now(), Outcome: "intent"})
	if store.Save() != nil {
		os.Exit(3)
	}
	if stage == "after-transmission" {
		client, _ := sonarr.New(os.Getenv("SRA_TEST_URL"), "fixture-key", time.Second, false)
		if _, e = client.Submit(context.Background(), sonarr.ImportCommand{Name: "ManualImport", ImportMode: "auto"}); e != nil {
			os.Exit(4)
		}
	}
	os.Exit(99)
}

func TestProcessCrashBeforeAndAfterTransmission(t *testing.T) {
	for _, stage := range []string{"before-transmission", "after-transmission"} {
		t.Run(stage, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				json.NewEncoder(w).Encode(sonarr.Command{ID: 42, Name: "ManualImport", Status: "queued"})
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "private", "state.db")
			child := exec.Command(os.Args[0], "-test.run=^TestCrashIntentChild$")
			child.Env = append(os.Environ(), "SRA_TEST_CRASH_STAGE="+stage, "SRA_TEST_STATE="+path, "SRA_TEST_URL="+server.URL)
			e := child.Run()
			exit, ok := e.(*exec.ExitError)
			if !ok || exit.ExitCode() != 99 {
				t.Fatalf("crash child failed: %v", e)
			}
			store, e := Open(path, server.URL, false)
			if e != nil {
				t.Fatal(e)
			}
			defer store.Close()
			if len(store.Data.Attempts) != 1 || store.Data.Attempts[0].Outcome != "uncertain" || !store.Data.Attempts[0].Pending() || store.Data.Budget(time.Now()) != 1 {
				t.Fatal("durable crash protection failed")
			}
			want := int32(0)
			if stage == "after-transmission" {
				want = 1
			}
			if requests.Load() != want {
				t.Fatalf("transmissions: %d want %d", requests.Load(), want)
			}
		})
	}
}
