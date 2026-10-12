package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calmcacil/sonarr-remediator/internal/control"
	"github.com/calmcacil/sonarr-remediator/internal/state"
)

func TestManualCLIOptionsAndLocalReceiptQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "state.db")
	store, err := state.Open(path, "http://fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	called := 0
	var captured control.Request
	server, err := control.Start(context.Background(), path, store.Data.Status.Identity, func(_ context.Context, r control.Request) control.Report {
		called++
		captured = r
		return control.Failure(r, 2, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	for _, args := range [][]string{{"trigger", "--execute", "--dry-run"}, {"trigger", "--dry-run=false"}, {"trigger", "--config", "config.yaml"}, {"trigger", "--timeout", "3m"}, {"trigger", "--timeout", "0s"}, {"trigger", "--download-id", ""}, {"trigger", "--unknown"}, {"trigger", "unexpected"}, {"triggers", "--execute"}} {
		var out, errOut bytes.Buffer
		if err = execute(append(args, "--state", path), &out, &errOut); err == nil {
			t.Fatalf("invalid options accepted: %v", args)
		}
	}
	if called != 0 {
		t.Fatal("invalid options reached observer")
	}
	for _, live := range []bool{false, true} {
		args := []string{"trigger", "--state", path, "--download-id", "opaque", "--download-id", "opaque", "--override-wait", "--json"}
		if live {
			args = append(args, "--execute")
		}
		var out, errOut bytes.Buffer
		err = execute(args, &out, &errOut)
		var exit *exitError
		if !errors.As(err, &exit) || exit.code != 2 {
			t.Fatalf("wrong no-action exit: %v", err)
		}
		var report control.Report
		if err = json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if captured.Execute != live || !captured.OverrideWait || len(captured.Downloads) != 1 || report.Mode == "" {
			t.Fatal("CLI changed request")
		}
		if live && (!strings.Contains(errOut.String(), captured.ID) || captured.ID == "") {
			t.Fatal("live request ID not emitted before transmission")
		}
	}
	server.Close()
	var out, errOut bytes.Buffer
	if err = execute([]string{"triggers", "--state", path, "--json"}, &out, &errOut); err != nil || !json.Valid(out.Bytes()) {
		t.Fatal("offline receipts failed", err)
	}
	if err = execute([]string{"trigger", "--state", path}, &out, &errOut); err == nil {
		t.Fatal("stopped observer accepted trigger")
	}
}
