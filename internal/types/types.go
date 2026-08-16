// Package types defines the domain types shared across the agent.
// Shapes follow SPEC §5.3, §5.4 and §6.
package types

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// ─── Queue ───────────────────────────────────────────────────────────

// Page is Sonarr's paged-list envelope (GET /api/v3/queue, /api/v3/history):
// records are nested under "records" rather than served as a bare array.
type Page[T any] struct {
	Page         int `json:"page"`
	PageSize     int `json:"pageSize"`
	TotalRecords int `json:"totalRecords"`
	Records      []T `json:"records"`
}

// QueueItem is one entry in Sonarr's download queue.
type QueueItem struct {
	ID                    int             `json:"id"`
	SeriesID              int             `json:"seriesId"`
	EpisodeID             int             `json:"episodeId"`
	SeriesTitle           string          `json:"seriesTitle"` // not present on Sonarr v4 queue payloads; kept for tests and non-v4 compatibility
	EpisodeTitle          string          `json:"episodeTitle"`
	Title                 string          `json:"title"` // release title (Sonarr v4)
	Quality               QualityModel    `json:"quality"`
	CustomFormats         []CustomFormat  `json:"customFormats"`
	CustomFormatScore     int             `json:"customFormatScore"`
	Size                  int64           `json:"size"`
	Status                string          `json:"status"`                // queued|paused|downloading|completed|warning|failed
	TrackedDownloadStatus string          `json:"trackedDownloadStatus"` // ok|warning|error
	TrackedDownloadState  string          `json:"trackedDownloadState"`  // downloading|importPending|importing|imported|importFailed|downloadFailed
	StatusMessages        []StatusMessage `json:"statusMessages"`
	ErrorMessage          string          `json:"errorMessage"`
	DownloadID            string          `json:"downloadId"`
	OutputPath            string          `json:"outputPath"`
	Added                 time.Time       `json:"added"`
}

// CustomFormat is a named Sonarr custom format applied to a release.
type CustomFormat struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// CustomFormatNames returns the names of the release's custom formats.
func (q QueueItem) CustomFormatNames() []string {
	out := make([]string, 0, len(q.CustomFormats))
	for _, cf := range q.CustomFormats {
		out = append(out, cf.Name)
	}
	return out
}

// StatusMessage is a named list of human-readable status strings on a queue item.
type StatusMessage struct {
	Title    string   `json:"title"`
	Messages []string `json:"messages"`
}

// CompositeKey returns the dedup/cross-reference key seriesId:episodeId:downloadId.
func (q QueueItem) CompositeKey() string {
	return itoa(q.SeriesID) + ":" + itoa(q.EpisodeID) + ":" + q.DownloadID
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ─── History ─────────────────────────────────────────────────────────

// HistoryItem is one entry in Sonarr's history.
type HistoryItem struct {
	ID          int               `json:"id"`
	SeriesID    int               `json:"seriesId"`
	EpisodeID   int               `json:"episodeId"`
	SourceTitle string            `json:"sourceTitle"`
	EventType   string            `json:"eventType"` // "grabbed"|"downloadFolderImported"|"downloadFailedImport"|"downloadIgnored"|"episodeFileDeleted"
	Quality     QualityModel      `json:"quality"`
	Date        time.Time         `json:"date"`
	Data        map[string]string `json:"data"`
}

// HistoryParams filters the history query. EventType is an int because
// Sonarr's API query parameter expects an integer; HistoryItem.EventType
// is a string because that is what the API response returns.
type HistoryParams struct {
	Page          int    `json:"page"`
	PageSize      int    `json:"pageSize"`
	SortKey       string `json:"sortKey"`
	SortDirection string `json:"sortDirection"`
	EventType     int    `json:"eventType,omitempty"` // 1=grabbed, 3=imported, 4=failedImport, 7=ignored
	SeriesID      int    `json:"seriesId,omitempty"`
	EpisodeID     int    `json:"episodeId,omitempty"`
}

// ─── Manual Import ───────────────────────────────────────────────────

// ManualImportCommand is the POST /api/v3/command body for the ManualImport
// command — the actual import step of the manual-import flow (SPEC §12).
// The reprocess POST (/api/v3/manualimport) only evaluates a file and
// returns a verdict; the command performs the import and completes the
// tracked download, which removes the queue item.
type ManualImportCommand struct {
	Name       string                    `json:"name"`       // "ManualImport"
	ImportMode string                    `json:"importMode"` // "auto"
	Files      []ManualImportCommandFile `json:"files"`
}

// ManualImportCommandFile is one file inside a ManualImportCommand.
// EpisodeIDs carries the episodes to import for the file; quality and
// languages come from Sonarr's own manual-import preview.
type ManualImportCommandFile struct {
	Path       string          `json:"path"`
	SeriesID   int             `json:"seriesId"`
	EpisodeIDs []int           `json:"episodeIds"`
	Quality    QualityModel    `json:"quality"`
	Languages  []LanguageModel `json:"languages"`
	DownloadID string          `json:"downloadId"`
}

// QualityModel carries a quality choice with its revision.
type QualityModel struct {
	Quality  Quality  `json:"quality"`
	Revision Revision `json:"revision"`
}

// Quality identifies a quality by id and name.
type Quality struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Revision carries the release revision version.
type Revision struct {
	Version int `json:"version"`
}

// LanguageModel identifies a language by id and name.
type LanguageModel struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// ─── Manual Import ───────────────────────────────────────────────────

// EpisodeLookup is one episode in the manual-import preview's episodes array.
type EpisodeLookup struct {
	ID            int    `json:"id"`
	EpisodeNumber int    `json:"episodeNumber"`
	SeasonNumber  int    `json:"seasonNumber"`
	Title         string `json:"title"`
}

// ImportRejection is Sonarr's reason a manual-import file cannot be imported.
type ImportRejection struct {
	Reason string `json:"reason"`
	Type   string `json:"type"` // permanent|temporary
}

// ManualImportFile is one file in the GET /api/v3/manualimport preview (and
// also the shape of one processed item in the evaluate-only POST
// /api/v3/manualimport response). Sonarr performs its own parsing and
// series/episode matching — anchored by the downloadId (SPEC §12) — and
// reports the outcome as rejections; the agent selects the file and submits
// it through the ManualImport command (SPEC §3.2).
type ManualImportFile struct {
	ID                int               `json:"id"`
	Path              string            `json:"path"`
	RelativePath      string            `json:"relativePath"`
	Name              string            `json:"name"`
	Size              int64             `json:"size"`
	ReleaseGroup      string            `json:"releaseGroup"`
	Quality           QualityModel      `json:"quality"`
	Languages         []LanguageModel   `json:"languages"`
	SeasonNumber      *int              `json:"seasonNumber"`
	Episodes          []EpisodeLookup   `json:"episodes"`
	Rejections        []ImportRejection `json:"rejections"`
	CustomFormatScore int               `json:"customFormatScore"`
}

// ─── Episode / File ──────────────────────────────────────────────────

// EpisodeResource is GET /api/v3/episode/{id}.
type EpisodeResource struct {
	ID            int    `json:"id"`
	SeriesID      int    `json:"seriesId"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	Title         string `json:"title"`
	HasFile       bool   `json:"hasFile"`
	EpisodeFileID int    `json:"episodeFileId"`
}

// EpisodeFileResource is GET /api/v3/episodefile/{id}.
// Sonarr's response does not include a quality ID; the pre-import check maps
// the quality name to a QualityDefinition to obtain a weight.
type EpisodeFileResource struct {
	ID                  int         `json:"id"`
	SeriesID            int         `json:"seriesId"`
	SeasonNumber        int         `json:"seasonNumber"`
	EpisodeNumber       int         `json:"episodeNumber"`
	RelativePath        string      `json:"relativePath"`
	Quality             QualityName `json:"quality"`
	QualityCutoffNotMet bool        `json:"qualityCutoffNotMet"`
	CustomFormatScore   int         `json:"customFormatScore"`
	Size                int64       `json:"size"`
}

// QualityName accepts either a plain quality name string or Sonarr's quality
// object ({quality:{name:...},revision:{...}}), normalizing to the name.
// Episode files are served the object form; the string form is tolerated for
// older payloads and test fixtures (SPEC §12).
type QualityName string

// UnmarshalJSON decodes a string verbatim or extracts the name from a
// quality object.
func (q *QualityName) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*q = QualityName(s)
		return nil
	}
	var qm QualityModel
	if err := json.Unmarshal(b, &qm); err == nil {
		*q = QualityName(qm.Quality.Name)
		return nil
	}
	return fmt.Errorf("cannot unmarshal %s into QualityName", b)
}

// ─── Series ──────────────────────────────────────────────────────────

// SeriesResource is GET /api/v3/series/{id}.
type SeriesResource struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	TVDBID int    `json:"tvdbId"`
	Path   string `json:"path"`
}

// ─── Definitions (fetched at startup) ────────────────────────────────

// QualityDefinition is one entry of GET /api/v3/qualitydefinition.
// Weight is Sonarr's internal ranking; higher = better.
type QualityDefinition struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Title  string `json:"title"`
	Weight int    `json:"weight"`
}

// Language is one entry of GET /api/v3/language.
type Language struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// ─── Download Client (for root folder discovery) ─────────────────────

// DownloadClientResource is one entry of GET /api/v3/downloadclient.
type DownloadClientResource struct {
	ID     int                   `json:"id"`
	Name   string                `json:"name"`
	Fields []DownloadClientField `json:"fields"`
}

// DownloadClientField is a name/value config field of a download client.
// Root paths live in fields named "downloadFolder" or "tvDownloadFolder".
type DownloadClientField struct {
	Name  string     `json:"name"`
	Value FlexString `json:"value"`
}

// FlexString accepts string, number, boolean, or null JSON values. Sonarr
// download client fields are heterogeneous (e.g. a port is a number), while
// the root-path fields the agent reads are strings; this keeps the common
// case typed while tolerating the rest (SPEC §12).
type FlexString string

// UnmarshalJSON accepts strings verbatim and stringifies numbers, booleans,
// and null (null becomes empty string).
func (s *FlexString) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*s = ""
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*s = FlexString(str)
		return nil
	}
	var num json.Number
	if err := json.Unmarshal(b, &num); err == nil {
		*s = FlexString(num.String())
		return nil
	}
	var boolean bool
	if err := json.Unmarshal(b, &boolean); err == nil {
		*s = FlexString(strconv.FormatBool(boolean))
		return nil
	}
	return fmt.Errorf("cannot unmarshal %s into FlexString", b)
}

// ─── System ──────────────────────────────────────────────────────────

// SystemStatus is GET /api/v3/system/status.
type SystemStatus struct {
	Version string `json:"version"` // e.g. "4.0.0.741"
}

// ─── Issues & Decisions ──────────────────────────────────────────────

// ActionType names the actions the executor can perform.
type ActionType string

const (
	ActionLogOnly      ActionType = "log_only"
	ActionRemoveQueue  ActionType = "remove_queue"
	ActionRetry        ActionType = "retry"
	ActionManualImport ActionType = "manual_import"
	ActionReconcile    ActionType = "reconcile"
)

// IssueType identifies the detector that produced an issue.
type IssueType string

const (
	IssueStuckDownload   IssueType = "stuck_download"
	IssueNotCustomFormat IssueType = "not_custom_format_upgrade"
	IssueTorrentError    IssueType = "torrent_client_error"
	IssueUnknownSeries   IssueType = "unknown_series"
	IssueImportFailed    IssueType = "import_failed"
	IssueReconcile       IssueType = "reconcile"
)

// Severity ranks issues.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Issue is a detected problem for one queue item.
type Issue struct {
	ID             string         `json:"id"`
	Type           IssueType      `json:"type"`
	Severity       Severity       `json:"severity"`
	QueueItem      QueueItem      `json:"queueItem"`
	RelatedHistory []HistoryItem  `json:"relatedHistory,omitempty"`
	Details        map[string]any `json:"details"`
	DetectedAt     time.Time      `json:"detectedAt"`
}

// ActionTypeFor maps an issue type to its default action.
func (i Issue) ActionTypeFor() ActionType {
	switch i.Type {
	case IssueStuckDownload, IssueNotCustomFormat, IssueTorrentError, IssueUnknownSeries:
		return ActionRemoveQueue
	case IssueImportFailed:
		return ActionManualImport
	case IssueReconcile:
		return ActionReconcile
	default:
		return ActionLogOnly
	}
}

// Priority ranks issue types for conflict resolution, most conservative first.
// Lower number = higher priority. See SPEC §3.7.
func (t IssueType) Priority() int {
	switch t {
	case IssueStuckDownload:
		return 2
	case IssueNotCustomFormat:
		return 2
	case IssueTorrentError:
		return 2
	case IssueUnknownSeries:
		return 2
	case IssueReconcile:
		return 2
	case IssueImportFailed:
		return 4
	default:
		return 1 // log_only
	}
}

// CheckResult records one safety check evaluation for the decision log.
type CheckResult struct {
	Check    string `json:"check"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Passed   bool   `json:"passed"`
}

// Decision is the outcome of the safety engine for one issue.
type Decision struct {
	Issue     Issue         `json:"issue"`
	Action    ActionType    `json:"action"`
	Checks    []CheckResult `json:"checks"`
	Approved  bool          `json:"approved"`
	Reason    string        `json:"reason,omitempty"`
	Timestamp time.Time     `json:"timestamp"`
	DryRun    bool          `json:"dryRun"`
}

// DetailsReconcilePlan is the Issue.Details key carrying a ReconcilePlan
// (SPEC §3.2): the executor reads it to execute the plan's winner import and
// discard removals.
const DetailsReconcilePlan = "reconcile_plan"

// ReconcilePlan is the episode-level reconciliation outcome (SPEC §3.2): the
// winner release to import and every other targeted release of the same
// episode, marked for discard. Produced by selector.Reconcile from the poll's
// targeted hits and carried to the executor inside Issue.Details.
//
// Invariants: Winner is one of the input releases; every Discard shares the
// Winner's series:episode pair; a plan with no discards is a single-release
// episode whose winner still needs the import-or-remove decision.
type ReconcilePlan struct {
	SeriesID  int         `json:"seriesId"`
	EpisodeID int         `json:"episodeId"`
	Winner    QueueItem   `json:"winner"`
	Discards  []QueueItem `json:"discards"`
}

// EpisodeKey returns the stable seriesId:episodeId identifier.
func (p ReconcilePlan) EpisodeKey() string {
	return strconv.Itoa(p.SeriesID) + ":" + strconv.Itoa(p.EpisodeID)
}
