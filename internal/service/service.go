package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/policy"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/state"
)

type Service struct {
	Config   config.Config
	Client   *sonarr.Client
	Store    *state.Store
	Log      *slog.Logger
	previous time.Time
	Now      func() time.Time
	sent     int
}

func New(c config.Config, client *sonarr.Client, store *state.Store, log *slog.Logger) *Service {
	return &Service{Config: c, Client: client, Store: store, Log: log, Now: time.Now}
}
func (s *Service) Run(ctx context.Context) error {
	st := &s.Store.Data.Status
	c := s.Config
	st.DryRun = c.DryRun
	st.Recovery = c.Recovery.Enabled
	st.Cleanup = c.Cleanup.Enabled
	st.Blocklist = c.Cleanup.Blocklist
	st.ReplacementSearch = c.Cleanup.ReplacementSearch
	st.PollInterval = c.PollInterval
	st.HourLimit = c.Limits.MutationsPerHour
	st.Ready = true
	if e := s.Store.Save(); e != nil {
		return e
	}
	s.Log.Info("service started", "dry_run", c.DryRun, "recovery", c.Recovery.Enabled, "cleanup_client_deletion", c.Cleanup.Enabled, "blocklist", c.Cleanup.Blocklist, "replacement_search", c.Cleanup.ReplacementSearch)
	defer func() { st.Ready = false; s.Store.Data.UpdateStatus(s.Now()); s.Store.Save() }()
	for {
		if ctx.Err() != nil {
			return nil
		}
		if e := s.Cycle(ctx); e != nil {
			return e
		}
		timer := time.NewTimer(c.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
func group(queue []sonarr.QueueRow) map[string][]sonarr.QueueRow {
	g := map[string][]sonarr.QueueRow{}
	for _, q := range queue {
		if q.DownloadID != "" {
			g[q.DownloadID] = append(g[q.DownloadID], q)
		}
	}
	return g
}
func (s *Service) failed(now time.Time, e error) error {
	s.expirePending(now)
	s.previous = time.Time{}
	st := &s.Store.Data.Status
	st.ReadFailure = e.Error()
	st.Connectivity = "degraded"
	s.Store.Data.UpdateStatus(now)
	s.Log.Warn("observation deferred", "reason", e.Error())
	return s.Store.Save()
}

func (s *Service) expirePending(now time.Time) {
	for j := range s.Store.Data.Attempts {
		a := &s.Store.Data.Attempts[j]
		if a.Pending() && a.Outcome != "partial" && now.Sub(a.Started) >= 10*time.Minute {
			a.Outcome = "uncertain"
			a.Reason = "verification deadline exceeded; required evidence unavailable; no automatic replay"
			if i := s.Store.Data.Issues[a.DownloadID]; i != nil {
				s.setIssue(i, "uncertain", a.Reason)
			}
		}
	}
}
func (s *Service) Cycle(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 50*time.Second)
	defer cancel()
	now := s.Now()
	s.sent = 0
	s.Client.BeginCycle()
	version, e := s.Client.Version(ctx)
	if e != nil {
		if strings.Contains(e.Error(), "unsupported Sonarr version") {
			return e
		}
		return s.failed(now, e)
	}
	queue, e := s.Client.Queue(ctx)
	if e != nil {
		return s.failed(now, e)
	}
	groups := group(queue)
	data := &s.Store.Data
	data.Status.Version = version
	data.Status.Connectivity = "connected"
	data.Status.ReadFailure = ""
	// Build required candidate observations before publishing a successful scan.
	observations := map[string]policy.Observation{}
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rows := groups[id]
		if !policy.Eligible(rows) {
			continue
		}
		o, e := s.observe(ctx, id, rows, queue)
		if e != nil {
			return s.failed(now, e)
		}
		observations[id] = o
	}
	if ctx.Err() != nil {
		return s.failed(now, errors.New("cycle deadline exhausted"))
	}
	for id, rows := range groups {
		i := data.Issues[id]
		identity := policy.QueueIdentity(rows)
		eligible := policy.Eligible(rows)
		if i == nil {
			if !eligible {
				continue
			}
			i = &state.Issue{DownloadID: id, FirstSeen: now}
			data.Issues[id] = i
		}
		if identity != i.Identity || !eligible || !i.Eligible || i.State == "resolved" {
			i.Elapsed = 0
			i.RecoveryDue = time.Time{}
			i.CleanupDue = time.Time{}
		}
		if eligible && i.Eligible && identity == i.Identity && !s.previous.IsZero() && now.After(s.previous) && now.Sub(s.previous) <= 2*s.Config.PollInterval && i.LastSeen.Equal(s.previous) {
			delta := now.Sub(s.previous)
			if delta > s.Config.PollInterval {
				delta = s.Config.PollInterval
			}
			i.Elapsed += delta
		}
		if i.Elapsed < s.Config.Recovery.Wait {
			i.RecoveryDue = time.Time{}
		} else if i.RecoveryDue.IsZero() {
			i.RecoveryDue = now
		}
		if i.Elapsed < s.Config.Cleanup.Wait {
			i.CleanupDue = time.Time{}
		} else if i.CleanupDue.IsZero() {
			i.CleanupDue = now
		}
		i.Identity = identity
		i.Rows = rows
		i.Title = rows[0].Title
		i.Eligible = eligible
		i.LastSeen = now
		i.ResolvedAt = time.Time{}
		if !eligible && !s.hasPending(id) {
			s.setIssue(i, "policy-blocked", "protected or noneligible tracked state")
		}
	}
	for id, i := range data.Issues {
		if _, ok := groups[id]; !ok && i.State != "resolved" && !s.hasPending(id) {
			i.Eligible = false
			i.Elapsed = 0
			i.ResolvedAt = now
			s.setIssue(i, "resolved", "externally resolved; complete queue scan shows absence")
		}
	}
	s.previous = now
	data.Status.CompleteScan = now
	for j := range data.Attempts {
		a := &data.Attempts[j]
		if a.Pending() {
			if e := s.verify(ctx, a, queue, now); e != nil {
				data.Status.ReadFailure = e.Error()
				s.Log.Warn("verification deferred", "download_id", a.DownloadID, "reason", e.Error())
			}
		}
	}
	candidates := data.SortedIssues(false)
	dueAt := func(i *state.Issue) time.Time {
		if o, ok := observations[i.DownloadID]; ok {
			d := policy.Evaluate(s.Config, o)
			if d.Kind == "import" {
				return i.RecoveryDue
			}
			if d.Kind == "remove_redundant" || d.Kind == "remove_unusable" {
				return i.CleanupDue
			}
		}
		return time.Time{}
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		x, y := dueAt(candidates[a]), dueAt(candidates[b])
		if x.IsZero() != y.IsZero() {
			return !x.IsZero()
		}
		if !x.Equal(y) {
			return x.Before(y)
		}
		return candidates[a].DownloadID < candidates[b].DownloadID
	})
	for _, i := range candidates {
		if s.hasPending(i.DownloadID) {
			status, reason := "in-progress", "pending/uncertain action protects this download and episodes"
			for _, a := range data.Attempts {
				if a.DownloadID == i.DownloadID && (a.Outcome == "uncertain" || a.Outcome == "partial") {
					status, reason = "uncertain", a.Reason
				}
			}
			s.setIssue(i, status, reason)
			continue
		}
		o, ok := observations[i.DownloadID]
		if !ok {
			continue
		}
		d := policy.Evaluate(s.Config, o)
		i.Fingerprint = d.Fingerprint
		i.Decision = d
		if d.Kind == "defer" {
			s.setIssue(i, "policy-blocked", d.Reason)
			continue
		}
		wait := s.Config.Recovery.Wait
		if d.Kind != "import" {
			wait = s.Config.Cleanup.Wait
		}
		if i.Elapsed < wait {
			s.setIssue(i, "waiting", fmt.Sprintf("%s; observed eligibility %s of %s", d.Reason, i.Elapsed, wait))
			continue
		}
		if why := s.gate(i.DownloadID, d, now, 0); why != "" {
			status := "retry-waiting"
			if strings.Contains(why, "exhausted") {
				status = "exhausted"
			}
			s.setIssue(i, status, why)
			continue
		}
		s.setIssue(i, "recoverable", d.Reason)
		if s.Config.DryRun {
			s.recommend(i, d, now)
			continue
		}
		if data.Status.ReadFailure != "" || s.sent >= s.Config.Limits.MutationsPerScan || ctx.Err() != nil {
			continue
		}
		if e := s.execute(ctx, i, d, now); e != nil {
			return e
		}
	}
	// A continuation is durable, independent of whether its old queue row still exists.
	for _, id := range sortedOperations(data.Operations) {
		op := data.Operations[id]
		if op.State != "pending-search" || s.Config.DryRun || s.sent >= s.Config.Limits.MutationsPerScan {
			continue
		}
		if e := s.search(ctx, op, now); e != nil {
			return e
		}
	}
	data.Prune(now, s.Config.HistoryRetention, s.Config.Limits.EpisodeCooldown)
	data.UpdateStatus(now)
	return s.Store.Save()
}
func sortedOperations(m map[string]*state.Operation) []string {
	var ids []string
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
func (s *Service) setIssue(i *state.Issue, status, reason string) {
	if i.State != status || i.Reason != reason {
		s.Log.Info("issue decision", "download_id", i.DownloadID, "episodes", i.Decision.EpisodeIDs, "state", status, "reason", reason)
	}
	i.State = status
	i.Reason = reason
}
func (s *Service) hasPending(id string) bool {
	for _, a := range s.Store.Data.Attempts {
		if a.DownloadID == id && a.Pending() {
			return true
		}
	}
	return false
}
func (s *Service) observe(ctx context.Context, id string, rows, queue []sonarr.QueueRow) (policy.Observation, error) {
	o := policy.Observation{DownloadID: id, Rows: rows, Queue: queue, Episodes: map[int]sonarr.Episode{}, Files: map[int]sonarr.EpisodeFile{}, Series: map[int]sonarr.Series{}, Profiles: map[int]sonarr.Profile{}}
	p, e := s.Client.Preview(ctx, id)
	if e != nil {
		return o, e
	}
	o.Preview = p
	intended := map[int]bool{}
	pack := len(p) > 1
	for _, r := range rows {
		if r.EpisodeID != nil && *r.EpisodeID > 0 {
			intended[*r.EpisodeID] = true
		}
	}
	for _, f := range p {
		if f.ReleaseType == "seasonPack" || f.ReleaseType == "multiEpisode" || len(f.Episodes) > 1 {
			pack = true
		}
		for _, ep := range f.Episodes {
			if ep.ID > 0 {
				intended[ep.ID] = true
			}
		}
	}
	o.MembershipComplete = len(intended) > 0 && len(p) > 0
	if pack {
		history, e := s.Client.History(ctx, id)
		if e != nil {
			return o, e
		}
		grabbed := false
		for _, h := range history {
			if h.EventType == "grabbed" {
				if h.EpisodeID <= 0 || h.SeriesID <= 0 {
					o.MembershipComplete = false
					continue
				}
				grabbed = true
				intended[h.EpisodeID] = true
			}
		}
		o.MembershipComplete = o.MembershipComplete && grabbed
	}
	for id := range intended {
		o.Intended = append(o.Intended, id)
	}
	sort.Ints(o.Intended)
	for _, eid := range o.Intended {
		ep, e := s.Client.Episode(ctx, eid)
		if e != nil {
			return o, e
		}
		o.Episodes[eid] = ep
		for _, r := range rows {
			if r.EpisodeID != nil && *r.EpisodeID == eid && r.SeriesID != nil && *r.SeriesID != ep.SeriesID {
				o.MembershipComplete = false
			}
		}
		if ep.SeriesID <= 0 {
			continue
		}
		if _, ok := o.Series[ep.SeriesID]; !ok {
			series, e := s.Client.Series(ctx, ep.SeriesID)
			if e != nil {
				return o, e
			}
			o.Series[ep.SeriesID] = series
			if series.QualityProfileID > 0 {
				if _, ok := o.Profiles[series.QualityProfileID]; !ok {
					profile, e := s.Client.Profile(ctx, series.QualityProfileID)
					if e != nil {
						return o, e
					}
					o.Profiles[profile.ID] = profile
				}
			}
		}
		if ep.HasFile != nil && *ep.HasFile && ep.EpisodeFileID != nil && *ep.EpisodeFileID > 0 {
			if _, ok := o.Files[*ep.EpisodeFileID]; !ok {
				f, e := s.Client.EpisodeFile(ctx, *ep.EpisodeFileID)
				if e != nil {
					return o, e
				}
				o.Files[f.ID] = f
			}
		}
	}
	media, e := s.Client.Media(ctx)
	if e != nil {
		return o, e
	}
	o.Propers = media.Propers
	return o, nil
}
func (s *Service) gate(id string, d policy.Decision, now time.Time, parent int64) string {
	if s.Store.Data.Budget(now) >= s.Config.Limits.MutationsPerHour {
		return "rolling hour mutation budget exhausted"
	}
	count := 0
	var last time.Time
	for _, a := range s.Store.Data.Attempts {
		if a.DryRun {
			continue
		}
		same := a.DownloadID == id
		overlap := false
		for _, eid := range d.EpisodeIDs {
			if slices.Contains(a.Decision.EpisodeIDs, eid) {
				overlap = true
			}
		}
		if (same || overlap) && a.Pending() {
			return "pending/uncertain action protects download or episode"
		}
		if a.ID != parent && (same || overlap) && (a.Outcome == "verified" || a.Outcome == "submitted") && now.Sub(a.Finished) < s.Config.Limits.EpisodeCooldown {
			return "episode cooldown"
		}
		if same && d.Kind == "import" && a.Kind == "import" && a.Fingerprint == d.Fingerprint {
			count++
			finished := a.Finished
			if finished.IsZero() {
				finished = a.Started
			}
			if finished.After(last) {
				last = finished
			}
		}
	}
	if d.Kind == "import" {
		if count >= s.Config.Recovery.MaxAttempts {
			return "recovery attempt budget exhausted"
		}
		if !last.IsZero() && now.Sub(last) < s.Config.Recovery.RetryInterval {
			return "known failed import retry interval"
		}
	}
	return ""
}
func (s *Service) recommend(i *state.Issue, d policy.Decision, now time.Time) {
	for j := len(s.Store.Data.Attempts) - 1; j >= 0; j-- {
		a := s.Store.Data.Attempts[j]
		if a.DownloadID == i.DownloadID && a.DryRun && a.Fingerprint == d.Fingerprint && a.Kind == d.Kind {
			return
		}
	}
	s.Store.Data.Attempts = append(s.Store.Data.Attempts, state.Attempt{ID: s.nextID(), DownloadID: i.DownloadID, Fingerprint: d.Fingerprint, Kind: d.Kind, Reason: d.Reason, DryRun: true, Started: now, Finished: now, Outcome: "recommended", Decision: d})
}
func (s *Service) nextID() int64 {
	var n int64
	for _, a := range s.Store.Data.Attempts {
		if a.ID > n {
			n = a.ID
		}
	}
	return n + 1
}
func (s *Service) reserve(i *state.Issue, d policy.Decision, now time.Time, before map[int]int) (int, error) {
	a := state.Attempt{ID: s.nextID(), DownloadID: i.DownloadID, Fingerprint: d.Fingerprint, Kind: d.Kind, Reason: d.Reason, Started: now, Outcome: "intent", Decision: d, BeforeFiles: before, Title: i.Title}
	s.Store.Data.Attempts = append(s.Store.Data.Attempts, a)
	s.setIssue(i, "in-progress", "durable intent recorded before transmission")
	if e := s.Store.Save(); e != nil {
		return 0, e
	}
	s.sent++
	return len(s.Store.Data.Attempts) - 1, nil
}
func (s *Service) execute(ctx context.Context, i *state.Issue, planned policy.Decision, now time.Time) error {
	s.Client.BeginCycle()
	queue, e := s.Client.Queue(ctx)
	if e != nil {
		s.Store.Data.Status.ReadFailure = e.Error()
		return nil
	}
	rows := group(queue)[i.DownloadID]
	if !policy.Eligible(rows) || policy.QueueIdentity(rows) != i.Identity {
		s.setIssue(i, "policy-blocked", "identity/eligibility changed during fresh execution checks")
		return nil
	}
	o, e := s.observe(ctx, i.DownloadID, rows, queue)
	if e != nil {
		s.Store.Data.Status.ReadFailure = e.Error()
		return nil
	}
	d := policy.Evaluate(s.Config, o)
	if d.Kind != planned.Kind || d.Fingerprint != planned.Fingerprint {
		s.setIssue(i, "policy-blocked", "candidate or policy changed during fresh execution checks")
		return nil
	}
	now = s.Now()
	if why := s.gate(i.DownloadID, d, now, 0); why != "" {
		s.setIssue(i, "retry-waiting", why)
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	before := map[int]int{}
	for id, ep := range o.Episodes {
		if ep.EpisodeFileID != nil {
			before[id] = *ep.EpisodeFileID
		}
	}
	idx, e := s.reserve(i, d, now, before)
	if e != nil {
		return e
	}
	a := &s.Store.Data.Attempts[idx]
	if d.Kind == "import" {
		cmd, e := s.Client.Submit(ctx, sonarr.ImportCommand{Name: "ManualImport", ImportMode: "auto", Files: d.Files})
		if e == nil {
			a.Ack = true
			a.CommandID = cmd.ID
		}
		s.response(a, e)
	} else {
		e = s.Client.Remove(ctx, rows[0].ID, d.Blocklist)
		if e == nil {
			a.Ack = true
		}
		s.response(a, e)
	}
	s.Log.Info("mutation attempted", "download_id", a.DownloadID, "episodes", d.EpisodeIDs, "action", d.Kind, "blocklist", d.Blocklist, "outcome", a.Outcome)
	return s.Store.Save()
}
func (s *Service) response(a *state.Attempt, e error) {
	if e != nil {
		a.Outcome = "uncertain"
		a.Reason = "ambiguous mutation result: " + e.Error()
	} else {
		a.Outcome = "pending"
		a.Reason = "acknowledged; awaiting correlated verification"
	}
}
