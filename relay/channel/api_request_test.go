package channel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	openaisdk "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeepaliveClientCompatibility(t *testing.T) {
	settings := operation_setting.GetGeneralSetting()
	oldEnabled, oldSeconds := settings.PingIntervalEnabled, settings.PingIntervalSeconds
	oldTimeout := constant.StreamingTimeout
	settings.PingIntervalEnabled, settings.PingIntervalSeconds = true, 1
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		settings.PingIntervalEnabled, settings.PingIntervalSeconds = oldEnabled, oldSeconds
		constant.StreamingTimeout = oldTimeout
	})
	service.InitHttpClient()
	const chunk = "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"
	params := openaisdk.ChatCompletionNewParams{
		Model:    "test-model",
		Messages: []openaisdk.ChatCompletionMessageParamUnion{openaisdk.UserMessage("test")},
	}
	t.Run("heartbeat reaches client before model output without changing first response time", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		observed := make(chan *relaycommon.RelayInfo, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, _ := gin.CreateTestContext(w)
			c.Request = r
			reader, writer := io.Pipe()
			defer reader.Close()
			go func() {
				defer writer.Close()
				select {
				case <-release:
					_, _ = io.WriteString(writer, chunk)
				case <-r.Context().Done():
				}
			}()
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
			helper.StreamScannerHandler(c, &http.Response{Body: reader}, info, func(data string, sr *helper.StreamResult) {})
			observed <- info
		}))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err, "headers must arrive before upstream releases any model data")
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		require.NoError(t, err)
		assert.Equal(t, ": PING\n", line)
		_ = resp.Body.Close()
		cancel()
		select {
		case info := <-observed:
			assert.True(t, info.FirstResponseTime.IsZero())
			assert.Zero(t, info.ReceivedResponseCount)
		case <-time.After(2 * time.Second):
			t.Fatal("scanner did not stop after client disconnected")
		}
	})
	for _, tc := range []struct {
		name   string
		stream bool
		status int
	}{
		{"stream comments are ignored by SDK", true, 200},
		{"nonstream JSON remains intact", false, 200},
		{"delayed rate limit keeps HTTP 429", true, 429},
		{"delayed gateway error keeps HTTP 502", true, 502},
		{"nonstream rate limit keeps HTTP 429", false, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status != 200 {
					// Exceed the configured heartbeat interval: pre-header
					// heartbeats would commit 200 and corrupt SDK errors.
					timer := time.NewTimer(1200 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.stream && tc.status == 200 {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				w.WriteHeader(tc.status)
				switch {
				case tc.status != 200:
					_, _ = io.WriteString(w, "{\"error\":{\"message\":\"test upstream failure\",\"type\":\"server_error\",\"code\":\"test_error\"}}")
				case tc.stream:
					_, _ = io.WriteString(w, chunk)
				default:
					_, _ = io.WriteString(w, "{\"id\":\"chatcmpl-test\",\"object\":\"chat.completion\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}")
				}
			}))
			defer upstream.Close()
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := gin.CreateTestContext(w)
				c.Request = r
				req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, IsStream: tc.stream}
				resp, err := doRequest(c, req, info)
				if err != nil {
					http.Error(w, err.Error(), 502)
					return
				}
				defer resp.Body.Close()
				if tc.stream && resp.StatusCode == 200 {
					helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
						if err := helper.StringData(c, data); err != nil {
							sr.Stop(err)
						}
					})
					helper.Done(c)
					return
				}
				w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
				w.WriteHeader(resp.StatusCode)
				_, _ = io.Copy(w, resp.Body)
			}))
			defer gateway.Close()
			client := openaisdk.NewClient(option.WithBaseURL(gateway.URL), option.WithAPIKey("local-test"), option.WithMaxRetries(0))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var err error
			if tc.stream {
				stream := client.Chat.Completions.NewStreaming(ctx, params)
				defer stream.Close()
				var content strings.Builder
				for stream.Next() {
					for _, choice := range stream.Current().Choices {
						content.WriteString(choice.Delta.Content)
					}
				}
				err = stream.Err()
				if tc.status == 200 {
					assert.Equal(t, "hello", content.String())
				}
			} else {
				result, resultErr := client.Chat.Completions.New(ctx, params)
				err = resultErr
				if tc.status == 200 {
					require.NoError(t, err)
					require.Len(t, result.Choices, 1)
					assert.Equal(t, "hello", result.Choices[0].Message.Content)
				}
			}
			if tc.status == 200 {
				require.NoError(t, err)
			} else {
				var apiErr *openaisdk.Error
				require.ErrorAs(t, err, &apiErr, "SDK must receive an HTTP error, not a successful empty stream")
				assert.Equal(t, tc.status, apiErr.StatusCode)
			}
		})
	}
}

func TestDoRequestRecordsTicketSummaryWithoutSecretOrStaleAttempt(t *testing.T) {
	service.InitHttpClient()
	state := strings.Repeat("s", 780)
	digest := sha256.Sum256([]byte(state))
	fingerprint := hex.EncodeToString(digest[:])
	for _, stream := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Codex-Turn-State", state)
			w.Header().Set("X-Sub2api-Ticket-Used-Length", "292")
			w.Header().Set("X-Sub2api-Ticket-Used-Fingerprint", fingerprint)
			_, _ = w.Write([]byte("{}"))
		}))
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte("{}")))
		req, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader([]byte("{}")))
		require.NoError(t, err)
		info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, IsStream: stream}
		resp, err := doRequest(ctx, req, info)
		require.NoError(t, err)
		_ = resp.Body.Close()
		server.Close()
		require.NotNil(t, info.CodexTicket.Returned)
		assert.Equal(t, 780, info.CodexTicket.Returned.Length)
		assert.Equal(t, fingerprint, info.CodexTicket.Returned.Fingerprint)
		assert.Equal(t, 292, info.CodexTicket.Used.Length)
		other := model.NewLogOther()
		service.AppendRelayLogAdminInfo(ctx, info, other)
		stored := other.JSONString()
		assert.NotContains(t, stored, state)
		var metadata map[string]any
		require.NoError(t, common.UnmarshalJsonStr(stored, &metadata))
		assert.NotContains(t, metadata, "codex_ticket")
		assert.Contains(t, metadata["admin_info"], "codex_ticket")
		info.InitChannelMeta(ctx)
		assert.Nil(t, info.CodexTicket)
		info.ObserveCodexTicketHeaders(http.Header{})
		require.NotNil(t, info.CodexTicket)
		assert.Nil(t, info.CodexTicket.Returned)
		assert.Nil(t, info.CodexTicket.Used)
	}
	info := &relaycommon.RelayInfo{}
	info.ObserveCodexTicketHeaders(http.Header{
		"X-Codex-Turn-State":                {strings.Repeat("s", 2049)},
		"X-Sub2api-Ticket-Used-Length":      {"780"},
		"X-Sub2api-Ticket-Used-Fingerprint": {strings.Repeat("z", 64)},
	})
	assert.Nil(t, info.CodexTicket.Returned)
	assert.Nil(t, info.CodexTicket.Used)
}

func TestNewTaskAPIRequestInheritsClientCancellation(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)

	upstream, err := newTaskAPIRequest(c, "https://provider.example/tasks", nil)
	require.NoError(t, err)
	cancel()

	require.ErrorIs(t, upstream.Context().Err(), context.Canceled)
}

func TestProcessHeaderOverride_ChannelTestSkipsPassthroughRules(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*": "",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Empty(t, headers)
}

func TestProcessHeaderOverride_ChannelTestSkipsClientHeaderPlaceholder(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Upstream-Trace": "{client_header:X-Trace-Id}",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	_, ok := headers["x-upstream-trace"]
	require.False(t, ok)
}

func TestProcessHeaderOverride_NonTestKeepsClientHeaderPlaceholder(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Upstream-Trace": "{client_header:X-Trace-Id}",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "trace-123", headers["x-upstream-trace"])
}

func TestProcessHeaderOverride_RuntimeOverrideIsFinalHeaderMap(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		IsChannelTest:             false,
		UseRuntimeHeadersOverride: true,
		RuntimeHeadersOverride: map[string]any{
			"x-static":  "runtime-value",
			"x-runtime": "runtime-only",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Static": "legacy-value",
				"X-Legacy": "legacy-only",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "runtime-value", headers["x-static"])
	require.Equal(t, "runtime-only", headers["x-runtime"])
	_, exists := headers["x-legacy"]
	require.False(t, exists)
}

func TestProcessHeaderOverride_PassthroughSkipsAcceptEncoding(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")
	ctx.Request.Header.Set("Accept-Encoding", "gzip")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*": "",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "trace-123", headers["x-trace-id"])

	_, hasAcceptEncoding := headers["accept-encoding"]
	require.False(t, hasAcceptEncoding)
}

func TestProcessHeaderOverride_PassHeadersTemplateSetsRuntimeHeaders(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("Originator", "Codex CLI")
	ctx.Request.Header.Set("Session_id", "sess-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		RequestHeaders: map[string]string{
			"Originator": "Codex CLI",
			"Session_id": "sess-123",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ParamOverride: map[string]any{
				"operations": []any{
					map[string]any{
						"mode":  "pass_headers",
						"value": []any{"Originator", "Session_id", "X-Codex-Beta-Features"},
					},
				},
			},
			HeadersOverride: map[string]any{
				"X-Static": "legacy-value",
			},
		},
	}

	_, err := relaycommon.ApplyParamOverrideWithRelayInfo([]byte(`{"model":"gpt-4.1"}`), info)
	require.NoError(t, err)
	require.True(t, info.UseRuntimeHeadersOverride)
	require.Equal(t, "Codex CLI", info.RuntimeHeadersOverride["originator"])
	require.Equal(t, "sess-123", info.RuntimeHeadersOverride["session_id"])
	_, exists := info.RuntimeHeadersOverride["x-codex-beta-features"]
	require.False(t, exists)
	require.Equal(t, "legacy-value", info.RuntimeHeadersOverride["x-static"])

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "Codex CLI", headers["originator"])
	require.Equal(t, "sess-123", headers["session_id"])
	_, exists = headers["x-codex-beta-features"]
	require.False(t, exists)

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyHeaderOverrideToRequest(upstreamReq, headers)
	require.Equal(t, "Codex CLI", upstreamReq.Header.Get("Originator"))
	require.Equal(t, "sess-123", upstreamReq.Header.Get("Session_id"))
	require.Empty(t, upstreamReq.Header.Get("X-Codex-Beta-Features"))
}

func TestToWebSocketURL(t *testing.T) {
	for input, want := range map[string]string{
		"https://api.openai.com/v1/responses":             "wss://api.openai.com/v1/responses",
		"http://127.0.0.1:3000/v1/responses":              "ws://127.0.0.1:3000/v1/responses",
		"wss://chatgpt.com/backend-api/codex/responses":   "wss://chatgpt.com/backend-api/codex/responses",
		"ws://127.0.0.1:3000/backend-api/codex/responses": "ws://127.0.0.1:3000/backend-api/codex/responses",
	} {
		assert.Equal(t, want, toWebSocketURL(input), input)
	}
}
