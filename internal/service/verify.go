package service

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/policy"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/state"
)

func (s *Service) verify(ctx context.Context, a *state.Attempt, queue []sonarr.QueueRow, now time.Time) (err error) {
	defer func() {
		if err != nil && a.Kind == "import" && len(a.VerifiedEpisodes) > 0 && a.Pending() {
			a.Outcome = "partial"
			a.Reason = "partial correlated import; remaining episode evidence unavailable"
			return
		}
		if err != nil && a.Pending() && a.Outcome != "partial" && now.Sub(a.Started) >= 10*time.Minute {
			a.Outcome = "uncertain"
			a.Reason = "verification deadline exceeded; required evidence unavailable; no automatic replay"
		}
	}()
	complete := false
	knownFailed := false
	switch a.Kind {
	case "import":
		if a.CommandID > 0 {
			cmd, e := s.Client.Command(ctx, a.CommandID)
			if e != nil {
				return e
			}
			if cmd.Name != "ManualImport" {
				return errors.New("import command identity mismatch")
			}
			switch cmd.Status {
			case "failed", "aborted", "cancelled":
				knownFailed = true
			case "queued", "started", "completed":
			default:
				return errors.New("unknown import command state")
			}
		}
		history, e := s.Client.History(ctx, a.DownloadID)
		if e != nil {
			return e
		}
		verified := []int{}
		for _, eid := range a.Decision.EpisodeIDs {
			var intended *sonarr.ImportFile
			for j := range a.Decision.Files {
				if slices.Contains(a.Decision.Files[j].EpisodeIDs, eid) {
					intended = &a.Decision.Files[j]
					break
				}
			}
			if intended != nil {
				for _, h := range history {
					if h.EventType == "downloadFolderImported" && h.EpisodeID == eid && h.SeriesID == intended.SeriesID && !h.Date.Before(a.Started) && !h.Date.After(now) && h.Data["droppedPath"] == intended.Path && h.Data["importedPath"] != "" {
						if !slices.Contains(a.VerifiedEpisodes, eid) {
							a.VerifiedEpisodes = append(a.VerifiedEpisodes, eid)
							sort.Ints(a.VerifiedEpisodes)
						}
						break
					}
				}
			}
			ep, f, p, e := s.current(ctx, eid)
			if e != nil {
				return e
			}
			if !policy.Acceptable(f, p) {
				continue
			}
			if intended == nil {
				if ep.EpisodeFileID != nil && *ep.EpisodeFileID == a.BeforeFiles[eid] {
					verified = append(verified, eid)
				}
				continue
			}
			if ep.SeriesID != intended.SeriesID || ep.EpisodeFileID == nil || *ep.EpisodeFileID == a.BeforeFiles[eid] {
				continue
			}
			for _, h := range history {
				if h.EventType == "downloadFolderImported" && h.EpisodeID == eid && h.SeriesID == ep.SeriesID && !h.Date.Before(a.Started) && !h.Date.After(now) && h.Data["droppedPath"] == intended.Path && h.Data["importedPath"] == f.Path {
					verified = append(verified, eid)
					break
				}
			}
		}
		complete = len(verified) == len(a.Decision.EpisodeIDs) && len(verified) > 0
		if !complete && len(a.VerifiedEpisodes) > 0 {
			a.Outcome = "partial"
			a.Reason = "partial correlated import; remaining episodes protected"
		}
	case "remove_redundant", "remove_unusable":
		absent := true
		for _, q := range queue {
			if q.DownloadID == a.DownloadID {
				absent = false
			}
		}
		if a.Ack && absent {
			a.RemovalVerified = true
		}
		if a.Decision.Blocklist && a.RemovalVerified && !a.BlocklistVerified {
			hs, e := s.Client.History(ctx, a.DownloadID)
			if e != nil {
				return e
			}
			failed := map[int]bool{}
			for _, h := range hs {
				if h.EventType == "downloadFailed" && h.SourceTitle == a.Title && !h.Date.Before(a.Started) && !h.Date.After(now) && slices.Contains(a.Decision.EpisodeIDs, h.EpisodeID) && slices.Contains(a.Decision.SeriesIDs, h.SeriesID) {
					failed[h.EpisodeID] = true
				}
			}
			entries, e := s.Client.Blocklist(ctx)
			if e != nil {
				return e
			}
			covered := map[int]bool{}
			counts := map[int]int{}
			for _, b := range entries {
				if b.SourceTitle != a.Title || b.Date.Before(a.Started) || b.Date.After(now) || !slices.Contains(a.Decision.SeriesIDs, b.SeriesID) {
					continue
				}
				for _, eid := range b.EpisodeIDs {
					if slices.Contains(a.Decision.EpisodeIDs, eid) {
						counts[eid]++
						covered[eid] = true
					}
				}
			}
			a.BlocklistVerified = true
			for _, eid := range a.Decision.EpisodeIDs {
				if !failed[eid] || !covered[eid] || counts[eid] != 1 {
					a.BlocklistVerified = false
				}
			}
		}
		complete = a.RemovalVerified && (!a.Decision.Blocklist || a.BlocklistVerified)
		if a.RemovalVerified && !complete {
			a.Outcome = "partial"
			a.Reason = "client removal verified; blocklist evidence unresolved"
		}
	case "search":
		if a.Ack && a.CommandID > 0 {
			cmd, e := s.Client.Command(ctx, a.CommandID)
			if e != nil {
				return e
			}
			if cmd.Name != "EpisodeSearch" {
				return errors.New("search command identity mismatch")
			}
			switch cmd.Status {
			case "queued", "started", "completed":
				complete = true
			case "failed", "aborted", "cancelled":
				knownFailed = true
			default:
				return errors.New("unknown search command state")
			}
		}
	}
	if complete {
		a.Finished = now
		a.Outcome = "verified"
		a.Reason = "correlated outcome verified"
		if a.Kind == "search" {
			a.Outcome = "submitted"
			a.Reason = "replacement search submitted; replacement availability is unknown"
			if op := s.Store.Data.Operations[a.DownloadID]; op != nil {
				op.State = "finished"
				op.Reason = a.Reason
			}
		}
		if a.Kind == "remove_unusable" && s.Config.Cleanup.ReplacementSearch {
			s.Store.Data.Operations[a.DownloadID] = &state.Operation{RemovalID: a.ID, DownloadID: a.DownloadID, EpisodeIDs: a.Decision.EpisodeIDs, State: "pending-search", Reason: "removal verified; fresh replacement checks pending"}
		}
		if i := s.Store.Data.Issues[a.DownloadID]; i != nil {
			i.ResolvedAt = now
			s.setIssue(i, "resolved", a.Reason)
		}
		s.Log.Info("action verified", "download_id", a.DownloadID, "action", a.Kind, "episodes", a.VerifiedEpisodes, "client_removal", a.RemovalVerified, "blocklist", a.BlocklistVerified)
	} else if knownFailed && len(a.VerifiedEpisodes) == 0 {
		a.Finished = now
		a.Outcome = "failed"
		a.Reason = "Sonarr command reported a known failure"
		if a.Kind == "search" {
			if op := s.Store.Data.Operations[a.DownloadID]; op != nil {
				op.State = "finished"
				op.Reason = "search failed; operator intervention required"
			}
		}
	} else if now.Sub(a.Started) >= 10*time.Minute && a.Outcome != "partial" {
		a.Outcome = "uncertain"
		a.Reason = "verification deadline exceeded; no automatic replay"
	}
	return nil
}
func (s *Service) current(ctx context.Context, id int) (sonarr.Episode, sonarr.EpisodeFile, sonarr.Profile, error) {
	ep, e := s.Client.Episode(ctx, id)
	if e != nil {
		return ep, sonarr.EpisodeFile{}, sonarr.Profile{}, e
	}
	if ep.HasFile == nil || ep.EpisodeFileID == nil || ep.Monitored == nil || ep.SeriesID <= 0 {
		return ep, sonarr.EpisodeFile{}, sonarr.Profile{}, errors.New("incomplete current episode")
	}
	series, e := s.Client.Series(ctx, ep.SeriesID)
	if e != nil {
		return ep, sonarr.EpisodeFile{}, sonarr.Profile{}, e
	}
	p, e := s.Client.Profile(ctx, series.QualityProfileID)
	if e != nil {
		return ep, sonarr.EpisodeFile{}, p, e
	}
	if e = policy.ValidateProfile(p); e != nil {
		return ep, sonarr.EpisodeFile{}, p, e
	}
	var f sonarr.EpisodeFile
	if *ep.HasFile {
		if *ep.EpisodeFileID <= 0 {
			return ep, f, p, errors.New("invalid current episode file reference")
		}
		f, e = s.Client.EpisodeFile(ctx, *ep.EpisodeFileID)
		if e != nil {
			return ep, f, p, e
		}
		if f.SeriesID != ep.SeriesID {
			return ep, f, p, errors.New("current episode file series mismatch")
		}
		if _, e = policy.CurrentAcceptable(f, p); e != nil {
			return ep, f, p, e
		}
	} else if *ep.EpisodeFileID != 0 {
		return ep, f, p, errors.New("contradictory missing-file state")
	}
	return ep, f, p, nil
}
func (s *Service) search(ctx context.Context, op *state.Operation, now time.Time) error {
	if !s.Config.Cleanup.Enabled || !s.Config.Cleanup.ReplacementSearch {
		op.Reason = "replacement search permission disabled"
		return nil
	}
	if s.Store.Data.Status.ReadFailure != "" || s.hasPending(op.DownloadID) {
		return nil
	}
	var parent *state.Attempt
	for j := range s.Store.Data.Attempts {
		if s.Store.Data.Attempts[j].ID == op.RemovalID {
			parent = &s.Store.Data.Attempts[j]
			break
		}
	}
	if parent == nil || parent.Kind != "remove_unusable" || parent.Outcome != "verified" {
		op.Reason = "verified removal evidence unavailable"
		return nil
	}
	for _, a := range s.Store.Data.Attempts {
		if !a.DryRun && a.DownloadID == op.DownloadID && a.Kind == "search" {
			op.State = "finished"
			op.Reason = "search attempt already recorded"
			return nil
		}
	}
	s.Client.BeginCycle()
	queue, e := s.Client.Queue(ctx)
	if e != nil {
		s.Store.Data.Status.ReadFailure = e.Error()
		return nil
	}
	i := s.Store.Data.Issues[op.DownloadID]
	if i == nil {
		return errors.New("search operation has no original observation")
	}
	for _, r := range i.Rows {
		if !policy.ClientAllowed(s.Config, r.DownloadClient) {
			op.Reason = "original download client excluded"
			return nil
		}
	}
	targets := []int{}
	for _, eid := range op.EpisodeIDs {
		ep, f, p, e := s.current(ctx, eid)
		if e != nil {
			s.Store.Data.Status.ReadFailure = e.Error()
			return nil
		}
		if !policy.SeriesAllowed(s.Config, ep.SeriesID) {
			op.Reason = "affected series excluded"
			return nil
		}
		series, e := s.Client.Series(ctx, ep.SeriesID)
		if e != nil {
			s.Store.Data.Status.ReadFailure = e.Error()
			return nil
		}
		if series.Monitored == nil {
			op.Reason = "missing series monitored state"
			return nil
		}
		if *series.Monitored && *ep.Monitored && !policy.Acceptable(f, p) {
			targets = append(targets, eid)
		}
	}
	if len(targets) == 0 {
		op.State = "finished"
		op.Reason = "no monitored missing episodes require replacement"
		return nil
	}
	if policy.Competing(queue, "", targets) {
		op.Reason = "competing active download"
		return nil
	}
	sort.Ints(targets)
	d := policy.Decision{Kind: "search", Reason: "verified unusable removal; monitored episodes still missing acceptable files", EpisodeIDs: targets, SeriesIDs: parent.Decision.SeriesIDs, Fingerprint: parent.Fingerprint}
	if why := s.gate(op.DownloadID, d, s.Now(), op.RemovalID); why != "" {
		op.Reason = why
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	idx, e := s.reserve(i, d, s.Now(), nil)
	if e != nil {
		return e
	}
	a := &s.Store.Data.Attempts[idx]
	cmd, e := s.Client.Submit(ctx, sonarr.SearchCommand{Name: "EpisodeSearch", EpisodeIDs: targets})
	if e == nil {
		a.Ack = true
		a.CommandID = cmd.ID
	}
	s.response(a, e)
	op.State = "search-attempted"
	op.Reason = a.Reason
	return s.Store.Save()
}

// Errors contain only local classifications, never raw response bodies or credentials.
func IsAuthentication(e error) bool {
	return e != nil && strings.Contains(e.Error(), "authentication failure")
}
