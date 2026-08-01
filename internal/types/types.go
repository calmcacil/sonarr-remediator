// Package types defines the domain types shared across the agent.
// Shapes follow SPEC §5.3, §5.4 and §6.
package types

import "time"

// ─── Queue ───────────────────────────────────────────────────────────

// QueueItem is one entry in Sonarr's download queue.
type QueueItem struct {
	ID                    int             `json:"id"`
	SeriesID              int             `json:"seriesId"`
	EpisodeID             int             `json:"episodeId"`
	SeriesTitle           string          `json:"seriesTitle"`
	EpisodeTitle          string          `json:"episodeTitle"`
	Quality               string          `json:"quality"`
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
	Quality     string            `json:"quality"`
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

// ManualImportRequest is the body for POST /api/v3/manualimport.
// ImportMode is intentionally not included; Sonarr uses its configured default.
type ManualImportRequest struct {
	Path         string        `json:"path"`
	SeriesID     int           `json:"seriesId"`
	SeasonNumber int           `json:"seasonNumber"`
	EpisodeID    int           `json:"episodeId"` // single episode per call; multiple calls for multi-ep files
	Quality      QualityModel  `json:"quality"`
	Language     LanguageModel `json:"language"`
	DownloadID   string        `json:"downloadId"`
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

// ─── Parse ───────────────────────────────────────────────────────────

// ParseResult is the response of GET /api/v3/parse.
type ParseResult struct {
	Title             string             `json:"title"`
	ParsedEpisodeInfo *ParsedEpisodeInfo `json:"parsedEpisodeInfo"`
	Series            *SeriesInfo        `json:"series"`
	Episodes          []EpisodeLookup    `json:"episodes"`
}

// ParsedEpisodeInfo is the parsed metadata inside a ParseResult.
type ParsedEpisodeInfo struct {
	ReleaseTitle           string        `json:"releaseTitle"`
	SeriesTitle            string        `json:"seriesTitle"`
	SeasonNumber           int           `json:"seasonNumber"`
	EpisodeNumbers         []int         `json:"episodeNumbers"`
	AbsoluteEpisodeNumbers []int         `json:"absoluteEpisodeNumbers"`
	FullSeason             bool          `json:"fullSeason"`
	Quality                QualityModel  `json:"quality"`
	Language               LanguageModel `json:"language"`
}

// SeriesInfo is the matched series inside a ParseResult.
type SeriesInfo struct {
	Title  string `json:"title"`
	TVDBID int    `json:"tvdbId"`
	ImdbID string `json:"imdbId"`
}

// EpisodeLookup is one episode in the parsed result's episodes array.
type EpisodeLookup struct {
	ID            int    `json:"id"`
	EpisodeNumber int    `json:"episodeNumber"`
	SeasonNumber  int    `json:"seasonNumber"`
	Title         string `json:"title"`
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
	ID                  int    `json:"id"`
	SeriesID            int    `json:"seriesId"`
	SeasonNumber        int    `json:"seasonNumber"`
	EpisodeNumber       int    `json:"episodeNumber"`
	RelativePath        string `json:"relativePath"`
	Quality             string `json:"quality"`
	QualityCutoffNotMet bool   `json:"qualityCutoffNotMet"`
	Size                int64  `json:"size"`
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
	Name  string `json:"name"`
	Value string `json:"value"`
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
)

// IssueType identifies the detector that produced an issue.
type IssueType string

const (
	IssueStuckDownload   IssueType = "stuck_download"
	IssueNotCustomFormat IssueType = "not_custom_format_upgrade"
	IssueImportFailed    IssueType = "import_failed"
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
	case IssueStuckDownload:
		return ActionRemoveQueue
	case IssueNotCustomFormat:
		return ActionRemoveQueue
	case IssueImportFailed:
		return ActionManualImport
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
