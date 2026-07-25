package types

import "time"

// ─── Queue ───────────────────────────────────────────────────────────

type QueueItem struct {
	ID                    int             `json:"id"`
	SeriesID              int             `json:"seriesId"`
	EpisodeID             int             `json:"episodeId"`
	SeriesTitle           string          `json:"seriesTitle"`
	EpisodeTitle          string          `json:"episodeTitle"`
	Quality               QualityModel    `json:"quality"`
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

type StatusMessage struct {
	Title    string   `json:"title"`
	Messages []string `json:"messages"`
}

type QueueDetailItem struct {
	QueueItem
	Episode *EpisodeResource `json:"episode,omitempty"`
}

type QueueState struct {
	Item      QueueItem
	FirstSeen time.Time
	LastSeen  time.Time
}

// ─── History ─────────────────────────────────────────────────────────

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

type HistoryParams struct {
	Page          int    `json:"page"`
	PageSize      int    `json:"pageSize"`
	SortKey       string `json:"sortKey"`
	SortDirection string `json:"sortDirection"`
	EventType     int    `json:"eventType,omitempty"`
	SeriesID      int    `json:"seriesId,omitempty"`
	EpisodeID     int    `json:"episodeId,omitempty"`
}

// ─── Manual Import ───────────────────────────────────────────────────

type ManualImportRequest struct {
	Path         string        `json:"path"`
	SeriesID     int           `json:"seriesId"`
	SeasonNumber int           `json:"seasonNumber"`
	EpisodeID    int           `json:"episodeId"`
	Quality      QualityModel  `json:"quality"`
	Language     LanguageModel `json:"language"`
	DownloadID   string        `json:"downloadId"`
}

type QualityModel struct {
	Quality  Quality  `json:"quality"`
	Revision Revision `json:"revision"`
}

type Quality struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type Revision struct {
	Version int `json:"version"`
}

type LanguageModel struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// ─── Parse ───────────────────────────────────────────────────────────

type ParseResult struct {
	Title             string             `json:"title"`
	ParsedEpisodeInfo *ParsedEpisodeInfo `json:"parsedEpisodeInfo"`
	Series            *SeriesInfo        `json:"series"`
	Episodes          []EpisodeLookup    `json:"episodes"`
}

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

type SeriesInfo struct {
	Title  string `json:"title"`
	TVDBID int    `json:"tvdbId"`
	ImdbID string `json:"imdbId"`
}

type EpisodeLookup struct {
	ID            int    `json:"id"`
	EpisodeNumber int    `json:"episodeNumber"`
	SeasonNumber  int    `json:"seasonNumber"`
	Title         string `json:"title"`
}

// ─── Episode / File ──────────────────────────────────────────────────

type EpisodeResource struct {
	ID            int    `json:"id"`
	SeriesID      int    `json:"seriesId"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	Title         string `json:"title"`
	HasFile       bool   `json:"hasFile"`
	EpisodeFileID int    `json:"episodeFileId"`
}

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

type SeriesResource struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	TVDBID int    `json:"tvdbId"`
	Path   string `json:"path"`
}

// ─── Definitions (Fetched at Startup) ────────────────────────────────

type QualityDefinition struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Title  string `json:"title"`
	Weight int    `json:"weight"`
}

type Language struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// ─── Download Client ─────────────────────────────────────────────────

type DownloadClientResource struct {
	ID     int                   `json:"id"`
	Name   string                `json:"name"`
	Fields []DownloadClientField `json:"fields"`
}

type DownloadClientField struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

// ─── System ──────────────────────────────────────────────────────────

type SystemStatus struct {
	Version string `json:"version"`
}

// ─── Issue / Decision ────────────────────────────────────────────────

type IssueType string

const (
	IssueStuckDownload    IssueType = "stuck_download"
	IssueNotCustomFormat  IssueType = "not_custom_format_upgrade"
	IssueImportFailed     IssueType = "import_failed"
	IssueCleanupCandidate IssueType = "cleanup_candidate"
)

type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

type Issue struct {
	ID             string         `json:"id"`
	Type           IssueType      `json:"type"`
	Severity       Severity       `json:"severity"`
	QueueItem      QueueItem      `json:"queueItem"`
	RelatedHistory []HistoryItem  `json:"relatedHistory,omitempty"`
	Details        map[string]any `json:"details"`
	DetectedAt     time.Time      `json:"detectedAt"`
}

type Decision struct {
	Issue      Issue             `json:"issue"`
	Rule       SafetyRule        `json:"rule"`
	Action     Action            `json:"action"`
	Conditions []ConditionResult `json:"conditions"`
	Approved   bool              `json:"approved"`
	Reason     string            `json:"reason,omitempty"`
	Timestamp  time.Time         `json:"timestamp"`
	DryRun     bool              `json:"dryRun"`
}

type ConditionResult struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Passed   bool   `json:"passed"`
}

// ─── Safety Rules ────────────────────────────────────────────────────

type SafetyRule struct {
	ID          string      `json:"id"`
	Description string      `json:"description"`
	Conditions  []Condition `json:"conditions"`
	Action      Action      `json:"action"`
	Enabled     bool        `json:"enabled"`
}

type Condition struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}

type Action struct {
	Type   string            `json:"type"`
	Params map[string]string `json:"params"`
}

// ─── Import Suggestion ───────────────────────────────────────────────

type ImportSuggestion struct {
	ID                  string               `json:"id"`
	FilePath            string               `json:"filePath"`
	FileSize            int64                `json:"fileSize"`
	SeriesTitle         string               `json:"seriesTitle"`
	SeriesID            int                  `json:"seriesId"`
	SeasonNumber        int                  `json:"seasonNumber"`
	EpisodeNumbers      []int                `json:"episodeNumbers"`
	Confidence          int                  `json:"confidence"`
	ConfidenceBreakdown *ConfidenceBreakdown `json:"confidenceBreakdown"`
	MatchDetails        string               `json:"matchDetails"`
	CreatedAt           time.Time            `json:"createdAt"`
	Status              string               `json:"status"`
	DownloadID          string               `json:"downloadId"`
	IgnoreUntil         *time.Time           `json:"ignoreUntil,omitempty"`
}

type ConfidenceBreakdown struct {
	ParseValid    bool `json:"parseValid"`
	TVDBMatch     bool `json:"tvdbMatch"`
	SeasonMatch   bool `json:"seasonMatch"`
	EpisodeMatch  bool `json:"episodeMatch"`
	QualityKnown  bool `json:"qualityKnown"`
	LanguageKnown bool `json:"languageKnown"`
	Total         int  `json:"total"`
}

// ─── Decision Log ────────────────────────────────────────────────────

type DecisionLog struct {
	Timestamp           string            `json:"timestamp"`
	DecisionID          string            `json:"decision_id"`
	Item                DecisionLogItem   `json:"item"`
	Trigger             string            `json:"trigger"`
	ConditionsEvaluated []ConditionResult `json:"conditions_evaluated"`
	Action              string            `json:"action"`
	Executed            bool              `json:"executed"`
	DryRun              bool              `json:"dry_run"`
}

type DecisionLogItem struct {
	Type    string `json:"type"`
	ID      int    `json:"id"`
	Title   string `json:"title"`
	Series  string `json:"series"`
	Episode string `json:"episode"`
}

// ─── API Response Types ──────────────────────────────────────────────

type AgentStatus struct {
	Version   string `json:"version"`
	Uptime    string `json:"uptime"`
	DryRun    bool   `json:"dryRun"`
	SonarrUp  bool   `json:"sonarrUp"`
	StartTime string `json:"startTime"`
}

type AgentStats struct {
	RecoveredImports int `json:"recoveredImports"`
	DownloadsRemoved int `json:"downloadsRemoved"`
	RetriesPerformed int `json:"retriesPerformed"`
	PendingReview    int `json:"pendingReview"`
}

type NotificationEvent struct {
	Type    string         `json:"type"`
	Title   string         `json:"title"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}
