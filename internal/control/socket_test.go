package control

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrivateControlRoundTripAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, stopped := make(chan struct{}), make(chan struct{})
	server, err := Start(ctx, path, "http://fixture", func(ctx context.Context, r Request) Report {
		if r.OverrideWait {
			close(entered)
			<-ctx.Done()
			close(stopped)
			return Failure(r, 1, "cancelled")
		}
		return Failure(r, 2, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	req, err := NewRequest(false, false, []string{"opaque-id", "opaque-id"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	report := Call(ctx, path, "http://fixture", req)
	if report.ExitCode != 2 || report.Identity != "http://fixture" || len(report.Downloads) != 1 {
		t.Fatalf("round trip failed: %+v", report)
	}
	if mismatch := Call(ctx, path, "http://other", req); mismatch.ExitCode != 1 {
		t.Fatal("foreign endpoint accepted")
	}
	requestCtx, requestCancel := context.WithCancel(ctx)
	req.OverrideWait = true
	result := make(chan Report, 1)
	go func() { result <- Call(requestCtx, path, "http://fixture", req) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request not dispatched")
	}
	requestCancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not cancel service work")
	}
	if r := <-result; r.ExitCode != 1 {
		t.Fatal("cancelled preview did not fail")
	}
}

func TestControlNeverUnlinksUnsafeFilesOrLiveSocket(t *testing.T) {
	for _, kind := range []string{"regular", "symlink", "live", "stale"} {
		t.Run(kind, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "control-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			path := filepath.Join(dir, "state.db")
			os.Chmod(filepath.Dir(path), 0700)
			socket, _ := SocketPath(path)
			var listener *net.UnixListener
			switch kind {
			case "regular":
				if err := os.WriteFile(socket, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("missing-target", socket); err != nil {
					t.Fatal(err)
				}
			case "live", "stale":
				var err error
				listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				listener.SetUnlinkOnClose(false)
				os.Chmod(socket, 0600)
				defer listener.Close()
				if kind == "stale" {
					listener.Close()
				}
			}
			before, _ := os.Lstat(socket)
			server, err := Start(context.Background(), path, "fixture", func(context.Context, Request) Report { return Report{} })
			if kind == "stale" {
				if err != nil {
					t.Fatal(err)
				}
				server.Close()
				if _, err = os.Lstat(socket); !os.IsNotExist(err) {
					t.Fatal("own socket left on shutdown")
				}
				return
			}
			if err == nil {
				server.Close()
				t.Fatal("unsafe collision accepted")
			}
			after, statErr := os.Lstat(socket)
			if statErr != nil || !os.SameFile(before, after) {
				t.Fatal("existing file was removed")
			}
		})
	}
}

func TestFramesRejectUnknownTrailingAndOversizedData(t *testing.T) {
	valid, _ := NewRequest(true, false, []string{"download"}, time.Minute)
	body, _ := json.Marshal(valid)
	for _, extra := range []string{`,"unexpected":true}`, `,"Execute":false}`, `,"execute":false}`} {
		var buf bytes.Buffer
		invalid := append(append([]byte{}, body[:len(body)-1]...), []byte(extra)...)
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(invalid)))
		buf.Write(header[:])
		buf.Write(invalid)
		if err := readFrame(&buf, MaxRequest, new(Request)); err == nil {
			t.Fatal("ambiguous or unknown authorization field accepted")
		}
	}
	for _, body := range []string{`{"unknown":true}`, `{"execute":"true"}`, `{} {}`, `null`} {
		var buf bytes.Buffer
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(len(body)))
		buf.Write(header[:])
		buf.WriteString(body)
		var req Request
		err := readFrame(&buf, MaxRequest, &req)
		if err == nil {
			err = req.Validate(time.Now())
		}
		if err == nil {
			t.Fatalf("invalid frame accepted: %s", body)
		}
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], MaxRequest+1)
	if err := readFrame(bytes.NewReader(header[:]), MaxRequest, new(Request)); err == nil {
		t.Fatal("oversized frame accepted")
	}
	r, err := NewRequest(false, false, []string{""}, time.Minute)
	if err == nil {
		t.Fatal("empty selector accepted")
	}
	r.Downloads = make([]string, MaxDownloads+1)
	for j := range r.Downloads {
		r.Downloads[j] = time.Unix(int64(j), 0).String()
	}
	if err = r.Validate(time.Now()); err == nil {
		t.Fatal("oversized selection accepted")
	}
}

func FuzzRequestFrame(f *testing.F) {
	r, _ := NewRequest(false, false, nil, time.Minute)
	b, _ := json.Marshal(r)
	var buf bytes.Buffer
	writeFrame(&buf, MaxRequest, r)
	f.Add(buf.Bytes())
	f.Add(b)
	f.Fuzz(func(t *testing.T, data []byte) {
		var req Request
		if readFrame(bytes.NewReader(data), MaxRequest, &req) == nil {
			req.Validate(time.Now())
		}
	})
}
