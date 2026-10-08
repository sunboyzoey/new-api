package channelmonitor

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGroupDrawingsReturnsTenSafeImagesOnly(t *testing.T) {
	yes := true
	jobs := []Evaluation{}
	for i := range 12 {
		jobs = append(jobs, Evaluation{EvaluationInput: EvaluationInput{ChannelID: 1, Model: "m", Kind: "pelican"}, Status: "completed", Renderable: &yes, CreatedAt: time.Unix(int64(i), 0), SVG: fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg"><text>%d</text></svg>`, i)})
	}
	jobs = append(jobs, Evaluation{EvaluationInput: EvaluationInput{ChannelID: 1, Model: "m", Kind: "pelican"}, Status: "completed", Renderable: &yes, SVG: `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`})
	jobs = append(jobs, Evaluation{EvaluationInput: EvaluationInput{ChannelID: 2, Model: "m", Kind: "pelican"}, Status: "completed", Renderable: &yes, SVG: `<svg xmlns="http://www.w3.org/2000/svg"/>`})
	channels := []EvaluationChannel{{ID: 1, Groups: []string{"g"}, Models: []string{"m"}}}
	drawings := GroupDrawings("g", "m", channels, jobs)
	if len(drawings) != 10 || drawings[0].At.Unix() != 11 || drawings[9].At.Unix() != 2 {
		t.Fatalf("bad drawings: %+v", drawings)
	}
	if len(GroupDrawings("private", "m", channels, jobs)) != 0 {
		t.Fatal("cross-group drawing leaked")
	}
}

func TestGroupDrawingsPreservesEachHistoricalPrompt(t *testing.T) {
	yes := true
	jobs := []Evaluation{
		{EvaluationInput: EvaluationInput{ChannelID: 1, Model: "m", Kind: "pelican", Prompt: "old static prompt"}, Status: "completed", Renderable: &yes, CreatedAt: time.Unix(1, 0), SVG: `<svg><text>old</text></svg>`},
		{EvaluationInput: EvaluationInput{ChannelID: 1, Model: "m", Kind: "pelican", Prompt: "new animation prompt"}, Status: "completed", Renderable: &yes, CreatedAt: time.Unix(2, 0), SVG: `<svg><text>new</text></svg>`},
	}
	drawings := GroupDrawings("g", "m", []EvaluationChannel{{ID: 1, Groups: []string{"g"}, Models: []string{"m"}}}, jobs)
	require.Len(t, drawings, 2)
	assert.Equal(t, "new animation prompt", drawings[0].Prompt)
	assert.Equal(t, "old static prompt", drawings[1].Prompt)
}
