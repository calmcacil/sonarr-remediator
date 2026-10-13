package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/control"
	"github.com/calmcacil/sonarr-remediator/internal/policy"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/state"
)

type manualCall struct {
	ctx     context.Context
	request control.Request
	result  chan control.Report
}
type plannedAction struct {
	issue     *state.Issue
	decision  policy.Decision
	operation *state.Operation
}

// Request transfers ownership to Run; handlers never inspect the mutable store.
func (s *Service) Request(ctx context.Context, req control.Request) control.Report {
	select {
	case s.admission <- struct{}{}:
	default:
		return control.Failure(req, 1, "busy: another manual request is executing or waiting")
	}
	call := manualCall{ctx: ctx, request: req, result: make(chan control.Report, 1)}
	select {
	case s.requests <- call:
	case <-ctx.Done():
		<-s.admission
		return control.Failure(req, 1, "manual request cancelled before dispatch")
	}
	select {
	case result := <-call.result:
		return result
	case <-ctx.Done():
		code := 1
		if req.Execute {
			code = 3
		}
		return control.Failure(req, code, "manual request cancelled; inspect triggers before another execution")
	}
}

func (s *Service) save() error {
	if s.request != nil && !s.request.Execute {
		return nil
	}
	return s.Store.Save()
}
func (s *Service) selected(id string) bool {
	return s.request == nil || len(s.request.Downloads) == 0 || slices.Contains(s.request.Downloads, id)
}

// Manual must run on the observer loop. A preview owns a deep projection and a
// GET-only client; neither aliases the live state nor advances its clock.
func (s *Service) Manual(ctx context.Context, req control.Request) control.Report {
	if err := req.ValidateStructure(); err != nil {
		return control.Failure(req, 1, err.Error())
	}
	if req.Execute {
		if receipt := s.Store.Data.Triggers[req.ID]; receipt != nil {
			if !sameRequest(receipt.Request, req) {
				return control.Failure(req, 1, "request identifier already accepted with different input")
			}
			return s.Store.Data.ReceiptReport(receipt)
		}
	}
	if err := req.Validate(time.Now()); err != nil {
		return control.Failure(req, 1, err.Error())
	}
	if ctx.Err() != nil {
		return control.Failure(req, 1, "manual request cancelled before evaluation")
	}
	ctx, cancel := context.WithDeadline(ctx, req.Expires)
	defer cancel()
	worker := *s
	worker.request = &req
	report := control.Failure(req, 2, "")
	report.BackgroundDryRun = s.Config.DryRun
	report.Permissions = control.Permissions{Recovery: s.Config.Recovery.Enabled, Cleanup: s.Config.Cleanup.Enabled, Blocklist: s.Config.Cleanup.Blocklist, ReplacementSearch: s.Config.Cleanup.ReplacementSearch}
	worker.report = &report
	worker.Client = s.Client.ForCycle(!req.Execute)
	if !req.Execute {
		body, err := json.Marshal(s.Store.Data)
		if err != nil {
			return control.Failure(req, 1, "cannot isolate preview state")
		}
		projection := &state.Store{}
		if err = json.Unmarshal(body, &projection.Data); err != nil {
			return control.Failure(req, 1, "cannot isolate preview state")
		}
		worker.Store = projection
	} else {
		if s.Store.Data.Triggers == nil {
			s.Store.Data.Triggers = map[string]*state.Receipt{}
		}
		s.Store.Data.Triggers[req.ID] = &state.Receipt{Request: req, Accepted: s.Now(), Status: "accepted", Report: report, AttemptIDs: []int64{}}
		if err := s.Store.Save(); err != nil {
			delete(s.Store.Data.Triggers, req.ID)
			return control.Failure(req, 1, err.Error())
		}
	}
	err := worker.Cycle(ctx)
	if sizeErr := worker.checkReportSize(); sizeErr != nil {
		err = errors.Join(err, sizeErr)
	}
	if req.Execute {
		s.previous = worker.previous
	}
	uncertain := false
	for _, item := range report.Items {
		if !req.Execute && strings.HasPrefix(item.Disposition, "would_") {
			report.ExitCode = 0
		}
		for _, effect := range item.Effects {
			if effect.Acknowledged {
				report.ExitCode = 0
			}
			if effect.Outcome == "uncertain" || effect.Outcome == "intent" {
				uncertain = true
			}
		}
	}
	if uncertain {
		report.ExitCode = 3
	}
	if err != nil || ctx.Err() != nil {
		report.ExitCode = 1
		if err != nil {
			report.Error = err.Error()
		} else {
			report.Error = "manual request cancelled or deadline exhausted"
		}
		if req.Execute && len(worker.Store.Data.Triggers[req.ID].AttemptIDs) > 0 {
			report.ExitCode = 3
		}
	}
	if req.Execute {
		receipt := s.Store.Data.Triggers[req.ID]
		receipt.Report, receipt.Status, receipt.Finished = report, "finished", s.Now()
		if err := s.Store.Save(); err != nil {
			report.ExitCode, report.Error = 3, err.Error()+"; inspect triggers before another execution"
		}
	}
	return report
}

func sameRequest(a, b control.Request) bool {
	return a.Version == b.Version && a.Execute == b.Execute && a.OverrideWait == b.OverrideWait && slices.Equal(a.Downloads, b.Downloads) && a.Issued.Equal(b.Issued) && a.Expires.Equal(b.Expires)
}

func (s *Service) initializeReport(queue []sonarr.QueueRow, groups map[string][]sonarr.QueueRow) error {
	ids := append([]string{}, s.request.Downloads...)
	if len(ids) == 0 {
		for id := range groups {
			ids = append(ids, id)
		}
		for id, op := range s.Store.Data.Operations {
			if op.State == "pending-search" && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) > control.MaxDownloads {
		return errors.New("selection exceeds 1000 downloads; narrow the download IDs")
	}
	slices.Sort(ids)
	s.report.QueueRows, s.report.SharedDownloads = len(queue), len(groups)
	s.report.Items = []control.Item{}
	used := s.Store.Data.Budget(s.Now())
	s.report.Budget = control.Budget{PerCycle: s.Config.Limits.MutationsPerScan, HourLimit: s.Config.Limits.MutationsPerHour, HourUsed: used, Available: max(0, min(s.Config.Limits.MutationsPerScan, s.Config.Limits.MutationsPerHour-used))}
	for _, id := range ids {
		rows := groups[id]
		item := control.Item{DownloadID: id, QueueRows: len(rows), Action: "defer", Disposition: "blocked", Reason: "protected or noneligible tracked state", Episodes: []int{}, Effects: []control.Effect{}}
		item.Wait = s.Config.Recovery.Wait
		if issue := s.Store.Data.Issues[id]; issue != nil {
			item.Observed = issue.Elapsed
		}
		item.Remaining = max(0, item.Wait-item.Observed)
		for _, row := range rows {
			if row.EpisodeID != nil && *row.EpisodeID > 0 && !slices.Contains(item.Episodes, *row.EpisodeID) {
				item.Episodes = append(item.Episodes, *row.EpisodeID)
			}
		}
		slices.Sort(item.Episodes)
		if len(rows) == 0 {
			item.Disposition, item.Reason = "not_present", "download absent from complete queue and pending search continuations"
			if op := s.Store.Data.Operations[id]; op != nil && op.State == "pending-search" {
				item.Disposition, item.Reason, item.Episodes = "blocked", op.Reason, op.EpisodeIDs
			}
		}
		s.report.Items = append(s.report.Items, item)
	}
	return nil
}
func (s *Service) reportItem(id string) *control.Item {
	if s.report == nil {
		return nil
	}
	for j := range s.report.Items {
		if s.report.Items[j].DownloadID == id {
			return &s.report.Items[j]
		}
	}
	return nil
}
func (s *Service) itemBlocked(id, disposition, reason string) {
	if item := s.reportItem(id); item != nil {
		item.Disposition, item.Reason = disposition, reason
	}
}
func (s *Service) previewReason(o policy.Observation, reason string) string {
	if len(o.Preview) == 0 {
		return "empty or unavailable manual-import preview"
	}
	if !o.MembershipComplete || len(o.Intended) == 0 {
		return "incomplete intended episode membership"
	}
	return reason
}
func (s *Service) describe(i *state.Issue, o policy.Observation, d policy.Decision) {
	item := s.reportItem(i.DownloadID)
	if item == nil {
		return
	}
	item.Action, item.Episodes, item.PreviewFiles, item.MembershipComplete = d.Kind, d.EpisodeIDs, len(o.Preview), o.MembershipComplete
	item.Wait = s.Config.Recovery.Wait
	if d.Kind != "import" {
		item.Wait = s.cleanupWait(d)
	}
	item.Observed, item.Remaining = i.Elapsed, max(0, item.Wait-i.Elapsed)
	if d.Kind != "import" {
		item.Observed = cleanupElapsed(i)
		item.Remaining = max(0, item.Wait-item.Observed)
	}
	item.WaitBypassed = s.request.OverrideWait && item.Remaining > 0 && d.Kind != "defer"
	item.ClientDeletion = d.Kind == "remove_redundant" || d.Kind == "remove_unusable" || d.Kind == "remove_blocked"
	item.Blocklist = d.Blocklist
	item.ReplacementSearch = d.Kind == "remove_unusable" && s.Config.Cleanup.ReplacementSearch
}
func (s *Service) planGate(d policy.Decision, plans []plannedAction, now time.Time) string {
	if len(plans) >= s.Config.Limits.MutationsPerScan || len(plans)+s.Store.Data.Budget(now) >= s.Config.Limits.MutationsPerHour {
		return "cycle or rolling hour mutation allowance allocated"
	}
	for _, plan := range plans {
		for _, eid := range d.EpisodeIDs {
			if slices.Contains(plan.decision.EpisodeIDs, eid) {
				return "another selected action protects this episode"
			}
		}
	}
	return ""
}
func (s *Service) checkReportSize() error {
	if s.report == nil {
		return nil
	}
	body, err := json.Marshal(s.report)
	if err != nil || len(body) > control.MaxResponse-(1<<20) {
		s.report.Items = []control.Item{}
		return errors.New("manual report exceeds limit; narrow the download IDs before execution")
	}
	return nil
}
func (s *Service) recordEffects(id string, before int) {
	item := s.reportItem(id)
	if item == nil {
		return
	}
	for _, a := range s.Store.Data.Attempts[before:] {
		item.Effects = append(item.Effects, state.Effect(a))
		item.Disposition, item.Reason = a.Outcome, a.Reason
	}
}
