package channelmonitor

import (
	"context"
	"errors"
	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCandyOnlyGradesStructuredIntegerAnswer(t *testing.T) {
	for _, tc := range []struct {
		text         string
		pass, graded bool
	}{
		{`21`, false, false}, {`{"answer":21}`, true, true}, {`{"answer":20}`, false, true}, {`{"answer":"21"}`, false, false}, {`{"answer":null}`, false, false}, {`{"answer":21.5}`, false, false}, {`{"answer":21} then {"answer":19}`, false, true},
	} {
		t.Run(tc.text, func(t *testing.T) {
			p, g := GradeCandy(tc.text)
			if p != tc.pass || g != tc.graded {
				t.Fatalf("unexpected grading %v %v", p, g)
			}
		})
	}
}
func TestSVGRejectsActiveOrExternalContent(t *testing.T) {
	for _, svg := range []string{`<svg><script>alert(1)</script></svg>`, `<svg onload="alert(1)"></svg>`, `<svg><image href="https://evil.invalid/a"/></svg>`, `<svg><foreignObject/></svg>`, `<!DOCTYPE svg><svg><use href="https://evil.invalid"/></svg>`, `<svg><path fill="url(http://evil.invalid)"/></svg>`, `<svg><g></svg>`} {
		if _, ok := ValidateSVG(svg); ok {
			t.Fatalf("unsafe SVG accepted: %s", svg)
		}
	}
	good := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20"><circle cx="10" cy="10" r="5"/></svg>`
	if out, ok := ValidateSVG("```svg\n" + good + "\n```"); !ok || out != good {
		t.Fatal("valid drawing rejected")
	}
}
func TestGenerateBothProtocolsAndProviderFailure(t *testing.T) {
	for _, protocol := range []string{"responses", "chat"} {
		t.Run(protocol, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-private" || r.Header.Get("X-Channel-Monitor-Traffic") != "quality" {
					t.Error("missing auth or quality classification")
				}
				want := "/v1/responses"
				if protocol == "chat" {
					want = "/v1/chat/completions"
				}
				if r.URL.Path != want {
					t.Error("incorrect protocol path")
				}
				if protocol == "chat" {
					w.Write([]byte(`{"choices":[{"message":{"content":"{\"answer\":21}"}}]}`))
				} else {
					w.Write([]byte(`{"output_text":"{\"answer\":21}","output":[{"content":[{"text":"duplicate"}]}]}`))
				}
			}))
			defer srv.Close()
			text, status, err := Generate(context.Background(), Upstream{BaseURL: srv.URL + "/v1", Key: "test-private"}, EvaluationInput{Model: "mock", Protocol: protocol, Effort: "high"}, "prompt")
			if err != nil || status != 200 || text != `{"answer":21}` {
				t.Fatalf("incorrect final answer or duplicated Responses output: %q %d %v", text, status, err)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`secret-private-error`))
	}))
	defer srv.Close()
	text, status, err := Generate(context.Background(), Upstream{BaseURL: srv.URL}, EvaluationInput{Protocol: "responses"}, "prompt")
	if err == nil || status != 429 || text != "" || strings.Contains(err.Error(), "secret") {
		t.Fatal("provider failure not safely classified")
	}
}
func TestEvaluatorPersistsCompletedJobAndSanitizesPublicResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &body))
		raw, err := common.Marshal(body)
		require.NoError(t, err)
		assert.Contains(t, string(raw), "Custom prompt 42")
		w.Write([]byte(`{"output_text":"{\"answer\":42}"}`))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	resolver := func(in EvaluationInput) (Upstream, error) {
		return Upstream{BaseURL: srv.URL, Key: "private-test-key"}, nil
	}
	e, err := NewEvaluator(path, resolver)
	if err != nil {
		t.Fatal(err)
	}
	expected := 42
	job, err := e.Submit(EvaluationInput{Prompt: "Custom prompt 42", ExpectedAnswer: &expected, ChannelID: 1, Model: "mock", Kind: "candy", Protocol: "responses", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("job did not complete")
		case <-ticker.C:
			got, _ := e.Get(job.ID)
			if got.Status == "completed" {
				if got.Passed == nil || !*got.Passed {
					t.Fatal("completed answer wasn't graded")
				}
				goto completed
			}
		}
	}
completed:
	pub := e.List(true)[0]
	if pub.Answer != "" || pub.SVG != "" || pub.TraceID != "" || pub.Prompt != "" {
		t.Fatal("public endpoint leaks private test details")
	}
	e.Close()
	restored, err := NewEvaluator(path, resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, err := restored.Get(job.ID)
	if err != nil || got.Status != "completed" {
		t.Fatal("result lost on restart")
	}
	assert.Equal(t, "Custom prompt 42", got.Prompt)
	require.NotNil(t, got.ExpectedAnswer)
	assert.Equal(t, 42, *got.ExpectedAnswer)
	b, _ := common.Marshal(restored.List(false))
	if strings.Contains(string(b), "private-test-key") {
		t.Fatal("credential persisted")
	}
}

func TestSVGAllowsLocalGradientsWithoutExternalResources(t *testing.T) {
	svg := `<svg xmlns="http://www.w3.org/2000/svg"><!-- a local gradient is normal SVG content --><defs><linearGradient id="sky"><stop offset="0%" stop-color="blue"/></linearGradient></defs><rect width="20" height="20" fill="url(#sky)"/></svg>`
	if _, ok := ValidateSVG(svg); !ok {
		t.Fatal("local SVG gradient incorrectly blocked")
	}
	svg = strings.Replace(svg, "url(#sky)", "url(https://evil.invalid/sky)", 1)
	if _, ok := ValidateSVG(svg); ok {
		t.Fatal("external gradient accepted")
	}
}

func TestScheduleConfigurationPersistsCustomPromptAndPreventsOverlap(t *testing.T) {
	input := EvaluationInput{ChannelID: 1, Model: "mock", Kind: "candy", Protocol: "responses", Effort: "medium", Prompt: "Return JSON answer 42"}
	expected := 42
	input.ExpectedAnswer = &expected
	e := &Evaluator{path: filepath.Join(t.TempDir(), "state.json"), resolve: func(EvaluationInput) (Upstream, error) { return Upstream{}, nil }, queue: make(chan Evaluation, 16), state: evalState{Evaluations: []Evaluation{}, Schedules: []Schedule{}}}
	require.NoError(t, e.SaveSchedule(Schedule{EvaluationInput: input, Enabled: true, IntervalMinutes: 5}))
	saved := e.Schedules()[0]
	require.NotEmpty(t, saved.ID)
	assert.Equal(t, input.Prompt, saved.Prompt)
	require.NotNil(t, saved.ExpectedAnswer)
	assert.Equal(t, 42, *saved.ExpectedAnswer)
	raw, err := os.ReadFile(e.path)
	require.NoError(t, err)
	var stored evalState
	require.NoError(t, common.Unmarshal(raw, &stored))
	require.Len(t, stored.Schedules, 1)
	assert.Equal(t, input.Prompt, stored.Schedules[0].Prompt)
	now := saved.NextAt
	due := e.collectDueSchedules(now)
	require.Len(t, due, 1)
	job, err := e.submit(due[0].EvaluationInput, due[0].ID)
	require.NoError(t, err)
	assert.Equal(t, saved.ID, job.ScheduleID)
	assert.Equal(t, input.Prompt, job.Prompt)
	assert.Empty(t, e.collectDueSchedules(now.Add(5*time.Minute)), "a queued run must prevent an overlapping run")
	saved.Enabled = false
	saved.Prompt = "Edited prompt"
	saved.IntervalMinutes = 10
	e.resolve = func(EvaluationInput) (Upstream, error) { return Upstream{}, errors.New("channel unavailable") }
	require.NoError(t, e.SaveSchedule(saved), "an unavailable channel must still allow disabling its schedule")
	assert.Empty(t, e.collectDueSchedules(now.Add(time.Hour)))
	assert.Equal(t, "Edited prompt", e.Schedules()[0].Prompt)
	assert.Len(t, e.Schedules(), 1, "editing must not create a second task")
	saved.ID = "missing"
	require.Error(t, e.SaveSchedule(saved))
	for _, interval := range []int{0, 4, 1441} {
		require.Error(t, e.SaveSchedule(Schedule{EvaluationInput: input, IntervalMinutes: interval}))
	}
	input.Prompt = strings.Repeat("x", 12001)
	require.Error(t, ValidateInput(input))
}
func TestCandyUsesConfiguredExpectedAnswer(t *testing.T) {
	passed, graded := GradeCandyAnswer(`{"answer":42}`, 42)
	assert.True(t, passed)
	assert.True(t, graded)
	passed, graded = GradeCandyAnswer(`{"answer":21}`, 42)
	assert.False(t, passed)
	assert.True(t, graded)
	passed, graded = GradeCandyAnswer(`{"answer":"42"}`, 42)
	assert.False(t, passed)
	assert.False(t, graded)
}
