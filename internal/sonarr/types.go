package sonarr

import "time"

type QueueRow struct {
	ID             int             `json:"id"`
	DownloadID     string          `json:"downloadId"`
	SeriesID       *int            `json:"seriesId"`
	EpisodeID      *int            `json:"episodeId"`
	DownloadClient string          `json:"downloadClient"`
	Title          string          `json:"title"`
	OutputPath     string          `json:"outputPath"`
	Status         string          `json:"status"`
	TrackedState   string          `json:"trackedDownloadState"`
	TrackedStatus  string          `json:"trackedDownloadStatus"`
	Added          *time.Time      `json:"added"`
	StatusMessages []StatusMessage `json:"statusMessages"`
}

type StatusMessage struct {
	Title    string   `json:"title"`
	Messages []string `json:"messages"`
}

func (q QueueRow) Eligible() bool {
	return q.DownloadID != "" && q.Status == "completed" && (q.TrackedState == "importBlocked" || q.TrackedState == "importFailed" || q.TrackedState == "importPending")
}

type NamedID struct {
	ID   int    `json:"id"`
	Name string `json:"name,omitempty"`
}
type Revision struct {
	Version  *int  `json:"version"`
	Real     *int  `json:"real"`
	IsRepack *bool `json:"isRepack"`
}
type Quality struct {
	Quality  *NamedID  `json:"quality"`
	Revision *Revision `json:"revision"`
}
type Series struct {
	ID               int    `json:"id"`
	QualityProfileID int    `json:"qualityProfileId"`
	Monitored        *bool  `json:"monitored"`
	SeriesType       string `json:"seriesType"`
}
type Episode struct {
	ID            int   `json:"id"`
	SeriesID      int   `json:"seriesId"`
	EpisodeFileID *int  `json:"episodeFileId"`
	HasFile       *bool `json:"hasFile"`
	Monitored     *bool `json:"monitored"`
}
type EpisodeFile struct {
	ID       int       `json:"id"`
	SeriesID int       `json:"seriesId"`
	Path     string    `json:"path"`
	Quality  Quality   `json:"quality"`
	Score    *int      `json:"customFormatScore"`
	Formats  []NamedID `json:"customFormats"`
}
type Rejection struct {
	Reason string `json:"reason"`
	Type   string `json:"type"`
}
type Preview struct {
	ID           int         `json:"id"`
	Path         string      `json:"path"`
	FolderName   string      `json:"folderName"`
	DownloadID   string      `json:"downloadId"`
	Series       *Series     `json:"series"`
	Episodes     []Episode   `json:"episodes"`
	Quality      Quality     `json:"quality"`
	Score        *int        `json:"customFormatScore"`
	Formats      []NamedID   `json:"customFormats"`
	Rejections   []Rejection `json:"rejections"`
	Languages    []NamedID   `json:"languages"`
	ReleaseGroup string      `json:"releaseGroup"`
	IndexerFlags *int        `json:"indexerFlags"`
	ReleaseType  string      `json:"releaseType"`
}
type ProfileItem struct {
	ID      int           `json:"id"`
	Quality *NamedID      `json:"quality"`
	Allowed *bool         `json:"allowed"`
	Items   []ProfileItem `json:"items"`
}
type FormatItem struct {
	Format int  `json:"format"`
	Score  *int `json:"score"`
}
type Profile struct {
	ID              int           `json:"id"`
	UpgradeAllowed  *bool         `json:"upgradeAllowed"`
	Cutoff          *int          `json:"cutoff"`
	CutoffScore     *int          `json:"cutoffFormatScore"`
	MinScore        *int          `json:"minFormatScore"`
	MinUpgradeScore *int          `json:"minUpgradeFormatScore"`
	Items           []ProfileItem `json:"items"`
	FormatItems     []FormatItem  `json:"formatItems"`
}
type MediaManagement struct {
	Propers string `json:"downloadPropersAndRepacks"`
}
type Command struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}
type History struct {
	ID          int               `json:"id"`
	DownloadID  string            `json:"downloadId"`
	SeriesID    int               `json:"seriesId"`
	EpisodeID   int               `json:"episodeId"`
	Date        time.Time         `json:"date"`
	EventType   string            `json:"eventType"`
	SourceTitle string            `json:"sourceTitle"`
	Data        map[string]string `json:"data"`
}
type Blocklist struct {
	ID          int       `json:"id"`
	SeriesID    int       `json:"seriesId"`
	EpisodeIDs  []int     `json:"episodeIds"`
	SourceTitle string    `json:"sourceTitle"`
	Date        time.Time `json:"date"`
}
type ImportFile struct {
	Path         string    `json:"path"`
	FolderName   string    `json:"folderName"`
	SeriesID     int       `json:"seriesId"`
	EpisodeIDs   []int     `json:"episodeIds"`
	Quality      Quality   `json:"quality"`
	Languages    []NamedID `json:"languages"`
	ReleaseGroup string    `json:"releaseGroup"`
	IndexerFlags int       `json:"indexerFlags"`
	ReleaseType  string    `json:"releaseType"`
	DownloadID   string    `json:"downloadId"`
}
type ImportCommand struct {
	Name       string       `json:"name"`
	ImportMode string       `json:"importMode"`
	Files      []ImportFile `json:"files"`
}
type SearchCommand struct {
	Name       string `json:"name"`
	EpisodeIDs []int  `json:"episodeIds"`
}
