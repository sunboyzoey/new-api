package channelmonitor

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

var requests metric.Int64Counter
var attempts metric.Int64Counter
var duration metric.Float64Histogram
var requestDuration metric.Float64Histogram
var ttft metric.Float64Histogram
var tokens metric.Int64Counter
var tracer trace.Tracer = otel.Tracer("newapi.channelmonitor")
var enabled bool

func Init(ctx context.Context) (func(context.Context) error, error) {
	if os.Getenv("CHANNEL_MONITOR_ENABLED") != "true" {
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	traceExporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res := resource.NewSchemaless(attribute.String("service.name", "new-api"))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(5*time.Second))))
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(traceExporter), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(.2))))
	otel.SetMeterProvider(mp)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	meter := mp.Meter("newapi.channelmonitor")
	requests, _ = meter.Int64Counter("newapi.requests")
	attempts, _ = meter.Int64Counter("newapi.channel.attempts")
	duration, _ = meter.Float64Histogram("newapi.channel.duration", metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(.1, .25, .5, 1, 2, 5, 10, 30, 60, 120, 300, 600))
	requestDuration, _ = meter.Float64Histogram("newapi.request.duration", metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(.1, .25, .5, 1, 2, 5, 10, 30, 60, 120, 300, 600))
	ttft, _ = meter.Float64Histogram("newapi.ttft", metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(.05, .1, .25, .5, 1, 2, 5, 10, 30, 60))
	tokens, _ = meter.Int64Counter("newapi.output.tokens")
	tracer = tp.Tracer("newapi.channelmonitor")
	enabled = true
	return func(ctx context.Context) error { return errors.Join(mp.Shutdown(ctx), tp.Shutdown(ctx)) }, nil
}

func RouteKind(model string) string {
	if strings.HasPrefix(model, "bps/") {
		return "bps"
	}
	if strings.HasPrefix(model, "ticket/") {
		return "ticket"
	}
	return "standard"
}
func ErrorKind(status int, code string) string {
	lower := strings.ToLower(code)
	switch {
	case status == 429:
		return "rate_limit"
	case status == 401 || status == 403:
		return "authentication"
	case status == 504 || strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline"):
		return "timeout"
	case strings.Contains(lower, "do_request") || strings.Contains(lower, "connect"):
		return "connection"
	case status >= 500:
		return "upstream_5xx"
	case status >= 400:
		return "request_4xx"
	case code != "":
		return "stream_error"
	default:
		return "none"
	}
}
func attrs(channel int, model, outcome, kind string, status int) []attribute.KeyValue {
	if len(model) > 128 {
		model = "other"
	}
	return []attribute.KeyValue{attribute.String("channel_id", strconv.Itoa(channel)), attribute.String("model", model), attribute.String("outcome", outcome), attribute.String("error_kind", kind), attribute.String("route_kind", RouteKind(model)), attribute.Int("http_status", status), attribute.String("traffic", "business")}
}
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !enabled || !(strings.HasPrefix(c.Request.URL.Path, "/v1/") || strings.HasPrefix(c.Request.URL.Path, "/v1beta/")) || (c.Request.Method != "POST" && c.Request.Header.Get("Upgrade") != "websocket") {
			c.Next()
			return
		}
		ctx := otel.GetTextMapPropagator().Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
		ctx, span := tracer.Start(ctx, "newapi.request", trace.WithSpanKind(trace.SpanKindServer))
		c.Request = c.Request.WithContext(ctx)
		c.Header("X-Monitor-Trace-ID", span.SpanContext().TraceID().String())
		start := time.Now()
		defer func() {
			span.SetAttributes(attribute.String("http.route", c.FullPath()), attribute.Int("http.response.status_code", c.Writer.Status()))
			if _, recorded := c.Get("channel_monitor_recorded"); !recorded {
				status := c.Writer.Status()
				outcome := "success"
				if status >= 400 {
					outcome = "failure"
					span.SetStatus(codes.Error, ErrorKind(status, ""))
				}
				id := common.GetContextKeyInt(c, constant.ContextKeyChannelId)
				model := common.GetContextKeyString(c, constant.ContextKeyOriginalModel)
				if id == 0 {
					model = "unknown"
					outcome = "excluded"
				}
				a := append(attrs(id, model, outcome, ErrorKind(status, ""), status), attribute.String("group", common.GetContextKeyString(c, constant.ContextKeyUsingGroup)))
				requests.Add(ctx, 1, metric.WithAttributes(a...))
				requestDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(a...))
			}
			span.End()
		}()
		c.Next()
	}
}
func BeginAttempt(c *gin.Context, channel int, model string) (func(*relaycommon.RelayInfo, *types.NewAPIError), context.Context) {
	if !enabled {
		return func(*relaycommon.RelayInfo, *types.NewAPIError) {}, c.Request.Context()
	}
	parent := c.Request.Context()
	ctx, span := tracer.Start(parent, "newapi.channel.attempt", trace.WithSpanKind(trace.SpanKindClient))
	c.Request = c.Request.WithContext(ctx)
	start := time.Now()
	return func(info *relaycommon.RelayInfo, err *types.NewAPIError) {
		outcome, status, kind := classify(ctx, info, err)
		group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
		if info != nil {
			group = info.UsingGroup
		}
		a := append(attrs(channel, model, outcome, kind, status), attribute.String("group", group))
		attempts.Add(ctx, 1, metric.WithAttributes(a...))
		duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(a...))
		span.SetAttributes(a...)
		if outcome == "failure" {
			span.SetStatus(codes.Error, kind)
		}
		span.End()
		c.Request = c.Request.WithContext(parent)
	}, ctx
}
func classify(ctx context.Context, info *relaycommon.RelayInfo, err *types.NewAPIError) (string, int, string) {
	status := 200
	code := ""
	outcome := "success"
	if err != nil {
		status = err.StatusCode
		code = string(err.GetErrorCode())
		outcome = "failure"
	}
	if info != nil {
		result := perfmetrics.ClassifyRelayOutcome(ctx, info, err)
		stream := info.StreamStatus.OutcomeSnapshot()
		// A peer may close after reading a complete Content-Length response
		// while billing finishes. The completed relay remains a health sample.
		complete := err == nil && !stream.HasErrors && (stream.Response == relaycommon.ResponseOutcomeCompleted || (!info.IsStream && stream.Response == relaycommon.ResponseOutcomeUnknown))
		if complete && !info.PerformanceBusinessRejection {
			result = perfmetrics.OutcomeSuccess
		}

		if result == perfmetrics.OutcomeIgnored {
			outcome = "excluded"
		}
		if result == perfmetrics.OutcomeFailure {
			outcome = "failure"
			if code == "" {
				code = "stream_error"
			}
		}
		if info.StreamStatus != nil {
			s := info.StreamStatus.OutcomeSnapshot()
			if s.ErrorStatus > 0 {
				status = s.ErrorStatus
			}
			if s.EndReason == relaycommon.StreamEndReasonTimeout {
				code = "timeout"
			}
		}
	}
	if err != nil {
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			code = "timeout"
		}
	}
	return outcome, status, ErrorKind(status, code)
}
func FinishRequest(c *gin.Context, info *relaycommon.RelayInfo, err *types.NewAPIError) {
	if !enabled || info == nil || info.ChannelMeta == nil {
		return
	}
	ctx := c.Request.Context()
	outcome, status, kind := classify(ctx, info, err)
	a := append(attrs(info.ChannelId, info.OriginModelName, outcome, kind, status), attribute.String("group", info.UsingGroup))
	requests.Add(ctx, 1, metric.WithAttributes(a...))
	requestDuration.Record(ctx, time.Since(info.StartTime).Seconds(), metric.WithAttributes(a...))
	if info.IsStream && info.HasSendResponse() {
		ttft.Record(ctx, info.FirstResponseTime.Sub(info.StartTime).Seconds(), metric.WithAttributes(a...))
	}
	if info.PerformanceOutputTokens > 0 {
		tokens.Add(ctx, int64(info.PerformanceOutputTokens), metric.WithAttributes(a...))
	}
	if outcome == "failure" {
		trace.SpanFromContext(ctx).SetStatus(codes.Error, kind)
	}
	c.Set("channel_monitor_recorded", true)
}
func Inject(ctx context.Context, header http.Header) {
	if enabled {
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(header))
	}
}
