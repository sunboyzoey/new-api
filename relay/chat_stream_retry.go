package relay

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const maxChatPrefix = 1 << 20

var errChatPrefix = errors.New("alternate channel did not reproduce the delivered chat prefix")

type chatTextDelta struct {
	field string
	index int64
	text  string
}

// Keep only the public text needed to validate a subsequent channel. Tool and
// other opaque deltas can pass on the original stream but cannot be spliced.
type chatPublicStream struct {
	deltas          []chatTextDelta
	size            int
	opaque, started bool
	id, model       string
	created         int64
}

type chatAttemptWriter struct {
	gin.ResponseWriter
	public                  *chatPublicStream
	replaying, opaquePrefix bool
	upstream                io.Closer
	cursor, offset, limit   int
	line                    []byte
	tail                    []byte
	done, comment           bool
	err                     error
	writeErr                error
	originalTools           *relaycommon.ResponsesUsageInfo
	imageCount              *int
	ratios                  map[string]float64
	thinking                relaycommon.ThinkingContentInfo
}

func passthroughChatEligibility(c *gin.Context, body common.ReplayableBody) service.ChatStreamReplayPolicy {
	if policy, found := common.GetContextKeyType[service.ChatStreamReplayPolicy](c, constant.ContextKeyChatStreamReplayPolicy); found {
		return policy
	}
	// Non-HTTP SDK callers may not run the distributor before the adapter.
	r, err := body.NewReader()
	if err != nil {
		return service.ChatStreamUnmanaged
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, (8<<20)+1))
	if err != nil || len(raw) > 8<<20 {
		return service.ChatStreamUnmanaged
	}
	return service.ClassifyChatStreamReplay(raw)
}

func isChatStreamRequest(c *gin.Context, info *relaycommon.RelayInfo) bool {
	return info.IsStream && info.RelayFormat == types.RelayFormatOpenAI && (info.RelayMode == relayconstant.RelayModeChatCompletions || info.RelayMode == relayconstant.RelayModeResponses) && common.GetContextKeyBool(c, constant.ContextKeyModelChannelFailover)
}

func startChatStreamAttempt(c *gin.Context, info *relaycommon.RelayInfo, eligibility service.ChatStreamReplayPolicy) (*chatAttemptWriter, *types.NewAPIError) {
	if !isChatStreamRequest(c, info) {
		return nil, nil
	}
	if format := info.GetFinalRequestRelayFormat(); format != types.RelayFormatOpenAI && format != types.RelayFormatOpenAIResponses {
		// Provider-native tools have different declarations (for example,
		// Claude input_schema). Keep their existing pre-output routing.
		eligibility = service.ChatStreamUnmanaged
	}
	public, _ := common.GetContextKeyType[*chatPublicStream](c, "chat_public_stream")
	if eligibility != service.ChatStreamReplayable {
		if public != nil && public.started {
			return nil, types.WithOpenAIError(types.OpenAIError{Code: "server_error", Type: "server_error", Message: "alternate channel cannot safely continue this chat stream"}, http.StatusBadGateway)
		}
		c.Set("chat_retry_unsafe", eligibility == service.ChatStreamExternalWork)
		c.Set("chat_retry_managed", false)
		return nil, nil
	}
	if public == nil || !public.started {
		public = &chatPublicStream{}
		c.Set("chat_public_stream", public)
	}
	w := &chatAttemptWriter{ResponseWriter: c.Writer, public: public, replaying: public.started, opaquePrefix: public.opaque, limit: len(public.deltas)}
	w.originalTools = info.ResponsesUsageInfo
	if w.originalTools != nil {
		tools := make(map[string]*relaycommon.BuildInToolInfo, len(w.originalTools.BuiltInTools))
		for name, tool := range w.originalTools.BuiltInTools {
			if tool != nil {
				snapshot := *tool
				tools[name] = &snapshot
			}
		}
		info.ResponsesUsageInfo = &relaycommon.ResponsesUsageInfo{BuiltInTools: tools}
	}
	w.imageCount = info.BillingImageCount
	w.ratios = info.PriceData.OtherRatios()
	w.thinking = info.ThinkingContentInfo
	c.Writer = w
	c.Set("chat_retry_managed", true)
	return w, nil
}

func (w *chatAttemptWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *chatAttemptWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func (w *chatAttemptWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && w.err == nil && w.writeErr == nil {
		line, rest, found := bytes.Cut(p, []byte{'\n'})
		limit := constant.StreamScannerMaxBufferMB << 20
		if limit <= 0 {
			limit = 128 << 20
		}
		if len(line) > limit-len(w.line) {
			w.err = errors.New("upstream chat SSE line exceeds scanner limit")
			break
		}
		if !found {
			w.line = append(w.line, line...)
			break
		}
		if len(w.line) > 0 {
			w.line = append(w.line, line...)
			line = w.line
		}
		w.acceptLine(strings.TrimSuffix(string(line), "\r"))
		if cap(w.line) > 64<<10 {
			w.line = nil
		} else {
			w.line = w.line[:0]
		}
		p = rest
	}
	if w.err != nil && w.upstream != nil {
		_ = w.upstream.Close()
		w.upstream = nil
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	// A mismatch is an upstream failure, not a failed client write. Report it
	// at the adaptation boundary; never forward a divergent suffix.
	return n, nil
}

func (w *chatAttemptWriter) send(s string) {
	if w.writeErr == nil {
		var n int
		n, w.writeErr = w.ResponseWriter.Write([]byte(s))
		if w.writeErr == nil && n != len(s) {
			w.writeErr = io.ErrShortWrite
		}
	}
}

func (w *chatAttemptWriter) acceptLine(line string) {
	if line == "" {
		if w.comment {
			w.send("\n")
		}
		w.comment = false
		return
	}
	w.comment = strings.HasPrefix(line, ":")
	if w.comment {
		w.send(line + "\n")
		return
	}
	data, ok := strings.CutPrefix(line, "data:")
	if !ok {
		w.err = errors.New("invalid upstream chat SSE frame")
		return
	}
	data = strings.TrimSpace(data)
	if data == "[DONE]" {
		if w.cursor < w.limit || w.replaying && w.opaquePrefix {
			w.err = errChatPrefix
			return
		}
		w.done = true
		return
	}
	if w.done || !gjson.Valid(data) {
		w.err = errors.New("invalid upstream chat stream")
		return
	}
	root := gjson.Parse(data)
	choiceList := root.Get("choices")
	if (!choiceList.IsArray() && !(choiceList.Type == gjson.Null && root.Get("usage").IsObject())) || root.Get("error").IsObject() {
		w.err = errors.New("upstream chat stream failed")
		return
	}
	choices := choiceList.Array()
	if w.replaying && w.opaquePrefix {
		w.err = errChatPrefix
		return
	}
	for _, key := range []string{"id", "model", "created"} {
		v := root.Get(key)
		if !v.Exists() {
			continue
		}
		var target any
		if (key == "id" || key == "model") && len(v.String()) > 4096 {
			w.opaqueFrame(data)
			return
		}
		switch key {
		case "id":
			if w.public.id == "" {
				w.public.id = strings.Clone(v.String())
			}
			if v.String() != w.public.id {
				target = w.public.id
			}
		case "model":
			if w.public.model == "" {
				w.public.model = strings.Clone(v.String())
			}
			if v.String() != w.public.model {
				target = w.public.model
			}
		case "created":
			if w.public.created == 0 {
				w.public.created = v.Int()
			}
			if v.Int() != w.public.created {
				target = w.public.created
			}
		}
		if target != nil {
			data, w.err = sjson.Set(data, key, target)
			if w.err != nil {
				return
			}
		}
	}

	knownFields := true
	root.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "choices", "id", "object", "model", "created", "system_fingerprint", "service_tier", "usage", "obfuscation":
		default:
			knownFields = false
		}
		return knownFields
	})
	if !knownFields {
		w.opaqueFrame(data)
		return
	}
	// An opaque chunk passes unchanged on the first attempt. It cannot be
	// combined with a different response's text, tool calls, or logprobs.
	if len(choices) > 1 {
		w.opaqueFrame(data)
		return
	}
	if len(choices) == 0 {
		if w.cursor >= w.limit {
			w.hold(data)
		}
		return
	}
	choice := choices[0]
	delta := choice.Get("delta")
	terminal := choice.Get("finish_reason").String() != ""
	field, txt := "", ""
	plain := delta.IsObject() && choice.Get("logprobs").Type == gjson.Null
	choice.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "index", "delta", "finish_reason", "logprobs":
		default:
			plain = false
		}
		return plain
	})
	delta.ForEach(func(k, v gjson.Result) bool {
		if v.Type == gjson.Null {
			return true
		}
		switch k.String() {
		case "role":
			plain = plain && v.String() == "assistant"
		case "content", "reasoning_content", "reasoning", "refusal":
			if v.Type != gjson.String {
				plain = false
				return false
			}
			if v.String() != "" {
				if field != "" {
					plain = false
					return false
				}
				field, txt = k.String(), v.String()
			}
		default:
			plain = false
		}
		return plain
	})
	if !plain {
		w.opaqueFrame(data)
		return
	}
	if terminal {
		if w.cursor < w.limit && field == "" {
			w.err = errChatPrefix
			return
		}
		if field == "" {
			w.hold(data)
			return
		}
		// A provider may put the first answer text and finish in one frame.
		// Release the text now and keep only its terminal and usage for success.
		last, err := sjson.SetRaw(data, "choices.0.delta", "{}")
		if err != nil {
			w.err = err
			return
		}
		w.hold(last)
		data, w.err = sjson.Set(data, "choices.0.finish_reason", nil)
		if w.err != nil {
			return
		}
	}
	if root.Get("usage").IsObject() && (terminal || w.replaying && w.cursor < w.limit) {
		data, w.err = sjson.Delete(data, "usage")
		if w.err != nil {
			return
		}
	}
	if field == "" {
		if w.replaying {
			return
		}
	} else {
		idx := choice.Get("index").Int()
		for w.cursor < w.limit && len(txt) > 0 {
			old := w.public.deltas[w.cursor]
			rem := old.text[w.offset:]
			n := min(len(txt), len(rem))
			if old.index != idx || old.field != field || txt[:n] != rem[:n] {
				w.err = errChatPrefix
				return
			}
			txt = txt[n:]
			w.offset += n
			if w.offset == len(old.text) {
				w.cursor++
				w.offset = 0
			}
		}
		if txt == "" {
			return
		}
		if w.replaying {
			data, w.err = sjson.Set(data, "choices.0.delta."+field, txt)
			if w.err != nil {
				return
			}
			data, _ = sjson.Delete(data, "choices.0.delta.role")
		}
		if !w.public.opaque {
			w.public.size += len(txt)
			if w.public.size > maxChatPrefix || len(w.public.deltas) >= 8192 {
				w.public.opaque = true
				w.public.deltas = nil
			} else {
				w.public.deltas = append(w.public.deltas, chatTextDelta{field: field, index: idx, text: strings.Clone(txt)})
			}
		}
	}
	w.public.started = true
	w.send("data: " + data + "\n\n")
}

func (w *chatAttemptWriter) hold(data string) {
	if len(data)+len(w.tail) > maxChatPrefix {
		if w.cursor < w.limit {
			w.err = errChatPrefix
			return
		}
		// Large or numerous usage-only extensions are already on the wire.
		// Keep the successful stream intact and refuse any later splice.
		w.public.opaque = true
		w.public.started = true
		w.send(string(w.tail))
		w.tail = nil
		w.send("data: " + data + "\n\n")
		return
	}
	w.tail = append(w.tail, "data: "...)
	w.tail = append(w.tail, data...)
	w.tail = append(w.tail, '\n', '\n')
}

func (w *chatAttemptWriter) opaqueFrame(data string) {
	if w.cursor < w.limit {
		w.err = errChatPrefix
		return
	}
	w.public.opaque = true
	w.public.started = true
	w.send("data: " + data + "\n\n")
}

func (w *chatAttemptWriter) finish(c *gin.Context, info *relaycommon.RelayInfo, observed *dto.Usage, apiErr *types.NewAPIError) *types.NewAPIError {
	if w == nil {
		return apiErr
	}
	c.Writer = w.ResponseWriter
	clientGone := c.Request.Context().Err() != nil || (info.StreamStatus != nil && (info.StreamStatus.EndReason == relaycommon.StreamEndReasonClientGone || info.StreamStatus.EndReason == relaycommon.StreamEndReasonPingFail))
	if apiErr == nil && !clientGone && w.writeErr == nil && w.err == nil {
		switch {
		case info.StreamStatus != nil && info.StreamStatus.ResponseFailed():
			w.err = errors.New("upstream chat stream failed")
		case !w.done || len(w.line) > 0 || w.cursor < w.limit:
			w.err = errors.New("upstream chat stream ended before the public response was completed")
		default:
			w.send(string(w.tail))
			w.send("data: [DONE]\n\n")
			if w.writeErr == nil {
				var target http.ResponseWriter = w.ResponseWriter
				if unwrapper, ok := target.(interface{ Unwrap() http.ResponseWriter }); ok {
					target = unwrapper.Unwrap()
				}
				w.writeErr = http.NewResponseController(target).Flush()
			}
			if w.writeErr == nil {
				return nil
			}
		}
	}
	if w.err != nil {
		apiErr = types.WithOpenAIError(types.OpenAIError{Code: "server_error", Type: "server_error", Message: w.err.Error()}, http.StatusBadGateway)
	}
	if w.writeErr != nil || clientGone {
		apiErr = types.WithOpenAIError(types.OpenAIError{Code: "server_error", Type: "server_error", Message: "downstream connection closed"}, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}
	if apiErr != nil {
		if c.GetBool("chat_retry_unsafe") || w.writeErr != nil || clientGone {
			types.ErrOptionWithSkipRetry()(apiErr)
		}
		if info.StreamStatus != nil {
			info.StreamStatus.MarkFailed(string(apiErr.GetErrorCode()), apiErr.ToOpenAIError().Type, apiErr.StatusCode)
		}
		toolCalls := 0
		if info.ResponsesUsageInfo != nil {
			for name, tool := range info.BuiltInTools {
				if tool == nil {
					continue
				}
				previous := 0
				if w.originalTools != nil && w.originalTools.BuiltInTools[name] != nil {
					previous = w.originalTools.BuiltInTools[name].CallCount
				}
				toolCalls += max(0, tool.CallCount-previous)
			}
		}
		if types.IsSkipRetryError(apiErr) {
			if observed != nil && (observed.TotalTokens > 0 || observed.PromptTokens > 0 || observed.CompletionTokens > 0 || toolCalls > 0) {
				settleChatUsage(c, info, observed)
			}
		} else {
			common.SetContextKey(c, constant.ContextKeyResponsesUpstreamRetryable, true)
			if observed != nil || toolCalls > 0 {
				in, out := 0, 0
				if observed != nil {
					in, out = observed.PromptTokens, observed.CompletionTokens
				}
				logger.LogInfo(c, fmt.Sprintf("discarded Chat attempt: channel=%d input_tokens=%d output_tokens=%d observed_tool_calls=%d", info.ChannelId, in, out, toolCalls))
			}
			info.ResponsesUsageInfo = w.originalTools
			info.BillingImageCount = w.imageCount
			info.PriceData.ReplaceOtherRatios(w.ratios)
			info.ThinkingContentInfo = w.thinking
		}
		return apiErr
	}
	return nil
}
