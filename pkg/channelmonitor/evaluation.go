package channelmonitor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

var errProbeTimeout = errors.New("upstream test request timed out")

const candyPrompt = `做一道选取策略题，不联网、不使用工具。袋内糖果按形状、口味计数：圆形苹果7、圆形桃子9、圆形西瓜8；星形苹果7、星形桃子6、星形西瓜4。手摸可以判断形状，不能判断口味，可以利用这一点决定选取策略。事先确定总共取出的数量，保证取得一颗苹果和一颗桃子且二者形状不同。最小数量是多少？说明保证性和最小性。最后独立输出一个JSON对象，格式为{"answer":整数,"reason":"简短理由"}。`
const pelicanPrompt = `创建一个HTML，内容是SVG绘制一个鹈鹕骑自行车的2D动画，你不需要任何测试`

type EvaluationInput struct {
	ChannelID      int    `json:"channel_id"`
	Model          string `json:"model"`
	Kind           string `json:"kind"`
	Protocol       string `json:"protocol"`
	Effort         string `json:"effort"`
	Prompt         string `json:"prompt,omitempty"`
	ExpectedAnswer *int   `json:"expected_answer,omitempty"`
}
type Evaluation struct {
	ID         string `json:"id"`
	ScheduleID string `json:"schedule_id,omitempty"`
	EvaluationInput
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Latency    float64    `json:"latency_seconds"`
	Answer     string     `json:"answer,omitempty"`
	Passed     *bool      `json:"passed"`
	Renderable *bool      `json:"renderable"`
	SVG        string     `json:"svg,omitempty"`
	Score      *int       `json:"score"`
	Review     string     `json:"review,omitempty"`
	ErrorKind  string     `json:"error_kind,omitempty"`
	TraceID    string     `json:"trace_id,omitempty"`
}
type Schedule struct {
	ID string `json:"id"`
	EvaluationInput
	Enabled         bool      `json:"enabled"`
	IntervalMinutes int       `json:"interval_minutes"`
	NextAt          time.Time `json:"next_at"`
}
type evalState struct {
	Evaluations []Evaluation `json:"evaluations"`
	Schedules   []Schedule   `json:"schedules"`
}
type Upstream struct {
	BaseURL string
	Key     string
	Client  *http.Client
}
type Resolver func(EvaluationInput) (Upstream, error)
type Evaluator struct {
	mu      sync.Mutex
	state   evalState
	path    string
	resolve Resolver
	queue   chan Evaluation
	closed  chan struct{}
}

func DefaultEvaluationPrompt(kind string) string {
	if kind == "pelican" {
		return pelicanPrompt
	}
	return candyPrompt
}

func ValidateInput(input EvaluationInput) error {
	if len(input.Prompt) > 12000 {
		return errors.New("prompt must not exceed 12000 bytes")
	}
	if input.Prompt != "" && strings.TrimSpace(input.Prompt) == "" {
		return errors.New("prompt must not be blank")
	}
	if input.ExpectedAnswer != nil && (*input.ExpectedAnswer < -1000000000 || *input.ExpectedAnswer > 1000000000) {
		return errors.New("expected answer is out of range")
	}
	if input.ChannelID <= 0 || len(input.Model) == 0 || len(input.Model) > 128 {
		return errors.New("channel and model are required")
	}
	if input.Kind != "candy" && input.Kind != "pelican" {
		return errors.New("unknown test kind")
	}
	if input.Protocol != "responses" && input.Protocol != "chat" {
		return errors.New("unknown request protocol")
	}
	switch input.Effort {
	case "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return errors.New("unknown reasoning effort")
	}
	return nil
}
func NewEvaluator(path string, resolve Resolver) (*Evaluator, error) {
	e := &Evaluator{path: path, resolve: resolve, queue: make(chan Evaluation, 16), closed: make(chan struct{}), state: evalState{Evaluations: []Evaluation{}, Schedules: []Schedule{}}}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(path); err == nil {
		if err = common.Unmarshal(b, &e.state); err != nil {
			return nil, errors.New("evaluation state is invalid")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	for i := range e.state.Evaluations {
		job := &e.state.Evaluations[i]
		if job.Kind == "pelican" && job.Status == "completed" {
			svg, valid := ValidateSVG(job.Answer)
			job.SVG = svg
			job.Renderable = &valid
			if valid && job.ErrorKind == "invalid_svg" {
				job.ErrorKind = ""
			}
			if !valid {
				job.Score = nil
			}
		}
		if e.state.Evaluations[i].Status == "queued" || e.state.Evaluations[i].Status == "running" {
			e.state.Evaluations[i].Status = "interrupted"
			e.state.Evaluations[i].ErrorKind = "service_restart"
		}
	}
	if err := e.saveLocked(); err != nil {
		return nil, err
	}
	// Keep quality probes sequential so they do not compete for a single-account CPA slot.
	for range 1 {
		go e.worker()
	}
	go e.scheduleLoop()
	return e, nil
}
func (e *Evaluator) Close() { close(e.closed) }
func (e *Evaluator) saveLocked() error {
	b, err := common.Marshal(e.state)
	if err != nil {
		return err
	}
	tmp := e.path + ".tmp"
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, e.path)
}
func randomID() string { b := make([]byte, 16); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func (e *Evaluator) Submit(input EvaluationInput) (Evaluation, error) {
	return e.submit(input, "")
}

func (e *Evaluator) submit(input EvaluationInput, scheduleID string) (Evaluation, error) {
	if err := ValidateInput(input); err != nil {
		return Evaluation{}, err
	}
	if _, err := e.resolve(input); err != nil {
		return Evaluation{}, err
	}
	if input.Prompt == "" {
		input.Prompt = DefaultEvaluationPrompt(input.Kind)
	}
	job := Evaluation{ID: randomID(), ScheduleID: scheduleID, EvaluationInput: input, Status: "queued", CreatedAt: time.Now().UTC()}

	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.queue) == cap(e.queue) {
		return Evaluation{}, errors.New("evaluation queue is full")
	}
	// Never evict work in progress when retaining the latest results.
	if len(e.state.Evaluations) >= 200 {
		found := -1
		for i, j := range e.state.Evaluations {
			if j.Status != "queued" && j.Status != "running" {
				found = i
				break
			}
		}
		if found < 0 {
			return Evaluation{}, errors.New("evaluation queue is full")
		}
		e.state.Evaluations = append(e.state.Evaluations[:found], e.state.Evaluations[found+1:]...)
	}
	e.state.Evaluations = append(e.state.Evaluations, job)
	if err := e.saveLocked(); err != nil {
		e.state.Evaluations = e.state.Evaluations[:len(e.state.Evaluations)-1]
		return Evaluation{}, err
	}
	e.queue <- job
	return job, nil
}
func (e *Evaluator) List(public bool) []Evaluation {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Evaluation, 0, len(e.state.Evaluations))
	for i := len(e.state.Evaluations) - 1; i >= 0; i-- {
		j := e.state.Evaluations[i]
		if public {
			j.Answer = ""
			j.Prompt = ""
			j.SVG = ""
			j.Review = ""
			j.TraceID = ""
		}
		out = append(out, j)
	}
	return out
}
func (e *Evaluator) Review(id string, score int, review string) error {
	if score < 1 || score > 5 || len(review) > 2000 {
		return errors.New("score must be 1 to 5 and review at most 2000 characters")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.state.Evaluations {
		j := &e.state.Evaluations[i]
		if j.ID == id {
			if j.Kind != "pelican" || j.Status != "completed" || j.Renderable == nil || !*j.Renderable {
				return errors.New("drawing is not ready for review")
			}
			j.Score = &score
			j.Review = review
			return e.saveLocked()
		}
	}
	return errors.New("evaluation not found")
}
func (e *Evaluator) Schedules() []Schedule {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Schedule, len(e.state.Schedules))
	copy(out, e.state.Schedules)
	return out
}
func (e *Evaluator) SaveSchedule(s Schedule) error {
	if s.IntervalMinutes < 5 || s.IntervalMinutes > 1440 {
		return errors.New("interval must be 5 to 1440 minutes")
	}
	if err := ValidateInput(s.EvaluationInput); err != nil {
		return err
	}
	if s.Prompt == "" {
		s.Prompt = DefaultEvaluationPrompt(s.Kind)
	}
	if s.Enabled {
		if _, err := e.resolve(s.EvaluationInput); err != nil {
			return err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s.NextAt = time.Now().UTC().Add(time.Duration(s.IntervalMinutes) * time.Minute)
	for i := range e.state.Schedules {
		if e.state.Schedules[i].ID == s.ID {
			e.state.Schedules[i] = s
			return e.saveLocked()
		}
	}
	if s.ID != "" {
		return errors.New("schedule not found")
	}
	if len(e.state.Schedules) >= 20 {
		return errors.New("at most 20 schedules are allowed")
	}
	s.ID = randomID()
	e.state.Schedules = append(e.state.Schedules, s)
	return e.saveLocked()
}
func (e *Evaluator) scheduleLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-e.closed:
			return
		case <-t.C:
			due := e.collectDueSchedules(time.Now().UTC())
			for _, schedule := range due {
				_, _ = e.submit(schedule.EvaluationInput, schedule.ID)
			}
		}
	}
}
func (e *Evaluator) collectDueSchedules(now time.Time) []Schedule {
	e.mu.Lock()
	defer e.mu.Unlock()
	due := []Schedule{}
	for i := range e.state.Schedules {
		s := &e.state.Schedules[i]
		if s.Enabled && !s.NextAt.After(now) {
			busy := false
			for _, job := range e.state.Evaluations {
				if job.ScheduleID == s.ID && (job.Status == "queued" || job.Status == "running") {
					busy = true
					break
				}
			}
			if !busy {
				due = append(due, *s)
			}
			s.NextAt = now.UTC().Add(time.Duration(s.IntervalMinutes) * time.Minute)
		}
	}
	_ = e.saveLocked()
	return due
}
func (e *Evaluator) update(job Evaluation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.state.Evaluations {
		if e.state.Evaluations[i].ID == job.ID {
			e.state.Evaluations[i] = job
			_ = e.saveLocked()
			return
		}
	}
}
func (e *Evaluator) worker() {
	for {
		select {
		case <-e.closed:
			return
		case job := <-e.queue:
			if job.ScheduleID != "" {
				e.mu.Lock()
				enabled := false
				for _, schedule := range e.state.Schedules {
					if schedule.ID == job.ScheduleID {
						enabled = schedule.Enabled
						break
					}
				}
				e.mu.Unlock()
				if !enabled {
					job.Status = "cancelled"
					at := time.Now().UTC()
					job.FinishedAt = &at
					e.update(job)
					continue
				}
			}
			job.Status = "running"
			e.update(job)
			start := time.Now()
			upstream, err := e.resolve(job.EvaluationInput)
			if err != nil {
				job.Status = "failed"
				job.ErrorKind = "configuration"
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				ctx, span := tracer.Start(ctx, "channel.quality.test")
				span.SetAttributes(attribute.Int("channel_id", job.ChannelID), attribute.String("model", job.Model), attribute.String("test_kind", job.Kind), attribute.String("traffic", "quality"))
				job.TraceID = span.SpanContext().TraceID().String()
				prompt := job.Prompt
				if prompt == "" {
					prompt = candyPrompt
					if job.Kind == "pelican" {
						prompt = pelicanPrompt
					}
					job.Prompt = prompt
				}
				answer, status, err := Generate(ctx, upstream, job.EvaluationInput, prompt)
				if err != nil {
					span.SetStatus(codes.Error, "quality generation failed")
				}
				cancel()
				span.End()
				job.Answer = answer
				if err != nil {
					job.Status = "failed"
					code := "generation_error"
					var netErr net.Error
					if errors.Is(err, errProbeTimeout) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
						code = "timeout"
					}
					job.ErrorKind = ErrorKind(status, code)
				} else {
					job.Status = "completed"
					if job.Kind == "candy" {
						expected := 21
						if job.ExpectedAnswer != nil {
							expected = *job.ExpectedAnswer
						}
						passed, valid := GradeCandyAnswer(answer, expected)
						if valid {
							job.Passed = &passed
						} else {
							job.ErrorKind = "ungraded_answer"
						}
					}
					if job.Kind == "pelican" {
						svg, valid := ValidateSVG(answer)
						job.Renderable = &valid
						job.SVG = svg
						if !valid {
							job.ErrorKind = "invalid_svg"
						}
					}
				}
			}
			job.Latency = time.Since(start).Seconds()
			now := time.Now().UTC()
			job.FinishedAt = &now
			e.update(job)
		}
	}
}
func Generate(ctx context.Context, upstream Upstream, input EvaluationInput, prompt string) (string, int, error) {
	payload := map[string]any{"model": input.Model, "stream": false}
	path := "/v1/responses"
	if input.Protocol == "responses" {
		payload["input"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": prompt}}}}
		payload["reasoning"] = map[string]any{"effort": input.Effort}
		payload["max_output_tokens"] = 8192
	} else {
		path = "/v1/chat/completions"
		payload["messages"] = []any{map[string]any{"role": "user", "content": prompt}}
		payload["reasoning_effort"] = input.Effort
		payload["max_completion_tokens"] = 8192
	}
	body, err := common.Marshal(payload)
	if err != nil {
		return "", 0, err
	}
	base := strings.TrimRight(upstream.BaseURL, "/")
	base = strings.TrimSuffix(base, "/v1")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+upstream.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Channel-Monitor-Traffic", "quality")
	Inject(ctx, req.Header)
	client := upstream.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	r, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		lower := strings.ToLower(string(b))
		if strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded") {
			return "", r.StatusCode, errProbeTimeout
		}
		return "", r.StatusCode, errors.New("upstream test request failed")
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		return "", 200, err
	}
	text, err := ResponseText(b)
	return text, 200, err
}
func ResponseText(b []byte) (string, error) {
	var result struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := common.Unmarshal(b, &result); err != nil {
		return "", errors.New("invalid upstream response")
	}
	text := result.OutputText
	if text == "" {
		for _, out := range result.Output {
			for _, part := range out.Content {
				text += part.Text
			}
		}
	}
	if text == "" && len(result.Choices) > 0 {
		text = result.Choices[0].Message.Content
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("upstream returned no final answer")
	}
	return text, nil
}

var answerJSON = regexp.MustCompile(`(?s)\{[^{}]*"answer"\s*:[^{}]*\}`)

func GradeCandy(text string) (bool, bool) { return GradeCandyAnswer(text, 21) }

func GradeCandyAnswer(text string, expected int) (bool, bool) {
	matches := answerJSON.FindAllString(text, -1)
	if len(matches) == 0 {
		return false, false
	}
	var answer struct {
		Answer *int `json:"answer"`
	}
	if common.UnmarshalJsonStr(matches[len(matches)-1], &answer) != nil || answer.Answer == nil {
		return false, false
	}
	return *answer.Answer == expected, true
}

var safeSVGFragmentURL = regexp.MustCompile(`(?i)url\(\s*["']?#[a-zA-Z_][a-zA-Z0-9_.:-]*["']?\s*\)`)

func ValidateSVG(text string) (string, bool) {
	lower := strings.ToLower(text)
	start := strings.Index(lower, "<svg")
	end := strings.LastIndex(lower, "</svg>")
	if start < 0 || end < start || len(text) > 1<<20 {
		return "", false
	}
	svg := text[start : end+6]
	if (strings.Contains(strings.ToLower(svg), "<!doctype") || strings.Contains(strings.ToLower(svg), "<!entity")) || strings.Contains(strings.ToLower(safeSVGFragmentURL.ReplaceAllString(svg, "")), "url(") {
		return "", false
	}
	decoder := xml.NewDecoder(strings.NewReader(svg))
	depth := 0
	root := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", false
		}
		switch v := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				if root || strings.ToLower(v.Name.Local) != "svg" || (v.Name.Space != "" && v.Name.Space != "http://www.w3.org/2000/svg") {
					return "", false
				}
				root = true
			}
			depth++
			switch strings.ToLower(v.Name.Local) {
			case "script", "foreignobject", "iframe", "object", "embed", "image", "audio", "video":
				return "", false
			}
			for _, a := range v.Attr {
				name := strings.ToLower(a.Name.Local)
				if strings.HasPrefix(name, "on") {
					return "", false
				}
				if name == "href" && !strings.HasPrefix(a.Value, "#") {
					return "", false
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	if !root || depth != 0 {
		return "", false
	}
	return svg, true
}
func (e *Evaluator) Get(id string) (Evaluation, error) {
	for _, j := range e.List(false) {
		if j.ID == id {
			return j, nil
		}
	}
	return Evaluation{}, fmt.Errorf("evaluation not found")
}
