package relay

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestChatStreamRetryMatchesWirePrefix(t *testing.T) {
	for _, tc := range []struct {
		name, alternate string
		matches         bool
	}{
		{"different chunk boundaries", "hello again", true},
		{"diverging text", "unrelated", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			common.SetContextKey(c, constant.ContextKeyModelChannelFailover, true)
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, IsStream: true, RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI, StreamStatus: relaycommon.NewStreamStatus()}
			eligibility := service.ClassifyChatStreamReplay([]byte(`{"stream":true}`))
			first, apiErr := startChatStreamAttempt(c, info, eligibility)
			require.Nil(t, apiErr)
			write := func(s string) {
				t.Helper()
				n, err := c.Writer.WriteString(s)
				require.NoError(t, err)
				require.Equal(t, len(s), n)
			}
			write(": PING\n")
			write("data: " + `{"id":"primary","model":"first-model","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"hel"},"logprobs":null,"finish_reason":null}]}`)
			write("\n\n")
			write("data: " + `{"id":"primary","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}` + "\n\n")
			require.Contains(t, rec.Body.String(), `"content":"lo"`)
			firstErr := types.NewOpenAIError(errors.New("truncated"), types.ErrorCodeBadResponse, http.StatusBadGateway)
			require.NotNil(t, first.finish(c, info, nil, firstErr))
			require.True(t, common.GetContextKeyBool(c, constant.ContextKeyResponsesUpstreamRetryable))
			info.StreamStatus = relaycommon.NewStreamStatus()
			second, apiErr := startChatStreamAttempt(c, info, eligibility)
			require.Nil(t, apiErr)
			write("data: " + `{"id":"secondary","model":"other-model","created":2,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n")
			write("data: " + `{"id":"secondary","model":"other-model","created":2,"choices":[{"index":0,"delta":{"content":"` + tc.alternate + `"},"logprobs":null,"finish_reason":"stop"}],"usage":{"total_tokens":5}}` + "\n\n")
			write("data: [DONE]\n\n")
			errFinal := second.finish(c, info, nil, nil)
			output := rec.Body.String()
			require.NotContains(t, output, "secondary")
			require.NotContains(t, output, "other-model")
			require.NotContains(t, output, tc.alternate)
			if tc.matches {
				require.Nil(t, errFinal)
				require.Contains(t, output, `"content":" again"`)
				require.Equal(t, 1, strings.Count(output, "[DONE]"))
				require.Equal(t, 1, strings.Count(output, `"total_tokens":5`))
			} else {
				require.NotNil(t, errFinal)
				require.Contains(t, errFinal.Error(), "prefix")
				require.NotContains(t, output, "[DONE]")
				require.True(t, info.StreamStatus.ResponseFailed())
			}
		})
	}
}

func TestChatStreamPreservesSuccessfulOpaqueChoices(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := &chatAttemptWriter{ResponseWriter: c.Writer, public: &chatPublicStream{}}
	c.Writer = w
	info := &relaycommon.RelayInfo{StreamStatus: relaycommon.NewStreamStatus()}
	frame := `{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call","type":"function","function":{"name":"sample","arguments":"{}"}}]},"finish_reason":"tool_calls"},{"index":1,"delta":{"content":"ok"},"finish_reason":"stop","logprobs":{"content":[]}}],"usage":{"total_tokens":2}}`
	_, err := w.WriteString("data: " + frame + "\n\ndata: [DONE]\n\n")
	require.NoError(t, err)
	require.Nil(t, w.finish(c, info, nil, nil))
	require.Equal(t, "data: "+frame+"\n\ndata: [DONE]\n\n", rec.Body.String())
	require.True(t, w.public.opaque)
}

func TestChatStreamDiscardsFailedBillingState(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyModelChannelFailover, true)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, IsStream: true, RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI, StreamStatus: relaycommon.NewStreamStatus(), ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{BuiltInTools: map[string]*relaycommon.BuildInToolInfo{"lookup": {CallCount: 1}}}}
	eligibility := service.ClassifyChatStreamReplay([]byte(`{"stream":true,"tools":[{"type":"function"}]}`))
	w, apiErr := startChatStreamAttempt(c, info, eligibility)
	require.Nil(t, apiErr)
	info.BuiltInTools["lookup"].CallCount = 3
	require.NotNil(t, w.finish(c, info, nil, types.NewOpenAIError(errors.New("failed attempt"), types.ErrorCodeBadResponse, http.StatusBadGateway)))
	require.Equal(t, 1, info.BuiltInTools["lookup"].CallCount)
}

func TestChatStreamUnsafeAlternateNeverDispatched(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyModelChannelFailover, true)
	info := &relaycommon.RelayInfo{IsStream: true, RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI}
	c.Set("chat_public_stream", &chatPublicStream{started: true})
	eligibility := service.ClassifyChatStreamReplay([]byte(`{"stream":true,"tools":[{"type":"mcp"}]}`))
	w, apiErr := startChatStreamAttempt(c, info, eligibility)
	require.Nil(t, w)
	require.NotNil(t, apiErr)
	require.Empty(t, rec.Body.String())
}

func TestChatTerminalContainingTextIsImmediatelyReadable(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := &chatAttemptWriter{ResponseWriter: c.Writer, public: &chatPublicStream{}}
	info := &relaycommon.RelayInfo{StreamStatus: relaycommon.NewStreamStatus()}
	_, err := w.WriteString("data: " + `{"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}],"usage":{"total_tokens":1}}` + "\n\n")
	require.NoError(t, err)
	first := strings.TrimSpace(strings.TrimPrefix(rec.Body.String(), "data: "))
	require.Equal(t, "answer", gjson.Get(first, "choices.0.delta.content").String())
	require.False(t, gjson.Get(first, "usage").Exists())
	require.Equal(t, gjson.Null, gjson.Get(first, "choices.0.finish_reason").Type)
	_, err = w.WriteString("data: [DONE]\n\n")
	require.NoError(t, err)
	require.Nil(t, w.finish(c, info, nil, nil))
	require.Equal(t, 1, strings.Count(rec.Body.String(), "answer"))
	require.Equal(t, 1, strings.Count(rec.Body.String(), "stop"))
}

func TestPassthroughChatRetryUsesCachedRequestPolicy(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyChatStreamReplayPolicy, service.ChatStreamReplayable)
	// A nil reader would panic if the relay tried to reopen or copy the raw
	// prompt here instead of reusing its ingress policy.
	require.Equal(t, service.ChatStreamReplayable, passthroughChatEligibility(c, nil))
}
