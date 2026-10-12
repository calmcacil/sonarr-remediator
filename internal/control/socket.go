package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

func SocketPath(statePath string) (string, error) {
	abs, err := filepath.Abs(statePath)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(abs))
	return filepath.Join(filepath.Dir(abs), ".trigger-"+hex.EncodeToString(h[:12])+".sock"), nil
}

func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}
func privateParent(path string) error {
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || !owned(info) {
		return errors.New("manual control requires an owned private state directory")
	}
	return nil
}
func privateSocket(path string) (os.FileInfo, error) {
	if err := privateParent(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 || !owned(info) {
		return nil, errors.New("manual control socket unavailable or unsafe")
	}
	return info, nil
}

type Server struct {
	listener *net.UnixListener
	path     string
	info     os.FileInfo
	cancel   context.CancelFunc
	done     chan struct{}
	handlers sync.WaitGroup
}

// Start requires the caller to hold the state store's exclusive owner lock.
func Start(parent context.Context, statePath, identity string, handle func(context.Context, Request) Report) (*Server, error) {
	if !peerSupported {
		return nil, errors.New("private manual control requires Linux peer credentials")
	}
	path, err := SocketPath(statePath)
	if err != nil {
		return nil, err
	}
	if err = privateParent(path); err != nil {
		return nil, err
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 || !owned(info) || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("manual control path collides with an unsafe existing file")
		}
		conn, dialErr := net.DialTimeout("unix", path, time.Second)
		if dialErr == nil {
			conn.Close()
			return nil, errors.New("manual control listener already exists")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, errors.New("cannot prove manual control socket stale")
		}
		if current, statErr := os.Lstat(path); statErr != nil || !os.SameFile(info, current) {
			return nil, errors.New("manual control socket changed during stale check")
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(statErr) {
		return nil, statErr
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errors.New("cannot bind private manual control socket (check state filesystem/path)")
	}
	listener.SetUnlinkOnClose(false)
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		os.Remove(path)
		return nil, err
	}
	info, err := privateSocket(path)
	if err != nil {
		listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	s := &Server{listener: listener, path: path, info: info, cancel: cancel, done: make(chan struct{})}
	abs, _ := filepath.Abs(statePath)
	go func() {
		defer close(s.done)
		stop := context.AfterFunc(ctx, func() { listener.Close() })
		defer stop()
		slots := make(chan struct{}, 8)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
				s.handlers.Add(1)
				go func() {
					defer s.handlers.Done()
					defer func() { <-slots }()
					s.serve(ctx, conn, abs, identity, handle)
				}()
			default:
				conn.Close()
			}
		}
	}()
	return s, nil
}

func (s *Server) Close() {
	s.cancel()
	s.listener.Close()
	<-s.done
	s.handlers.Wait()
	if info, err := os.Lstat(s.path); err == nil && os.SameFile(s.info, info) {
		os.Remove(s.path)
	}
}

func (s *Server) serve(parent context.Context, conn *net.UnixConn, statePath, identity string, handle func(context.Context, Request) Report) {
	defer conn.Close()
	stop := context.AfterFunc(parent, func() { conn.Close() })
	defer stop()
	if err := samePeer(conn); err != nil {
		return
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	var req Request
	if err := readFrame(conn, MaxRequest, &req); err != nil {
		writeFrame(conn, MaxResponse, Failure(req, 1, "invalid manual request: "+err.Error()))
		return
	}
	if err := req.ValidateStructure(); err != nil {
		writeFrame(conn, MaxResponse, Failure(req, 1, err.Error()))
		return
	}
	conn.SetDeadline(maxTime(req.Expires, time.Now().Add(5*time.Second)))
	// Allow a short receipt lookup for expired, already-accepted identifiers.
	// Manual evaluation itself enforces the original request deadline.
	ctx, cancel := context.WithDeadline(parent, maxTime(req.Expires, time.Now().Add(5*time.Second)))
	defer cancel()
	// After the one request frame, EOF means the caller disconnected. Additional
	// bytes are invalid and cancel the same unit of work.
	disconnected := make(chan struct{})
	go func() { defer close(disconnected); var b [1]byte; conn.Read(b[:]); cancel() }()
	defer func() { conn.Close(); <-disconnected }()
	report := handle(ctx, req)
	report.StatePath, report.Identity = statePath, identity
	if err := writeFrame(conn, MaxResponse, report); err != nil {
		return
	}
}

func readFrame(reader io.Reader, limit int, value any) error {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return errors.New("incomplete frame")
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || uint64(n) > uint64(limit) {
		return errors.New("oversized or empty frame")
	}
	body := make([]byte, int(n))
	if _, err := io.ReadFull(reader, body); err != nil {
		return errors.New("incomplete frame")
	}
	if _, ok := value.(*Request); ok {
		fields := map[string]json.RawMessage{}
		object := json.NewDecoder(bytes.NewReader(body))
		first, err := object.Token()
		if err != nil || first != json.Delim('{') {
			return errors.New("invalid request object")
		}
		for object.More() {
			key, err := object.Token()
			if err != nil {
				return errors.New("invalid request field")
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("invalid request field")
			}
			switch name {
			case "schema_version", "request_id", "execute", "override_wait", "download_ids", "issued", "expires":
			default:
				return errors.New("unknown request field")
			}
			if _, exists := fields[name]; exists {
				return errors.New("duplicate request field")
			}
			var raw json.RawMessage
			if object.Decode(&raw) != nil {
				return errors.New("invalid request value")
			}
			fields[name] = raw
		}
		for _, name := range []string{"schema_version", "execute", "override_wait", "download_ids", "issued", "expires"} {
			raw, exists := fields[name]
			if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return errors.New("missing or null request field")
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errors.New("invalid JSON fields")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
func writeFrame(writer io.Writer, limit int, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(body) > limit {
		return errors.New("manual report exceeds size limit")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	_, err = io.Copy(writer, io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(body)))
	return err
}

func Call(ctx context.Context, statePath, identity string, req Request) Report {
	if err := req.Validate(time.Now()); err != nil {
		return Failure(req, 1, err.Error())
	}
	if body, err := json.Marshal(req); err != nil || len(body) > MaxRequest {
		return Failure(req, 1, "manual request exceeds 16 KiB; narrow the download IDs")
	}
	path, err := SocketPath(statePath)
	if err != nil {
		return Failure(req, 1, "invalid state path")
	}
	if _, err = privateSocket(path); err != nil {
		return Failure(req, 1, err.Error())
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return Failure(req, 1, "running observer unavailable")
	}
	conn := connection.(*net.UnixConn)
	defer conn.Close()
	if err = samePeer(conn); err != nil {
		return Failure(req, 1, err.Error())
	}
	conn.SetDeadline(req.Expires)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	code := 1
	if req.Execute {
		code = 3
	}
	if err = writeFrame(conn, MaxRequest, req); err != nil {
		return Failure(req, code, "request transmission incomplete; inspect triggers before a new execution")
	}
	var report Report
	if err = readFrame(conn, MaxResponse, &report); err != nil {
		return Failure(req, code, "manual response unavailable; inspect triggers before a new execution")
	}
	abs, _ := filepath.Abs(statePath)
	if report.Version != Version || report.StatePath != abs || report.Identity != identity || report.RequestID != req.ID || report.ExitCode < 0 || report.ExitCode > 3 {
		return Failure(req, code, fmt.Sprintf("manual response identity/schema mismatch; inspect triggers %s", req.ID))
	}
	return report
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
