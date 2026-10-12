package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/state"
)

func TestLocalCommandsNeedNoCredentialsAndNoObserver(t *testing.T) {
	p := filepath.Join(t.TempDir(), "private", "state.db")
	s, e := state.Open(p, "http://sonarr", false)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	s.Data.Status.Ready = true
	s.Data.Status.DryRun = true
	s.Data.Status.PollInterval = time.Minute
	s.Data.Status.Connectivity = "connected"
	s.Data.Status.CompleteScan = time.Now()
	s.Data.Status.Heartbeat = time.Now()
	s.Data.Issues["download"] = &state.Issue{DownloadID: "download", State: "waiting", Reason: "observed wait", FirstSeen: time.Now()}
	if e = s.Save(); e != nil {
		t.Fatal(e)
	}
	for _, command := range [][]string{{"status"}, {"issues"}, {"show", "download"}, {"actions"}} {
		var out, errOut bytes.Buffer
		args := append(command, "--state", p, "--json")
		if e = execute(args, &out, &errOut); e != nil {
			t.Fatal(e)
		}
		if !json.Valid(out.Bytes()) {
			t.Fatal("non-JSON query result")
		}
	}
	var out, errOut bytes.Buffer
	if e = execute([]string{"healthcheck", "--state", p}, &out, &errOut); e != nil {
		t.Fatal(e)
	}
	if e = execute([]string{"show", "unknown", "--state", p}, &out, &errOut); e == nil {
		t.Fatal("unknown ID accepted")
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if e = execute([]string{"issues", "--state", missing}, &out, &errOut); e == nil {
		t.Fatal("missing store accepted")
	}
	if _, e = os.Stat(missing); !os.IsNotExist(e) {
		t.Fatal("local query created state")
	}
	s.Data.Status.Ready = false
	s.Save()
	if e = execute([]string{"healthcheck", "--state", p}, &out, &errOut); e == nil {
		t.Fatal("stopped observer healthy")
	}
	out.Reset()
	if e = execute([]string{"version"}, &out, &errOut); e != nil || strings.TrimSpace(out.String()) != version {
		t.Fatal("version command failed")
	}
}

func TestReviewStatusPreservesPersistedHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "state.db")
	s, e := state.Open(path, "http://sonarr", false)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	heartbeat := time.Now().Add(-10 * time.Minute)
	s.Data.Status.Heartbeat = heartbeat
	if e = s.Save(); e != nil {
		t.Fatal(e)
	}
	var out, errOut bytes.Buffer
	if e = execute([]string{"status", "--state", path, "--json"}, &out, &errOut); e != nil {
		t.Fatal(e)
	}
	var status state.Status
	if e = json.Unmarshal(out.Bytes(), &status); e != nil {
		t.Fatal(e)
	}
	if !status.Heartbeat.Equal(heartbeat) {
		t.Fatalf("query fabricated current heartbeat: got %v want persisted %v", status.Heartbeat, heartbeat)
	}
}
