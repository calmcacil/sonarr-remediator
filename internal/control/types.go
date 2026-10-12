package control

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"time"
)

const (
	Version      = 1
	MaxRequest   = 16 << 10
	MaxResponse  = 16 << 20
	MaxDownloads = 1000
	MaxTimeout   = 2 * time.Minute
)

type Request struct {
	Version      int       `json:"schema_version"`
	ID           string    `json:"request_id,omitempty"`
	Execute      bool      `json:"execute"`
	OverrideWait bool      `json:"override_wait"`
	Downloads    []string  `json:"download_ids"`
	Issued       time.Time `json:"issued"`
	Expires      time.Time `json:"expires"`
}

func NewRequest(execute, override bool, ids []string, timeout time.Duration) (Request, error) {
	r := Request{Version: Version, Execute: execute, OverrideWait: override, Downloads: ids, Issued: time.Now()}
	r.Expires = r.Issued.Add(timeout)
	if execute {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return r, err
		}
		r.ID = hex.EncodeToString(b[:])
	}
	return r, r.Validate(time.Now())
}

func (r *Request) Validate(now time.Time) error {
	if err := r.ValidateStructure(); err != nil {
		return err
	}
	if r.Issued.After(now.Add(time.Second)) || !r.Expires.After(now) {
		return errors.New("expired or invalid request timeout; maximum is two minutes")
	}
	return nil
}

func (r *Request) ValidateStructure() error {
	if r.Version != Version {
		return errors.New("unsupported manual control schema")
	}
	if r.Execute {
		b, err := hex.DecodeString(r.ID)
		if err != nil || len(b) != 16 {
			return errors.New("invalid request identifier")
		}
	} else if r.ID != "" {
		return errors.New("preview cannot have a live request identifier")
	}
	if r.Issued.IsZero() || !r.Expires.After(r.Issued) || r.Expires.Sub(r.Issued) > MaxTimeout {
		return errors.New("expired or invalid request timeout; maximum is two minutes")
	}
	ids := append([]string{}, r.Downloads...)
	for _, id := range ids {
		if id == "" {
			return errors.New("download IDs must not be empty")
		}
	}
	slices.Sort(ids)
	r.Downloads = slices.Compact(ids)
	if len(r.Downloads) > MaxDownloads {
		return errors.New("selection exceeds 1000 downloads; narrow the download IDs")
	}
	return nil
}

type Permissions struct {
	Recovery          bool `json:"recovery"`
	Cleanup           bool `json:"cleanup"`
	Blocklist         bool `json:"blocklist"`
	ReplacementSearch bool `json:"replacement_search"`
}
type Budget struct {
	PerCycle  int `json:"per_cycle"`
	HourLimit int `json:"hour_limit"`
	HourUsed  int `json:"hour_used"`
	Available int `json:"available"`
}
type Effect struct {
	AttemptID         int64  `json:"attempt_id"`
	Action            string `json:"action"`
	Outcome           string `json:"outcome"`
	Acknowledged      bool   `json:"acknowledged"`
	Reason            string `json:"reason"`
	RemovalVerified   bool   `json:"removal_verified"`
	BlocklistVerified bool   `json:"blocklist_verified"`
}
type Item struct {
	DownloadID         string        `json:"download_id"`
	QueueRows          int           `json:"queue_rows"`
	Action             string        `json:"policy_action"`
	Disposition        string        `json:"disposition"`
	Reason             string        `json:"reason"`
	Episodes           []int         `json:"episode_ids"`
	PreviewFiles       int           `json:"preview_file_count"`
	MembershipComplete bool          `json:"membership_complete"`
	Wait               time.Duration `json:"configured_wait_ns"`
	Observed           time.Duration `json:"observed_progress_ns"`
	Remaining          time.Duration `json:"remaining_wait_ns"`
	WaitBypassed       bool          `json:"wait_bypassed"`
	ClientDeletion     bool          `json:"client_deletion_can_delete_data"`
	Blocklist          bool          `json:"blocklist"`
	ReplacementSearch  bool          `json:"replacement_search"`
	Effects            []Effect      `json:"effects"`
}
type Report struct {
	Source           string      `json:"source"`
	Version          int         `json:"schema_version"`
	RequestID        string      `json:"request_id,omitempty"`
	StatePath        string      `json:"state_path,omitempty"`
	Identity         string      `json:"sonarr_identity,omitempty"`
	Mode             string      `json:"mode"`
	BackgroundDryRun bool        `json:"background_dry_run"`
	OverrideWait     bool        `json:"override_wait"`
	Downloads        []string    `json:"download_ids"`
	Permissions      Permissions `json:"permissions"`
	Budget           Budget      `json:"budget"`
	QueueRows        int         `json:"queue_rows"`
	SharedDownloads  int         `json:"shared_downloads"`
	Items            []Item      `json:"items"`
	ExitCode         int         `json:"exit_code"`
	Error            string      `json:"error,omitempty"`
}

func Failure(r Request, code int, reason string) Report {
	mode := "preview"
	if r.Execute {
		mode = "execute"
	}
	return Report{Source: "manual", Version: Version, RequestID: r.ID, Mode: mode, OverrideWait: r.OverrideWait, Downloads: r.Downloads, ExitCode: code, Error: reason, Items: []Item{}}
}
