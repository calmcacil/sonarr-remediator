package policy

import (
	"errors"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
)

type rank struct {
	Order   int
	Allowed bool
}

func profileRanks(p sonarr.Profile) (map[int]rank, int, error) {
	if p.UpgradeAllowed == nil || p.Cutoff == nil || p.CutoffScore == nil || p.MinScore == nil || p.MinUpgradeScore == nil || *p.MinUpgradeScore < 0 || p.Items == nil || p.FormatItems == nil {
		return nil, 0, errors.New("incomplete quality profile")
	}
	ranks := map[int]rank{}
	cutoff := -1
	for i, item := range p.Items {
		if item.Allowed == nil {
			return nil, 0, errors.New("missing quality permission")
		}
		children := item.Items
		if item.Quality != nil {
			if len(children) > 0 {
				return nil, 0, errors.New("contradictory quality group")
			}
			children = []sonarr.ProfileItem{item}
		}
		if len(children) == 0 {
			return nil, 0, errors.New("empty quality group")
		}
		if item.ID == *p.Cutoff && item.Quality == nil {
			cutoff = i
		}
		for _, child := range children {
			if child.Quality == nil || child.Allowed == nil || len(child.Items) > 0 {
				return nil, 0, errors.New("unsupported quality group")
			}
			id := child.Quality.ID
			if _, ok := ranks[id]; ok || id < 0 {
				return nil, 0, errors.New("duplicate quality")
			}
			ranks[id] = rank{i, *item.Allowed && *child.Allowed}
			if id == *p.Cutoff {
				cutoff = i
			}
		}
	}
	if cutoff < 0 {
		return nil, 0, errors.New("unknown quality cutoff")
	}
	return ranks, cutoff, nil
}
func ValidQuality(q sonarr.Quality) bool {
	return q.Quality != nil && q.Quality.ID > 0 && q.Revision != nil && q.Revision.Version != nil && *q.Revision.Version >= 1 && q.Revision.Real != nil && *q.Revision.Real >= 0 && q.Revision.IsRepack != nil
}
func RevisionCompare(a, b sonarr.Quality) int {
	if *a.Revision.Real > *b.Revision.Real {
		return 1
	}
	if *a.Revision.Real < *b.Revision.Real {
		return -1
	}
	if *a.Revision.Version > *b.Revision.Version {
		return 1
	}
	if *a.Revision.Version < *b.Revision.Version {
		return -1
	}
	return 0
}
func score(p sonarr.Profile, formats []sonarr.NamedID, reported *int) (int, error) {
	if reported == nil || formats == nil {
		return 0, errors.New("missing custom format data")
	}
	weights := map[int]int{}
	for _, f := range p.FormatItems {
		if f.Format <= 0 || f.Score == nil {
			return 0, errors.New("invalid custom format profile")
		}
		if _, ok := weights[f.Format]; ok {
			return 0, errors.New("duplicate custom format weight")
		}
		weights[f.Format] = *f.Score
	}
	sum := 0
	seen := map[int]bool{}
	for _, f := range formats {
		v, ok := weights[f.ID]
		if !ok || seen[f.ID] {
			return 0, errors.New("unknown or duplicate custom format")
		}
		seen[f.ID] = true
		sum += v
	}
	if sum != *reported {
		return 0, errors.New("custom format score contradicts current profile")
	}
	return sum, nil
}
func Acceptable(f sonarr.EpisodeFile, p sonarr.Profile) bool {
	acceptable, e := CurrentAcceptable(f, p)
	return e == nil && acceptable
}

func ValidateProfile(p sonarr.Profile) error {
	_, _, e := profileRanks(p)
	if e != nil {
		return e
	}
	_, e = score(p, []sonarr.NamedID{}, new(int))
	return e
}

// CurrentAcceptable distinguishes a known unacceptable file from missing policy data.
func CurrentAcceptable(f sonarr.EpisodeFile, p sonarr.Profile) (bool, error) {
	ranks, _, e := profileRanks(p)
	if e != nil || !ValidQuality(f.Quality) || f.Path == "" {
		return false, errors.New("incomplete current file/profile information")
	}
	r, ok := ranks[f.Quality.Quality.ID]
	if !ok {
		return false, errors.New("unknown current file quality")
	}
	s, e := score(p, f.Formats, f.Score)
	if e != nil {
		return false, e
	}
	return r.Allowed && s >= *p.MinScore, nil
}

// Compare follows Sonarr's quality groups, then applicable revision/format branch.
// It also requires upgradeAllowed for all replacements, including propers.
func Compare(q sonarr.Quality, formats []sonarr.NamedID, reported *int, old sonarr.EpisodeFile, p sonarr.Profile, propers string) (int, bool, string) {
	ranks, cutoff, e := profileRanks(p)
	if e != nil || !ValidQuality(q) || !ValidQuality(old.Quality) {
		return 0, false, "incomplete quality/profile information"
	}
	if propers != "preferAndUpgrade" && propers != "doNotUpgrade" && propers != "doNotPrefer" {
		return 0, false, "unknown proper/repack preference"
	}
	n, nok := ranks[q.Quality.ID]
	o, ook := ranks[old.Quality.Quality.ID]
	if !nok || !ook {
		return 0, false, "unknown quality in current profile"
	}
	ns, e := score(p, formats, reported)
	if e != nil {
		return 0, false, e.Error()
	}
	os, e := score(p, old.Formats, old.Score)
	if e != nil {
		return 0, false, e.Error()
	}
	if n.Order < o.Order {
		return -1, false, "candidate has inferior quality"
	}
	revision := RevisionCompare(q, old.Quality)
	if n.Order == o.Order && propers != "doNotPrefer" && revision < 0 {
		return -1, false, "candidate has inferior revision"
	}
	if n.Order == o.Order && ns < os {
		return -1, false, "candidate has inferior custom format score"
	}
	if n.Order == o.Order && ns == os && (propers == "doNotPrefer" || revision == 0) {
		return 0, false, "equal or upgrade cutoff/minimum improvement not met"
	}
	if !n.Allowed || ns < *p.MinScore {
		return 1, false, "candidate does not meet allowed quality/minimum score"
	}
	if !*p.UpgradeAllowed {
		return 1, false, "upgrades disabled; conservative gate also preserves revision upgrades"
	}
	if n.Order > o.Order {
		if o.Order < cutoff {
			return 1, true, "strict quality improvement"
		}
		return 1, false, "quality cutoff already met"
	}
	if revision > 0 && q.Quality.ID == old.Quality.Quality.ID && propers == "preferAndUpgrade" {
		return 1, true, "strict revision improvement"
	}
	if ns > os && os < *p.CutoffScore && ns-os >= *p.MinUpgradeScore {
		return 1, true, "strict custom format improvement"
	}
	if revision > 0 && propers != "doNotPrefer" {
		return 1, false, "revision improvement is not permitted by proper policy"
	}
	return 1, false, "custom format cutoff/minimum improvement not met"
}
