package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/control"
	"github.com/calmcacil/sonarr-remediator/internal/policy"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	_ "modernc.org/sqlite"
)

const Schema = 2

type Status struct {
	ManualControl     string         `json:"manual_control,omitempty"`
	Identity          string         `json:"sonarr_identity"`
	Version           string         `json:"sonarr_version"`
	DryRun            bool           `json:"dry_run"`
	Recovery          bool           `json:"recovery_enabled"`
	Cleanup           bool           `json:"cleanup_enabled"`
	Blocklist         bool           `json:"blocklist_enabled"`
	ReplacementSearch bool           `json:"replacement_search_enabled"`
	Ready             bool           `json:"local_ready"`
	Connectivity      string         `json:"connectivity"`
	ReadFailure       string         `json:"required_read_failure,omitempty"`
	Heartbeat         time.Time      `json:"heartbeat"`
	CompleteScan      time.Time      `json:"complete_scan"`
	PollInterval      time.Duration  `json:"poll_interval_ns"`
	HourLimit         int            `json:"hour_limit"`
	BudgetUsed        int            `json:"mutations_last_hour"`
	IssueCounts       map[string]int `json:"issue_counts"`
	Unresolved        int            `json:"unresolved_outcomes"`
}
type Issue struct {
	DownloadID       string            `json:"download_id"`
	Identity         string            `json:"identity"`
	Fingerprint      string            `json:"fingerprint"`
	Title            string            `json:"title"`
	Rows             []sonarr.QueueRow `json:"queue_rows"`
	FirstSeen        time.Time         `json:"first_seen"`
	EligibleSince    time.Time         `json:"eligible_since,omitempty"`
	LastSeen         time.Time         `json:"last_seen"`
	Eligible         bool              `json:"eligible"`
	Elapsed          time.Duration     `json:"eligible_elapsed_ns"`
	Rejected         bool              `json:"sonarr_rejected,omitempty"`
	RejectedElapsed  time.Duration     `json:"rejected_elapsed_ns,omitempty"`
	RejectedLastSeen time.Time         `json:"rejected_last_seen,omitempty"`
	RecoveryDue      time.Time         `json:"recovery_due,omitempty"`
	CleanupDue       time.Time         `json:"cleanup_due,omitempty"`
	State            string            `json:"state"`
	Reason           string            `json:"reason"`
	Decision         policy.Decision   `json:"decision"`
	ResolvedAt       time.Time         `json:"resolved_at,omitempty"`
}
type Attempt struct {
	Source            string          `json:"source,omitempty"`
	RequestID         string          `json:"request_id,omitempty"`
	WaitOverride      bool            `json:"wait_override,omitempty"`
	ID                int64           `json:"id"`
	DownloadID        string          `json:"download_id"`
	Fingerprint       string          `json:"fingerprint"`
	Kind              string          `json:"kind"`
	Reason            string          `json:"reason"`
	DryRun            bool            `json:"dry_run"`
	Started           time.Time       `json:"started"`
	Finished          time.Time       `json:"finished,omitempty"`
	Outcome           string          `json:"outcome"`
	CommandID         int             `json:"command_id,omitempty"`
	Ack               bool            `json:"acknowledged"`
	Decision          policy.Decision `json:"decision"`
	BeforeFiles       map[int]int     `json:"before_files"`
	VerifiedEpisodes  []int           `json:"verified_episodes,omitempty"`
	RemovalVerified   bool            `json:"removal_verified"`
	BlocklistVerified bool            `json:"blocklist_verified"`
	Title             string          `json:"title"`
}

func (a Attempt) Pending() bool {
	return !a.DryRun && (a.Outcome == "intent" || a.Outcome == "pending" || a.Outcome == "uncertain" || a.Outcome == "partial")
}

type Operation struct {
	RemovalID  int64  `json:"removal_id"`
	DownloadID string `json:"download_id"`
	EpisodeIDs []int  `json:"episode_ids"`
	State      string `json:"state"`
	Reason     string `json:"reason"`
}
type Data struct {
	Triggers   map[string]*Receipt   `json:"manual_triggers"`
	Status     Status                `json:"status"`
	Issues     map[string]*Issue     `json:"issues"`
	Attempts   []Attempt             `json:"attempts"`
	Operations map[string]*Operation `json:"operations"`
}

type Receipt struct {
	Request    control.Request `json:"request"`
	Accepted   time.Time       `json:"accepted"`
	Finished   time.Time       `json:"finished,omitempty"`
	Status     string          `json:"status"`
	Report     control.Report  `json:"report"`
	AttemptIDs []int64         `json:"attempt_ids"`
}
type Store struct {
	db       *sql.DB
	lock     *os.File
	Data     Data
	readOnly bool
}

func Open(path, identity string, readOnly bool) (*Store, error) {
	abs, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	s := &Store{readOnly: readOnly}
	if readOnly {
		if _, e = os.Stat(abs); e != nil {
			return nil, errors.New("state store does not exist")
		}
	} else {
		parent := filepath.Dir(abs)
		if e = os.MkdirAll(parent, 0700); e != nil {
			return nil, errors.New("state directory is not writable")
		}
		st, e := os.Stat(parent)
		if e != nil || st.Mode().Perm()&0077 != 0 {
			return nil, errors.New("state directory must be private (mode 0700)")
		}
		for _, p := range []string{abs, abs + ".lock", abs + "-wal", abs + "-shm"} {
			if st, e := os.Lstat(p); e == nil && (!st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0) {
				return nil, errors.New("state files must be private regular files")
			}
		}
		fd, e := syscall.Open(abs+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
		if e != nil {
			return nil, errors.New("cannot open state owner lock")
		}
		s.lock = os.NewFile(uintptr(fd), abs+".lock")
		if e = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
			s.lock.Close()
			return nil, errors.New("another observer owns this state store")
		}
		f, e := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			s.Close()
			return nil, errors.New("cannot create state store")
		}
		f.Close()
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := url.Values{}
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(FULL)")
	}
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	s.db, e = sql.Open("sqlite", u.String())
	if e != nil {
		s.Close()
		return nil, e
	}
	s.db.SetMaxOpenConns(1)
	var version int
	if e = s.db.QueryRow("PRAGMA user_version").Scan(&version); e != nil {
		s.Close()
		return nil, errors.New("cannot read state schema")
	}
	newStore := version == 0 && !readOnly
	if newStore {
		tx, e := s.db.Begin()
		if e != nil {
			s.Close()
			return nil, e
		}
		_, e = tx.Exec("CREATE TABLE records (kind TEXT PRIMARY KEY, body TEXT NOT NULL); PRAGMA user_version=2;")
		if e != nil {
			tx.Rollback()
			s.Close()
			return nil, e
		}
		if e = tx.Commit(); e != nil {
			s.Close()
			return nil, e
		}
	} else if version != Schema && version != 1 {
		s.Close()
		return nil, fmt.Errorf("unsupported state schema %d", version)
	}
	s.Data = Data{Issues: map[string]*Issue{}, Operations: map[string]*Operation{}, Triggers: map[string]*Receipt{}}
	rows, e := s.db.Query("SELECT kind,body FROM records")
	if e != nil {
		s.Close()
		return nil, e
	}
	recordCount := 0
	for rows.Next() {
		recordCount++
		var kind, body string
		if e = rows.Scan(&kind, &body); e != nil {
			break
		}
		var target any
		switch kind {
		case "status":
			target = &s.Data.Status
		case "observations":
			target = &s.Data.Issues
		case "attempts":
			target = &s.Data.Attempts
		case "operations":
			target = &s.Data.Operations
		case "manual_triggers":
			if version == 1 {
				e = errors.New("unexpected receipt record in schema 1")
			}
			target = &s.Data.Triggers
		default:
			e = errors.New("unknown state record")
		}
		if e != nil {
			break
		}
		if e = json.Unmarshal([]byte(body), target); e != nil {
			break
		}
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		s.Close()
		return nil, errors.New("corrupt state records")
	}
	expected := 5
	if version == 1 {
		expected = 4
	}
	if !newStore && (recordCount != expected || s.Data.Status.Identity == "" || s.Data.Issues == nil || s.Data.Operations == nil || s.Data.Triggers == nil) {
		s.Close()
		return nil, errors.New("incomplete state records; refusing to reset safety history")
	}
	if readOnly && s.Data.Status.Identity == "" {
		s.Close()
		return nil, errors.New("state store is not initialized")
	}
	for _, issue := range s.Data.Issues {
		if issue == nil {
			s.Close()
			return nil, errors.New("corrupt null observation; refusing to reset safety history")
		}
	}
	for _, op := range s.Data.Operations {
		if op == nil {
			s.Close()
			return nil, errors.New("corrupt null operation; refusing to reset safety history")
		}
	}
	if e = s.Data.validateReceipts(); e != nil {
		s.Close()
		return nil, e
	}
	if !readOnly {
		if s.Data.Status.Identity != "" && s.Data.Status.Identity != identity {
			s.Close()
			return nil, errors.New("state store belongs to another Sonarr instance; choose a new store")
		}
		s.Data.Status.Identity = identity
		for i := range s.Data.Attempts {
			a := &s.Data.Attempts[i]
			if a.Outcome == "intent" {
				a.Outcome = "uncertain"
				a.Reason = "process interrupted before acknowledgement; requires read reconciliation"
			}
		}
		for _, receipt := range s.Data.Triggers {
			if receipt.Status == "accepted" {
				receipt.Status = "interrupted"
				receipt.Finished = time.Now()
				receipt.Report.ExitCode = 3
				receipt.Report.Error = "observer interrupted; unsubmitted work will not resume; inspect linked effects"
			}
		}
		if e = s.Save(); e != nil {
			s.Close()
			return nil, e
		}
	}
	return s, nil
}
func (s *Store) Save() error {
	if s.readOnly {
		return errors.New("read-only state store")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return errors.New("state transaction failed; mutations paused")
	}
	records := map[string]any{"status": s.Data.Status, "observations": s.Data.Issues, "attempts": s.Data.Attempts, "operations": s.Data.Operations, "manual_triggers": s.Data.Triggers}
	for kind, v := range records {
		b, e := json.Marshal(v)
		if e == nil {
			_, e = tx.Exec("INSERT INTO records(kind,body) VALUES(?,?) ON CONFLICT(kind) DO UPDATE SET body=excluded.body", kind, string(b))
		}
		if e != nil {
			tx.Rollback()
			return errors.New("state write failed; mutations paused")
		}
	}
	// The fifth record and schema marker commit together, after identity and all
	// existing records have been validated. Older readers refuse the new format.
	if _, e = tx.Exec("PRAGMA user_version=2"); e != nil {
		tx.Rollback()
		return errors.New("state schema migration failed; mutations paused")
	}
	if e = tx.Commit(); e != nil {
		return errors.New("state commit failed; mutations paused")
	}
	return nil
}
func (s *Store) Close() {
	if s.db != nil {
		s.db.Close()
	}
	if s.lock != nil {
		syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		s.lock.Close()
	}
}
func (d *Data) Budget(now time.Time) int {
	n := 0
	for _, a := range d.Attempts {
		if !a.DryRun && a.Started.After(now.Add(-time.Hour)) {
			n++
		}
	}
	return n
}
func (d *Data) UpdateStatus(now time.Time) {
	d.Status.Heartbeat = now
	d.AggregateStatus(now)
}

func (d *Data) AggregateStatus(now time.Time) {
	d.Status.BudgetUsed = d.Budget(now)
	d.Status.IssueCounts = map[string]int{}
	d.Status.Unresolved = 0
	for _, i := range d.Issues {
		d.Status.IssueCounts[i.State]++
	}
	for _, a := range d.Attempts {
		if a.Pending() {
			d.Status.Unresolved++
		}
	}
}
func (d *Data) Prune(now time.Time, retention, cooldown time.Duration) {
	protectedAttempts := map[int64]bool{}
	for id, r := range d.Triggers {
		unresolved := r.Status == "accepted"
		for _, a := range d.Attempts {
			if a.RequestID == id && d.retainAttempt(a, now, retention, cooldown) {
				unresolved = true
			}
			if op := d.Operations[a.DownloadID]; a.RequestID == id && op != nil && op.State != "finished" {
				unresolved = true
			}
		}
		if !unresolved && !r.Finished.IsZero() && now.Sub(r.Finished) > retention {
			delete(d.Triggers, id)
			continue
		}
		for _, aid := range r.AttemptIDs {
			protectedAttempts[aid] = true
		}
	}
	for id, i := range d.Issues {
		if i.State == "resolved" && !i.ResolvedAt.IsZero() && now.Sub(i.ResolvedAt) > retention {
			protected := false
			for _, a := range d.Attempts {
				if a.DownloadID == id && a.Pending() {
					protected = true
				}
			}
			if op := d.Operations[id]; op != nil && op.State != "finished" {
				protected = true
			}
			if !protected {
				delete(d.Issues, id)
				delete(d.Operations, id)
			}
		}
	}
	kept := d.Attempts[:0]
	for _, a := range d.Attempts {
		if protectedAttempts[a.ID] || d.retainAttempt(a, now, retention, cooldown) {
			kept = append(kept, a)
		}
	}
	d.Attempts = kept
}

func (d *Data) retainAttempt(a Attempt, now time.Time, retention, cooldown time.Duration) bool {
	i := d.Issues[a.DownloadID]
	needed := i != nil && i.State != "resolved"
	if op := d.Operations[a.DownloadID]; op != nil && op.State != "finished" && op.RemovalID == a.ID {
		needed = true
	}
	from := a.Finished
	if from.IsZero() {
		from = a.Started
	}
	return a.Pending() || needed || a.Started.After(now.Add(-time.Hour)) || a.Finished.After(now.Add(-cooldown)) || from.After(now.Add(-retention))
}
func (d *Data) SortedIssues(includeResolved bool) []*Issue {
	var rows []*Issue
	for _, i := range d.Issues {
		if includeResolved || i.State != "resolved" {
			rows = append(rows, i)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].FirstSeen.Equal(rows[j].FirstSeen) {
			return rows[i].DownloadID < rows[j].DownloadID
		}
		return rows[i].FirstSeen.Before(rows[j].FirstSeen)
	})
	return rows
}
func (s Status) Healthy(now time.Time) bool {
	return s.Ready && s.ReadFailure == "" && s.Connectivity == "connected" && s.PollInterval > 0 && !s.Heartbeat.IsZero() && !s.CompleteScan.IsZero() && !s.Heartbeat.After(now) && !s.CompleteScan.After(now) && now.Sub(s.Heartbeat) <= 3*s.PollInterval && now.Sub(s.CompleteScan) <= 3*s.PollInterval
}
