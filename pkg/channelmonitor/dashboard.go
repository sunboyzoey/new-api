package channelmonitor

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

type Channel struct {
	ID     int      `json:"id"`
	Name   string   `json:"name"`
	Status int      `json:"status"`
	Models []string `json:"models"`
}
type Health struct {
	Channel
	Requests           float64  `json:"requests"`
	Failures           float64  `json:"failures"`
	Attempts           float64  `json:"attempts"`
	AttemptFailures    float64  `json:"attempt_failures"`
	FailureRate        *float64 `json:"failure_rate"`
	AttemptFailureRate *float64 `json:"attempt_failure_rate"`
	P95                *float64 `json:"p95_seconds"`
	TTFT               *float64 `json:"ttft_p95_seconds"`
	RateLimits         float64  `json:"rate_limits"`
	Timeouts           float64  `json:"timeouts"`
	ConnectionErrors   float64  `json:"connection_errors"`
	AuthErrors         float64  `json:"auth_errors"`
	UpstreamErrors     float64  `json:"upstream_errors"`
	OutputTokens       float64  `json:"output_tokens"`
}
type Trend struct {
	At       int64   `json:"at"`
	Attempts float64 `json:"attempts"`
	Failures float64 `json:"failures"`
}
type Dashboard struct {
	Window    string      `json:"window"`
	Available bool        `json:"available"`
	StartedAt time.Time   `json:"started_at"`
	UpdatedAt time.Time   `json:"updated_at"`
	Channels  []Health    `json:"channels"`
	Trend     []Trend     `json:"trend"`
	Alerts    []string    `json:"alerts"`
	Coverage  string      `json:"coverage"`
	CPA       []CPAMetric `json:"cpa"`
}
type CPAMetric struct {
	Route   string  `json:"route"`
	Outcome string  `json:"outcome"`
	Count   float64 `json:"count"`
}
type promSeries struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
	Values [][]any           `json:"values"`
}
type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []promSeries `json:"result"`
	} `json:"data"`
}

var monitoringStarted = func() time.Time {
	if t, err := time.Parse(time.RFC3339, os.Getenv("CHANNEL_MONITOR_STARTED_AT")); err == nil {
		return t
	}
	return time.Now().UTC()
}()
var promClient = &http.Client{Timeout: 4 * time.Second}

func ValidWindow(w string) string {
	switch w {
	case "5m", "1h", "24h":
		return w
	default:
		return "1h"
	}
}
func PromQuery(ctx context.Context, expr string, window string, rangeQuery bool) ([]promSeries, error) {
	return promQueryAt(ctx, expr, window, rangeQuery, time.Now(), 0)
}
func promQueryAt(ctx context.Context, expr, window string, rangeQuery bool, at time.Time, rangeStep int) ([]promSeries, error) {
	base := os.Getenv("CHANNEL_MONITOR_PROMETHEUS")
	if base == "" {
		base = "http://127.0.0.1:9090"
	}
	params := url.Values{"query": {expr}, "time": {strconv.FormatInt(at.Unix(), 10)}}
	path := "/api/v1/query"
	if rangeQuery {
		path = "/api/v1/query_range"
		d, _ := time.ParseDuration(ValidWindow(window))
		now := at
		params.Set("start", strconv.FormatInt(now.Add(-d).Unix(), 10))
		params.Set("end", strconv.FormatInt(now.Unix(), 10))
		step := int(d.Seconds() / 60)
		if step < 5 {
			step = 5
		}
		if rangeStep > 0 {
			step = rangeStep
		}
		params.Set("step", strconv.Itoa(step))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := promClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("monitoring backend unavailable")
	}
	var body promResponse
	if err = common.DecodeJson(resp.Body, &body); err != nil {
		return nil, err
	}
	if body.Status != "success" {
		return nil, errors.New("monitoring query failed")
	}
	return body.Data.Result, nil
}
func number(v any) (float64, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	n, e := strconv.ParseFloat(s, 64)
	return n, e == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
}
func value(series promSeries) (float64, bool) {
	if len(series.Value) != 2 {
		return 0, false
	}
	return number(series.Value[1])
}
func percent(failed, total float64) *float64 {
	if total <= 0 {
		return nil
	}
	n := 100 * failed / total
	return &n
}
func Snapshot(ctx context.Context, channels []Channel, window string) (Dashboard, error) {
	window = ValidWindow(window)
	result := Dashboard{Window: window, StartedAt: monitoringStarted, UpdatedAt: time.Now().UTC(), Channels: []Health{}, Trend: []Trend{}, Alerts: []string{}, CPA: []CPAMetric{}, Coverage: "NewAPI requests and relay attempts; CPA model executions; quality probes are separate"}
	index := map[int]*Health{}
	for _, ch := range channels {
		h := Health{Channel: ch}
		result.Channels = append(result.Channels, h)
	}
	for i := range result.Channels {
		index[result.Channels[i].ID] = &result.Channels[i]
	}

	// New series have no earlier zero sample. Use their cumulative value until
	// the selected window has an older sample, then use reset-aware increase.
	count := func(selector string) string {
		return "((increase(" + selector + "[" + window + "]) and (" + selector + " offset " + window + ")) or (" + selector + " unless (" + selector + " offset " + window + ")))"
	}
	sum := func(selector string) string { return "sum by(channel_id)(" + count(selector) + ")" }
	queries := map[string]string{
		"requests":         sum(`newapi_requests_total{outcome=~"success|failure"}`),
		"failures":         sum(`newapi_requests_total{outcome="failure"}`),
		"attempts":         sum(`newapi_channel_attempts_total{outcome=~"success|failure"}`),
		"attempt_failures": sum(`newapi_channel_attempts_total{outcome="failure"}`),
		"p95":              "histogram_quantile(0.95,sum by(le,channel_id)(" + count(`newapi_channel_duration_seconds_bucket{outcome=~"success|failure"}`) + "))",
		"ttft":             "histogram_quantile(0.95,sum by(le,channel_id)(" + count(`newapi_ttft_seconds_bucket{outcome=~"success|failure"}`) + "))",
		"errors":           "sum by(channel_id,error_kind)(" + count(`newapi_channel_attempts_total{outcome="failure"}`) + ")",
		"tokens":           sum(`newapi_output_tokens_total`),
		"cpa":              "sum by(route_kind,outcome)(" + count(`cpa_executions_total{traffic="business",monitor_schema="2"}`) + ")",
	}

	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	for kind, query := range queries {
		wg.Add(1)
		go func(kind, query string) {
			defer wg.Done()
			series, err := PromQuery(ctx, query, window, false)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for _, s := range series {
				v, ok := value(s)
				if !ok {
					continue
				}
				if kind == "cpa" {
					result.CPA = append(result.CPA, CPAMetric{Route: s.Metric["route_kind"], Outcome: s.Metric["outcome"], Count: v})
					continue
				}
				id, _ := strconv.Atoi(s.Metric["channel_id"])
				h := index[id]
				if h == nil {
					continue
				}
				switch kind {
				case "requests":
					h.Requests = v
				case "failures":
					h.Failures = v
				case "attempts":
					h.Attempts = v
				case "attempt_failures":
					h.AttemptFailures = v
				case "p95":
					h.P95 = &v
				case "ttft":
					h.TTFT = &v
				case "tokens":
					h.OutputTokens = v
				case "errors":
					switch s.Metric["error_kind"] {
					case "rate_limit":
						h.RateLimits = v
					case "timeout":
						h.Timeouts = v
					case "connection":
						h.ConnectionErrors = v
					case "authentication":
						h.AuthErrors = v
					case "upstream_5xx":
						h.UpstreamErrors = v
					}
				}
			}
		}(kind, query)
	}
	wg.Wait()
	if firstErr != nil {
		return result, firstErr
	}
	result.Available = true
	for i := range result.Channels {
		h := &result.Channels[i]
		h.FailureRate = percent(h.Failures, h.Requests)
		h.AttemptFailureRate = percent(h.AttemptFailures, h.Attempts)
		if h.Attempts >= 10 && h.AttemptFailureRate != nil && *h.AttemptFailureRate >= 20 {
			result.Alerts = append(result.Alerts, strconv.Itoa(h.ID))
		}
	}
	trends := map[int64]*Trend{}
	for _, kind := range []string{"attempts", "failures"} {
		filter := `outcome=~"success|failure"`
		if kind == "failures" {
			filter = `outcome="failure"`
		}
		series, err := PromQuery(ctx, `sum(rate(newapi_channel_attempts_total{`+filter+`}[1m])) * 60`, window, true)
		if err != nil {
			continue
		}
		for _, s := range series {
			for _, pair := range s.Values {
				if len(pair) != 2 {
					continue
				}
				atFloat, ok := pair[0].(float64)
				if !ok {
					continue
				}
				v, ok := number(pair[1])
				if !ok {
					continue
				}
				at := int64(atFloat)
				point := trends[at]
				if point == nil {
					point = &Trend{At: at}
					trends[at] = point
				}
				if kind == "attempts" {
					point.Attempts = v
				} else {
					point.Failures = v
				}
			}
		}
	}
	for _, point := range trends {
		result.Trend = append(result.Trend, *point)
	}
	sort.Slice(result.Trend, func(i, j int) bool { return result.Trend[i].At < result.Trend[j].At })
	return result, nil
}
