package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

const GroupCacheBucketSeconds = int64(300)

type GroupCacheHealth struct {
	Group            string   `json:"group"`
	ChannelID        int      `json:"channel_id"`
	Model            string   `json:"model"`
	Requests         int64    `json:"requests"`
	Errors           int64    `json:"errors"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	CacheSamples     int64    `json:"cache_samples"`
	CacheInputTokens int64    `json:"cache_input_tokens"`
	CacheHitRate     *float64 `json:"cache_hit_rate"`
}

// GroupRatePoint contains only the counts used to weight cache and error rates.
type GroupRatePoint struct {
	Timestamp        int64    `json:"timestamp"`
	Requests         int64    `json:"requests"`
	Errors           int64    `json:"errors"`
	CacheSamples     int64    `json:"cache_samples"`
	CacheInputTokens int64    `json:"cache_input_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheHitRate     *float64 `json:"cache_hit_rate"`
}

func (row GroupCacheHealth) RatePoint(timestamp int64) GroupRatePoint {
	return GroupRatePoint{
		Timestamp: timestamp, Requests: row.Requests, Errors: row.Errors,
		CacheSamples: row.CacheSamples, CacheInputTokens: row.CacheInputTokens, CacheReadTokens: row.CacheReadTokens,
		CacheHitRate: SamplePercentage(row.CacheReadTokens, row.CacheInputTokens),
	}
}

// SamplePercentage returns nil when the observation has no measurable denominator.
func SamplePercentage(numerator, denominator int64) *float64 {
	if denominator <= 0 {
		return nil
	}
	rate := float64(numerator) * 100 / float64(denominator)
	return &rate
}

type GroupCacheTrendPoint struct {
	GroupRatePoint
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

type GroupCacheTrend struct {
	Group  string                 `json:"group"`
	Points []GroupCacheTrendPoint `json:"points"`
}

type GroupCacheReport struct {
	Trends        []GroupCacheTrend `json:"trends"`
	BucketSeconds int64             `json:"bucket_seconds"`

	Groups     []GroupCacheHealth `json:"groups"`
	Channels   []GroupCacheHealth `json:"channels"`
	StartedAt  int64              `json:"started_at"`
	FinishedAt int64              `json:"finished_at"`
	Truncated  bool               `json:"truncated"`
}

var groupCacheReportCache struct {
	sync.Mutex
	report   *GroupCacheReport
	database *gorm.DB
	expires  time.Time
}

// GetGroupCacheReport scans a bounded recent log window, not vendor-specific
// SQL JSON expressions. The same query works with a separate log database.
func GetGroupCacheReport(ctx context.Context) (*GroupCacheReport, error) {
	groupCacheReportCache.Lock()
	defer groupCacheReportCache.Unlock()
	if groupCacheReportCache.database == model.LOG_DB && time.Now().Before(groupCacheReportCache.expires) {
		return groupCacheReportCache.report, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	now := time.Now().Unix()
	var logs []model.Log
	err := model.LOG_DB.WithContext(ctx).Model(&model.Log{}).
		Select("id", "request_id", "created_at", "type", "channel_id", "group", "model_name", "prompt_tokens", "completion_tokens", "other", "token_name").
		Where("created_at >= ? AND created_at <= ? AND type IN ?", now-86400, now, []int{model.LogTypeConsume, model.LogTypeError}).
		Order("created_at desc").Order("id desc").Limit(50001).Find(&logs).Error
	if err != nil {
		return nil, err
	}
	truncated := len(logs) > 50000
	if truncated {
		logs = logs[:50000]
	}
	samples := groupCacheSamples(logs)
	report := aggregateGroupCacheHealth(samples)
	report.Trends = aggregateGroupCacheTrends(samples, now-86400, now)
	report.BucketSeconds = GroupCacheBucketSeconds
	report.StartedAt, report.FinishedAt, report.Truncated = now-86400, now, truncated
	groupCacheReportCache.report = &report
	groupCacheReportCache.database = model.LOG_DB
	groupCacheReportCache.expires = time.Now().Add(30 * time.Second)
	return &report, nil
}

type groupCacheSample struct {
	*model.Log
	failed bool
}

// A stream can settle usage and still fail. Count it once, with both its
// measured usage and its final outcome. Older per-attempt errors stay separate.
func groupCacheSamples(logs []model.Log) []groupCacheSample {
	type requestKey struct {
		requestID string
		channelID int
	}
	finalErrors := make(map[requestKey]int)
	samples := make([]groupCacheSample, len(logs))
	for i := range logs {
		l := &logs[i]
		samples[i] = groupCacheSample{l, l.Type == model.LogTypeError}
		if l.Type == model.LogTypeError && l.RequestId != "" && gjson.Get(l.Other, "admin_info.final_request_error").Bool() {
			finalErrors[requestKey{l.RequestId, l.ChannelId}] = i
		}
	}
	if len(finalErrors) == 0 {
		return samples
	}
	for i := range logs {
		l := &logs[i]
		if l.Type != model.LogTypeConsume {
			continue
		}
		key := requestKey{l.RequestId, l.ChannelId}
		if j, ok := finalErrors[key]; ok {
			samples[i].failed = true
			samples[j].Log = nil
			delete(finalErrors, key)
		}
	}
	return slices.DeleteFunc(samples, func(s groupCacheSample) bool { return s.Log == nil })
}

func AggregateGroupCacheHealth(logs []model.Log) GroupCacheReport {
	return aggregateGroupCacheHealth(groupCacheSamples(logs))
}

// Keep routing groups distinct and exclude synthetic tests. Input means
// total prompt input. Cache reads and writes are its subsets.
func aggregateGroupCacheHealth(logs []groupCacheSample) GroupCacheReport {
	groups := map[string]*GroupCacheHealth{}
	channels := map[string]*GroupCacheHealth{}
	for _, log := range logs {
		other := gjson.Parse(log.Other)
		if other.Get("monitor_test").Exists() || log.TokenName == "模型测试" {
			continue
		}
		key := fmt.Sprintf("%s\x00%d\x00%s", log.Group, log.ChannelId, log.ModelName)
		if groups[log.Group] == nil {
			groups[log.Group] = &GroupCacheHealth{Group: log.Group}
		}
		if channels[key] == nil {
			channels[key] = &GroupCacheHealth{Group: log.Group, ChannelID: log.ChannelId, Model: log.ModelName}
		}
		read := max(int64(0), other.Get("cache_tokens").Int())
		write := max(int64(0), other.Get("cache_write_tokens").Int())
		if !other.Get("cache_write_tokens").Exists() {
			write = max(int64(0), other.Get("cache_creation_tokens_5m").Int()) + max(int64(0), other.Get("cache_creation_tokens_1h").Int())
			if write == 0 {
				write = max(int64(0), other.Get("cache_creation_tokens").Int())
			}
		}
		input := max(int64(0), int64(log.PromptTokens))
		if other.Get("input_tokens_total").Exists() {
			input = max(int64(0), other.Get("input_tokens_total").Int())
		} else if other.Get("usage_semantic").String() == "anthropic" || other.Get("claude").Bool() {
			input += read + write
		}
		path := other.Get("admin_info.usage_billing_path").String()
		// Older logs always synthesized cache_tokens=0. Such a zero alone cannot
		// prove the upstream reported cache usage; show coverage rather than invent it.
		known := (read > 0 || write > 0 || other.Get("cache_usage_reported").Bool()) && path != "local" && !strings.Contains(path, "estimated") && input >= read+write
		for _, row := range []*GroupCacheHealth{groups[log.Group], channels[key]} {
			row.Requests++
			if log.failed {
				row.Errors++
			}
			if log.Type == model.LogTypeError {
				continue
			}
			row.InputTokens += input
			row.OutputTokens += max(int64(0), int64(log.CompletionTokens))
			if known {
				row.CacheSamples++
				row.CacheInputTokens += input
				row.CacheReadTokens += read
				row.CacheWriteTokens += write
			}
		}
	}
	report := GroupCacheReport{Groups: []GroupCacheHealth{}, Channels: []GroupCacheHealth{}}
	for _, row := range groups {
		row.CacheHitRate = SamplePercentage(row.CacheReadTokens, row.CacheInputTokens)
		report.Groups = append(report.Groups, *row)
	}
	for _, row := range channels {
		row.CacheHitRate = SamplePercentage(row.CacheReadTokens, row.CacheInputTokens)
		report.Channels = append(report.Channels, *row)
	}
	slices.SortFunc(report.Groups, func(a, b GroupCacheHealth) int { return strings.Compare(a.Group, b.Group) })
	slices.SortFunc(report.Channels, func(a, b GroupCacheHealth) int {
		if n := strings.Compare(a.Group, b.Group); n != 0 {
			return n
		}
		if a.ChannelID < b.ChannelID {
			return -1
		}
		if a.ChannelID > b.ChannelID {
			return 1
		}
		return strings.Compare(a.Model, b.Model)
	})
	return report
}

// AggregateGroupCacheTrends reuses the summary's token semantics for each
// five-minute bucket. Window-relative boundaries make 1h/6h/24h views exact.
// Missing intervals keep a nil ratio; they are not fabricated zero-percent hits.
func AggregateGroupCacheTrends(logs []model.Log, startedAt, finishedAt int64) []GroupCacheTrend {
	return aggregateGroupCacheTrends(groupCacheSamples(logs), startedAt, finishedAt)
}

func aggregateGroupCacheTrends(logs []groupCacheSample, startedAt, finishedAt int64) []GroupCacheTrend {
	if startedAt >= finishedAt {
		return []GroupCacheTrend{}
	}
	buckets := map[int64][]groupCacheSample{}
	lastBucket := (finishedAt - startedAt - 1) / GroupCacheBucketSeconds
	for _, log := range logs {
		if log.CreatedAt < startedAt || log.CreatedAt > finishedAt {
			continue
		}
		bucket := min((log.CreatedAt-startedAt)/GroupCacheBucketSeconds, lastBucket)
		buckets[bucket] = append(buckets[bucket], log)
	}
	groups := map[string]map[int64]GroupCacheHealth{}
	for bucket, rows := range buckets {
		for _, row := range aggregateGroupCacheHealth(rows).Groups {
			if groups[row.Group] == nil {
				groups[row.Group] = map[int64]GroupCacheHealth{}
			}
			groups[row.Group][bucket] = row
		}
	}
	trends := make([]GroupCacheTrend, 0, len(groups))
	for group, rows := range groups {
		trend := GroupCacheTrend{Group: group, Points: make([]GroupCacheTrendPoint, 0, lastBucket+1)}
		for bucket := int64(0); bucket <= lastBucket; bucket++ {
			row := rows[bucket]
			trend.Points = append(trend.Points, GroupCacheTrendPoint{
				GroupRatePoint: row.RatePoint(startedAt + bucket*GroupCacheBucketSeconds),
				InputTokens:    row.InputTokens, OutputTokens: row.OutputTokens, CacheWriteTokens: row.CacheWriteTokens,
			})
		}
		trends = append(trends, trend)
	}
	slices.SortFunc(trends, func(a, b GroupCacheTrend) int { return strings.Compare(a.Group, b.Group) })
	return trends
}
