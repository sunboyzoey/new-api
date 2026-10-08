package channelmonitor

import (
	"context"
	"sync"
	"time"
)

// GroupHealth deliberately excludes channel identities and internal diagnostics.
type GroupHealth struct {
	Name        string                `json:"name"`
	Models      []string              `json:"models"`
	Requests    float64               `json:"requests"`
	Failures    float64               `json:"failures"`
	FailureRate *float64              `json:"failure_rate"`
	P95         *float64              `json:"p95_seconds"`
	TTFT        *float64              `json:"ttft_p95_seconds"`
	Average     *float64              `json:"average_seconds"`
	History     []RequestPoint        `json:"request_history"`
	Quality     []PublicQualityResult `json:"quality"`
}
type GroupDashboard struct {
	Window           string        `json:"window"`
	Available        bool          `json:"available"`
	QualityAvailable bool          `json:"quality_available"`
	IntervalSeconds  int           `json:"interval_seconds"`
	NextUpdateAt     time.Time     `json:"next_update_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
	Groups           []GroupHealth `json:"groups"`
}

type RequestPoint struct {
	At          int64    `json:"at"`
	Requests    float64  `json:"requests"`
	Failures    float64  `json:"failures"`
	FailureRate *float64 `json:"failure_rate"`
}

func GroupSnapshot(ctx context.Context, groups []GroupHealth, window string) (GroupDashboard, error) {
	return GroupSnapshotAt(ctx, groups, window, time.Now())
}
func GroupSnapshotAt(ctx context.Context, groups []GroupHealth, window string, at time.Time) (GroupDashboard, error) {
	window = ValidWindow(window)
	result := GroupDashboard{Window: window, UpdatedAt: at.UTC(), IntervalSeconds: 300, NextUpdateAt: at.Add(5 * time.Minute).UTC(), Groups: groups}
	index := map[string]*GroupHealth{}
	for i := range result.Groups {
		index[result.Groups[i].Name] = &result.Groups[i]
	}
	count := func(selector string) string {
		return "((increase(" + selector + "[" + window + "]) and (" + selector + " offset " + window + ")) or (" + selector + " unless (" + selector + " offset " + window + ")))"
	}
	queries := map[string]string{
		"requests": "sum by(group)(" + count(`newapi_requests_total{traffic="business",group!="",outcome=~"success|failure"}`) + ")",
		"failures": "sum by(group)(" + count(`newapi_requests_total{traffic="business",group!="",outcome="failure"}`) + ")",
		"p95":      "histogram_quantile(0.95,sum by(le,group)(" + count(`newapi_request_duration_seconds_bucket{traffic="business",group!="",outcome=~"success|failure"}`) + "))",
		"average":  "sum by(group)(" + count(`newapi_request_duration_seconds_sum{traffic="business",group!="",outcome=~"success|failure"}`) + ") / sum by(group)(" + count(`newapi_request_duration_seconds_count{traffic="business",group!="",outcome=~"success|failure"}`) + ")",
		"ttft":     "histogram_quantile(0.95,sum by(le,group)(" + count(`newapi_ttft_seconds_bucket{traffic="business",group!="",outcome=~"success|failure"}`) + "))",
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	for kind, query := range queries {
		wg.Go(func() {
			series, err := promQueryAt(ctx, query, window, false, at, 0)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for _, s := range series {
				row := index[s.Metric["group"]]
				n, ok := value(s)
				if row == nil || !ok {
					continue
				}
				switch kind {
				case "requests":
					row.Requests = n
				case "failures":
					row.Failures = n
				case "average":
					row.Average = &n
				case "p95":
					row.P95 = &n
				case "ttft":
					row.TTFT = &n
				}
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return result, firstErr
	}
	result.Available = true
	for i := range result.Groups {
		row := &result.Groups[i]
		row.FailureRate = percent(row.Failures, row.Requests)
	}
	if err := requestHistory(ctx, result.Groups, at); err != nil {
		result.Available = false
		return result, err
	}
	return result, nil
}

// Each cell is a completed five-minute interval, never a synthetic health sample.
func requestHistory(ctx context.Context, groups []GroupHealth, at time.Time) error {
	end := at.Truncate(5 * time.Minute)
	index := map[string]map[int64]*RequestPoint{}
	for i := range groups {
		groups[i].History = make([]RequestPoint, 12)
		index[groups[i].Name] = map[int64]*RequestPoint{}
		for j := range groups[i].History {
			point := &groups[i].History[j]
			point.At = end.Add(time.Duration(j-11) * 5 * time.Minute).Unix()
			index[groups[i].Name][point.At] = point
		}
	}
	for _, kind := range []string{"requests", "failures"} {
		outcome := `outcome=~"success|failure"`
		if kind == "failures" {
			outcome = `outcome="failure"`
		}
		selector := `newapi_requests_total{traffic="business",group!="",` + outcome + `}`
		expr := "sum by(group)(((increase(" + selector + "[5m]) and (" + selector + " offset 5m)) or (" + selector + " unless (" + selector + " offset 5m))))"
		series, err := promQueryAt(ctx, expr, "1h", true, end, 300)
		if err != nil {
			return err
		}
		for _, s := range series {
			points := index[s.Metric["group"]]
			for _, pair := range s.Values {
				if len(pair) != 2 {
					continue
				}
				timestamp, ok := pair[0].(float64)
				if !ok {
					continue
				}
				point := points[int64(timestamp)]
				n, valid := number(pair[1])
				if point == nil || !valid {
					continue
				}
				if kind == "requests" {
					point.Requests = n
				} else {
					point.Failures = n
				}
			}
		}
	}
	for i := range groups {
		for j := range groups[i].History {
			p := &groups[i].History[j]
			p.FailureRate = percent(p.Failures, p.Requests)
		}
	}
	return nil
}
