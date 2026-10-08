package channelmonitor

import (
	"slices"
	"time"
)

type PublicDrawing struct {
	At     time.Time `json:"at"`
	SVG    string    `json:"svg"`
	Prompt string    `json:"prompt"`
}

func GroupDrawings(group, model string, channels []EvaluationChannel, jobs []Evaluation) []PublicDrawing {
	allowed := map[int]bool{}
	for _, ch := range channels {
		if slices.Contains(ch.Groups, group) && slices.Contains(ch.Models, model) {
			allowed[ch.ID] = true
		}
	}
	out := []PublicDrawing{}
	for _, job := range jobs {
		if !allowed[job.ChannelID] || job.Model != model || job.Kind != "pelican" || job.Status != "completed" || job.Renderable == nil || !*job.Renderable {
			continue
		}
		svg, valid := ValidateSVG(job.SVG)
		if !valid {
			continue
		}
		at := job.CreatedAt
		if job.FinishedAt != nil {
			at = *job.FinishedAt
		}
		out = append(out, PublicDrawing{At: at, SVG: svg, Prompt: job.Prompt})
	}
	slices.SortStableFunc(out, func(a, b PublicDrawing) int { return b.At.Compare(a.At) })
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}
