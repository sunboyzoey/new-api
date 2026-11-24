package openai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// responsesDeliveryWriter models delivery failures and a client closing the
// connection immediately after receiving a flushed frame.
type responsesDeliveryWriter struct {
	*httptest.ResponseRecorder
	writeErr   error
	flushErr   error
	shortWrite bool
	afterFlush context.CancelFunc
}

func (w *responsesDeliveryWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.shortWrite {
		return len(p) - 1, nil
	}
	return w.ResponseRecorder.Write(p)
}

func (w *responsesDeliveryWriter) FlushError() error {
	if w.flushErr != nil {
		return w.flushErr
	}
	w.ResponseRecorder.Flush()
	if w.afterFlush != nil {
		w.afterFlush()
		// Give the scanner's cancellation branch a chance to run before the
		// handler marks the terminal event done, without timing-based sleeps.
		runtime.Gosched()
	}
	return nil
}

func TestOaiResponsesStreamTerminalDelivery(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	deliveryErr := errors.New("downstream write failed")
	for _, tc := range []struct {
		name             string
		event            string
		outcome          string
		cancelAfterFlush bool
		writeErr         error
		flushErr         error
		shortWrite       bool
		wantGone         bool
	}{
		{name: "completed without EOF", event: "response.completed", outcome: "completed"},
		{name: "done alias without EOF", event: "response.done", outcome: "completed"},
		{name: "client closes after completion", event: "response.completed", outcome: "completed", cancelAfterFlush: true},
		{name: "upstream failure remains failed", event: "response.failed", outcome: "failed", cancelAfterFlush: true},
		{name: "incomplete remains incomplete", event: "response.incomplete", outcome: "incomplete"},
		{name: "cancelled remains cancelled", event: "response.cancelled", outcome: "cancelled"},
		{name: "error envelope remains failed", event: "error", outcome: "failed"},
		{name: "disconnect before terminal", event: "response.output_text.delta", cancelAfterFlush: true, wantGone: true},
		{name: "terminal write failure", event: "response.completed", outcome: "completed", writeErr: deliveryErr, wantGone: true},
		{name: "terminal short write", event: "response.completed", outcome: "completed", shortWrite: true, wantGone: true},
		{name: "terminal flush failure", event: "response.completed", outcome: "completed", flushErr: deliveryErr, wantGone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writer := &responsesDeliveryWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: tc.writeErr, flushErr: tc.flushErr, shortWrite: tc.shortWrite}
			if tc.cancelAfterFlush {
				writer.afterFlush = cancel
			}
			c, _ := gin.CreateTestContext(writer)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			payload, err := common.Marshal(map[string]any{
				"type":     tc.event,
				"response": map[string]any{"usage": map[string]any{"input_tokens": 7, "output_tokens": 3, "total_tokens": 10}},
			})
			require.NoError(t, err)
			body := &blockingBody{chunk: []byte("data: " + string(payload) + "\n\n"), closed: make(chan struct{})}
			defer body.Close()
			info := &relaycommon.RelayInfo{OriginModelName: "gpt-6-astra", DisablePing: true, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-6-astra"}}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}
			var usage *dto.Usage
			var apiErr *types.NewAPIError
			done := make(chan struct{})
			go func() {
				defer close(done)
				usage, apiErr = OaiResponsesStreamHandler(c, info, resp)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				cancel()
				_ = body.Close()
				<-done
				t.Fatal("handler waited for upstream EOF after terminal delivery")
			}
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, tc.outcome, info.StreamStatus.ResponseOutcome())
			wantReason := relaycommon.StreamEndReasonDone
			if tc.wantGone {
				wantReason = relaycommon.StreamEndReasonClientGone
			}
			assert.Equal(t, wantReason, info.StreamStatus.EndReason)
			service.MarkRequestPolicySuccess(c, info.StreamStatus)
			assert.Equal(t, !tc.wantGone && tc.outcome == "completed", service.RequestPolicy(c).Successful,
				"only a successfully delivered completion is a successful request")
			if tc.writeErr != nil || tc.flushErr != nil || tc.shortWrite {
				assert.True(t, info.StreamStatus.HasErrors())
			}
			if tc.outcome == "completed" {
				assert.Equal(t, 7, usage.PromptTokens, "keep upstream usage even if delivery fails")
				assert.Equal(t, 3, usage.CompletionTokens)
			}
			if !tc.wantGone {
				assert.Equal(t, "event: "+tc.event+"\ndata: "+string(payload)+"\n\n", writer.Body.String())
			}
			select {
			case <-body.closed:
			default:
				t.Error("upstream body was not closed")
			}
		})
	}
}

func TestOaiResponsesHandlerCountsOutputCallsNotDeclarations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	operation_setting.SetToolPriceForTest("priced_fn", 5.0)
	t.Cleanup(func() {
		operation_setting.DeleteToolPriceForTest("priced_fn")
	})

	body, err := common.Marshal(dto.OpenAIResponsesResponse{
		Tools: []map[string]any{
			{"type": "web_search_preview"},
			{"type": "file_search"},
		},
		Output: []dto.ResponsesOutput{
			{Type: dto.BuildInCallWebSearchCall},
			{Type: dto.BuildInCallWebSearchCall},
			{Type: dto.BuildInCallFunctionCall, Name: "priced_fn"},
			{Type: dto.BuildInCallFunctionCall, Name: "unpriced_fn"},
		},
		Usage: &dto.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolWebSearchPreview: {ToolName: dto.BuildInToolWebSearchPreview, CallCount: 0},
				dto.BuildInToolFileSearch:       {ToolName: dto.BuildInToolFileSearch, CallCount: 0},
			},
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	usage, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 2, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview].CallCount)
	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolFileSearch].CallCount)
	require.Contains(t, info.ResponsesUsageInfo.BuiltInTools, "priced_fn")
	assert.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools["priced_fn"].CallCount)
	assert.NotContains(t, info.ResponsesUsageInfo.BuiltInTools, "unpriced_fn")
}

func TestOaiResponsesHandlerDeclaredToolsWithoutOutputCountZero(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body, err := common.Marshal(dto.OpenAIResponsesResponse{
		Tools: []map[string]any{
			{"type": "web_search_preview"},
			{"type": "file_search"},
		},
		Output: []dto.ResponsesOutput{
			{Type: "message", Role: "assistant"},
		},
		Usage: &dto.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolWebSearchPreview: {ToolName: dto.BuildInToolWebSearchPreview, CallCount: 0},
				dto.BuildInToolFileSearch:       {ToolName: dto.BuildInToolFileSearch, CallCount: 0},
			},
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	_, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview].CallCount)
	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolFileSearch].CallCount)
}

func TestOaiResponsesHandlerCountsCompletedImageGenerationOutputs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body, err := common.Marshal(dto.OpenAIResponsesResponse{
		Status: []byte(`"completed"`),
		Output: []dto.ResponsesOutput{
			{
				Type:   dto.ResponsesOutputTypeImageGenerationCall,
				ID:     "img_1",
				Status: "completed",
				Result: "base64-a",
			},
			{
				Type:   dto.ResponsesOutputTypeImageGenerationCall,
				ID:     "img_2",
				Status: "completed",
				Result: "base64-b",
			},
			{
				Type:   dto.ResponsesOutputTypeImageGenerationCall,
				ID:     "img_empty",
				Status: "completed",
				Result: "",
			},
		},
		Usage: &dto.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.1"}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	_, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.Contains(t, info.ResponsesUsageInfo.BuiltInTools, dto.BuildInToolImageGeneration)
	assert.Equal(t, 2, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
	assert.False(t, c.GetBool("image_generation_call"))
}

func TestOaiResponsesHandlerCountsImageGenerationOnIncompleteStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body, err := common.Marshal(dto.OpenAIResponsesResponse{
		Status: []byte(`"incomplete"`),
		Output: []dto.ResponsesOutput{
			{
				Type:   dto.ResponsesOutputTypeImageGenerationCall,
				ID:     "img_1",
				Status: "completed",
				Result: "base64-a",
			},
		},
		Usage: &dto.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolImageGeneration: {ToolName: dto.BuildInToolImageGeneration, CallCount: 0},
			},
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	_, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	assert.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
}

func runResponsesImageBillingStream(t *testing.T, events ...string) *relaycommon.RelayInfo {
	t.Helper()
	gin.SetMode(gin.TestMode)
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		constant.StreamingTimeout = oldTimeout
	})

	var body strings.Builder
	for _, event := range events {
		body.WriteString("data: ")
		body.WriteString(event)
		body.WriteString("\n\n")
	}
	body.WriteString("data: [DONE]\n\n")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(common.RequestIdKey, "responses-image-billing-test")
	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		DisablePing:     true,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-5.1",
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body.String())),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	_, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, info.ResponsesUsageInfo)
	require.Contains(t, info.ResponsesUsageInfo.BuiltInTools, dto.BuildInToolImageGeneration)
	return info
}

func TestOaiResponsesStreamHandlerDeduplicatesCompletedImageOutput(t *testing.T) {
	item := `{"type":"image_generation_call","id":"img_1","call_id":"call_1","status":"completed","result":"base64-a"}`
	info := runResponsesImageBillingStream(
		t,
		`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
		`{"type":"response.completed","response":{"status":"completed","output":[`+item+`],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	)

	assert.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
}

func TestOaiResponsesStreamHandlerCountsImageOutputOnIncomplete(t *testing.T) {
	info := runResponsesImageBillingStream(
		t,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"image_generation_call","id":"img_1","status":"completed","result":"base64-a"}}`,
		`{"type":"response.incomplete","response":{"status":"incomplete"}}`,
	)

	assert.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
}

func TestOaiResponsesStreamHandlerDoesNotCountPartialImageEvent(t *testing.T) {
	info := runResponsesImageBillingStream(
		t,
		`{"type":"response.image_generation_call.partial_image","output_index":0,"partial_image_b64":"partial-bytes"}`,
		`{"type":"response.completed","response":{"status":"completed","output":[]}}`,
	)

	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
}

func TestOaiResponsesHandlerRewritesSGLangCreatedAtToInt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeSGLang},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_1","object":"response","created_at":1786588600.0,"status":"completed","output":[]}`)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	_, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	assert.Contains(t, w.Body.String(), `"created_at":1786588600`)
	assert.NotContains(t, w.Body.String(), `1786588600.0`)
}

func TestOaiResponsesStreamHandlerRewritesSGLangCreatedAtToInt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		constant.StreamingTimeout = oldTimeout
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(common.RequestIdKey, "sglang-created-at-test")

	info := &relaycommon.RelayInfo{
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeSGLang,
			UpstreamModelName: "served-model",
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"created_at\":1.7865886E9,\"status\":\"completed\",\"output\":[]}}\n\n" +
				"data: [DONE]\n\n",
		)),
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	_, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	assert.Contains(t, w.Body.String(), `"created_at":1786588600`)
	assert.NotContains(t, w.Body.String(), `1.7865886E9`)
	assert.NotContains(t, w.Body.String(), `1786588600.0`)
}

func TestOaiResponsesStreamHandlerKeepsNonSGLangCreatedAt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		constant.StreamingTimeout = oldTimeout
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(common.RequestIdKey, "openai-created-at-test")

	info := &relaycommon.RelayInfo{
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "gpt-test"},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"created_at\":1786588600.0,\"status\":\"completed\",\"output\":[]}}\n\n" +
				"data: [DONE]\n\n",
		)),
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	_, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	assert.Contains(t, w.Body.String(), `1786588600.0`)
}

// A converted hosted web search reaches Azure and Xinference Chat Completions
// as web_search_options, like OpenAI; the upstream decides whether it applies.
// Azure Responses bills web search by tool_usage.web_search.num_requests (Bing
// transactions) under bing_web_search, and the explicit zeros silence the
// web_search_call items, which leaves one surcharge row.
func TestAzureHostedWebSearch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		constant.StreamingTimeout = oldTimeout
	})

	t.Run("Claude to Chat", func(t *testing.T) {
		const body = `{"model":"gpt-5.1","max_tokens":64,"messages":[{"role":"user","content":"Weather?"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
		for _, channelType := range []int{constant.ChannelTypeAzure, constant.ChannelTypeXinference, constant.ChannelTypeOpenAI} {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			info := &relaycommon.RelayInfo{
				RelayFormat: types.RelayFormatClaude,
				ChannelMeta: &relaycommon.ChannelMeta{ChannelType: channelType, UpstreamModelName: "gpt-5.1"},
			}
			adaptor := &Adaptor{}
			adaptor.Init(info)
			var request dto.ClaudeRequest
			require.NoError(t, common.UnmarshalJsonStr(body, &request))
			converted, err := adaptor.ConvertClaudeRequest(c, info, &request)
			require.NoError(t, err)
			wire, err := common.Marshal(converted)
			require.NoError(t, err)
			assert.True(t, strings.Contains(string(wire), `"web_search_options"`), channelType)
			var codes []string
			for _, diagnostic := range info.ConversionDiagnostics() {
				codes = append(codes, diagnostic.Code)
			}
			assert.False(t, slices.Contains(codes, "hosted_web_search_omitted"), channelType)
		}
	})

	const searchCall = `{"type":"web_search_call","status":"completed","action":{"type":"search","query":"go"}}`
	const usage = `"usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13},"tool_usage":{"web_search":{"num_requests":5}}`
	output := strings.Repeat(searchCall+",", 3) + `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Go 1.25"}]}`
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{
			name:        "json",
			contentType: "application/json",
			body:        `{"id":"resp_1","object":"response","model":"gpt-5.1","status":"completed","output":[` + output + `],` + usage + `}`,
		},
		{
			name:        "stream",
			contentType: "text/event-stream",
			body: strings.Repeat(`data: {"type":"response.output_item.done","output_index":0,"item":`+searchCall+"}\n\n", 3) +
				`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.1","status":"completed","output":[` + output + `],` + usage + "}}\n\ndata: [DONE]\n\n",
		},
	}
	for _, tt := range tests {
		t.Run("Responses "+tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			info := &relaycommon.RelayInfo{
				RelayMode:       relayconstant.RelayModeResponses,
				RelayFormat:     types.RelayFormatOpenAIResponses,
				OriginModelName: "gpt-5.1",
				IsStream:        tt.name == "stream",
				DisablePing:     true,
				ChannelMeta:     &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeAzure, UpstreamModelName: "gpt-5.1"},
			}
			adaptor := &Adaptor{}
			adaptor.Init(info)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(tt.body)),
				Header:     http.Header{"Content-Type": []string{tt.contentType}},
			}
			var apiErr *types.NewAPIError
			if info.IsStream {
				_, apiErr = OaiResponsesStreamHandler(c, info, resp)
			} else {
				_, apiErr = OaiResponsesHandler(c, info, resp)
			}
			require.Nil(t, apiErr)
			billed := map[string]int{}
			for name, tool := range info.ResponsesUsageInfo.BuiltInTools {
				if tool.CallCount > 0 {
					billed[name] = tool.CallCount
				}
			}
			assert.Equal(t, map[string]int{"bing_web_search": 5}, billed)
		})
	}
}

func TestManagedResponsesTerminalDisconnect(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	for _, tc := range []struct {
		name, event, outcome string
		flushError           bool
		wantError            bool
	}{
		{"completed then disconnect", "response.completed", "completed", false, false},
		{"done then disconnect", "response.done", "completed", false, false},
		{"incomplete then disconnect", "response.incomplete", "incomplete", false, false},
		{"cancelled then disconnect", "response.cancelled", "cancelled", false, false},
		{"disconnect before terminal", "response.output_text.delta", "failed", false, true},
		{"terminal flush fails", "response.completed", "failed", true, true},
		{"upstream failure", "response.failed", "failed", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writer := &responsesDeliveryWriter{ResponseRecorder: httptest.NewRecorder(), afterFlush: cancel}
			if tc.flushError {
				writer.flushErr = errors.New("downstream flush failed")
			}
			c, _ := gin.CreateTestContext(writer)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			info := &relaycommon.RelayInfo{ResponsesRetryManaged: true, DisablePing: true, ChannelMeta: &relaycommon.ChannelMeta{}}
			payload, err := common.Marshal(map[string]any{"type": tc.event, "delta": "hello", "response": map[string]any{"usage": map[string]any{"input_tokens": 7, "output_tokens": 3}}})
			require.NoError(t, err)
			usage, apiErr := OaiResponsesStreamHandler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data: " + string(payload) + "\n\n"))})
			if tc.wantError {
				require.NotNil(t, apiErr)
			} else {
				require.Nil(t, apiErr)
				assert.ErrorIs(t, ctx.Err(), context.Canceled)
				assert.Equal(t, "event: "+tc.event+"\ndata: "+string(payload)+"\n\n", writer.Body.String())
			}
			require.NotNil(t, usage)
			if tc.event != "response.output_text.delta" {
				assert.Equal(t, 10, usage.TotalTokens)
			}
			assert.Equal(t, tc.outcome, info.StreamStatus.ResponseOutcome())
		})
	}
}

func TestResponsesProgressiveAttemptDelivery(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	const partialText = "visible model text"
	const delta = `{"type":"response.output_text.delta","delta":"` + partialText + `"}`
	const usage = `"usage":{"input_tokens":23,"output_tokens":7}`
	const failed = `{"type":"response.failed","response":{"status":"failed","error":{"type":"server_error","message":"upstream failed"},` + usage + `}}`
	const completed = `{"type":"response.completed","response":{"status":"completed",` + usage + `}}`
	const incomplete = `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},` + usage + `}}`
	const search = `{"type":"response.output_item.added","item":{"type":"web_search_call","id":"item_1"}}`
	const external = `{"type":"response.output_item.added","item":{"type":"mcp_call","id":"item_2"}}`
	for _, tc := range []struct {
		name   string
		events []string
		tools  string
		failed bool
		retry  bool
	}{
		{"hosted search fails before visible text", []string{search, failed}, `[{"type":"namespace","tools":[{"type":"function"}]},{"type":"web_search"}]`, true, true},
		{"unexpected external action blocks replay", []string{delta, external, failed}, "", true, false},
		{"success releases original event sequence", []string{delta, search, completed}, "", false, false},
		{"incomplete releases legitimate partial result", []string{delta, incomplete}, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			common.SetContextKey(c, constant.ContextKeyResponsesChannelFailover, true)
			c.Set("responses_retry_managed", true)
			info := &relaycommon.RelayInfo{
				Request:               &dto.OpenAIResponsesRequest{Tools: []byte(tc.tools)},
				ResponsesRetryManaged: true, DisablePing: true, ChannelMeta: &relaycommon.ChannelMeta{},
			}
			var stream strings.Builder
			for _, event := range tc.events {
				stream.WriteString("data: " + event + "\n\n")
			}
			usage, apiErr := OaiResponsesStreamHandler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(stream.String()))})
			require.NotNil(t, usage)
			assert.Equal(t, 30, usage.TotalTokens)
			assert.Equal(t, tc.retry, common.GetContextKeyBool(c, constant.ContextKeyResponsesUpstreamRetryable))
			if tc.failed {
				require.NotNil(t, apiErr)
				if tc.retry {
					assert.Empty(t, recorder.Body.String(), "pre-text search activity is held until the attempt can answer")
				} else {
					assert.Contains(t, recorder.Body.String(), partialText)
					assert.NotContains(t, recorder.Body.String(), "event: response.failed", "the router finalizes the error once")
				}
			} else {
				require.Nil(t, apiErr)
				var want strings.Builder
				for _, event := range tc.events {
					var decoded dto.ResponsesStreamResponse
					require.NoError(t, common.UnmarshalJsonStr(event, &decoded))
					want.WriteString("event: " + decoded.Type + "\ndata: " + event + "\n\n")
				}
				assert.Equal(t, want.String(), recorder.Body.String())
			}
		})
	}
}
