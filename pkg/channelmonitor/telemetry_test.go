package channelmonitor

import (
	"context"
	"errors"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRetryFailureDoesNotBecomeFinalFailure(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer provider.Shutdown(context.Background())
	meter := provider.Meter("test")
	requests, _ = meter.Int64Counter("requests")
	attempts, _ = meter.Int64Counter("attempts")
	duration, _ = meter.Float64Histogram("duration")
	requestDuration, _ = meter.Float64Histogram("request_duration")
	ttft, _ = meter.Float64Histogram("ttft")
	tokens, _ = meter.Int64Counter("tokens")
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	oldTracer := tracer
	tracer = tp.Tracer("test")
	enabled = true
	defer func() { enabled = false; tracer = oldTracer }()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	info := &relaycommon.RelayInfo{OriginModelName: "mock", StartTime: time.Now(), UsingGroup: "initial-group"}
	info.ChannelMeta = &relaycommon.ChannelMeta{ChannelId: 128}
	finish, _ := BeginAttempt(c, 128, "mock")
	finish(info, types.NewErrorWithStatusCode(errors.New("private error body"), types.ErrorCode("rate_limit_exceeded"), 429))
	finish, _ = BeginAttempt(c, 128, "mock")
	info.UsingGroup = "final-group"
	finish(info, nil)
	FinishRequest(c, info, nil)
	var out metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	totals := map[string]map[string]int64{}
	for _, sm := range out.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			totals[m.Name] = map[string]int64{}
			for _, point := range sum.DataPoints {
				v, _ := point.Attributes.Value("outcome")
				g, _ := point.Attributes.Value("group")
				wantGroup := "final-group"
				if v.AsString() == "failure" {
					wantGroup = "initial-group"
				}
				if g.AsString() != wantGroup {
					t.Fatalf("request group misattributed: %s", g.AsString())
				}
				totals[m.Name][v.AsString()] += point.Value
			}
		}
	}
	if totals["attempts"]["failure"] != 1 || totals["attempts"]["success"] != 1 || totals["requests"]["success"] != 1 || totals["requests"]["failure"] != 0 {
		t.Fatalf("retry and final outcomes mixed: %+v", totals)
	}
	for _, span := range exporter.GetSpans() {
		for _, a := range span.Attributes {
			if a.Value.AsString() == "private error body" {
				t.Fatal("upstream body leaked into trace")
			}
		}
	}
}
func TestStreamingHTTP200StillClassifiesFailedTerminal(t *testing.T) {
	info := &relaycommon.RelayInfo{StreamStatus: &relaycommon.StreamStatus{}}
	info.StreamStatus.MarkFailed("rate_limit_exceeded", "rate_limit_error", 429)
	outcome, status, kind := classify(context.Background(), info, nil)
	if outcome != "failure" || status != 429 || kind != "rate_limit" {
		t.Fatalf("stream failure hidden by HTTP 200: %s %d %s", outcome, status, kind)
	}
}
func TestNoRequestsHaveUnknownFailureRate(t *testing.T) {
	if percent(0, 0) != nil {
		t.Fatal("empty window appears healthy")
	}
}

func TestWrappedLocalDeadlineIsClassifiedAsTimeout(t *testing.T) {
	err := types.NewErrorWithStatusCode(context.DeadlineExceeded, types.ErrorCode("do_request_failed"), 500)
	outcome, _, kind := classify(context.Background(), &relaycommon.RelayInfo{}, err)
	if outcome != "failure" || kind != "timeout" {
		t.Fatalf("deadline misclassified: %s %s", outcome, kind)
	}
}

func TestCompletedResponseCountsWhenPeerClosesAfterReading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome, _, _ := classify(ctx, &relaycommon.RelayInfo{}, nil)
	if outcome != "success" {
		t.Fatal("completed non-stream response discarded after normal connection close")
	}
	info := &relaycommon.RelayInfo{IsStream: true, StreamStatus: &relaycommon.StreamStatus{}}
	info.StreamStatus.MarkCompleted()
	outcome, _, _ = classify(ctx, info, nil)
	if outcome != "success" {
		t.Fatal("completed stream discarded after terminal event")
	}
	info.StreamStatus = &relaycommon.StreamStatus{}
	info.StreamStatus.MarkCancelled()
	outcome, _, _ = classify(ctx, info, nil)
	if outcome != "excluded" {
		t.Fatal("unfinished canceled stream must stay excluded")
	}
}
