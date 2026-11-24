package relay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	openaichannel "github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	openaisdk "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func earlyResponsesSettings(t *testing.T) {
	t.Helper()
	settings := operation_setting.GetGeneralSetting()
	old := *settings
	timeout := constant.StreamingTimeout
	settings.ResponsesEarlyAckEnabled = true
	settings.ResponsesEarlyAckDelayMS = operation_setting.DefaultResponsesEarlyAckDelayMS
	settings.PingIntervalEnabled = false
	constant.StreamingTimeout = 10
	t.Cleanup(func() { *settings = old; constant.StreamingTimeout = timeout })
	service.InitHttpClient()
}

func earlyResponsesContext(w http.ResponseWriter, r *http.Request, upstream string, request *dto.OpenAIResponsesRequest) (*gin.Context, *relaycommon.RelayInfo) {
	c, _ := gin.CreateTestContext(w)
	c.Request = r
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-6-astra")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelId, 142)
	common.SetContextKey(c, constant.ContextKeyUserId, 7)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, upstream)
	common.SetContextKey(c, constant.ContextKeyChannelKey, "test-key")
	return c, relaycommon.GenRelayInfoResponses(c, request)
}

func TestEarlyResponsesSDKLifecycle(t *testing.T) {
	earlyResponsesSettings(t)
	operation_setting.GetGeneralSetting().PingIntervalEnabled = true
	operation_setting.GetGeneralSetting().PingIntervalSeconds = 1
	for _, headersFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("headers_first_%v", headersFirst), func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if headersFirst {
					w.(http.Flusher).Flush()
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, earlyResponsesSuccessEvents)
			}))
			defer upstream.Close()
			type result struct {
				info  *relaycommon.RelayInfo
				usage *dto.Usage
				err   error
			}
			observed := make(chan result, 1)
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req dto.OpenAIResponsesRequest
				if err := common.DecodeJson(r.Body, &req); err != nil {
					observed <- result{err: err}
					return
				}
				c, info := earlyResponsesContext(w, r, upstream.URL, &req)
				info.ResponsesRetryManaged = true
				c.Set("responses_retry_managed", true)
				adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, &req)
				if apiErr != nil {
					observed <- result{err: apiErr}
					return
				}
				defer closer.Close()
				helper.PrepareEarlyResponses(info, body)
				defer info.EarlyResponses.Stop()
				response, err := adaptor.DoRequest(c, info, body)
				if err != nil {
					observed <- result{err: err}
					return
				}
				usage, apiErr := adaptor.DoResponse(c, response.(*http.Response), info)
				if apiErr != nil {
					observed <- result{err: apiErr}
					return
				}
				observed <- result{info: info, usage: usage.(*dto.Usage)}
			}))
			defer gateway.Close()
			// A second new-api parser models the caller's gateway FRT metric.
			peerObserved := make(chan *relaycommon.RelayInfo, 1)
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := &dto.OpenAIResponsesRequest{Model: "gpt-6-astra", Stream: common.GetPointer(true)}
				c, peerInfo := earlyResponsesContext(w, r, gateway.URL, req)
				peerInfo.InitChannelMeta(c)
				outgoing, err := http.NewRequestWithContext(r.Context(), http.MethodPost, gateway.URL+"/v1/responses", r.Body)
				if err != nil {
					return
				}
				outgoing.Header.Set("Content-Type", "application/json")
				// Use the actual relay body lifecycle, which cancels pending reads
				// when the terminal event closes the stream. Leave the timer nil
				// so the peer cannot generate its own ack and mask gateway latency.
				peerInfo.EarlyResponses = &relaycommon.EarlyResponsesState{}
				response, err := channel.DoRequest(c, outgoing, peerInfo)
				if err != nil {
					return
				}
				_, _ = openaichannel.OaiResponsesStreamHandler(c, peerInfo, response)
				peerObserved <- peerInfo
			}))
			defer peer.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sdk := openaisdk.NewClient(option.WithBaseURL(peer.URL+"/v1"), option.WithAPIKey("test"), option.WithMaxRetries(0))
			started := time.Now()
			stream := sdk.Responses.NewStreaming(ctx, responses.ResponseNewParams{Model: "gpt-6-astra", Input: responses.ResponseNewParamsInputUnion{OfString: openaisdk.String("hello")}})
			defer stream.Close()
			require.True(t, stream.Next(), "%v", stream.Err())
			ack := stream.Current()
			assert.Equal(t, "response.created", ack.Type)
			assert.Less(t, time.Since(started), time.Second, "peer must receive an event within one second while upstream is still blocked")
			assert.True(t, strings.HasPrefix(ack.Response.ID, service.EarlyResponseIDPrefix))
			assert.Equal(t, int64(0), ack.SequenceNumber)
			assert.Empty(t, ack.Response.Output)
			unblock()
			eventTypes := []string{ack.Type}
			text := ""
			sequence := int64(1)
			completed := false
			for stream.Next() {
				event := stream.Current()
				eventTypes = append(eventTypes, event.Type)
				assert.Equal(t, sequence, event.SequenceNumber)
				sequence++
				if event.Response.ID != "" {
					assert.Equal(t, ack.Response.ID, event.Response.ID)
				}
				if event.Type == "response.output_text.delta" {
					text += event.Delta.OfString
				}
				if event.Type == "response.completed" {
					completed = true
					assert.Equal(t, text, event.Response.OutputText())
					assert.Equal(t, int64(7), event.Response.Usage.InputTokens)
					assert.Equal(t, int64(3), event.Response.Usage.OutputTokens)
				}
			}
			require.NoError(t, stream.Err())
			assert.True(t, completed)
			assert.Equal(t, "hello", text, "no placeholder whitespace reaches final text")
			assert.Equal(t, 1, strings.Count(strings.Join(eventTypes, ","), "response.created"))
			got := <-observed
			require.NoError(t, got.err)
			assert.Equal(t, 7, got.usage.PromptTokens)
			assert.Equal(t, 3, got.usage.CompletionTokens)
			assert.Equal(t, "completed", got.info.StreamStatus.ResponseOutcome())
			assert.True(t, got.info.FirstResponseTime.After(got.info.EarlyResponses.SentAt))
			assert.True(t, got.info.FirstTextTime.After(got.info.FirstResponseTime))
			assert.Equal(t, 9, got.info.ReceivedResponseCount, "synthetic event is excluded from upstream event count")
			peerInfo := <-peerObserved
			assert.GreaterOrEqual(t, peerInfo.FirstResponseTime.Sub(peerInfo.StartTime), time.Duration(0))
			assert.Less(t, peerInfo.FirstResponseTime.Sub(peerInfo.StartTime), time.Second)
			assert.Equal(t, 10, peerInfo.ReceivedResponseCount, "peer observes the acknowledgment as an event")
			t.Logf("peer first event=%s; local ack=%s; real first text=%s", peerInfo.FirstResponseTime.Sub(peerInfo.StartTime), got.info.EarlyResponses.SentAt.Sub(got.info.StartTime), got.info.FirstTextTime.Sub(got.info.StartTime))
			upstreamID, err := service.ResolveEarlyResponseAlias(7, 142, ack.Response.ID)
			require.NoError(t, err)
			assert.Equal(t, "resp_real", upstreamID)
			_, err = service.ResolveEarlyResponseAlias(8, 142, ack.Response.ID)
			assert.Error(t, err, "aliases cannot cross users")
			_, err = service.ResolveEarlyResponseAlias(7, 143, ack.Response.ID)
			assert.Error(t, err, "aliases cannot cross channels")
			logContext, _ := gin.CreateTestContext(httptest.NewRecorder())
			other := model.NewLogOther()
			service.AppendRelayLogAdminInfo(logContext, got.info, other)
			assert.Equal(t, got.info.EarlyResponses.SentAt.Sub(got.info.StartTime).Milliseconds(), other.Snapshot()["early_ack_ms"])
			assert.Equal(t, got.info.FirstTextTime.Sub(got.info.StartTime).Milliseconds(), other.Snapshot()["first_text_ms"])
		})
	}
}

const earlyResponsesSuccessEvents = `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_real","object":"response","model":"gpt-6-astra","created_at":1,"status":"in_progress","output":[]}}

data: {"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_real","object":"response","status":"in_progress"}}

data: {"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"msg_real","type":"message","role":"assistant","status":"in_progress","content":[]}}

data: {"type":"response.content_part.added","sequence_number":3,"item_id":"msg_real","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}

data: {"type":"response.output_text.delta","sequence_number":4,"item_id":"msg_real","output_index":0,"content_index":0,"delta":"hello"}

data: {"type":"response.output_text.done","sequence_number":5,"item_id":"msg_real","output_index":0,"content_index":0,"text":"hello"}

data: {"type":"response.content_part.done","sequence_number":6,"item_id":"msg_real","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hello","annotations":[]}}

data: {"type":"response.output_item.done","sequence_number":7,"output_index":0,"item":{"id":"msg_real","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}}

data: {"type":"response.completed","sequence_number":8,"response":{"id":"resp_real","object":"response","model":"gpt-6-astra","created_at":1,"status":"completed","output":[{"id":"msg_real","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}

`

func TestEarlyResponsesEligibility(t *testing.T) {
	earlyResponsesSettings(t)
	for _, tc := range []struct {
		name, body    string
		enabled, want bool
	}{
		{"plain", "{\"stream\":true,\"input\":\"hello\"}", true, true},
		{"off", "{\"stream\":true,\"input\":\"hello\"}", false, false},
		{"nonstream", "{\"stream\":false,\"input\":\"hello\"}", true, false},
		{"tools", "{\"stream\":true,\"input\":\"hello\",\"tools\":[{\"type\":\"function\",\"name\":\"test\"}]}", true, true},
		{"structured", "{\"stream\":true,\"input\":\"hello\",\"text\":{\"format\":{\"type\":\"json_schema\"}}}", true, true},
		{"tool_result", "{\"stream\":true,\"input\":[{\"type\":\"function_call_output\",\"output\":\"ok\"}]}", true, true},
		{"background", "{\"stream\":true,\"input\":\"hello\",\"background\":true}", true, true},
		{"conversation", "{\"stream\":true,\"input\":\"hello\",\"conversation\":\"conv_1\"}", true, true},
		{"image", "{\"stream\":true,\"input\":[{\"role\":\"user\",\"content\":[{\"type\":\"input_image\"}]}]}", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			operation_setting.GetGeneralSetting().ResponsesEarlyAckEnabled = tc.enabled
			info := &relaycommon.RelayInfo{IsStream: true, RelayMode: relayconstant.RelayModeResponses, RelayFormat: types.RelayFormatOpenAIResponses, StartTime: time.Now(), ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI}}
			body, closer, err := relaycommon.NewOutboundJSONBody([]byte(tc.body))
			require.NoError(t, err)
			defer closer.Close()
			helper.PrepareEarlyResponses(info, body)
			defer info.EarlyResponses.Stop()
			assert.Equal(t, tc.want, info.EarlyResponses != nil)
		})
	}
}

func TestEarlyAdvancedCustomEligibility(t *testing.T) {
	earlyResponsesSettings(t)
	for _, tc := range []struct {
		name                 string
		downstream, upstream types.RelayFormat
		mode                 int
		stream               bool
		reason               string
	}{
		{"native_chat", types.RelayFormatOpenAI, types.RelayFormatOpenAI, relayconstant.RelayModeChatCompletions, true, "eligible"},
		{"native_responses", types.RelayFormatOpenAIResponses, types.RelayFormatOpenAIResponses, relayconstant.RelayModeResponses, true, "eligible"},
		{"chat_to_responses", types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses, relayconstant.RelayModeChatCompletions, true, "eligible"},
		{"responses_to_chat", types.RelayFormatOpenAIResponses, types.RelayFormatOpenAI, relayconstant.RelayModeResponses, true, "eligible"},
		{"chat_to_claude", types.RelayFormatOpenAI, types.RelayFormatClaude, relayconstant.RelayModeChatCompletions, true, "unsupported_upstream_protocol"},
		{"responses_to_gemini", types.RelayFormatOpenAIResponses, types.RelayFormatGemini, relayconstant.RelayModeResponses, true, "unsupported_upstream_protocol"},
		{"claude_to_chat", types.RelayFormatClaude, types.RelayFormatOpenAI, relayconstant.RelayModeChatCompletions, true, "unsupported_protocol"},
		{"gemini_to_responses", types.RelayFormatGemini, types.RelayFormatOpenAIResponses, relayconstant.RelayModeResponses, true, "unsupported_protocol"},
		{"nonstream", types.RelayFormatOpenAI, types.RelayFormatOpenAI, relayconstant.RelayModeChatCompletions, false, "nonstream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{IsStream: tc.stream, RelayMode: tc.mode, RelayFormat: tc.downstream, FinalRequestRelayFormat: tc.upstream, StartTime: time.Now(), ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeAdvancedCustom}}
			body, closer, err := relaycommon.NewOutboundJSONBody([]byte(`{"stream":true}`))
			require.NoError(t, err)
			defer closer.Close()
			helper.PrepareEarlyResponses(info, body)
			defer info.EarlyResponses.Stop()
			assert.Equal(t, tc.reason, info.EarlyAckReason)
			assert.Equal(t, tc.reason == "eligible", info.EarlyResponses != nil)
		})
	}
}

func TestEarlyAdvancedCustomStreamLifecycle(t *testing.T) {
	earlyResponsesSettings(t)
	for _, tc := range []struct {
		name, incoming, upstream, converter string
		headersFirst, passthrough, failure  bool
	}{
		{"chat", "/v1/chat/completions", "/v1/chat/completions", "", false, false, false},
		{"responses", "/v1/responses", "/v1/responses", "", false, false, false},
		{"chat_to_responses", "/v1/chat/completions", "/v1/responses", relayconvert.ConverterOpenAIChatToOpenAIResponses, false, false, false},
		{"responses_to_chat", "/v1/responses", "/v1/chat/completions", relayconvert.ConverterOpenAIResponsesToOpenAIChat, false, false, false},
		{"chat_headers_first", "/v1/chat/completions", "/v1/chat/completions", "", true, false, false},
		{"responses_headers_first", "/v1/responses", "/v1/responses", "", true, false, false},
		{"chat_passthrough", "/v1/chat/completions", "/v1/chat/completions", "", false, true, false},
		{"responses_passthrough", "/v1/responses", "/v1/responses", "", false, true, false},
		{"responses_to_chat_failure", "/v1/responses", "/v1/chat/completions", relayconvert.ConverterOpenAIResponsesToOpenAIChat, false, false, true},
		{"chat_to_responses_failure", "/v1/chat/completions", "/v1/responses", relayconvert.ConverterOpenAIChatToOpenAIResponses, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, tc.upstream, r.URL.Path)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.headersFirst {
					w.(http.Flusher).Flush()
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				wire := earlyChatTextEvents
				if tc.upstream == "/v1/responses" {
					wire = earlyResponsesSuccessEvents
				}
				if tc.failure {
					upstreamError := map[string]any{"message": "upstream failed", "type": "server_error", "code": "server_error"}
					payload := map[string]any{"error": upstreamError}
					if tc.upstream == "/v1/responses" {
						payload = map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_real", "status": "failed", "error": upstreamError}}
					}
					raw, err := common.Marshal(payload)
					if !assert.NoError(t, err) {
						return
					}
					wire = "data: " + string(raw) + "\n\n"
				}
				_, _ = io.WriteString(w, wire)
			}))
			defer upstream.Close()
			type result struct {
				info  *relaycommon.RelayInfo
				usage *dto.Usage
				err   error
			}
			observed := make(chan result, 1)
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				chatReq := &dto.GeneralOpenAIRequest{Model: "gpt-6-astra", Stream: common.GetPointer(true), Messages: []dto.Message{{Role: "user", Content: "hello"}}}
				responsesReq := &dto.OpenAIResponsesRequest{Model: "gpt-6-astra", Stream: common.GetPointer(true), Input: []byte("\"hello\"")}
				c, info := earlyResponsesContext(w, r, upstream.URL, responsesReq)
				if tc.incoming == "/v1/chat/completions" {
					c, info = earlyChatContext(w, r, upstream.URL, chatReq)
				}
				earlyAdvancedCustomChannel(c, info, dto.AdvancedCustomRoute{IncomingPath: tc.incoming, UpstreamPath: tc.upstream, Converter: tc.converter, PassThroughBodyEnabled: tc.passthrough})
				adaptor := GetAdaptor(info.ApiType)
				adaptor.Init(info)
				var converted any
				var err error
				if tc.passthrough {
					converted = responsesReq
					if tc.incoming == "/v1/chat/completions" {
						converted = chatReq
					}
				} else if tc.incoming == "/v1/chat/completions" {
					converted, err = adaptor.ConvertOpenAIRequest(c, info, chatReq)
				} else {
					converted, err = adaptor.ConvertOpenAIResponsesRequest(c, info, *responsesReq)
				}
				if err != nil {
					observed <- result{err: err}
					return
				}
				relaycommon.AppendRequestConversionFromRequest(info, converted)
				raw, err := common.Marshal(converted)
				if err != nil {
					observed <- result{err: err}
					return
				}
				body, closer, err := relaycommon.NewOutboundJSONBody(raw)
				if err != nil {
					observed <- result{err: err}
					return
				}
				defer closer.Close()
				helper.PrepareEarlyResponses(info, body)
				defer info.EarlyResponses.Stop()
				response, err := adaptor.DoRequest(c, info, body)
				if err != nil {
					observed <- result{err: err}
					return
				}
				usage, apiErr := adaptor.DoResponse(c, response.(*http.Response), info)
				if apiErr != nil {
					observed <- result{info: info, err: apiErr}
					return
				}
				observed <- result{info: info, usage: usage.(*dto.Usage)}
			}))
			defer gateway.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+tc.incoming, nil)
			require.NoError(t, err)
			started := time.Now()
			response, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer response.Body.Close()
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.NotEmpty(t, response.Header.Get("X-Newapi-Early-Ack"))
			scanner := bufio.NewScanner(response.Body)
			var events []gjson.Result
			for scanner.Scan() {
				data, ok := strings.CutPrefix(scanner.Text(), "data: ")
				if !ok || data == "[DONE]" {
					continue
				}
				event := gjson.Parse(data)
				events = append(events, event)
				if len(events) == 1 {
					assert.Less(t, time.Since(started), time.Second)
					assert.Empty(t, event.Get("choices.0.delta.content").String())
					assert.False(t, event.Get("usage").Exists())
					unblock()
				}
			}
			require.NoError(t, scanner.Err())
			require.NotEmpty(t, events)
			got := <-observed
			if tc.failure {
				require.Error(t, got.err)
			} else {
				require.NoError(t, got.err)
				require.NotNil(t, got.usage)
				assert.Equal(t, 10, got.usage.TotalTokens)
			}
			require.NotNil(t, got.info.EarlyResponses)
			assert.True(t, got.info.EarlyResponses.Finished)
			assert.Equal(t, "sent", got.info.EarlyAckReason)
			var text strings.Builder
			created, completed, failed := 0, false, 0
			for i, event := range events {
				if tc.incoming == "/v1/chat/completions" {
					if event.Get("error").Exists() {
						failed++
						continue
					}
					assert.Equal(t, events[0].Get("id").String(), event.Get("id").String())
					text.WriteString(event.Get("choices.0.delta.content").String())
					if event.Get("choices.0.finish_reason").String() == "stop" {
						completed = true
					}
				} else {
					assert.Equal(t, int64(i), event.Get("sequence_number").Int())
					if id := event.Get("response.id"); id.Exists() {
						assert.Equal(t, events[0].Get("response.id").String(), id.String())
					}
					switch event.Get("type").String() {
					case "response.created":
						created++
					case "response.output_text.delta":
						text.WriteString(event.Get("delta").String())
					case "response.completed":
						completed = true
					case "response.failed":
						failed++
					}
				}
			}
			if tc.incoming == "/v1/responses" {
				assert.Equal(t, 1, created)
			}
			if tc.failure {
				assert.False(t, completed)
				assert.Equal(t, 1, failed)
				assert.Empty(t, text.String())
			} else {
				assert.True(t, completed)
				assert.Zero(t, failed)
				assert.Equal(t, "hello", text.String())
			}
			t.Logf("early ack: %s", got.info.EarlyResponses.SentAt.Sub(got.info.StartTime))
		})
	}
}

func earlyAdvancedCustomChannel(c *gin.Context, info *relaycommon.RelayInfo, route dto.AdvancedCustomRoute) {
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeAdvancedCustom)
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{route}}})
	info.InitChannelMeta(c)
}

func TestEarlyResponsesHTTPFailure(t *testing.T) {
	earlyResponsesSettings(t)
	for _, tc := range []struct {
		name            string
		status          int
		early, advanced bool
	}{
		{"late_429", 429, true, false}, {"late_502", 502, true, false}, {"early_429", 429, false, false}, {"early_502", 502, false, false}, {"advanced_late_429", 429, true, true}, {"advanced_late_502", 502, true, true}, {"advanced_early_502", 502, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			operation_setting.GetGeneralSetting().ResponsesEarlyAckDelayMS = operation_setting.DefaultResponsesEarlyAckDelayMS
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.early {
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "{\"error\":{\"message\":\"upstream rejected\",\"type\":\"server_error\",\"code\":\"test_error\"}}")
			}))
			defer upstream.Close()
			type result struct {
				info     *relaycommon.RelayInfo
				apiErr   *types.NewAPIError
				decision service.PolicyDecision
				reserved int
			}
			observed := make(chan result, 1)
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := &dto.OpenAIResponsesRequest{Model: "gpt-6-astra", Input: []byte("\"hello\""), Stream: common.GetPointer(true)}
				c, info := earlyResponsesContext(w, r, upstream.URL, req)
				if tc.advanced {
					earlyAdvancedCustomChannel(c, info, dto.AdvancedCustomRoute{IncomingPath: "/v1/responses", UpstreamPath: "/v1/responses"})
				}
				if !tc.early {
					operation_setting.GetGeneralSetting().ResponsesEarlyAckDelayMS = 1500
				}
				reservation := &earlyAckReservation{held: 100}
				info.Billing = reservation
				apiErr := ResponsesHelper(c, info)
				apiErr = RefundFailedRequestBilling(c, info, apiErr)
				if !c.GetBool("responses_early_ack_committed") && apiErr != nil {
					c.JSON(apiErr.StatusCode, gin.H{"error": apiErr.ToOpenAIError()})
				}
				observed <- result{info: info, apiErr: apiErr, decision: service.DecideRelayRetry(c, apiErr, 3), reserved: reservation.held}
			}))
			defer gateway.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sdk := openaisdk.NewClient(option.WithBaseURL(gateway.URL+"/v1"), option.WithAPIKey("test"), option.WithMaxRetries(0))
			stream := sdk.Responses.NewStreaming(ctx, responses.ResponseNewParams{Model: "gpt-6-astra", Input: responses.ResponseNewParamsInputUnion{OfString: openaisdk.String("hello")}})
			defer stream.Close()
			if tc.early {
				require.True(t, stream.Next(), "%v", stream.Err())
				first := stream.Current()
				assert.Equal(t, "response.created", first.Type)
				unblock()
				require.True(t, stream.Next(), "%v", stream.Err())
				failed := stream.Current()
				assert.Equal(t, "response.failed", failed.Type)
				assert.Equal(t, first.Response.ID, failed.Response.ID)
				assert.Equal(t, int64(1), failed.SequenceNumber)
				assert.False(t, stream.Next())
				require.NoError(t, stream.Err())
			} else {
				assert.False(t, stream.Next())
				var apiError *openaisdk.Error
				require.ErrorAs(t, stream.Err(), &apiError)
				assert.Equal(t, tc.status, apiError.StatusCode)
			}
			got := <-observed
			require.NotNil(t, got.apiErr)
			assert.Equal(t, tc.status, got.apiErr.StatusCode, "logs/refunds retain original upstream status")
			assert.Zero(t, got.reserved, "upstream HTTP rejection refunds the reserved quota despite early HTTP 200")
			assert.Zero(t, got.info.ReceivedResponseCount)
			assert.True(t, got.info.FirstTextTime.IsZero())
			assert.False(t, got.info.FirstResponseTime.After(got.info.StartTime))
			if tc.early {
				assert.Equal(t, "stop", got.decision.Action)
				assert.True(t, types.IsSkipRetryError(got.apiErr))
				assert.Equal(t, "failed", got.info.StreamStatus.ResponseOutcome())
			}
		})
	}
}

func TestEarlyResponsesAliasRequest(t *testing.T) {
	earlyResponsesSettings(t)
	const alias = "resp_newapi_ack_continuation"
	const upstreamID = "resp_newapi_ack_from_another_gateway"
	require.NoError(t, service.SaveEarlyResponseAlias(7, 142, alias, upstreamID))
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough_%v", passthrough), func(t *testing.T) {
			req := &dto.OpenAIResponsesRequest{Model: "gpt-6-astra", Input: []byte("\"hello\""), PreviousResponseID: alias, Stream: common.GetPointer(true)}
			raw, err := common.Marshal(req)
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(raw)))
			request.Header.Set("Content-Type", "application/json")
			c, info := earlyResponsesContext(httptest.NewRecorder(), request, "http://upstream.invalid", req)
			common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: passthrough})
			_, body, closer, apiErr := PrepareResponsesRequest(c, info, req)
			require.Nil(t, apiErr)
			defer closer.Close()
			wire, err := io.ReadAll(body)
			require.NoError(t, err)
			assert.Equal(t, upstreamID, gjson.GetBytes(wire, "previous_response_id").String())
			assert.Equal(t, alias, req.PreviousResponseID, "do not mutate original request across retries")
			assert.Equal(t, alias, info.ResponsePreviousAlias)
		})
	}
}

func TestEarlyResponsesStreamFailures(t *testing.T) {
	earlyResponsesSettings(t)
	for _, tc := range []struct {
		name, wire, outcome string
		prompt, completion  int
		ack                 bool
	}{
		{name: "empty EOF", outcome: "failed", ack: true},
		{name: "malformed", wire: "data: {broken}\n\n", outcome: "failed", ack: true},
		{name: "upstream failure", wire: "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_error\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"failure\"}}}\n\n", outcome: "failed", ack: true},
		{name: "incomplete usage", wire: "data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_partial\",\"status\":\"incomplete\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n", outcome: "incomplete", prompt: 7, completion: 3, ack: true},
		{name: "real created then EOF", wire: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_started\",\"status\":\"in_progress\"}}\n\n", outcome: "failed", prompt: 11, ack: true},
		{name: "real event beats timer", wire: earlyResponsesSuccessEvents, outcome: "completed", prompt: 7, completion: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := &dto.OpenAIResponsesRequest{Model: "gpt-6-astra", Input: []byte("\"hello\""), Stream: common.GetPointer(true)}
			c, info := earlyResponsesContext(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "http://upstream.invalid", req)
			info.InitChannelMeta(c)
			info.SetEstimatePromptTokens(11)
			raw, err := common.Marshal(req)
			require.NoError(t, err)
			body, closer, err := relaycommon.NewOutboundJSONBody(raw)
			require.NoError(t, err)
			defer closer.Close()
			helper.PrepareEarlyResponses(info, body)
			require.NotNil(t, info.EarlyResponses)
			defer info.EarlyResponses.Stop()
			if tc.ack {
				require.NoError(t, helper.SendEarlyResponsesAck(c, info))
			}
			usage, apiErr := openaichannel.OaiResponsesStreamHandler(c, info, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.wire))})
			require.Nil(t, apiErr)
			assert.Equal(t, tc.prompt, usage.PromptTokens)
			assert.Equal(t, tc.completion, usage.CompletionTokens)
			assert.Equal(t, tc.outcome, info.StreamStatus.ResponseOutcome())
			service.MarkRequestPolicySuccess(c, info.StreamStatus)
			assert.Equal(t, tc.outcome == "completed", service.RequestPolicy(c).Successful)
			if tc.ack {
				assert.Contains(t, recorder.Body.String(), "event: response.created")
				assert.Contains(t, recorder.Body.String(), "event: response."+tc.outcome)
				assert.NotContains(t, recorder.Body.String(), "\"delta\":\" \"")
			} else {
				assert.True(t, info.EarlyResponses.SentAt.IsZero())
				assert.Contains(t, recorder.Body.String(), "\"id\":\"resp_real\"")
				assert.NotContains(t, recorder.Body.String(), service.EarlyResponseIDPrefix)
			}
		})
	}
}

func TestEarlyResponsesClientCancellation(t *testing.T) {
	earlyResponsesSettings(t)
	for _, tc := range []struct{ headersFirst, advanced bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("headers_first_%v_advanced_%v", tc.headersFirst, tc.advanced), func(t *testing.T) {
			upstreamCancelled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// A real upstream consumes the request before awaiting generation. Go's
				// HTTP server detects disconnects after the request body has been read.
				_, _ = io.Copy(io.Discard, r.Body)
				if tc.headersFirst {
					w.Header().Set("Content-Type", "text/event-stream")
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
				close(upstreamCancelled)
			}))
			defer upstream.Close()
			finished := make(chan struct{})
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)
				req := &dto.OpenAIResponsesRequest{Model: "gpt-6-astra", Input: []byte("\"hello\""), Stream: common.GetPointer(true)}
				c, info := earlyResponsesContext(w, r, upstream.URL, req)
				if tc.advanced {
					earlyAdvancedCustomChannel(c, info, dto.AdvancedCustomRoute{IncomingPath: "/v1/responses", UpstreamPath: "/v1/responses"})
				}
				adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, req)
				if apiErr != nil {
					return
				}
				defer closer.Close()
				helper.PrepareEarlyResponses(info, body)
				defer info.EarlyResponses.Stop()
				response, err := adaptor.DoRequest(c, info, body)
				if err != nil {
					return
				}
				_, _ = adaptor.DoResponse(c, response.(*http.Response), info)
			}))
			defer gateway.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sdk := openaisdk.NewClient(option.WithBaseURL(gateway.URL+"/v1"), option.WithAPIKey("test"), option.WithMaxRetries(0))
			stream := sdk.Responses.NewStreaming(ctx, responses.ResponseNewParams{Model: "gpt-6-astra", Input: responses.ResponseNewParamsInputUnion{OfString: openaisdk.String("hello")}})
			defer stream.Close()
			require.True(t, stream.Next(), "%v", stream.Err())
			assert.Equal(t, "response.created", stream.Current().Type)
			cancel()
			require.NoError(t, stream.Close())
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("gateway did not stop after cancellation")
			}
			select {
			case <-upstreamCancelled:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream request was not canceled")
			}
		})
	}
}

// Models the existing BillingSettler contract without connecting to a ledger.
type earlyAckReservation struct{ held int }

func (r *earlyAckReservation) Settle(quota int) error   { r.held = 0; return nil }
func (r *earlyAckReservation) Refund(*gin.Context)      { r.held = 0 }
func (r *earlyAckReservation) NeedsRefund() bool        { return r.held > 0 }
func (r *earlyAckReservation) GetPreConsumedQuota() int { return r.held }
func (r *earlyAckReservation) Reserve(quota int) error  { r.held = quota; return nil }

func earlyChatContext(w http.ResponseWriter, r *http.Request, upstream string, req *dto.GeneralOpenAIRequest) (*gin.Context, *relaycommon.RelayInfo) {
	c, _ := earlyResponsesContext(w, r, upstream, &dto.OpenAIResponsesRequest{})
	info := relaycommon.GenRelayInfoOpenAI(c, req)
	info.InitChannelMeta(c)
	info.ShouldIncludeUsage = true
	return c, info
}

const earlyChatTextEvents = "data: {\"id\":\"chat-real\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-6-astra\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat-real\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-6-astra\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat-real\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-6-astra\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\ndata: [DONE]\n\n"

func TestEarlyChatSDKLifecycle(t *testing.T) {
	earlyResponsesSettings(t)
	for _, tc := range []struct {
		name, wire, text           string
		viaResponses, headersFirst bool
	}{
		{"real_first", earlyChatTextEvents, "hello", false, false},
		{"native_text", earlyChatTextEvents, "hello", false, false},
		{"native_headers_first", earlyChatTextEvents, "hello", false, true},
		{"converted_responses", earlyResponsesSuccessEvents, "hello", true, false},
		{"structured", strings.ReplaceAll(earlyChatTextEvents, "hello", "{\\\"ok\\\":true}"), "{\"ok\":true}", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.name == "real_first" {
					first, _, _ := strings.Cut(tc.wire, "\n\n")
					_, _ = io.WriteString(w, first+"\n\n")
					w.(http.Flusher).Flush()
				}
				if tc.headersFirst {
					w.(http.Flusher).Flush()
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, tc.wire)
			}))
			defer upstream.Close()
			type result struct {
				info  *relaycommon.RelayInfo
				usage *dto.Usage
				err   error
			}
			observed := make(chan result, 1)
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req dto.GeneralOpenAIRequest
				if err := common.DecodeJson(r.Body, &req); err != nil {
					observed <- result{err: err}
					return
				}
				c, info := earlyChatContext(w, r, upstream.URL, &req)
				adaptor := &openaichannel.Adaptor{}
				adaptor.Init(info)
				defer func() { info.EarlyResponses.Stop() }()
				if tc.viaResponses {
					u, err := textRequestViaResponses(c, info, adaptor, &req)
					if err != nil {
						observed <- result{err: err}
						return
					}
					observed <- result{info: info, usage: u}
					return
				}
				raw, err := common.Marshal(req)
				if err != nil {
					observed <- result{err: err}
					return
				}
				body, closer, err := relaycommon.NewOutboundJSONBody(raw)
				if err != nil {
					observed <- result{err: err}
					return
				}
				defer closer.Close()
				helper.PrepareEarlyResponses(info, body)
				resp, err := adaptor.DoRequest(c, info, body)
				if err != nil {
					observed <- result{err: err}
					return
				}
				u, apiErr := adaptor.DoResponse(c, resp.(*http.Response), info)
				if apiErr != nil {
					observed <- result{err: apiErr}
					return
				}
				observed <- result{info: info, usage: u.(*dto.Usage)}
			}))
			defer gateway.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// First read at the gateway boundary: an unmodified peer may buffer
			// one frame downstream, but its scanner must still record our ack.
			client := openaisdk.NewClient(option.WithBaseURL(gateway.URL+"/v1"), option.WithAPIKey("test"))
			started := time.Now()
			stream := client.Chat.Completions.NewStreaming(ctx, openaisdk.ChatCompletionNewParams{Model: "gpt-6-astra", Messages: []openaisdk.ChatCompletionMessageParamUnion{openaisdk.UserMessage("hello")}})
			defer stream.Close()
			require.True(t, stream.Next(), "%v", stream.Err())
			first := stream.Current()
			assert.Less(t, time.Since(started), time.Second)
			require.Len(t, first.Choices, 1)
			assert.Equal(t, "assistant", string(first.Choices[0].Delta.Role))
			assert.Empty(t, first.Choices[0].Delta.Content)
			assert.Empty(t, first.Choices[0].FinishReason)
			assert.Zero(t, first.Usage.TotalTokens)
			unblock()
			text := ""
			finish := ""
			total := int64(0)
			for stream.Next() {
				chunk := stream.Current()
				assert.Equal(t, first.ID, chunk.ID)
				assert.Equal(t, first.Created, chunk.Created)
				for _, ch := range chunk.Choices {
					text += ch.Delta.Content
					if ch.FinishReason != "" {
						finish = ch.FinishReason
					}
				}
				if chunk.Usage.TotalTokens > 0 {
					total = chunk.Usage.TotalTokens
				}
			}
			require.NoError(t, stream.Err())
			assert.Equal(t, tc.text, text)
			assert.Equal(t, "stop", finish)
			assert.Equal(t, int64(10), total)
			got := <-observed
			require.NoError(t, got.err)
			require.NotNil(t, got.info.EarlyResponses)
			assert.Equal(t, 10, got.usage.TotalTokens)
			assert.True(t, got.info.FirstResponseTime.After(got.info.EarlyResponses.SentAt))
			assert.True(t, got.info.FirstTextTime.After(got.info.EarlyResponses.SentAt))
			if tc.name == "real_first" {
				assert.True(t, got.info.EarlyResponses.SentAt.IsZero(), "real role chunk must be flushed before another event arrives")
				assert.Equal(t, "upstream_first", got.info.EarlyAckReason)
			} else {
				assert.False(t, got.info.EarlyResponses.SentAt.IsZero())
			}
			assert.True(t, got.info.EarlyResponses.Finished)
			// Replaying the observed acknowledgment through a second new-api
			// parser proves it counts the role event as its first response.
			peerRecorder := httptest.NewRecorder()
			pc, pi := earlyChatContext(peerRecorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "", &dto.GeneralOpenAIRequest{Stream: common.GetPointer(true)})
			ackRaw, err := common.Marshal(map[string]any{"id": first.ID, "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}}}})
			require.NoError(t, err)
			_, apiErr := openaichannel.OaiStreamHandler(pc, pi, &http.Response{Body: io.NopCloser(strings.NewReader("data: " + string(ackRaw) + "\n\n" + earlyChatTextEvents))})
			require.Nil(t, apiErr)
			assert.Less(t, pi.FirstResponseTime.Sub(pi.StartTime), time.Second)
		})
	}
}

func TestEarlyResponsesToolsAndStructured(t *testing.T) {
	earlyResponsesSettings(t)
	const bodyJSON = "{\"model\":\"gpt-6-astra\",\"stream\":true,\"tools\":[{\"type\":\"function\",\"name\":\"lookup\",\"parameters\":{\"type\":\"object\"}}],\"tool_choice\":{\"type\":\"function\",\"name\":\"lookup\"},\"text\":{\"format\":{\"type\":\"json_schema\",\"name\":\"result\",\"schema\":{\"type\":\"object\"}}},\"input\":[{\"type\":\"function_call_output\",\"call_id\":\"call_previous\",\"output\":\"ok\"},{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\"}]}"
	var req dto.OpenAIResponsesRequest
	require.NoError(t, common.UnmarshalJsonStr(bodyJSON, &req))
	w := httptest.NewRecorder()
	c, info := earlyResponsesContext(w, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "", &req)
	info.InitChannelMeta(c)
	body, closer, err := relaycommon.NewOutboundJSONBody([]byte(bodyJSON))
	require.NoError(t, err)
	defer closer.Close()
	helper.PrepareEarlyResponses(info, body)
	require.NotNil(t, info.EarlyResponses)
	defer info.EarlyResponses.Stop()
	require.NoError(t, helper.SendEarlyResponsesAck(c, info))
	lines := strings.Split(w.Body.String(), "\n")
	var ack string
	for _, l := range lines {
		if strings.HasPrefix(l, "data: ") {
			ack = strings.TrimPrefix(l, "data: ")
		}
	}
	for _, key := range []string{"tools", "tool_choice", "text"} {
		assert.JSONEq(t, gjson.Get(bodyJSON, key).Raw, gjson.Get(ack, "response."+key).Raw)
	}
	r, err := body.NewReader()
	require.NoError(t, err)
	original, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.JSONEq(t, bodyJSON, string(original))
	wire := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_tool\",\"status\":\"in_progress\"}}\n\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"fc_1\",\"delta\":\"{}\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_tool\",\"status\":\"completed\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{}\"}],\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n"
	u, apiErr := openaichannel.OaiResponsesStreamHandler(c, info, &http.Response{Body: io.NopCloser(strings.NewReader(wire))})
	require.Nil(t, apiErr)
	assert.Equal(t, 10, u.TotalTokens)
	assert.True(t, info.FirstTextTime.IsZero())
	var events []string
	for _, l := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(l, "data: ") {
			events = append(events, strings.TrimPrefix(l, "data: "))
		}
	}
	require.Len(t, events, 5)
	assert.Equal(t, "{}", gjson.Get(events[3], "delta").String())
	assert.Equal(t, "call_1", gjson.Get(events[4], "response.output.0.call_id").String())
	assert.Equal(t, "{}", gjson.Get(events[4], "response.output.0.arguments").String())
	for i, e := range events {
		assert.Equal(t, int64(i), gjson.Get(e, "sequence_number").Int())
	}
	assert.Equal(t, info.EarlyResponses.ID, gjson.Get(events[4], "response.id").String())
}

func TestEarlyChatHTTPFailure(t *testing.T) {
	earlyResponsesSettings(t)
	for _, tc := range []struct {
		name            string
		status          int
		early, advanced bool
	}{{"late_429", 429, true, false}, {"late_502", 502, true, false}, {"early_429", 429, false, false}, {"early_502", 502, false, false}, {"advanced_late_429", 429, true, true}, {"advanced_late_502", 502, true, true}, {"advanced_early_502", 502, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			operation_setting.GetGeneralSetting().ResponsesEarlyAckDelayMS = operation_setting.DefaultResponsesEarlyAckDelayMS
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if tc.early {
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "{\"error\":{\"code\":\"test_error\",\"type\":\"server_error\",\"message\":\"upstream rejected\"}}")
			}))
			defer upstream.Close()
			type result struct {
				info     *relaycommon.RelayInfo
				apiErr   *types.NewAPIError
				decision service.PolicyDecision
				held     int
			}
			observed := make(chan result, 1)
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := &dto.GeneralOpenAIRequest{Model: "gpt-6-astra", Stream: common.GetPointer(true), Messages: []dto.Message{{Role: "user", Content: "hello"}}}
				c, info := earlyChatContext(w, r, upstream.URL, req)
				if tc.advanced {
					earlyAdvancedCustomChannel(c, info, dto.AdvancedCustomRoute{IncomingPath: "/v1/chat/completions", UpstreamPath: "/v1/chat/completions"})
				}
				reservation := &earlyAckReservation{held: 100}
				info.Billing = reservation
				apiErr := TextHelper(c, info)
				apiErr = RefundFailedRequestBilling(c, info, apiErr)
				if !c.GetBool("responses_early_ack_committed") && apiErr != nil {
					c.JSON(apiErr.StatusCode, gin.H{"error": apiErr.ToOpenAIError()})
				}
				observed <- result{info, apiErr, service.DecideRelayRetry(c, apiErr, 3), reservation.held}
			}))
			defer gateway.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sdk := openaisdk.NewClient(option.WithBaseURL(gateway.URL+"/v1"), option.WithAPIKey("test"), option.WithMaxRetries(0))
			stream := sdk.Chat.Completions.NewStreaming(ctx, openaisdk.ChatCompletionNewParams{Model: "gpt-6-astra", Messages: []openaisdk.ChatCompletionMessageParamUnion{openaisdk.UserMessage("hello")}})
			defer stream.Close()
			if tc.early {
				require.True(t, stream.Next(), "%v", stream.Err())
				require.Len(t, stream.Current().Choices, 1)
				assert.Empty(t, stream.Current().Choices[0].Delta.Content)
				unblock()
			}
			assert.False(t, stream.Next())
			require.Error(t, stream.Err())
			assert.Contains(t, stream.Err().Error(), "upstream rejected")
			got := <-observed
			require.NotNil(t, got.apiErr)
			assert.Equal(t, tc.status, got.apiErr.StatusCode)
			assert.Zero(t, got.held)
			assert.Zero(t, got.info.ReceivedResponseCount)
			assert.True(t, got.info.FirstTextTime.IsZero())
			if tc.early {
				assert.Equal(t, "stop", got.decision.Action)
				assert.Equal(t, "failed", got.info.StreamStatus.ResponseOutcome())
				assert.True(t, types.IsSkipRetryError(got.apiErr))
			} else {
				assert.False(t, got.info.EarlyResponses.SentAt.After(got.info.StartTime))
			}
		})
	}
}

func TestEarlyChatToolsAndFailureAccounting(t *testing.T) {
	earlyResponsesSettings(t)
	const toolWire = "data: {\"id\":\"chat-real\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-6-astra\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\"}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat-real\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-6-astra\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat-real\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-6-astra\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\ndata: [DONE]\n\n"
	for _, tc := range []struct {
		name, wire string
		total      int
		success    bool
	}{
		{"tool_arguments", toolWire, 10, true},
		{"empty_eof", "", 0, false},
		{"malformed", "data: {broken}\n\n", 0, false},
		{"upstream_error", "data: {\"error\":{\"code\":\"quota\",\"type\":\"rate_limit_error\",\"message\":\"limited\"}}\n\n", 0, false},
		{"upstream_error_with_usage", "data: {\"error\":{\"code\":\"failed\",\"message\":\"upstream failed\"},\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n", 10, false},
		{"partial_failure", strings.ReplaceAll(earlyChatTextEvents, "\"finish_reason\":\"stop\"", "\"finish_reason\":null"), 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := &dto.GeneralOpenAIRequest{Model: "gpt-6-astra", Stream: common.GetPointer(true)}
			c, info := earlyChatContext(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "", req)
			info.SetEstimatePromptTokens(11)
			body, closer, err := relaycommon.NewOutboundJSONBody([]byte("{\"stream\":true,\"messages\":[],\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"lookup\"}}]}"))
			require.NoError(t, err)
			defer closer.Close()
			helper.PrepareEarlyResponses(info, body)
			require.NotNil(t, info.EarlyResponses)
			defer info.EarlyResponses.Stop()
			require.NoError(t, helper.SendEarlyResponsesAck(c, info))
			usage, apiErr := openaichannel.OaiStreamHandler(c, info, &http.Response{Body: io.NopCloser(strings.NewReader(tc.wire))})
			if !tc.success && tc.total == 0 {
				require.NotNil(t, apiErr)
				reservation := &earlyAckReservation{held: 100}
				info.Billing = reservation
				RefundFailedRequestBilling(c, info, apiErr)
				assert.Zero(t, reservation.held)
				assert.Nil(t, usage)
			} else {
				require.Nil(t, apiErr)
				assert.Equal(t, tc.total, usage.TotalTokens)
			}
			output := w.Body.String()
			assert.Equal(t, tc.success, strings.Contains(output, "data: [DONE]"))
			assert.Equal(t, tc.success, info.StreamStatus.ResponseOutcome() == "completed")
			if tc.success {
				assert.Contains(t, output, "\"call_1\"")
				assert.Contains(t, output, "\"arguments\":\"{\"")
				assert.Contains(t, output, "\"arguments\":\"}\"")
				assert.Contains(t, output, "\"finish_reason\":\"tool_calls\"")
				assert.True(t, info.FirstTextTime.IsZero())
				assert.NotContains(t, output, "chat-real")
			} else {
				assert.Contains(t, output, "\"error\"")
				assert.Equal(t, "failed", info.StreamStatus.ResponseOutcome())
				if tc.name == "empty_eof" || tc.name == "malformed" || tc.name == "partial_failure" {
					code := "server_error"
					if tc.name == "malformed" {
						code = "bad_response"
					}
					assert.Contains(t, output, "\"code\":\""+code+"\"")
					assert.Contains(t, output, "\"type\":\"server_error\"")
					assert.Equal(t, 1, strings.Count(output, "\"error\":"))
				}
			}
		})
	}
}

func TestEarlyChatClientCancellation(t *testing.T) {
	earlyResponsesSettings(t)
	for _, tc := range []struct{ headersFirst, advanced bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("headers_first_%v_advanced_%v", tc.headersFirst, tc.advanced), func(t *testing.T) {
			cancelled := make(chan struct{})
			finished := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if tc.headersFirst {
					w.Header().Set("Content-Type", "text/event-stream")
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
				close(cancelled)
			}))
			defer upstream.Close()
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)
				req := &dto.GeneralOpenAIRequest{Model: "gpt-6-astra", Stream: common.GetPointer(true)}
				c, info := earlyChatContext(w, r, upstream.URL, req)
				if tc.advanced {
					earlyAdvancedCustomChannel(c, info, dto.AdvancedCustomRoute{IncomingPath: "/v1/chat/completions", UpstreamPath: "/v1/chat/completions"})
				}
				body, closer, err := relaycommon.NewOutboundJSONBody([]byte("{\"model\":\"gpt-6-astra\",\"stream\":true,\"messages\":[]}"))
				if err != nil {
					return
				}
				defer closer.Close()
				helper.PrepareEarlyResponses(info, body)
				defer info.EarlyResponses.Stop()
				a := GetAdaptor(info.ApiType)
				a.Init(info)
				resp, err := a.DoRequest(c, info, body)
				if err != nil {
					return
				}
				_, _ = a.DoResponse(c, resp.(*http.Response), info)
			}))
			defer gateway.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sdk := openaisdk.NewClient(option.WithBaseURL(gateway.URL+"/v1"), option.WithAPIKey("test"))
			stream := sdk.Chat.Completions.NewStreaming(ctx, openaisdk.ChatCompletionNewParams{Model: "gpt-6-astra", Messages: []openaisdk.ChatCompletionMessageParamUnion{openaisdk.UserMessage("hello")}})
			defer stream.Close()
			require.True(t, stream.Next(), "%v", stream.Err())
			cancel()
			require.NoError(t, stream.Close())
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("relay did not stop")
			}
			select {
			case <-cancelled:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream not cancelled")
			}
		})
	}
}

func TestManagedResponsesRetryBoundary(t *testing.T) {
	earlyResponsesSettings(t)
	// Exercise heartbeat-enabled production settings without timing sleeps.
	operation_setting.GetGeneralSetting().PingIntervalEnabled = true
	oldRanges := operation_setting.AutomaticRetryStatusCodeRanges
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 429, End: 429}, {Start: 502, End: 502}}
	t.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = oldRanges })
	lifecycle := "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_discarded\",\"status\":\"in_progress\",\"output\":[]}}\n\n"
	textDelta := "data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"delta\":\"partial\"}\n\n"
	tool := "data: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"run\",\"arguments\":\"\"}}\n\n"
	emptyMessage := "data: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"item\":{\"type\":\"message\",\"content\":[]}}\n\n"
	reasoning := "data: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"rs_discarded\",\"type\":\"reasoning\",\"summary\":[],\"encrypted_content\":\"opaque_discarded\"}}\n\n"
	reasoningDone := strings.Replace(reasoning, "response.output_item.added", "response.output_item.done", 1)
	summaryPart := `data: {"type":"response.reasoning_summary_part.added","sequence_number":2,"item_id":"rs_discarded","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}` + "\n\n"
	summaryDelta := `data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_discarded","summary_index":0,"delta":"","obfuscation":"padding"}` + "\n\n"
	summaryTextDone := `data: {"type":"response.reasoning_summary_text.done","item_id":"rs_discarded","summary_index":0,"text":""}` + "\n\n"
	summaryPartDone := strings.Replace(summaryPart, "_part.added", "_part.done", 1)
	emptySummaryItem := strings.Replace(reasoning, `"summary":[]`, `"summary":[{"type":"summary_text","text":""}]`, 1)
	protocolFailure := "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"basispoints_protocol_error\",\"type\":\"server_error\",\"message\":\"invalid_tool_transport\"}}}\n\n"
	usageFailure := "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3},\"error\":{\"code\":\"server_error\",\"message\":\"failed after work\"}}}\n\n"
	largeReasoning := strings.Replace(reasoning, "opaque_discarded", strings.Repeat("x", 40<<10), 1)
	progress := strings.Replace(strings.Replace(lifecycle, "response.created", "response.in_progress", 1), "\"output\":[]", "\"output\":[],\"metadata\":{\"note\":\""+strings.Repeat("x", 38<<10)+"\"}", 1)
	readFailure := "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"upstream_stream_read_error\",\"type\":\"invalid_request_error\",\"message\":\"Upstream response stream was interrupted\",\"param\":\"input\",\"provider_detail\":{\"reason\":\"reset\"}}}}\n\n"
	for _, ack := range []bool{false, true} {
		for _, tc := range []struct {
			name, wire, code                string
			status                          int
			retry, output, readError, usage bool
			metadataOnly                    bool
		}{
			{name: "EOF", wire: lifecycle, code: "server_error", status: 502, retry: true},
			{name: "empty message EOF", wire: lifecycle + emptyMessage, code: "server_error", status: 502, retry: true},
			{name: "encrypted reasoning EOF", wire: lifecycle + reasoning, code: "server_error", status: 502, retry: true},
			{name: "encrypted reasoning protocol failure", wire: lifecycle + reasoning + reasoningDone + protocolFailure, code: "basispoints_protocol_error", status: 502, retry: true},
			{name: "encrypted reasoning read failure", wire: lifecycle + reasoning, code: "server_error", status: 502, retry: true, readError: true},
			{name: "encrypted reasoning usage failure", wire: lifecycle + reasoning + usageFailure, code: "server_error", status: 502, usage: true},
			{name: "encrypted reasoning then text", wire: lifecycle + reasoning + textDelta + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true},
			{name: "encrypted reasoning then tool", wire: lifecycle + reasoning + tool + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true},
			{name: "visible reasoning summary", wire: lifecycle + strings.Replace(reasoning, "\"summary\":[]", "\"summary\":[{\"type\":\"summary_text\",\"text\":\"visible reasoning\"}]", 1) + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true},
			{name: "visible reasoning content", wire: lifecycle + strings.Replace(reasoning, "\"summary\":[]", "\"content\":[{\"type\":\"reasoning_text\",\"text\":\"visible reasoning\"}]", 1) + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true},
			{name: "unrecognized reasoning summary", wire: lifecycle + strings.Replace(reasoning, "\"summary\":[]", "\"summary\":{\"text\":\"visible reasoning\"}", 1) + protocolFailure, code: "server_error", status: 502, retry: true},
			{name: "duplicate numbered progress at limit", wire: lifecycle + progress + progress + protocolFailure, code: "basispoints_protocol_error", status: 502, retry: true},
			{name: "different numbered progress at limit", wire: lifecycle + progress + strings.Replace(progress, `"sequence_number":0`, `"sequence_number":1`, 1) + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true, metadataOnly: true},
			{name: "unknown incoming event", wire: lifecycle + "data: {\"type\":\"private-upstream-label\"}\n\n" + textDelta + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true},
			{name: "empty summary then failure", wire: lifecycle + reasoning + summaryPart + protocolFailure, code: "basispoints_protocol_error", status: 502, retry: true},
			{name: "empty complete summary then failure", wire: lifecycle + emptySummaryItem + summaryPart + summaryDelta + summaryTextDone + summaryPartDone + protocolFailure, code: "basispoints_protocol_error", status: 502, retry: true},
			{name: "summary text on part", wire: lifecycle + strings.Replace(summaryPart, `"text":""`, `"text":"private summary"`, 1) + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true},
			{name: "summary text on delta", wire: lifecycle + summaryPart + strings.Replace(summaryDelta, `"delta":""`, `"delta":" "`, 1) + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true},
			{name: "summary text on done", wire: lifecycle + summaryPart + strings.Replace(summaryTextDone, `"text":""`, `"text":"private summary"`, 1) + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true},
			{name: "empty reasoning text delta and done", wire: lifecycle + strings.Replace(summaryDelta, "reasoning_summary_text", "reasoning_text", 1) + strings.Replace(summaryTextDone, "reasoning_summary_text", "reasoning_text", 1) + protocolFailure, code: "basispoints_protocol_error", status: 502, retry: true},
			{name: "visible reasoning delta", wire: lifecycle + strings.Replace(strings.Replace(summaryDelta, "reasoning_summary_text", "reasoning_text", 1), `"delta":""`, `"delta":"private summary"`, 1) + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true},
			{name: "summary text on item", wire: lifecycle + strings.Replace(emptySummaryItem, `"text":""`, `"text":"private summary"`, 1) + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true},
			{name: "summary part without text", wire: lifecycle + strings.Replace(summaryPart, `,"text":""`, "", 1) + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true},
			{name: "summary with usage signal", wire: lifecycle + strings.Replace(summaryPart, `"sequence_number":2`, `"sequence_number":2,"usage":{"output_tokens":1}`, 1) + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true},
			{name: "summary with tool extension", wire: lifecycle + strings.Replace(summaryPart, `"text":""`, `"text":"","tool_calls":[{}]`, 1) + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true},
			{name: "empty summary then tool", wire: lifecycle + summaryPart + tool + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true},
			{name: "empty summary then failure with usage", wire: lifecycle + summaryPart + usageFailure, code: "server_error", status: 502, usage: true},
			{name: "empty summary cumulative limit", wire: lifecycle + strings.Repeat(summaryPart, 512) + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true, metadataOnly: true},
			{name: "encrypted reasoning cumulative limit", wire: lifecycle + largeReasoning + largeReasoning + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true, metadataOnly: true},
			{name: "encrypted reasoning single limit", wire: lifecycle + strings.Replace(reasoning, "opaque_discarded", strings.Repeat("x", 65<<10), 1) + protocolFailure, code: "basispoints_protocol_error", status: 502, output: true, metadataOnly: true},
			{name: "encrypted reasoning signature failure", wire: lifecycle + reasoning + "data: {\"type\":\"error\",\"error\":{\"code\":\"thinking_signature_invalid\",\"type\":\"invalid_request_error\",\"message\":\"cannot decrypt\"}}\n\n", code: "thinking_signature_invalid", status: 400},
			{name: "encrypted reasoning rate limit", wire: lifecycle + reasoning + "data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"limited\"}\n\n", code: "rate_limit_exceeded", status: 429, retry: true},
			{name: "transport interruption", wire: lifecycle, code: "server_error", status: 502, retry: true, readError: true},
			{name: "explicit stream interruption", wire: lifecycle + readFailure, code: "upstream_stream_read_error", status: 502, retry: true},
			{name: "stream interruption after text", wire: lifecycle + textDelta + readFailure, code: "upstream_stream_read_error", status: 502, output: true},
			{name: "stream interruption after tool", wire: lifecycle + tool + readFailure, code: "upstream_stream_read_error", status: 502, output: true},
			{name: "malformed", wire: lifecycle + "data: {bad}\n\n", code: "server_error", status: 502, retry: true},
			{name: "missing type", wire: lifecycle + "data: {}\n\n", code: "server_error", status: 502, retry: true},
			{name: "bare failed", wire: lifecycle + "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n", code: "server_error", status: 502, retry: true},
			{name: "large metadata", wire: strings.Replace(lifecycle, "\"output\":[]", "\"output\":[],\"metadata\":{\"note\":\""+strings.Repeat("x", 65<<10)+"\"}", 1), code: "server_error", status: 502, output: true},
			{name: "tool protocol", wire: lifecycle + "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"tool_protocol_error\",\"type\":\"server_error\",\"message\":\"bad upstream tool protocol\"}}}\n\n", code: "tool_protocol_error", status: 502, retry: true},
			{name: "signature", wire: lifecycle + "data: {\"type\":\"response.failed\",\"request_id\":\"upstream_test\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"thinking_signature_invalid\",\"type\":\"invalid_request_error\",\"message\":\"cannot decrypt\"}}}\n\n", code: "thinking_signature_invalid", status: 400},
			{name: "input tool mismatch", wire: "data: {\"type\":\"error\",\"error\":{\"code\":\"invalid_tool_call\",\"type\":\"invalid_request_error\",\"message\":\"missing tool result\"}}\n\n", code: "invalid_tool_call", status: 400},
			{name: "auth", wire: "data: {\"type\":\"error\",\"error\":{\"code\":\"invalid_api_key\",\"type\":\"authentication_error\",\"message\":\"unauthorized\"}}\n\n", code: "invalid_api_key", status: 401},
			{name: "rate limit", wire: lifecycle + "data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"limited\"}\n\n", code: "rate_limit_exceeded", status: 429, retry: true},
			{name: "partial text", wire: lifecycle + textDelta, code: "server_error", status: 502, output: true},
			{name: "tool delivered", wire: lifecycle + tool, code: "server_error", status: 502, output: true},
			{name: "failure with usage", wire: lifecycle + "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3},\"error\":{\"code\":\"server_error\",\"type\":\"server_error\",\"message\":\"failed after work\"}}}\n\n", code: "server_error", status: 502, usage: true},
		} {
			t.Run(fmt.Sprintf("ack_%v/%s", ack, tc.name), func(t *testing.T) {
				operation_setting.GetGeneralSetting().ResponsesEarlyAckEnabled = ack
				recorder := httptest.NewRecorder()
				req := &dto.OpenAIResponsesRequest{Model: "gpt-6-astra", Input: []byte("\"hello\""), Stream: common.GetPointer(true)}
				c, info := earlyResponsesContext(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "http://upstream.invalid", req)
				info.InitChannelMeta(c)
				info.ResponsesRetryManaged = true
				c.Set("responses_retry_managed", true)
				info.SetEstimatePromptTokens(11)
				raw, err := common.Marshal(req)
				require.NoError(t, err)
				body, closer, err := relaycommon.NewOutboundJSONBody(raw)
				require.NoError(t, err)
				defer closer.Close()
				helper.PrepareEarlyResponses(info, body)
				if ack {
					require.NoError(t, helper.SendEarlyResponsesAck(c, info))
				}
				var reader io.Reader = strings.NewReader(tc.wire)
				if tc.readError {
					reader = io.MultiReader(reader, iotest.ErrReader(errors.New("upstream transport read failed")))
				}
				usage, apiErr := openaichannel.OaiResponsesStreamHandler(c, info, &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(reader)})
				require.NotNil(t, apiErr)
				assert.Equal(t, tc.code, string(apiErr.GetErrorCode()))
				assert.Equal(t, tc.status, apiErr.StatusCode)
				if strings.Contains(tc.wire, "upstream_stream_read_error") {
					assert.Equal(t, "Upstream response stream was interrupted", apiErr.ToOpenAIError().Message)
				}
				decision := service.DecideRelayRetry(c, apiErr, 1)
				assert.Equal(t, tc.retry, decision.Action == "retry", decision.Reason)
				other := model.NewLogOther()
				service.AppendRelayLogAdminInfo(c, info, other)
				commit := gjson.Get(other.JSONString(), "admin_info.responses_stream_commit")
				assert.Equal(t, tc.output, commit.Exists())
				if commit.Exists() {
					assert.Positive(t, commit.Get("event_bytes").Int())
					assert.False(t, gjson.Get(other.JSONString(), "responses_stream_commit").Exists())
					assert.NotContains(t, commit.Raw, "private-upstream-label")
					assert.NotContains(t, commit.Raw, "opaque_discarded")
				}
				switch tc.name {
				case "encrypted reasoning cumulative limit", "different numbered progress at limit", "empty summary cumulative limit":
					assert.Equal(t, "prelude_buffer_limit", commit.Get("reason").String())
					assert.Greater(t, commit.Get("buffered_bytes").Int()+commit.Get("event_bytes").Int(), int64(64<<10))
					assert.False(t, commit.Get("has_output_text").Bool())
					assert.False(t, commit.Get("has_tool_call").Bool())
				case "summary text on part", "summary text on delta", "summary text on done", "summary text on item", "visible reasoning delta":
					assert.True(t, commit.Get("has_reasoning_text").Bool())
					assert.False(t, commit.Get("has_output_text").Bool())
					assert.NotContains(t, commit.Raw, "private summary")
				case "partial text", "encrypted reasoning then text":
					assert.Equal(t, "response.output_text.delta", commit.Get("event_type").String())
					assert.True(t, commit.Get("has_output_text").Bool())
				case "tool delivered", "encrypted reasoning then tool", "empty summary then tool":
					assert.Equal(t, "response.output_item.added", commit.Get("event_type").String())
					assert.Equal(t, "function_call", commit.Get("item_type").String())
					assert.True(t, commit.Get("has_tool_call").Bool())
				case "unknown incoming event":
					assert.Equal(t, "other", commit.Get("event_type").String())
					assert.Equal(t, "event_forwarded", commit.Get("reason").String())
					assert.False(t, commit.Get("has_output_text").Bool(), "the later delta must not replace the first commit")
				}
				if tc.name == "summary text on delta" {
					wire := recorder.Body.String()
					assert.Equal(t, 1, strings.Count(wire, "event: response.reasoning_summary_part.added"))
					assert.Less(t, strings.Index(wire, "event: response.reasoning_summary_part.added"), strings.Index(wire, "event: response.reasoning_summary_text.delta"))
				}
				if tc.usage {
					require.NotNil(t, usage)
					assert.Equal(t, 10, usage.TotalTokens)
				} else if !tc.output || tc.metadataOnly || tc.name == "large metadata" {
					assert.Nil(t, usage)
				}
				if !ack && !tc.output {
					assert.False(t, c.Writer.Written(), "heartbeat must not force HTTP 200 before known failure")
				}
				assert.NotContains(t, recorder.Body.String(), "response.failed", "defer final failure until router exhausts fallback")
				if !tc.output {
					assert.NotContains(t, recorder.Body.String(), "opaque_discarded", "failed attempt reasoning must not reach the caller")
				}
				if tc.retry {
					assert.Equal(t, "attempt_budget_exhausted", service.DecideRelayRetry(c, apiErr, 0).Reason)
					info.ChannelId = 143
					helper.PrepareEarlyResponses(info, body)
					secondUsage, secondErr := openaichannel.OaiResponsesStreamHandler(c, info, &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(earlyResponsesSuccessEvents))})
					require.Nil(t, secondErr)
					require.NotNil(t, secondUsage)
					assert.Equal(t, 10, secondUsage.TotalTokens)
					assert.NotContains(t, recorder.Body.String(), "resp_discarded")
					assert.NotContains(t, recorder.Body.String(), "rs_discarded")
					assert.NotContains(t, recorder.Body.String(), "opaque_discarded")
					assert.NotContains(t, recorder.Body.String(), "response.failed")
					if ack {
						resolved, err := service.ResolveEarlyResponseAlias(7, 143, info.EarlyResponses.ID)
						require.NoError(t, err)
						assert.Equal(t, "resp_real", resolved)
					}
				} else if c.Writer.Written() {
					require.NoError(t, helper.FailEarlyResponses(c, info, apiErr))
					wire := recorder.Body.String()
					assert.Equal(t, 1, strings.Count(wire, "event: response.failed"))
					assert.NotContains(t, wire, "[DONE]")
					if strings.Contains(tc.wire, "upstream_stream_read_error") {
						assert.Contains(t, wire, "\"code\":\"upstream_stream_read_error\"")
						assert.Contains(t, wire, "\"param\":\"input\"")
						assert.Contains(t, wire, "\"provider_detail\":{\"reason\":\"reset\"}")
						assert.Contains(t, wire, "\"type\":\"invalid_request_error\"")
					}
					if tc.name == "signature" {
						assert.Contains(t, wire, "upstream_test")
						assert.Contains(t, wire, "invalid_request_error")
					}
				}
				info.EarlyResponses.Stop()
			})
		}
	}
}

func TestManagedResponsesHTTPFailure(t *testing.T) {
	earlyResponsesSettings(t)
	for _, ack := range []bool{false, true} {
		for _, tc := range []struct {
			status     int
			code, kind string
			retry      bool
		}{
			{502, "upstream_unavailable", "server_error", true},
			{429, "rate_limit_exceeded", "rate_limit_error", true},
			{400, "thinking_signature_invalid", "invalid_request_error", false},
			{401, "invalid_api_key", "authentication_error", false},
			{403, "access_denied", "server_error", false},
		} {
			t.Run(fmt.Sprintf("ack_%v/status_%d", ack, tc.status), func(t *testing.T) {
				operation_setting.GetGeneralSetting().ResponsesEarlyAckEnabled = ack
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Request-ID", "upstream_http_test")
					w.WriteHeader(tc.status)
					payload, _ := common.Marshal(map[string]any{"error": map[string]any{"code": tc.code, "type": tc.kind, "message": "upstream rejected"}})
					_, _ = w.Write(payload)
				}))
				defer upstream.Close()
				recorder := httptest.NewRecorder()
				req := &dto.OpenAIResponsesRequest{Model: "gpt-6-astra", Input: []byte("\"hello\""), Stream: common.GetPointer(true)}
				c, info := earlyResponsesContext(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), upstream.URL, req)
				info.InitChannelMeta(c)
				info.ResponsesRetryManaged = true
				c.Set("responses_retry_managed", true)
				reservation := &earlyAckReservation{held: 100}
				info.Billing = reservation
				raw, err := common.Marshal(req)
				require.NoError(t, err)
				body, closer, err := relaycommon.NewOutboundJSONBody(raw)
				require.NoError(t, err)
				defer closer.Close()
				helper.PrepareEarlyResponses(info, body)
				if ack {
					require.NoError(t, helper.SendEarlyResponsesAck(c, info))
				}
				apiErr := ResponsesHelper(c, info)
				require.NotNil(t, apiErr)
				assert.Equal(t, tc.status, apiErr.StatusCode)
				wantCode := tc.code
				assert.Equal(t, wantCode, string(apiErr.GetErrorCode()))
				assert.Equal(t, tc.retry, service.DecideRelayRetry(c, apiErr, 1).Action == "retry")
				assert.NotContains(t, recorder.Body.String(), "response.failed")
				assert.Equal(t, 100, reservation.held, "attempt failure must not refund before routing finishes")
				apiErr = RefundFailedRequestBilling(c, info, apiErr)
				assert.Zero(t, reservation.held)
				if ack {
					require.NoError(t, helper.FailEarlyResponses(c, info, apiErr))
					assert.Equal(t, 200, recorder.Code)
					assert.Contains(t, recorder.Body.String(), "upstream_http_test")
					assert.Contains(t, recorder.Body.String(), "\"type\":\""+tc.kind+"\"")
					assert.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.failed"))
				} else {
					assert.False(t, c.Writer.Written())
					c.JSON(apiErr.StatusCode, gin.H{"error": apiErr.ToOpenAIError()})
					assert.Equal(t, tc.status, recorder.Code)
				}
				info.EarlyResponses.Stop()
			})
		}
	}
}

func TestManagedResponsesRetryFinalization(t *testing.T) {
	earlyResponsesSettings(t)
	for _, code := range []string{"server_error", "rate_limit_exceeded", "upstream_stream_read_error"} {
		original := types.WithOpenAIError(types.OpenAIError{Code: code, Type: code, Message: "failed"}, 502, types.ErrOptionWithSkipRetry())
		normalized := service.NormalizeResponsesRetryError(original)
		assert.True(t, types.IsSkipRetryError(normalized), "settled usage must never become replayable during error normalization")
	}
	req := &dto.OpenAIResponsesRequest{Model: "gpt-6-astra", Input: []byte("\"hello\""), Stream: common.GetPointer(true)}
	recorder := httptest.NewRecorder()
	c, info := earlyResponsesContext(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "http://upstream.invalid", req)
	info.InitChannelMeta(c)
	info.ResponsesRetryManaged = true
	c.Set("responses_retry_managed", true)
	raw, err := common.Marshal(req)
	require.NoError(t, err)
	body, closer, err := relaycommon.NewOutboundJSONBody(raw)
	require.NoError(t, err)
	defer closer.Close()
	helper.PrepareEarlyResponses(info, body)
	require.NoError(t, helper.SendEarlyResponsesAck(c, info))
	var lastErr *types.NewAPIError
	for attempt := range 3 {
		helper.PrepareEarlyResponses(info, body)
		_, lastErr = openaichannel.OaiResponsesStreamHandler(c, info, &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))})
		require.NotNil(t, lastErr)
		assert.Equal(t, attempt < 2, service.DecideRelayRetry(c, lastErr, 2-attempt).Action == "retry")
		assert.NotContains(t, recorder.Body.String(), "response.failed")
	}
	require.NoError(t, helper.FailEarlyResponses(c, info, lastErr))
	require.NoError(t, helper.FailEarlyResponses(c, info, lastErr))
	assert.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.created"))
	assert.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.failed"))
	assert.NotContains(t, recorder.Body.String(), "[DONE]")
	ctx, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(ctx)
	assert.Equal(t, "client_cancelled", service.DecideRelayRetry(c, lastErr, 2).Reason)
}

func TestChatStreamFailureClassification(t *testing.T) {
	earlyResponsesSettings(t)
	for _, protocol := range []string{"native", "responses_to_chat", "chat_to_responses"} {
		converted := protocol == "responses_to_chat"
		reverse := protocol == "chat_to_responses"
		for _, ack := range []bool{false, true} {
			if reverse && ack {
				continue
			}
			for _, tc := range []struct {
				name, code, kind  string
				status            int
				partial, reported bool
			}{
				{"normal_eof", "", "", 0, false, true},
				{"eof", "server_error", "server_error", 502, false, false},
				{"read_error", "server_error", "server_error", 502, false, false},
				{"malformed", "bad_response", "server_error", 502, false, false},
				{"tool_protocol_error", "tool_protocol_error", "server_error", 502, false, false},
				{"upstream_stream_read_error", "upstream_stream_read_error", "server_error", 502, false, false},
				{"thinking_signature_invalid", "thinking_signature_invalid", "invalid_request_error", 400, false, false},
				{"upstream_authentication_required", "upstream_authentication_required", "authentication_error", 401, false, false},
				{"rate_limit_exceeded", "rate_limit_exceeded", "rate_limit_error", 429, false, false},
				{"partial_eof", "server_error", "server_error", 502, true, false},
				{"partial_read_error", "server_error", "server_error", 502, true, false},
				{"partial_error", "tool_protocol_error", "server_error", 502, true, true},
				{"tool_partial_error", "tool_protocol_error", "server_error", 502, true, true},
				{"error_usage", "tool_protocol_error", "server_error", 502, false, true},
			} {
				if reverse && tc.name == "malformed" {
					tc.code, tc.kind = "bad_response_body", "server_error"
				}
				t.Run(fmt.Sprintf("%s/ack_%v/%s", protocol, ack, tc.name), func(t *testing.T) {
					operation_setting.GetGeneralSetting().ResponsesEarlyAckEnabled = ack
					w := httptest.NewRecorder()
					c, info := earlyChatContext(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "", &dto.GeneralOpenAIRequest{Model: "gpt-6-astra", Stream: common.GetPointer(true)})
					if reverse {
						info.RelayFormat = types.RelayFormatOpenAIResponses
						info.RelayMode = relayconstant.RelayModeResponses
					}
					common.SetContextKey(c, constant.ContextKeyIsStream, true)
					c.Set(common.UpstreamRequestIdKey, "test-upstream-id")
					info.SetEstimatePromptTokens(7)
					if ack {
						body, closer, err := relaycommon.NewOutboundJSONBody([]byte("{\"stream\":true,\"messages\":[]}"))
						require.NoError(t, err)
						defer closer.Close()
						helper.PrepareEarlyResponses(info, body)
						require.NotNil(t, info.EarlyResponses)
						defer info.EarlyResponses.Stop()
						require.NoError(t, helper.SendEarlyResponsesAck(c, info))
					}
					var wire string
					if tc.partial {
						chunk := map[string]any{"id": "chat-real", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}, "finish_reason": nil}}}
						if converted {
							chunk = map[string]any{"type": "response.output_text.delta", "delta": "hello", "output_index": 0, "content_index": 0}
						}
						if tc.name == "tool_partial_error" {
							chunk["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}}, "finish_reason": nil}}
							if converted {
								chunk = map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "tool_1", "type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": "{}"}}
							}
						}
						raw, err := common.Marshal(chunk)
						require.NoError(t, err)
						wire += "data: " + string(raw) + "\n\n"
					}
					switch tc.name {
					case "normal_eof":
						wire = strings.ReplaceAll(earlyChatTextEvents, "data: [DONE]\n\n", "")
						if converted {
							wire = earlyResponsesSuccessEvents
						}
					case "eof", "partial_eof", "read_error", "partial_read_error":
					case "malformed":
						wire += "data: {broken}\n\n"
					default:
						code, kind := tc.name, tc.kind
						if tc.name == "partial_error" || tc.name == "tool_partial_error" || tc.name == "error_usage" {
							code, kind = "tool_protocol_error", "invalid_request_error"
						}
						if tc.name == "tool_protocol_error" || tc.name == "upstream_stream_read_error" {
							kind = "invalid_request_error"
						}
						failure := map[string]any{"code": code, "type": kind, "message": "original upstream failure", "param": "input"}
						frame := map[string]any{"error": failure}
						if converted {
							frame = map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_real", "status": "failed", "error": failure}}
						}
						if tc.reported {
							if converted {
								frame["response"].(map[string]any)["usage"] = map[string]any{"input_tokens": 7, "output_tokens": 3, "total_tokens": 10}
							} else {
								frame["usage"] = map[string]any{"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}
							}
						}
						raw, err := common.Marshal(frame)
						require.NoError(t, err)
						wire += "data: " + string(raw) + "\n\n"
					}
					var reader io.Reader = strings.NewReader(wire)
					if tc.name == "read_error" || tc.name == "partial_read_error" {
						reader = io.MultiReader(reader, iotest.ErrReader(io.ErrUnexpectedEOF))
					}
					handler := openaichannel.OaiStreamHandler
					if converted {
						handler = openaichannel.OaiResponsesToChatStreamHandler
					}
					if reverse {
						handler = openaichannel.OaiChatToResponsesStreamHandler
					}
					usage, apiErr := handler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(reader)})
					if tc.name == "normal_eof" {
						require.Nil(t, apiErr)
						require.NotNil(t, usage)
						assert.Equal(t, 10, usage.TotalTokens)
						assert.Equal(t, "completed", info.StreamStatus.ResponseOutcome())
						assert.NotContains(t, w.Body.String(), "server_error")
						if reverse {
							assert.Contains(t, w.Body.String(), "event: response.completed")
						} else {
							assert.Equal(t, 1, strings.Count(w.Body.String(), "data: [DONE]"))
						}
						return
					}
					outcome := info.StreamStatus.OutcomeSnapshot()
					assert.Equal(t, tc.code, outcome.ErrorCode)
					assert.Equal(t, tc.kind, outcome.ErrorType)
					assert.Equal(t, tc.status, outcome.ErrorStatus)
					assert.Equal(t, "failed", info.StreamStatus.ResponseOutcome())
					if !ack && !tc.partial && !tc.reported {
						require.NotNil(t, apiErr)
						assert.Equal(t, tc.status, apiErr.StatusCode)
						assert.Equal(t, tc.code, string(apiErr.GetErrorCode()))
						assert.False(t, c.Writer.Written())
						assert.Nil(t, usage)
						decision := service.DecideRelayRetry(c, apiErr, 3)
						if tc.status == 400 || tc.status == 401 {
							assert.Equal(t, "stop", decision.Action)
						}
					} else {
						if !tc.partial && !tc.reported {
							require.NotNil(t, apiErr)
							assert.Equal(t, tc.status, apiErr.StatusCode)
							assert.Nil(t, usage)
							reservation := &earlyAckReservation{held: 100}
							info.Billing = reservation
							RefundFailedRequestBilling(c, info, apiErr)
							assert.Zero(t, reservation.held)
							usage = &dto.Usage{}
						} else {
							require.Nil(t, apiErr)
							require.NotNil(t, usage)
						}
						if tc.reported {
							assert.Equal(t, 10, usage.TotalTokens)
						} else if !tc.partial {
							assert.Zero(t, usage.TotalTokens)
						} else {
							assert.Positive(t, usage.TotalTokens)
						}
						output := w.Body.String()
						assert.NotContains(t, output, "[DONE]")
						assert.NotContains(t, output, "\"finish_reason\":\"stop\"")
						assert.Equal(t, 1, strings.Count(output, "\"error\":"))
						assert.Contains(t, output, "\"code\":\""+tc.code+"\"")
						if !reverse || tc.partial {
							assert.Contains(t, output, "test-upstream-id")
						}
						if tc.name == "tool_partial_error" {
							if reverse {
								assert.Equal(t, 1, strings.Count(output, "\"delta\":\"{}\""))
							} else {
								assert.Equal(t, 1, strings.Count(output, "\"arguments\":\"{}\""))
							}
						} else if tc.partial {
							if reverse {
								assert.Equal(t, 1, strings.Count(output, "\"delta\":\"hello\""))
								assert.NotContains(t, output, "event: response.completed")
								assert.Contains(t, output, "event: response.failed")
							} else {
								assert.Equal(t, 1, strings.Count(output, "\"content\":\"hello\""))
							}
						}
						retryErr := types.NewOpenAIError(errors.New("failed"), types.ErrorCodeBadResponse, 502)
						assert.Equal(t, "stop", service.DecideRelayRetry(c, retryErr, 3).Action)
					}
				})
			}
		}
	}
}

// Exercise the no-retry HTTP contract with early acknowledgments disabled.
func TestResponsesFailureHTTPStatusAndNormalStream(t *testing.T) {
	earlyResponsesSettings(t)
	operation_setting.GetGeneralSetting().ResponsesEarlyAckEnabled = false
	lifecycle := `{"type":"response.created","sequence_number":0,"response":{"id":"resp_real","status":"in_progress","output":[]}}`
	reasoning := `{"type":"response.output_item.added","sequence_number":1,"item":{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"opaque"}}`
	for _, tc := range []struct {
		name, terminal, code string
		status               int
		readError            bool
	}{
		{"server failure", `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"unavailable"}}}`, "server_error", 502, false},
		{"signature failure", `{"type":"response.failed","response":{"status":"failed","error":{"code":"thinking_signature_invalid","type":"invalid_request_error","message":"invalid signature"}}}`, "thinking_signature_invalid", 400, false},
		{"signature first event", `{"type":"response.failed","response":{"status":"failed","error":{"code":"thinking_signature_invalid","type":"invalid_request_error","message":"invalid signature"}}}`, "thinking_signature_invalid", 400, false},
		{"transport failure", "", "server_error", 502, true},
		{"missing terminal", "", "server_error", 502, false},
		{"normal completion", `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_real","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":0,"total_tokens":2}}}`, "", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, info := earlyResponsesContext(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "", &dto.OpenAIResponsesRequest{Model: "gpt-6-astra", Stream: common.GetPointer(true)})
			info.InitChannelMeta(c)
			info.ResponsesRetryManaged = true
			c.Set("responses_retry_managed", true)
			wire := "data: " + lifecycle + "\n\ndata: " + reasoning + "\n\n"
			if tc.name == "signature first event" {
				wire = ""
			}
			if tc.terminal != "" {
				wire += "data: " + tc.terminal + "\n\n"
			}
			var reader io.Reader = strings.NewReader(wire)
			if tc.readError {
				reader = io.MultiReader(reader, iotest.ErrReader(io.ErrUnexpectedEOF))
			}
			usage, apiErr := openaichannel.OaiResponsesStreamHandler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(reader)})
			if tc.code == "" {
				require.Nil(t, apiErr)
				require.NotNil(t, usage)
				assert.Equal(t, 2, usage.TotalTokens)
				// Buffered metadata is delivered in order and without payload changes.
				assert.Equal(t, "event: response.created\ndata: "+lifecycle+"\n\nevent: response.output_item.added\ndata: "+reasoning+"\n\nevent: response.completed\ndata: "+tc.terminal+"\n\n", recorder.Body.String())
			} else {
				require.NotNil(t, apiErr)
				assert.Nil(t, usage)
				assert.False(t, c.Writer.Written())
				assert.Equal(t, "stop", service.DecideRelayRetry(c, apiErr, 0).Action)
				c.Writer.Header().Set("Content-Type", "application/json")
				c.JSON(apiErr.StatusCode, gin.H{"error": apiErr.ToOpenAIError()})
				assert.JSONEq(t, fmt.Sprintf(`{"error":{"code":%q,"type":%q,"message":%q,"param":""}}`, tc.code, apiErr.ToOpenAIError().Type, apiErr.ToOpenAIError().Message), recorder.Body.String())
				assert.NotContains(t, recorder.Body.String(), "opaque")
			}
			assert.Equal(t, tc.status, recorder.Code)
		})
	}
}

// A retryable attempt can keep an acknowledged client active without exposing
// that attempt's identity, usage, tools, or metadata.
func TestManagedResponsesBufferedProgressPreservesRetry(t *testing.T) {
	earlyResponsesSettings(t)
	oldRanges := operation_setting.AutomaticRetryStatusCodeRanges
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 502, End: 502}}
	t.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = oldRanges })
	for _, ack := range []bool{true, false} {
		t.Run(fmt.Sprintf("ack_%v", ack), func(t *testing.T) {
			operation_setting.GetGeneralSetting().ResponsesEarlyAckEnabled = ack
			recorder := httptest.NewRecorder()
			req := &dto.OpenAIResponsesRequest{Model: "gpt-6-astra", Input: []byte(`"hello"`), Stream: common.GetPointer(true)}
			c, info := earlyResponsesContext(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "http://upstream.invalid", req)
			info.InitChannelMeta(c)
			info.ResponsesRetryManaged = true
			c.Set("responses_retry_managed", true)
			raw, err := common.Marshal(req)
			require.NoError(t, err)
			body, closer, err := relaycommon.NewOutboundJSONBody(raw)
			require.NoError(t, err)
			defer closer.Close()
			helper.PrepareEarlyResponses(info, body)
			if ack {
				require.NoError(t, helper.SendEarlyResponsesAck(c, info))
				info.EarlyResponses.SentAt = time.Now().Add(-time.Minute)
			}
			lifecycle := `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_discarded","status":"in_progress","output":[],"metadata":{"secret":"private-attempt"}}}

`
			progress := `data: {"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_discarded","status":"in_progress","output":[],"metadata":{"secret":"private-attempt"}}}

`
			failure := `data: {"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","type":"server_error","message":"transient"}}}

`
			usage, apiErr := openaichannel.OaiResponsesStreamHandler(c, info, &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(lifecycle + progress + progress + failure))})
			require.NotNil(t, apiErr)
			assert.Nil(t, usage)
			assert.Equal(t, "retry", service.DecideRelayRetry(c, apiErr, 1).Action)
			assert.False(t, c.GetBool("responses_content_delivered"))
			assert.False(t, info.EarlyResponses.FirstEvent)
			assert.Empty(t, info.EarlyResponses.UpstreamID)
			assert.NotContains(t, recorder.Body.String(), "resp_discarded")
			assert.NotContains(t, recorder.Body.String(), "private-attempt")
			if ack {
				assert.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.in_progress"), "active progress must reach the client, with duplicate notifications rate limited")
				assert.Contains(t, recorder.Body.String(), info.EarlyResponses.ID)
			} else {
				assert.False(t, c.Writer.Written(), "do not force HTTP 200 when early acknowledgment is disabled")
			}
			info.ChannelId = 143
			helper.PrepareEarlyResponses(info, body)
			usage, apiErr = openaichannel.OaiResponsesStreamHandler(c, info, &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(earlyResponsesSuccessEvents))})
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, 10, usage.TotalTokens)
			assert.NotContains(t, recorder.Body.String(), "resp_discarded")
			assert.NotContains(t, recorder.Body.String(), "private-attempt")
			assert.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.completed"))
			scanner := bufio.NewScanner(strings.NewReader(recorder.Body.String()))
			previous := int64(-1)
			for scanner.Scan() {
				if line, ok := strings.CutPrefix(scanner.Text(), "data: "); ok && gjson.Get(line, "sequence_number").Exists() {
					n := gjson.Get(line, "sequence_number").Int()
					assert.Greater(t, n, previous)
					previous = n
				}
			}
			require.NoError(t, scanner.Err())
			info.EarlyResponses.Stop()
		})
	}
}

func TestNativeAdvancedResponsesUpstreamFailover(t *testing.T) {
	earlyResponsesSettings(t)
	operation_setting.GetGeneralSetting().ResponsesEarlyAckEnabled = false
	const path = "/v1/responses"
	failed := "data: " + `{"type":"error","code":"invalid_api_key","message":"invalid upstream key"}` + "\n\n"
	created := "data: " + `{"type":"response.created","response":{"id":"failed-attempt","output":[]}}` + "\n\n"
	for _, tc := range []struct {
		name, body, converter, mime string
		status                      int
		stream, disconnect, retry   bool
	}{
		{name: "queue timeout", status: 503, body: `{"error":{"code":"bps_queue_timeout","message":"scheduler rejected auth pick"}}`, stream: true, retry: true},
		{name: "stream upstream auth failure", status: 200, body: created + failed, stream: true, retry: true},
		{name: "upstream EOF", status: 200, body: created, stream: true, retry: true},
		{name: "upstream cancel", status: 200, body: "data: " + `{"type":"response.cancelled","response":{"status":"cancelled","output":[]}}` + "\n\n", stream: true, retry: true},
		{name: "nonstream failed 200", status: 200, body: `{"status":"failed","error":{"code":"bad_credentials","message":"upstream denied"}}`, retry: true},
		{name: "HTTP failure after tool execution", status: 403, body: `{"error":{"type":"invalid_request_error","message":"failed"},"response":{"output":[{"type":"mcp_call","status":"completed"}]}}`},
		{name: "nonstream failed after tool execution", status: 200, body: `{"status":"failed","output":[{"type":"mcp_call","status":"completed"}]}`},
		{name: "stream received failed JSON", stream: true, mime: "application/json", status: 200, body: `{"status":"failed","error":{"code":"invalid_api_key"}}`, retry: true},
		{name: "stream received completed JSON", stream: true, mime: "application/json", status: 200, body: `{"status":"completed","output":[]}`},
		{name: "upstream transport failed", stream: true, retry: true},
		{name: "downstream cancelled", stream: true, disconnect: true},
		{name: "converted route", status: 403, stream: true, body: `{"error":{"type":"authentication_error","code":"invalid_api_key"}}`, converter: relayconvert.ConverterOpenAIResponsesToOpenAIChat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.mime != "" {
					w.Header().Set("Content-Type", tc.mime)
				} else if tc.status == 200 && tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			if tc.status == 0 {
				upstream.Close()
			}
			incoming := httptest.NewRequest(http.MethodPost, path, nil)
			if tc.disconnect {
				ctx, cancel := context.WithCancel(incoming.Context())
				cancel()
				incoming = incoming.WithContext(ctx)
			}
			req := &dto.OpenAIResponsesRequest{Model: "gpt-6-astra", Input: []byte(`"hello"`), Stream: common.GetPointer(tc.stream)}
			recorder := httptest.NewRecorder()
			c, info := earlyResponsesContext(recorder, incoming, upstream.URL, req)
			upstreamPath := path
			if tc.converter != "" {
				upstreamPath = "/v1/chat/completions"
			}
			earlyAdvancedCustomChannel(c, info, dto.AdvancedCustomRoute{IncomingPath: path, UpstreamPath: upstreamPath, Converter: tc.converter})
			info.ResponsesRetryManaged = tc.stream
			apiErr := ResponsesHelper(c, info)
			require.NotNil(t, apiErr)
			assert.Equal(t, tc.converter == "", common.GetContextKeyBool(c, constant.ContextKeyResponsesChannelFailover))
			assert.Equal(t, tc.retry, common.GetContextKeyBool(c, constant.ContextKeyResponsesUpstreamRetryable))
			if tc.converter == "" {
				assert.Equal(t, tc.retry, service.DecideRelayRetry(c, apiErr, 1).Action == "retry")
			}
			assert.Empty(t, recorder.Body.String(), "failed attempts must not expose upstream output")
		})
	}
}

func TestResponsesChannelFailoverAfterUpstreamWork(t *testing.T) {
	earlyResponsesSettings(t)
	const path = "/v1/responses"
	const rejected = `"error":{"code":"invalid_api_key","type":"authentication_error","message":"upstream rejected"}`
	for _, tc := range []struct {
		name        string
		text, usage bool
	}{
		{"text already delivered", true, false},
		{"usage in generic error event", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &dto.OpenAIResponsesRequest{Model: "gpt-6-astra", Input: []byte(`"hello"`), Stream: common.GetPointer(true)}
			recorder := httptest.NewRecorder()
			c, info := earlyResponsesContext(recorder, httptest.NewRequest(http.MethodPost, path, nil), "", req)
			info.InitChannelMeta(c)
			info.ResponsesRetryManaged = true
			c.Set("responses_retry_managed", true)
			common.SetContextKey(c, constant.ContextKeyResponsesChannelFailover, true)
			wire := ""
			if tc.text {
				wire = "data: " + `{"type":"response.output_text.delta","delta":"already seen"}` + "\n\n"
			}
			billing := ""
			if tc.usage {
				billing = `,"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}`
			}
			wire += "data: " + `{"type":"error","response":{"status":"failed",` + rejected + billing + "}}\n\n"
			observed, err := openaichannel.OaiResponsesStreamHandler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(wire))})
			require.NotNil(t, err)
			require.NotNil(t, observed)
			if tc.text {
				assert.Contains(t, recorder.Body.String(), "already seen")
			}
			if tc.usage {
				assert.Equal(t, 10, observed.TotalTokens)
				httpUsage, work := openaichannel.ResponsesHTTPFailureUsage(info, []byte(`{"response":{`+rejected+billing+`}}`))
				require.True(t, work)
				require.NotNil(t, httpUsage)
				assert.Equal(t, 10, httpUsage.TotalTokens)
				tool := `[{"type":"file_search_call","id":"fs_1"}]`
				envelope := `{"usage":{"input_tokens":4},"output":` + tool + `,"response":{"usage":{"output_tokens":3,"total_tokens":7},"output":` + tool + `}}`
				httpUsage, work = openaichannel.ResponsesHTTPFailureUsage(info, []byte(envelope))
				require.True(t, work)
				require.NotNil(t, httpUsage)
				assert.Equal(t, 4, httpUsage.PromptTokens)
				assert.Equal(t, 3, httpUsage.CompletionTokens)
				assert.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolFileSearch].CallCount)

			}
			assert.False(t, common.GetContextKeyBool(c, constant.ContextKeyResponsesUpstreamRetryable))
			assert.Equal(t, "stop", service.DecideRelayRetry(c, err, 1).Action)
		})
	}
}
