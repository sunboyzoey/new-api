package channelmonitor

import (
	"github.com/QuantumNous/new-api/common"
	"strings"
	"testing"
	"time"
)

func TestPublicGroupQualityMatchesEnabledGroupAndModelAndOnlyExportsResults(t *testing.T) {
	yes, no := true, false
	score := 4
	now := time.Now().UTC()
	groups := []GroupHealth{{Name: "public", Models: []string{"m"}}, {Name: "other", Models: []string{"m"}}}
	channels := []EvaluationChannel{{ID: 1, Groups: []string{"public"}, Models: []string{"m"}}, {ID: 2, Groups: []string{"other"}, Models: []string{"m"}}}
	jobs := []Evaluation{
		{ID: "private-job", EvaluationInput: EvaluationInput{ChannelID: 1, Model: "m", Kind: "candy"}, Status: "completed", CreatedAt: now, Passed: &yes, Answer: "private-answer", SVG: "private-svg", Review: "private-review", TraceID: "private-trace"},
		{EvaluationInput: EvaluationInput{ChannelID: 1, Model: "m", Kind: "candy"}, Status: "completed", CreatedAt: now.Add(time.Second), Passed: &no},
		{EvaluationInput: EvaluationInput{ChannelID: 1, Model: "m", Kind: "candy"}, Status: "failed", CreatedAt: now.Add(2 * time.Second)},
		{EvaluationInput: EvaluationInput{ChannelID: 1, Model: "m", Kind: "pelican"}, Status: "completed", CreatedAt: now, Renderable: &yes, Score: &score},
		{EvaluationInput: EvaluationInput{ChannelID: 2, Model: "m", Kind: "candy"}, Status: "completed", CreatedAt: now, Passed: &yes},
		{EvaluationInput: EvaluationInput{ChannelID: 3, Model: "m", Kind: "candy"}, Status: "completed", CreatedAt: now, Passed: &yes},
		{EvaluationInput: EvaluationInput{ChannelID: 1, Model: "removed", Kind: "candy"}, Status: "completed", CreatedAt: now, Passed: &yes},
		{EvaluationInput: EvaluationInput{ChannelID: 1, Model: "m", Kind: "candy"}, Status: "running", CreatedAt: now},
	}
	AttachGroupQuality(groups, channels, jobs)
	candy := groups[0].Quality[0]
	if candy.Passed != 1 || candy.Failed != 1 || candy.Errors != 1 || len(candy.History) != 3 {
		t.Fatalf("invalid result counts: %+v", candy)
	}
	if candy.History[0].State != "passed" || candy.History[2].State != "error" {
		t.Fatal("history is not chronological")
	}
	drawing := groups[0].Quality[1]
	if drawing.History[0].Score == nil || *drawing.History[0].Score != 4 {
		t.Fatal("human score not published")
	}
	if groups[1].Quality[0].Passed != 1 {
		t.Fatal("cross-group probe leaked")
	}
	raw, err := common.Marshal(groups)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"channel_id", "private-", "trace_id", "answer", "svg", "review", "protocol"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("private field leaked: %s", private)
		}
	}
}
func TestPublicGroupQualityDoesNotTurnUngradedOrInvalidDrawingIntoPass(t *testing.T) {
	no := false
	score := 5
	groups := []GroupHealth{{Name: "public", Models: []string{"m"}}}
	jobs := []Evaluation{{EvaluationInput: EvaluationInput{ChannelID: 1, Model: "m", Kind: "candy"}, Status: "completed"}, {EvaluationInput: EvaluationInput{ChannelID: 1, Model: "m", Kind: "pelican"}, Status: "completed", Renderable: &no, Score: &score}}
	AttachGroupQuality(groups, []EvaluationChannel{{ID: 1, Groups: []string{"public"}, Models: []string{"m"}}}, jobs)
	if groups[0].Quality[0].Ungraded != 1 || groups[0].Quality[0].Passed != 0 {
		t.Fatal("unknown candy marked passed")
	}
	if groups[0].Quality[1].History[0].Score != nil {
		t.Fatal("invalid SVG published a score")
	}
}
func TestPublicGroupQualityCountsOnlyTheDisplayedHistory(t *testing.T) {
	groups := []GroupHealth{{Name: "g", Models: []string{"m"}}}
	yes, no := true, false
	jobs := []Evaluation{}
	for i := range 80 {
		passed := &yes
		if i < 20 {
			passed = &no
		}
		jobs = append(jobs, Evaluation{EvaluationInput: EvaluationInput{ChannelID: 1, Model: "m", Kind: "candy"}, Status: "completed", Passed: passed, CreatedAt: time.Unix(int64(i), 0)})
	}
	AttachGroupQuality(groups, []EvaluationChannel{{ID: 1, Groups: []string{"g"}, Models: []string{"m"}}}, jobs)
	r := groups[0].Quality[0]
	if len(r.History) != 60 || r.Passed != 60 || r.Failed != 0 {
		t.Fatalf("history and counters disagree: %+v", r)
	}
}
