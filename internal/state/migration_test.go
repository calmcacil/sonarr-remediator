package state

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/control"
)

func schemaOne(t *testing.T, rejectMigration bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "state.db")
	s, err := Open(path, "http://fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	s.Data.Attempts = []Attempt{{ID: 1, DownloadID: "download", Outcome: "uncertain", Started: time.Now()}}
	if err = s.Save(); err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec("DELETE FROM records WHERE kind='manual_triggers'; PRAGMA user_version=1")
	if err != nil {
		t.Fatal(err)
	}
	if rejectMigration {
		_, err = s.db.Exec("CREATE TRIGGER reject_receipts BEFORE INSERT ON records WHEN NEW.kind='manual_triggers' BEGIN SELECT RAISE(ABORT, 'injected migration failure'); END")
		if err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	return path
}

func schemaVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err = db.QueryRow("PRAGMA user_version").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSchemaOneReadOnlyAndAtomicMigration(t *testing.T) {
	path := schemaOne(t, false)
	read, err := Open(path, "", true)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(read.Data.Attempts)
	if len(read.Data.Triggers) != 0 {
		t.Fatal("schema1 invented manual requests")
	}
	read.Close()
	if schemaVersion(t, path) != 1 {
		t.Fatal("query migrated state")
	}
	if _, err = Open(path, "http://foreign", false); err == nil {
		t.Fatal("foreign identity migrated state")
	}
	if schemaVersion(t, path) != 1 {
		t.Fatal("rejected owner changed schema")
	}
	store, err := Open(path, "http://fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	after, _ := json.Marshal(store.Data.Attempts)
	if string(before) != string(after) || schemaVersion(t, path) != 2 {
		t.Fatal("migration lost safety history")
	}
	var records int
	if err = store.db.QueryRow("SELECT COUNT(*) FROM records").Scan(&records); err != nil || records != 5 {
		t.Fatal("incomplete schema2 migration")
	}
	var integrity string
	if err = store.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal("migration damaged SQLite")
	}
}

func TestSchemaMigrationFailureLeavesOldRecords(t *testing.T) {
	path := schemaOne(t, true)
	if _, err := Open(path, "http://fixture", false); err == nil {
		t.Fatal("injected migration failure ignored")
	}
	if schemaVersion(t, path) != 1 {
		t.Fatal("failed migration advanced schema")
	}
	read, err := Open(path, "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	if len(read.Data.Attempts) != 1 || read.Data.Attempts[0].Outcome != "uncertain" {
		t.Fatal("failed migration changed history")
	}
}

func TestInterruptedReceiptRetainsEffectAndDoesNotResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "state.db")
	store, err := Open(path, "http://fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	req, err := control.NewRequest(true, true, []string{"download"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	report := control.Failure(req, 2, "")
	report.Items = []control.Item{{DownloadID: "download", Disposition: "would_import"}}
	store.Data.Triggers[req.ID] = &Receipt{Request: req, Accepted: time.Now(), Status: "accepted", Report: report, AttemptIDs: []int64{1}}
	store.Data.Attempts = []Attempt{{ID: 1, RequestID: req.ID, Source: "manual", DownloadID: "download", Started: time.Now(), Outcome: "intent"}}
	if err = store.Save(); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = Open(path, "http://fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	receipt := store.Data.Triggers[req.ID]
	if receipt.Status != "interrupted" || store.Data.Attempts[0].Outcome != "uncertain" {
		t.Fatal("restart created executable work")
	}
	view := store.Data.ReceiptReport(receipt)
	if len(view.Items[0].Effects) != 1 || view.Items[0].Effects[0].Outcome != "uncertain" {
		t.Fatal("interrupted receipt hid effect")
	}
	store.Data.Prune(time.Now().Add(1000*time.Hour), 720*time.Hour, 30*time.Minute)
	if len(store.Data.Triggers) != 1 || len(store.Data.Attempts) != 1 {
		t.Fatal("unresolved receipt pruned")
	}
	store.Data.Attempts[0].Outcome = "verified"
	store.Data.Attempts[0].Finished = time.Now()
	store.Data.Prune(time.Now().Add(1000*time.Hour), 720*time.Hour, 30*time.Minute)
	if len(store.Data.Triggers) != 0 || len(store.Data.Attempts) != 0 {
		t.Fatal("finished receipt did not follow retention")
	}
}

func TestReceiptRetainedWithOldFailedAttemptNeededForRetryProtection(t *testing.T) {
	now := time.Now()
	req, err := control.NewRequest(true, true, []string{"download"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	d := Data{Issues: map[string]*Issue{"download": {DownloadID: "download", State: "exhausted"}}, Operations: map[string]*Operation{}, Triggers: map[string]*Receipt{req.ID: {Request: req, Accepted: now, Finished: now, Status: "finished", Report: control.Failure(req, 2, ""), AttemptIDs: []int64{1}}}, Attempts: []Attempt{{ID: 1, DownloadID: "download", RequestID: req.ID, Source: "manual", Started: now, Finished: now, Outcome: "failed"}}}
	d.Prune(now.Add(1000*time.Hour), 720*time.Hour, 30*time.Minute)
	if len(d.Triggers) != 1 || d.validateReceipts() != nil {
		t.Fatal("retry protection outlived its required manual receipt")
	}
	d.Issues["download"].State = "resolved"
	d.Issues["download"].ResolvedAt = now
	d.Prune(now.Add(1000*time.Hour), 720*time.Hour, 30*time.Minute)
	if len(d.Triggers) != 0 || len(d.Attempts) != 0 {
		t.Fatal("resolved retry protection did not expire together")
	}
}
