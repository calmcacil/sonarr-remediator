package state

import (
	"errors"
	"slices"

	"github.com/calmcacil/sonarr-remediator/internal/control"
)

func (d *Data) validateReceipts() error {
	for id, r := range d.Triggers {
		if r == nil || r.Request.ID != id || !r.Request.Execute || r.Request.ValidateStructure() != nil || r.Accepted.IsZero() || r.Report.Version != control.Version || r.Report.RequestID != id || (r.Status != "accepted" && r.Status != "finished" && r.Status != "interrupted") {
			return errors.New("corrupt manual receipt; refusing to reset safety history")
		}
		seen := map[int64]bool{}
		for _, aid := range r.AttemptIDs {
			if seen[aid] {
				return errors.New("duplicate manual attempt reference")
			}
			seen[aid] = true
			found := false
			for _, a := range d.Attempts {
				if a.ID == aid && a.RequestID == id && a.Source == "manual" {
					found = true
					break
				}
			}
			if !found {
				return errors.New("manual receipt references missing effect evidence")
			}
		}
	}
	for _, a := range d.Attempts {
		if a.RequestID != "" {
			r := d.Triggers[a.RequestID]
			if r == nil || a.Source != "manual" || !slices.Contains(r.AttemptIDs, a.ID) {
				return errors.New("manual attempt has no matching receipt")
			}
		}
	}
	return nil
}

func Effect(a Attempt) control.Effect {
	return control.Effect{AttemptID: a.ID, Action: a.Kind, Outcome: a.Outcome, Acknowledged: a.Ack, Reason: a.Reason, RemovalVerified: a.RemovalVerified, BlocklistVerified: a.BlocklistVerified}
}

// ReceiptReport projects current effect evidence without modifying the accepted
// request's report or maintaining a second outcome authority.
func (d *Data) ReceiptReport(receipt *Receipt) control.Report {
	report := receipt.Report
	report.Downloads = append([]string{}, report.Downloads...)
	report.Items = append([]control.Item{}, report.Items...)
	for j := range report.Items {
		item := &report.Items[j]
		item.Episodes = append([]int{}, item.Episodes...)
		item.Effects = []control.Effect{}
		for _, a := range d.Attempts {
			if a.RequestID == receipt.Request.ID && a.DownloadID == item.DownloadID {
				item.Effects = append(item.Effects, Effect(a))
				item.Disposition, item.Reason = a.Outcome, a.Reason
			}
		}
	}
	return report
}
