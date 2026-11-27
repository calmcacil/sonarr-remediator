package sonarr

import (
	"errors"
	"regexp"
	"slices"
)

type v5QueueResource struct {
	QueueRow
	EpisodeIDs []int `json:"episodeIds"`
}

var v5ExecutableWarning = regexp.MustCompile(`^Caution: Found executable file with extension: (?:'\.[A-Za-z0-9]+'|\.[A-Za-z0-9]+(?:, \.[A-Za-z0-9]+)*)$`)

func normalizeV5Queue(wire []v5QueueResource) ([]QueueRow, error) {
	downloads := make(map[string]struct{}, len(wire))
	count := 0
	for _, item := range wire {
		if err := item.validate(); err != nil {
			return nil, err
		}
		if item.EpisodeIDs == nil || item.EpisodeID != nil || item.TrackedState == "importFailed" {
			return nil, errors.New("invalid Sonarr v5 queue resource")
		}
		if item.DownloadID != "" {
			if _, seen := downloads[item.DownloadID]; seen {
				return nil, errors.New("duplicate Sonarr v5 queue download identity")
			}
			downloads[item.DownloadID] = struct{}{}
		}
		slices.Sort(item.EpisodeIDs)
		prev := 0
		for _, id := range item.EpisodeIDs {
			if id <= prev {
				return nil, errors.New("invalid or duplicated Sonarr v5 queue episode")
			}
			prev = id
		}
		count += max(1, len(item.EpisodeIDs))
	}

	// Pages count wire resources; only the policy sees one row per episode.
	rows := make([]QueueRow, 0, count)
	for _, item := range wire {
		row := item.QueueRow
		row.nativeV5 = true
		if row.TrackedStatus == "warning" {
			for _, status := range row.StatusMessages {
				if slices.ContainsFunc(status.Messages, v5ExecutableWarning.MatchString) {
					row.executableWarning = true
					break
				}
			}
		}
		if len(item.EpisodeIDs) == 0 {
			rows = append(rows, row)
		}
		for i := range item.EpisodeIDs {
			row.EpisodeID = &item.EpisodeIDs[i]
			rows = append(rows, row)
		}
	}
	return rows, nil
}

type v5Preview struct {
	Preview
	Rejections []struct {
		Code    string `json:"reason"`
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"rejections"`
}

func (v v5Preview) normalize() Preview {
	p := v.Preview
	if v.Rejections != nil {
		p.Rejections = make([]Rejection, len(v.Rejections))
		for i, r := range v.Rejections {
			p.Rejections[i] = Rejection{Reason: r.Message, Type: r.Type, Code: r.Code, v5: true}
		}
	}
	return p
}

func knownV5Rejection(code string) bool {
	switch code {
	case "fileLocked", "unknownSeries", "dangerousFile", "executableFile", "userRejectedExtension",
		"archiveFile", "seriesFolder", "invalidFilePath", "unsupportedExtension", "partialSeason",
		"seasonExtra", "invalidSeasonOrEpisode", "unableToParse", "error", "decisionError", "noEpisodes",
		"missingAbsoluteEpisodeNumber", "episodeAlreadyImported", "titleMissing", "titleTba", "minimumFreeSpace",
		"fullSeason", "noAudio", "episodeUnexpected", "episodeNotFoundInRelease", "sample", "sampleIndeterminate",
		"unpacking", "existingFileHasMoreEpisodes", "splitEpisode", "unverifiedSceneMapping", "notQualityUpgrade",
		"notRevisionUpgrade", "notCustomFormatUpgrade", "notCustomFormatUpgradeAfterRename", "multiSeason", "multipleSeries":
		return true
	}
	return false
}
