package service

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/state"
)

type summaryRecord struct {
	Message         string         `json:"msg"`
	Trigger         string         `json:"trigger"`
	Connectivity    string         `json:"connectivity"`
	CompleteScan    time.Time      `json:"complete_scan"`
	Active          int            `json:"active_issues"`
	Issues          map[string]int `json:"issue_counts"`
	Unresolved      int            `json:"unresolved_actions"`
	Mutations       map[string]int `json:"mutation_attempts"`
	Outcomes        map[string]int `json:"action_outcomes"`
	Recommendations int            `json:"dry_run_recommendations"`
}

func captureLogs(service *Service) *bytes.Buffer {
	output := &bytes.Buffer{}
	service.Log = slog.New(slog.NewJSONHandler(output, nil))
	return output
}

func summaries(t *testing.T, output *bytes.Buffer) []summaryRecord {
	t.Helper()
	var records []summaryRecord
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	for decoder.More() {
		var record summaryRecord
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record.Message == "service summary" {
			records = append(records, record)
		}
	}
	return records
}

func TestDecisionLoggingLevels(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
			t.Run(format+"/"+level.String(), func(t *testing.T) {
				service, _, now, _ := setup(t, true)
				service.Config.Recovery.Wait = time.Hour
				output := &bytes.Buffer{}
				options := &slog.HandlerOptions{Level: level}
				var handler slog.Handler = slog.NewJSONHandler(output, options)
				if format == "text" {
					handler = slog.NewTextHandler(output, options)
				}
				service.Log = slog.New(handler)
				due(t, service, now)
				decisions := strings.Count(output.String(), "issue decision")
				if level == slog.LevelDebug {
					if decisions != 3 || !strings.Contains(output.String(), "download_id") || !strings.Contains(output.String(), "observed eligibility") {
						t.Fatalf("missing detailed DEBUG decisions: %s", output)
					}
				} else if decisions != 0 {
					t.Fatalf("decision spam at %s: %s", level, output)
				}
				wantSummaries := 0
				if level <= slog.LevelInfo {
					wantSummaries = 1
				}
				if strings.Count(output.String(), "service summary") != wantSummaries {
					t.Fatalf("wrong summary filtering: %s", output)
				}
			})
		}
	}
}

func TestSummaryCadenceAndDryRun(t *testing.T) {
	service, server, now, _ := setup(t, true)
	output := captureLogs(service)
	cycle(t, service, now, 0)
	for minute := 1; minute < 30; minute++ {
		cycle(t, service, now, time.Minute)
	}
	cycle(t, service, now, 59*time.Second)
	if records := summaries(t, output); len(records) != 1 || records[0].Trigger != "startup" || records[0].Issues["waiting"] != 1 {
		t.Fatalf("routine cycles produced summaries: %+v", records)
	}
	cycle(t, service, now, time.Second)
	records := summaries(t, output)
	if len(records) != 2 || records[1].Trigger != "interval" || records[1].Recommendations != 1 || records[1].Active != 1 || len(records[1].Mutations) != 0 || len(records[1].Outcomes) != 0 {
		t.Fatalf("incorrect 30-minute summary: %+v", records)
	}
	cycle(t, service, now, 30*time.Minute)
	records = summaries(t, output)
	if len(records) != 3 || records[2].Recommendations != 0 || len(server.mutations) != 0 {
		t.Fatalf("recommendation repeated or mutated: %+v", records)
	}
	server.empty = true
	cycle(t, service, now, time.Minute)
	if len(summaries(t, output)) != 3 {
		t.Fatal("external resolution triggered a summary")
	}
	cycle(t, service, now, 29*time.Minute)
	records = summaries(t, output)
	if len(records) != 4 || records[3].Active != 0 || len(records[3].Issues) != 0 {
		t.Fatalf("resolved history inflated active counts: %+v", records)
	}
}

func TestSummaryEmptyDeferredAndRestart(t *testing.T) {
	service, server, now, _ := setup(t, true)
	server.empty = true
	service.Store.Data.Attempts = append(service.Store.Data.Attempts, state.Attempt{ID: 1, Kind: "import", Outcome: "verified", Started: *now, Finished: *now})
	output := captureLogs(service)
	cycle(t, service, now, 0)
	scan := service.Store.Data.Status.CompleteScan
	server.readFail = true
	cycle(t, service, now, 29*time.Minute+59*time.Second)
	if !strings.Contains(output.String(), "observation deferred") || len(summaries(t, output)) != 1 {
		t.Fatal("warning delayed or summary too early")
	}
	cycle(t, service, now, time.Second)
	records := summaries(t, output)
	if len(records) != 2 || records[0].Active != 0 || len(records[0].Outcomes) != 0 || records[1].Connectivity != "degraded" || !records[1].CompleteScan.Equal(scan) {
		t.Fatalf("incorrect empty/deferred/history summary: %+v", records)
	}
	restarted := New(service.Config, service.Client, service.Store, service.Log)
	restarted.Now = service.Now
	cycle(t, restarted, now, 0)
	records = summaries(t, output)
	if len(records) != 3 || records[2].Trigger != "startup" || len(records[2].Mutations) != 0 || len(records[2].Outcomes) != 0 || records[2].Connectivity != "degraded" {
		t.Fatalf("restart replayed history: %+v", records)
	}
}

func TestSummaryMutationAndOutcomes(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "acknowledged", true: "ambiguous"}[ambiguous], func(t *testing.T) {
			service, server, now, _ := setup(t, false)
			server.ambiguous = ambiguous
			output := captureLogs(service)
			due(t, service, now)
			records := summaries(t, output)
			outcome := "pending"
			if ambiguous {
				outcome = "uncertain"
			}
			if len(records) != 2 || records[1].Trigger != "action" || records[1].Mutations["import"] != len(server.mutations) || records[1].Outcomes[outcome] != 1 || records[1].Unresolved != 1 || strings.Count(output.String(), "mutation attempted") != 1 {
				t.Fatalf("mutation missing from logs: %+v\n%s", records, output)
			}
			if !ambiguous {
				command := server.commands[1]
				command.Status = "failed"
				server.commands[1] = command
			}
			for minute := 1; minute < 30; minute++ {
				cycle(t, service, now, time.Minute)
				// Prevent a known failed import from being retried in this test.
				service.Config.Recovery.Enabled = false
			}
			cycle(t, service, now, 59*time.Second)
			if len(summaries(t, output)) != 2 {
				t.Fatal("action summary did not reset the interval")
			}
			cycle(t, service, now, time.Second)
			records = summaries(t, output)
			if len(records) != 3 || len(records[2].Mutations) != 0 || records[2].Outcomes["pending"] != 0 || records[2].Outcomes["uncertain"] != 0 {
				t.Fatalf("unchanged outcomes repeated: %+v", records)
			}
			if !ambiguous && records[2].Outcomes["failed"] != 1 {
				t.Fatalf("known failure not summarized once: %+v", records)
			}
		})
	}
}

func TestSummaryRemovalAndSearch(t *testing.T) {
	service, server, now, _ := setup(t, false)
	service.Config.Cleanup.Enabled = true
	service.Config.Cleanup.Blocklist = true
	service.Config.Cleanup.ReplacementSearch = true
	server.preview.Rejections = []sonarr.Rejection{{Reason: "Sample", Type: "permanent"}}
	output := captureLogs(service)
	// The first cycle is both startup and action-triggered.
	service.Config.Cleanup.Wait = 0
	cycle(t, service, now, 0)
	records := summaries(t, output)
	if len(records) != 1 || records[0].Trigger != "action" || records[0].Mutations["remove_unusable"] != 1 {
		t.Fatalf("removal/startup not coalesced: %+v", records)
	}
	server.history = []sonarr.History{{ID: 1, DownloadID: server.row.DownloadID, SeriesID: 10, EpisodeID: 100, Date: *now, SourceTitle: "release", EventType: "downloadFailed"}}
	server.blocks = []sonarr.Blocklist{{ID: 1, SeriesID: 10, EpisodeIDs: []int{100}, Date: *now, SourceTitle: "release"}}
	cycle(t, service, now, time.Minute)
	records = summaries(t, output)
	if len(records) != 2 || records[1].Mutations["search"] != 1 || records[1].Outcomes["verified"] != 1 || records[1].Outcomes["pending"] != 1 || len(server.mutations) != 2 || strings.Count(output.String(), "mutation attempted") != 2 || !strings.Contains(output.String(), "action verified") {
		t.Fatalf("search or verification invisible: %+v\n%s", records, output)
	}
	cycle(t, service, now, time.Minute)
	cycle(t, service, now, 29*time.Minute)
	records = summaries(t, output)
	if len(records) != 3 || records[2].Outcomes["submitted"] != 1 || records[2].Active != 0 {
		t.Fatalf("search completion not summarized: %+v", records)
	}
	t.Log(output.String())
}

func TestSummaryManualPreviewExecutionAndReplay(t *testing.T) {
	service, server, now, _ := setup(t, true)
	output := captureLogs(service)
	cycle(t, service, now, 0)
	cycle(t, service, now, 29*time.Minute)
	preview := service.Manual(context.Background(), request(t, false, true, server.row.DownloadID))
	if preview.ExitCode != 0 || len(summaries(t, output)) != 1 {
		t.Fatalf("preview emitted a summary: %+v", preview)
	}
	cycle(t, service, now, time.Minute)
	if len(summaries(t, output)) != 2 {
		t.Fatal("preview reset the live timer")
	}
	cycle(t, service, now, time.Minute)
	req := request(t, true, true, server.row.DownloadID)
	live := service.Manual(context.Background(), req)
	records := summaries(t, output)
	if live.ExitCode != 0 || len(records) != 3 || records[2].Mutations["import"] != 1 || records[2].Trigger != "action" {
		t.Fatalf("manual mutation summary missing: %+v %+v", live, records)
	}
	service.Manual(context.Background(), req)
	cycle(t, service, now, 29*time.Minute+59*time.Second)
	if len(summaries(t, output)) != 3 || len(server.mutations) != 1 {
		t.Fatal("manual timer/replay produced extra activity")
	}
	cycle(t, service, now, time.Second)
	records = summaries(t, output)
	if len(records) != 4 || len(records[3].Mutations) != 0 || records[3].Outcomes["uncertain"] != 1 {
		t.Fatalf("manual outcome/timer not shared: %+v", records)
	}
}

func TestSummaryMultipleMutationsInOneCycle(t *testing.T) {
	service, first, now, _ := setup(t, false)
	second := fixture()
	second.row.ID, second.row.DownloadID, second.row.EpisodeID = 2, "second-download", pointer(101)
	second.episode.ID = 101
	second.preview.DownloadID = second.row.DownloadID
	second.preview.Episodes = []sonarr.Episode{second.episode}
	second.preview.Path = "/downloads/second/file.mkv"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/api/v3/queue":
			json.NewEncoder(writer).Encode(map[string]any{"page": 1, "pageSize": 100, "totalRecords": 2, "records": []sonarr.QueueRow{first.row, second.row}})
		case req.URL.Query().Get("downloadId") == second.row.DownloadID:
			second.ServeHTTP(writer, req)
		case req.URL.Path == "/api/v3/episode/101":
			json.NewEncoder(writer).Encode(second.episode)
		default:
			first.ServeHTTP(writer, req)
		}
	}))
	t.Cleanup(server.Close)
	var err error
	service.Client, err = sonarr.New(server.URL, "test-secret", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	service.Config.Limits.MutationsPerScan = 2
	service.Config.Recovery.Wait = 0
	output := captureLogs(service)
	cycle(t, service, now, 0)
	records := summaries(t, output)
	if len(records) != 1 || records[0].Mutations["import"] != 2 || records[0].Outcomes["pending"] != 2 || len(first.mutations) != 2 || strings.Count(output.String(), "mutation attempted") != 2 {
		t.Fatalf("multiple mutations not coalesced: %+v\n%s", records, output)
	}
}

func TestSummarySaveFailure(t *testing.T) {
	for _, afterTransmission := range []bool{false, true} {
		t.Run(map[bool]string{false: "reservation", true: "after-transmission"}[afterTransmission], func(t *testing.T) {
			service, fakeSonarr, _, _ := setup(t, false)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
				fakeSonarr.ServeHTTP(writer, req)
				if req.Method != "GET" {
					service.Store.Close()
				}
			}))
			t.Cleanup(server.Close)
			var err error
			service.Client, err = sonarr.New(server.URL, "test-secret", time.Second, false)
			if err != nil {
				t.Fatal(err)
			}
			service.Config.Recovery.Wait = 0
			if !afterTransmission {
				service.Store.Close()
			}
			output := captureLogs(service)
			if err = service.Cycle(context.Background()); err == nil {
				t.Fatal("save failure was hidden")
			}
			records := summaries(t, output)
			if len(records) != 1 || records[0].Mutations["import"] != len(fakeSonarr.mutations) || len(records[0].Outcomes) != len(fakeSonarr.mutations) {
				t.Fatalf("save failure lost or invented action activity: %+v", records)
			}
			if afterTransmission && (records[0].Trigger != "action" || records[0].Outcomes["pending"] != 1) {
				t.Fatalf("transmitted action invisible after save failure: %+v", records)
			}
		})
	}
}
