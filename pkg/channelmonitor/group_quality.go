package channelmonitor

import (
	"slices"
	"strings"
	"time"
)

type EvaluationChannel struct {
	ID     int
	Groups []string
	Models []string
}

// PublicQualityResult is an allowlist: no channel IDs, prompts, answers or trace IDs.
type PublicQualityPoint struct {
	At    time.Time `json:"at"`
	State string    `json:"state"`
	Score *int      `json:"score,omitempty"`
}
type PublicQualityResult struct {
	Model    string               `json:"model"`
	Kind     string               `json:"kind"`
	Passed   int                  `json:"passed"`
	Failed   int                  `json:"failed"`
	Errors   int                  `json:"errors"`
	Ungraded int                  `json:"ungraded"`
	History  []PublicQualityPoint `json:"history"`
}

// AttachGroupQuality associates probes with the currently enabled group/model
// configuration. These are channel probe results, not claims of group-wide IQ.
func AttachGroupQuality(groups []GroupHealth, channels []EvaluationChannel, jobs []Evaluation) {
	byChannel := map[int]EvaluationChannel{}
	for _, ch := range channels {
		byChannel[ch.ID] = ch
	}
	for i := range groups {
		group := &groups[i]
		group.Quality = []PublicQualityResult{}
		index := map[string]*PublicQualityResult{}
		for _, name := range group.Models {
			for _, kind := range []string{"candy", "pelican"} {
				group.Quality = append(group.Quality, PublicQualityResult{Model: name, Kind: kind, History: []PublicQualityPoint{}})
			}
		}
		for j := range group.Quality {
			r := &group.Quality[j]
			index[r.Model+"\x00"+r.Kind] = r
		}
		for _, job := range jobs {
			ch, ok := byChannel[job.ChannelID]
			if !ok || !slices.Contains(ch.Groups, group.Name) || !slices.Contains(ch.Models, job.Model) {
				continue
			}
			row := index[job.Model+"\x00"+job.Kind]
			if row == nil {
				continue
			}
			point := PublicQualityPoint{At: job.CreatedAt, State: "ungraded"}
			if job.FinishedAt != nil {
				point.At = *job.FinishedAt
			}
			switch job.Status {
			case "completed":
				passed := job.Passed
				if job.Kind == "pelican" {
					passed = job.Renderable
				}
				if passed == nil {
					point.State = "ungraded"
				} else if *passed {
					point.State = "passed"
				} else {
					point.State = "failed"
				}
				if job.Kind == "pelican" && point.State == "passed" && job.Score != nil && *job.Score >= 1 && *job.Score <= 5 {
					score := *job.Score
					point.Score = &score
				}
			case "failed", "interrupted":
				point.State = "error"
			default:
				continue // Public result history contains no queued/running jobs.
			}
			row.History = append(row.History, point)
		}
		for j := range group.Quality {
			row := &group.Quality[j]
			slices.SortStableFunc(row.History, func(a, b PublicQualityPoint) int { return a.At.Compare(b.At) })
			if len(row.History) > 60 {
				row.History = row.History[len(row.History)-60:]
			}
			for _, point := range row.History {
				switch point.State {
				case "passed":
					row.Passed++
				case "failed":
					row.Failed++
				case "error":
					row.Errors++
				default:
					row.Ungraded++
				}
			}

		}
	}
}

func SplitMonitorConfiguration(value string) []string {
	groups := []string{}
	for group := range strings.SplitSeq(value, ",") {
		if group = strings.TrimSpace(group); group != "" {
			groups = append(groups, group)
		}
	}
	return groups
}
