package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
)

type Observation struct {
	DownloadID         string
	Rows               []sonarr.QueueRow
	Queue              []sonarr.QueueRow
	Preview            []sonarr.Preview
	Episodes           map[int]sonarr.Episode
	Files              map[int]sonarr.EpisodeFile
	Series             map[int]sonarr.Series
	Profiles           map[int]sonarr.Profile
	Propers            string
	Intended           []int
	MembershipComplete bool
}
type Decision struct {
	Kind            string              `json:"kind"`
	Reason          string              `json:"reason"`
	Fingerprint     string              `json:"fingerprint"`
	EpisodeIDs      []int               `json:"episode_ids"`
	SeriesIDs       []int               `json:"series_ids"`
	Files           []sonarr.ImportFile `json:"files,omitempty"`
	Blocklist       bool                `json:"blocklist"`
	EpisodeMismatch bool                `json:"episode_mismatch,omitempty"`
}

// Match Sonarr's verdict, not inferred episode differences or queue warnings.
var unexpectedEpisodeRejection = regexp.MustCompile(`^(?:Episode [0-9]+x[0-9]{2,} was|Episodes [0-9]+x[0-9]{2,}(?:, [0-9]+x[0-9]{2,})+ were) unexpected considering the .+ folder name$`)

func Eligible(rows []sonarr.QueueRow) bool {
	if len(rows) == 0 {
		return false
	}
	for _, q := range rows {
		if !q.Eligible() {
			return false
		}
	}
	return true
}

// RejectedCleanup trusts Sonarr's import verdict for removing the whole download.
// Matching/profile proof is required for imports, not for this cleanup path.
// The boolean records a scoped rejection even while cleanup is disabled.
func RejectedCleanup(c config.Config, o Observation) (Decision, bool) {
	d := Decision{Kind: "defer", Fingerprint: Fingerprint(o), EpisodeIDs: []int{}, SeriesIDs: []int{}}
	if !Eligible(o.Rows) || len(o.Preview) == 0 {
		return d, false
	}
	restricted := len(c.Scope.IncludeSeriesIDs) > 0 || len(c.Scope.ExcludeSeriesIDs) > 0
	knownQueueSeries := false
	addTarget := func(series, episode int) bool {
		if series < 0 || episode < 0 {
			return false
		}
		if series > 0 {
			if !SeriesAllowed(c, series) {
				return false
			}
			if !slices.Contains(d.SeriesIDs, series) {
				d.SeriesIDs = append(d.SeriesIDs, series)
			}
		}
		if episode > 0 && !slices.Contains(d.EpisodeIDs, episode) {
			d.EpisodeIDs = append(d.EpisodeIDs, episode)
		}
		return true
	}
	for _, q := range o.Rows {
		if q.DownloadID != o.DownloadID || !ClientAllowed(c, q.DownloadClient) {
			return d, false
		}
		series, episode := 0, 0
		if q.SeriesID != nil {
			series = *q.SeriesID
			knownQueueSeries = knownQueueSeries || series > 0
		}
		if q.EpisodeID != nil {
			episode = *q.EpisodeID
		}
		if !addTarget(series, episode) {
			return d, false
		}
	}
	paths := map[string]bool{}
	ids := map[int]bool{}
	rejected := false
	episodeMismatch := false
	for _, f := range o.Preview {
		if f.ID <= 0 || ids[f.ID] || strings.TrimSpace(f.Path) == "" || paths[f.Path] || f.DownloadID != o.DownloadID || f.Rejections == nil {
			return d, false
		}
		paths[f.Path] = true
		ids[f.ID] = true
		series := 0
		if f.Series != nil {
			series = f.Series.ID
		}
		if restricted && series <= 0 && !knownQueueSeries || !addTarget(series, 0) {
			return d, false
		}
		for _, ep := range f.Episodes {
			if !addTarget(ep.SeriesID, ep.ID) {
				return d, false
			}
		}
		for _, r := range f.Rejections {
			if strings.TrimSpace(r.Reason) == "" || r.Type != "temporary" && r.Type != "permanent" {
				return d, false
			}
			rejected = true
			episodeMismatch = episodeMismatch || r.Type == "permanent" && unexpectedEpisodeRejection.MatchString(r.Reason)
		}
	}
	if rejected {
		slices.Sort(d.EpisodeIDs)
		slices.Sort(d.SeriesIDs)
		switch {
		case !c.Cleanup.Enabled:
			d.Reason = "Sonarr rejects import; cleanup disabled"
		case Competing(o.Queue, o.DownloadID, d.EpisodeIDs):
			d.Reason = "competing active or unidentified download"
		default:
			d.Kind, d.Reason = "remove_blocked", "Sonarr rejects import; remove the rejected shared download"
			if episodeMismatch {
				d.EpisodeMismatch = true
				d.Reason = "Sonarr reports unexpected episodes for the folder; remove the failed shared download"
			}
		}
	}
	return d, rejected
}

func QueueIdentity(rows []sonarr.QueueRow) string {
	type identity struct {
		Download, Client, Path, Title string
		Series, Episode               int
	}
	var ids []identity
	for _, q := range rows {
		x := identity{Download: q.DownloadID, Client: q.DownloadClient, Path: q.OutputPath, Title: q.Title}
		if q.SeriesID != nil {
			x.Series = *q.SeriesID
		}
		if q.EpisodeID != nil {
			x.Episode = *q.EpisodeID
		}
		ids = append(ids, x)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, _ := json.Marshal(ids[i])
		b, _ := json.Marshal(ids[j])
		return string(a) < string(b)
	})
	return hash(ids)
}
func hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func Fingerprint(o Observation) string {
	type material struct {
		Path, Download, ReleaseType string
		Series                      int
		Episodes                    []int
		Quality                     sonarr.Quality
		Formats                     []int
		Score                       *int
		Rejections                  []sonarr.Rejection
	}
	var m []material
	for _, f := range o.Preview {
		v := material{Path: f.Path, Download: f.DownloadID, ReleaseType: f.ReleaseType, Quality: f.Quality, Score: f.Score}
		if f.Series != nil {
			v.Series = f.Series.ID
		}
		for _, e := range f.Episodes {
			v.Episodes = append(v.Episodes, e.ID)
		}
		for _, c := range f.Formats {
			v.Formats = append(v.Formats, c.ID)
		}
		sort.Ints(v.Episodes)
		sort.Ints(v.Formats)
		v.Rejections = append([]sonarr.Rejection{}, f.Rejections...)
		sort.Slice(v.Rejections, func(i, j int) bool { return v.Rejections[i].Reason < v.Rejections[j].Reason })
		m = append(m, v)
	}
	sort.Slice(m, func(i, j int) bool { return m[i].Path < m[j].Path })
	return hash(struct {
		Queue    string
		Files    []material
		Intended []int
	}{QueueIdentity(o.Rows), m, o.Intended})
}
func SeriesAllowed(c config.Config, id int) bool {
	return id > 0 && !slices.Contains(c.Scope.ExcludeSeriesIDs, id) && (len(c.Scope.IncludeSeriesIDs) == 0 || slices.Contains(c.Scope.IncludeSeriesIDs, id))
}
func ClientAllowed(c config.Config, name string) bool {
	return name != "" && !slices.Contains(c.Scope.ExcludeDownloadClients, name) && (len(c.Scope.IncludeDownloadClients) == 0 || slices.Contains(c.Scope.IncludeDownloadClients, name))
}
func Competing(queue []sonarr.QueueRow, download string, episodes []int) bool {
	for _, q := range queue {
		if download != "" && q.DownloadID == download {
			continue
		}
		if q.Status == "completed" {
			switch q.TrackedState {
			case "importBlocked", "importFailed", "imported":
				continue
			}
		}
		// Unknown active target cannot prove absence of competition.
		if q.EpisodeID == nil || *q.EpisodeID <= 0 || slices.Contains(episodes, *q.EpisodeID) {
			return true
		}
	}
	return false
}
func Evaluate(c config.Config, o Observation) Decision {
	d := Decision{Kind: "defer", Fingerprint: Fingerprint(o), EpisodeIDs: append([]int{}, o.Intended...)}
	block := func(reason string) Decision { d.Reason = reason; return d }
	if !Eligible(o.Rows) {
		return block("protected or noneligible tracked state")
	}
	if o.Propers != "preferAndUpgrade" && o.Propers != "doNotUpgrade" && o.Propers != "doNotPrefer" {
		return block("unknown proper/repack preference")
	}
	for _, q := range o.Rows {
		if q.DownloadID != o.DownloadID || !ClientAllowed(c, q.DownloadClient) {
			return block("download/client scope unavailable or excluded")
		}
		if q.SeriesID != nil && *q.SeriesID > 0 && !SeriesAllowed(c, *q.SeriesID) {
			return block("series excluded")
		}
	}
	if len(o.Preview) == 0 || !o.MembershipComplete || len(o.Intended) == 0 {
		return block("empty preview or unresolved intended membership")
	}
	if Competing(o.Queue, o.DownloadID, o.Intended) {
		return block("competing active or unidentified download")
	}
	seen := map[int]bool{}
	paths := map[string]bool{}
	seriesSeen := map[int]bool{}
	importable, redundant, unusable := true, true, true
	for _, f := range o.Preview {
		if f.ID <= 0 || f.Path == "" || paths[f.Path] || f.DownloadID != o.DownloadID || f.Series == nil || f.Series.ID <= 0 || len(f.Episodes) == 0 || !ValidQuality(f.Quality) || f.Score == nil || f.Formats == nil || f.Rejections == nil || f.Languages == nil || len(f.Languages) == 0 || f.IndexerFlags == nil || *f.IndexerFlags < 0 {
			return block("incomplete or conflicting preview")
		}
		paths[f.Path] = true
		if f.ReleaseType != "singleEpisode" && f.ReleaseType != "multiEpisode" && f.ReleaseType != "seasonPack" {
			return block("unknown release type")
		}
		knownSeries, matchedSeries := false, false
		for _, row := range o.Rows {
			if row.SeriesID != nil && *row.SeriesID > 0 {
				knownSeries = true
				if *row.SeriesID == f.Series.ID {
					matchedSeries = true
				}
			}
		}
		if knownSeries && !matchedSeries {
			return block("preview series disagrees with known queue identity")
		}
		s, ok := o.Series[f.Series.ID]
		if !ok || s.ID != f.Series.ID || s.Monitored == nil || s.QualityProfileID <= 0 || !SeriesAllowed(c, s.ID) {
			return block("series scope/profile unresolved or excluded")
		}
		switch s.SeriesType {
		case "standard", "daily", "anime":
		default:
			return block("unknown series type")
		}
		p, ok := o.Profiles[s.QualityProfileID]
		if !ok || p.ID != s.QualityProfileID {
			return block("missing current profile")
		}
		if _, _, e := profileRanks(p); e != nil {
			return block(e.Error())
		}
		seriesSeen[s.ID] = true
		isUnusable := len(f.Rejections) > 0
		redRejections := true
		for _, r := range f.Rejections {
			if r.Type != "permanent" && r.Type != "temporary" {
				return block("unknown rejection type")
			}
			if r.Reason != "Sample" && r.Reason != "No audio tracks detected" {
				isUnusable = false
			}
			if !strings.HasPrefix(r.Reason, "Not a Custom Format upgrade for existing episode file(s). New: ") && !strings.HasPrefix(r.Reason, "Not an upgrade for existing episode file(s). Existing quality: ") && r.Reason != "Not a quality revision upgrade for existing episode file(s)" {
				redRejections = false
			}
		}
		if len(f.Rejections) > 0 {
			importable = false
		}
		if !redRejections {
			redundant = false
		}
		if !isUnusable {
			unusable = false
		}
		imp := sonarr.ImportFile{Path: f.Path, FolderName: f.FolderName, SeriesID: s.ID, Quality: f.Quality, Languages: f.Languages, ReleaseGroup: f.ReleaseGroup, IndexerFlags: *f.IndexerFlags, ReleaseType: f.ReleaseType, DownloadID: o.DownloadID}
		for _, pe := range f.Episodes {
			if pe.ID <= 0 || seen[pe.ID] || pe.SeriesID != s.ID || !slices.Contains(o.Intended, pe.ID) {
				return block("ambiguous/conflicting episode match")
			}
			seen[pe.ID] = true
			e, ok := o.Episodes[pe.ID]
			if !ok || e.ID != pe.ID || e.SeriesID != s.ID || e.HasFile == nil || e.EpisodeFileID == nil || e.Monitored == nil {
				return block("missing current episode state")
			}
			imp.EpisodeIDs = append(imp.EpisodeIDs, e.ID)
			if !*e.HasFile {
				if *e.EpisodeFileID != 0 {
					return block("contradictory episode-file state")
				}
				redundant = false
				fake := sonarr.EpisodeFile{Quality: f.Quality, Score: f.Score, Formats: f.Formats, Path: f.Path}
				if !Acceptable(fake, p) {
					importable = false
				}
			} else {
				old, ok := o.Files[*e.EpisodeFileID]
				if !ok || old.ID != *e.EpisodeFileID || old.SeriesID != s.ID {
					return block("missing existing file identity")
				}
				if !Acceptable(old, p) {
					redundant = false
				}
				comparison, upgrade, why := Compare(f.Quality, f.Formats, f.Score, old, p, o.Propers)
				if !upgrade {
					importable = false
					d.Reason = why
				}
				if comparison > 0 || upgrade {
					redundant = false
				}
				// Comparison failure cannot prove redundancy.
				if comparison == 0 && why != "equal or upgrade cutoff/minimum improvement not met" {
					redundant = false
				}
			}
		}
		sort.Ints(imp.EpisodeIDs)
		d.Files = append(d.Files, imp)
	}
	for _, id := range o.Intended {
		if seen[id] {
			continue
		}
		e, ok := o.Episodes[id]
		if !ok || e.HasFile == nil || !*e.HasFile || e.EpisodeFileID == nil || !SeriesAllowed(c, e.SeriesID) {
			return block("intended episode missing from preview and library")
		}
		s, ok := o.Series[e.SeriesID]
		if !ok {
			return block("missing expected series")
		}
		p, ok := o.Profiles[s.QualityProfileID]
		f, fok := o.Files[*e.EpisodeFileID]
		if !ok || !fok || f.SeriesID != e.SeriesID || !Acceptable(f, p) {
			return block("unmatched intended episode lacks acceptable current file")
		}
		seriesSeen[e.SeriesID] = true
	}
	for id := range seriesSeen {
		d.SeriesIDs = append(d.SeriesIDs, id)
	}
	sort.Ints(d.SeriesIDs)
	if importable {
		if c.Recovery.Enabled {
			d.Kind = "import"
			d.Reason = "every previewed episode is missing or a permitted strict improvement"
			return d
		}
		return block("recovery disabled")
	}
	if redundant {
		if c.Cleanup.Enabled {
			d.Kind = "remove_redundant"
			d.Reason = "all intended episodes satisfied; every candidate equal or inferior"
			return d
		}
		return block("redundant; cleanup disabled")
	}
	if unusable {
		if c.Cleanup.Enabled {
			d.Kind = "remove_unusable"
			d.Blocklist = c.Cleanup.Blocklist
			d.Reason = "every media file has an explicit Sample/No audio tracks verdict"
			return d
		}
		return block("unusable; cleanup disabled")
	}
	if d.Reason == "" {
		d.Reason = "preview rejection or unresolved upgrade requires operator intervention"
	}
	return d
}
func Describe(d Decision) string { return fmt.Sprintf("%s: %s", d.Kind, d.Reason) }
