package openai

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func OaiResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	channelFailover := common.GetContextKeyBool(c, constant.ContextKeyResponsesChannelFailover)

	// read response body
	var responsesResponse dto.OpenAIResponsesResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		helper.MarkResponsesUpstreamFailure(c)
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	err = common.Unmarshal(responseBody, &responsesResponse)
	if err != nil {
		helper.MarkResponsesUpstreamFailure(c)
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	var failure *types.NewAPIError
	oaiError := responsesResponse.GetOpenAIError()
	status := gjson.GetBytes(responseBody, "status").String()
	if channelFailover && (oaiError != nil || status == "failed" || status == "cancelled" || status == "canceled") {
		if oaiError == nil {
			oaiError = &types.OpenAIError{Code: "upstream_response_" + status, Type: "server_error", Message: "upstream response " + status}
		}
		failure = service.NormalizeResponsesRetryError(types.WithOpenAIError(*oaiError, http.StatusBadGateway))
	} else if oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	info.ObserveResponseModel(responsesResponse.Model)
	if failure == nil {
		responseBody = rewriteSGLangResponsesCreatedAt(info, responseBody, "created_at", responsesResponse.CreatedAt)
		if info.ResponsePreviousAlias != "" {
			patched, patchErr := sjson.SetBytes(responseBody, "previous_response_id", info.ResponsePreviousAlias)
			if patchErr != nil {
				return nil, types.NewOpenAIError(patchErr, types.ErrorCodeBadResponseBody, http.StatusBadGateway)
			}
			responseBody = patched
		}
		if status == "completed" && info.ChannelMeta != nil {
			service.RecordFallbackResponseOwner(c, info.ChannelId, responsesResponse.ID)
		}
		service.IOCopyBytesGracefully(c, resp, responseBody)
	}

	usage := &dto.Usage{}
	service.ApplyResponsesUsage(usage, responsesResponse.Usage)
	observeNativeResponsesTools(info, responsesResponse.Output, responseBody)
	if failure != nil {
		if responsesFailureHasUsage(info, usage) {
			types.ErrOptionWithSkipRetry()(failure)
			return usage, failure
		}
		if len(responsesResponse.Output) > 0 {
			// An upstream tool can already have executed even without itemized usage.
			types.ErrOptionWithSkipRetry()(failure)
			return nil, failure
		}
		helper.MarkResponsesUpstreamFailure(c)
		return nil, failure
	}
	return usage, nil
}

// ResponsesHTTPFailureUsage checks whether an HTTP error envelope already
// reports usage or output from a tool. Gate retry before exposing the error.
func ResponsesHTTPFailureUsage(info *relaycommon.RelayInfo, body []byte) (*dto.Usage, bool) {
	root := gjson.ParseBytes(body)
	roots := []gjson.Result{root}
	if nested := root.Get("response"); nested.IsObject() {
		roots = append(roots, nested)
	}
	usage := &dto.Usage{}
	var output []dto.ResponsesOutput
	observedWork := false
	// A gateway may repeat a terminal's output at both envelope levels.
	// Keep separate items in one list, but count duplicated observations once.
	outerItems := make(map[string]int)
	for level, payload := range roots {
		switch payload.Get("status").String() {
		case "completed", "queued", "in_progress":
			observedWork = true
		}
		if raw := payload.Get("usage"); raw.IsObject() {
			var source dto.Usage
			if err := common.UnmarshalJsonStr(raw.Raw, &source); err != nil {
				observedWork = true
			} else {
				service.ApplyResponsesUsage(usage, &source)
			}
		}
		for _, item := range payload.Get("output").Array() {
			observedWork = true
			var entry dto.ResponsesOutput
			if common.UnmarshalJsonStr(item.Raw, &entry) != nil {
				continue
			}
			key := entry.ID
			if key == "" {
				key = entry.CallId
			}
			if key == "" {
				key = item.Raw
			}
			if level == 1 && outerItems[key] > 0 {
				outerItems[key]--
				continue
			}
			if level == 0 && len(roots) > 1 {
				outerItems[key]++
			}
			output = append(output, entry)
		}
	}
	observeNativeResponsesTools(info, output, body)
	if len(roots) == 2 {
		info.ApplyVendorToolUsage(common.StringToByteSlice(roots[1].Raw))
	}
	if responsesFailureHasUsage(info, usage) {
		return usage, true
	}
	return nil, observedWork
}

func observeNativeResponsesTools(info *relaycommon.RelayInfo, items []dto.ResponsesOutput, responseBody []byte) {
	// Count actual tool invocations from Output (not tool declarations).
	for _, output := range items {
		switch output.Type {
		case dto.BuildInCallWebSearchCall:
			info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
		case dto.BuildInCallFileSearchCall:
			info.CountBillableToolCall(dto.BuildInCallFileSearchCall, "")
		case dto.BuildInCallFunctionCall:
			info.CountBillableToolCall(dto.BuildInCallFunctionCall, output.Name)
		}
	}
	info.ApplyVendorToolUsage(responseBody)

	imageCounter := &relaycommon.ImageGenerationCallCounter{}
	for i := range items {
		imageCounter.Observe(&items[i], &i)
	}
	imageCounter.Commit(info)
}

func responsesFailureHasUsage(info *relaycommon.RelayInfo, usage *dto.Usage) bool {
	if usage.TotalTokens > 0 || usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.BillingUsage != nil || info.BillingImageCount != nil {
		return true
	}
	if info.ResponsesUsageInfo != nil {
		for _, tool := range info.ResponsesUsageInfo.BuiltInTools {
			if tool != nil && tool.CallCount > 0 {
				return true
			}
		}
	}
	return false
}

func OaiResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, "invalid response or response body")
		return nil, types.NewError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse)
	}

	defer service.CloseResponseBodyGracefully(resp)

	accumulator := service.NewResponsesUsageAccumulator(info)
	managed := info.ResponsesRetryManaged
	channelFailover := common.GetContextKeyBool(c, constant.ContextKeyResponsesChannelFailover)
	upstreamFailure := false
	upstreamOutput := false
	var attemptError *types.NewAPIError
	var pending []string
	pendingBytes := 0
	forwarding := false
	delivered := false
	terminalDelivered := false
	if managed && info.EarlyResponses == nil {
		info.EarlyResponses = &relaycommon.EarlyResponsesState{Response: map[string]any{
			"id": "resp_" + uuid.NewString(), "object": "response", "created_at": time.Now().Unix(),
			"model": info.OriginModelName, "output": []any{}, "usage": nil,
		}}
	}

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {

		// 检查当前数据是否包含 completed 状态和 usage 信息
		var streamResponse dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
			logger.LogError(c, "failed to unmarshal stream response: "+err.Error())
			if managed {
				upstreamFailure = true
				attemptError = types.WithOpenAIError(types.OpenAIError{Code: "server_error", Type: "server_error", Message: "invalid upstream Responses event"}, http.StatusBadGateway)
				sr.Stop(attemptError)
				return
			}
			sr.Error(err)
			return
		}
		if streamResponse.Response != nil {
			data = string(rewriteSGLangResponsesCreatedAt(info, []byte(data), "response.created_at", streamResponse.Response.CreatedAt))
		}
		if managed {
			if streamResponse.Type == "" {
				upstreamFailure = true
				attemptError = types.WithOpenAIError(types.OpenAIError{Code: "server_error", Type: "server_error", Message: "upstream Responses event has no type"}, http.StatusBadGateway)
				sr.Stop(attemptError)
				return
			}
			status := gjson.Get(data, "response.status").String()
			failed := streamResponse.Type == "error" || streamResponse.Type == "response.error" || streamResponse.Type == "response.failed" || status == "failed"
			cancelled := streamResponse.Type == "response.cancelled" || streamResponse.Type == "response.canceled" || status == "cancelled" || status == "canceled"
			if failed || (channelFailover && cancelled) {
				upstreamFailure = true
				info.EarlyResponses.FailureEvent = data
				rawError := gjson.Get(data, "response.error")
				if !rawError.IsObject() {
					rawError = gjson.Get(data, "error")
				}
				flatError := !rawError.IsObject()
				if flatError {
					rawError = gjson.Parse(data)
				}
				wire := types.OpenAIError{}
				_ = common.UnmarshalJsonStr(rawError.Raw, &wire)
				if flatError {
					// The envelope type names an event, not the error category.
					wire.Type = ""
				}
				if wire.Code == nil && wire.Type == "" {
					wire.Code, wire.Type = "server_error", "server_error"
				}
				if wire.Message == "" {
					wire.Message = "upstream response failed"
				}
				status := int(gjson.Get(data, "status_code").Int())
				if status < 400 || status > 599 {
					// Unknown explicit upstream codes are not assumed transient.
					status = http.StatusBadRequest
				}
				attemptError = service.NormalizeResponsesRetryError(types.WithOpenAIError(wire, status))
				// Observe authoritative usage even on a failed terminal. Explicit
				// usage prevents replay and is settled by ResponsesHelper once.
				if channelFailover {
					// Account for failures even when reported in a generic error
					// frame that carries a terminal response.
					streamResponse.Type = "response.failed"
					upstreamOutput = streamResponse.Response != nil && len(streamResponse.Response.Output) > 0
				}
				accumulator.Observe(&streamResponse, common.StringToByteSlice(data))
				sr.Stop(attemptError)
				return
			}
			lifecycleOnly := (streamResponse.Type == "response.created" || streamResponse.Type == "response.in_progress") && len(gjson.Get(data, "response.output").Array()) == 0
			lifecycleOnly = lifecycleOnly || (streamResponse.Type == "response.output_item.added" && gjson.Get(data, "item.type").String() == "message" && len(gjson.Get(data, "item.content").Array()) == 0)
			lifecycleOnly = lifecycleOnly || (streamResponse.Type == "response.content_part.added" && gjson.Get(data, "part.type").String() == "output_text" && gjson.Get(data, "part.text").String() == "")
			lifecycleOnly = lifecycleOnly || (streamResponse.Type == "response.output_text.delta" && streamResponse.Delta == "")
			switch streamResponse.Type {
			case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
				"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
				"response.reasoning_text.delta", "response.reasoning_text.done":
				lifecycleOnly = emptyReasoningPrelude(data, streamResponse.Type)
			case dto.ResponsesOutputTypeItemAdded, dto.ResponsesOutputTypeItemDone:
				if streamResponse.Item != nil && streamResponse.Item.Type == "reasoning" {
					lifecycleOnly = emptyReasoningPrelude(data, streamResponse.Type)
				}
			}
			if !forwarding && lifecycleOnly {
				// A repeated numbered progress event with the exact same payload
				// is a duplicate. Keep one copy and still refresh the public heartbeat.
				fits := pendingBytes+len(data) <= 64<<10
				duplicate := !fits && streamResponse.Type == "response.in_progress" && streamResponse.SequenceNumber != nil &&
					streamResponse.Response != nil && streamResponse.Response.ID != "" && len(pending) > 0 && pending[len(pending)-1] == data
				if fits || duplicate {
					if streamResponse.Type == "response.in_progress" {
						if err := helper.SendBufferedResponsesProgress(c, info); err != nil {
							info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, err)
							attemptError = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
							sr.Stop(err)
							return
						}
					}
					if !duplicate {
						pending = append(pending, data)
						pendingBytes += len(data)
					}
					return
				}
			}
			// Bound memory without rejecting valid large metadata. Once any
			// attempt-specific frame is exposed, conservatively forbid replay.
			if !forwarding {
				reason := service.ResponsesCommitEvent
				if lifecycleOnly {
					reason = service.ResponsesCommitBufferLimit
				}
				service.MarkResponsesStreamCommit(c, &streamResponse, reason, pendingBytes, len(data))
				forwarding = true
			}
			delivered = delivered || !lifecycleOnly
			for _, buffered := range pending {
				var event dto.ResponsesStreamResponse
				_ = common.UnmarshalJsonStr(buffered, &event)
				typ, rewritten, err := helper.RewriteEarlyResponsesEvent(c, info, event, buffered)
				if err != nil {
					attemptError = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusBadGateway)
					sr.Stop(err)
					return
				}
				event.Type = typ
				if err := helper.ResponseChunkData(c, event, rewritten); err != nil {
					info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, err)
					attemptError = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
					sr.Stop(err)
					return
				}
				if info.EarlyResponses.SentAt.IsZero() && event.Response != nil {
					_ = common.UnmarshalJsonStr(gjson.Get(buffered, "response").Raw, &info.EarlyResponses.Response)
				}
			}
			pending = nil
		}
		accumulator.Observe(&streamResponse, common.StringToByteSlice(data))
		if info.FirstTextTime.IsZero() {
			hasText := streamResponse.Type == "response.output_text.delta" && streamResponse.Delta != ""
			hasText = hasText || (streamResponse.Type == "response.output_text.done" && streamResponse.Text != nil && *streamResponse.Text != "")
			if streamResponse.Response != nil {
				for _, output := range streamResponse.Response.Output {
					for _, part := range output.Content {
						hasText = hasText || (part.Type == "output_text" && part.Text != "")
					}
				}
			}
			if hasText {
				info.FirstTextTime = time.Now()
			}
		}
		eventType, rewritten, rewriteErr := helper.RewriteEarlyResponsesEvent(c, info, streamResponse, data)
		if rewriteErr != nil {
			info.StreamStatus.MarkFailed("early_ack_rewrite_failed", "server_error", http.StatusBadGateway)
			if managed {
				attemptError = types.NewOpenAIError(rewriteErr, types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
			}
			sr.Stop(rewriteErr)
			return
		}
		streamResponse.Type, data = eventType, rewritten
		if managed && info.EarlyResponses.SentAt.IsZero() {
			info.EarlyResponses.Sequence = int(gjson.Get(data, "sequence_number").Int()) + 1
			if streamResponse.Response != nil {
				_ = common.UnmarshalJsonStr(gjson.Get(data, "response").Raw, &info.EarlyResponses.Response)
			}
		}
		if (eventType == "response.completed" || eventType == "response.done") && info.ChannelMeta != nil && streamResponse.Response != nil && (info.EarlyResponses == nil || info.EarlyResponses.SentAt.IsZero()) {
			service.RecordFallbackResponseOwner(c, info.ChannelId, streamResponse.Response.ID)
		}
		if err := helper.ResponseChunkData(c, streamResponse, data); err != nil {
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, err)
			if managed {
				attemptError = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
			}
			sr.Stop(err)
			return
		}
		// Responses terminal events carry the final usage. Do not wait for an
		// optional [DONE] or EOF after forwarding one to the client.
		if info.StreamStatus.ResponseOutcome() != "" {
			terminalDelivered = true
			if info.EarlyResponses != nil {
				info.EarlyResponses.Finished = true
			}
			sr.Done()
		}
	})

	common.SetContextKey(c, constant.ContextKeyResponseStreamStatus, info.StreamStatus)
	info.StreamStatus.RequireTerminal()
	if managed {
		outcome := info.StreamStatus.OutcomeSnapshot()
		// A client may close immediately after receiving a terminal frame.
		// Once its write and flush succeeded, cancellation cannot undo delivery.
		clientGone := !terminalDelivered && (c.Request.Context().Err() != nil || outcome.EndReason == relaycommon.StreamEndReasonClientGone || outcome.EndReason == relaycommon.StreamEndReasonPingFail)
		if attemptError == nil && (clientGone || outcome.Response == relaycommon.ResponseOutcomeUnknown) {
			switch outcome.EndReason {
			case relaycommon.StreamEndReasonDone, relaycommon.StreamEndReasonEOF, relaycommon.StreamEndReasonTimeout, relaycommon.StreamEndReasonScannerErr:
				upstreamFailure = true
			}
			message := "upstream stream ended before a terminal event"
			if clientGone {
				message = "downstream connection closed"
			}
			attemptError = types.WithOpenAIError(types.OpenAIError{Code: "server_error", Type: "server_error", Message: message}, http.StatusBadGateway)
		}
		if attemptError != nil {
			attemptError = service.NormalizeResponsesRetryError(attemptError)
			info.StreamStatus.MarkFailed(string(attemptError.GetErrorCode()), attemptError.ToOpenAIError().Type, attemptError.StatusCode)
			usage := accumulator.Finish()
			if clientGone || forwarding {
				types.ErrOptionWithSkipRetry()(attemptError)
			}
			if delivered || responsesFailureHasUsage(info, usage) {
				types.ErrOptionWithSkipRetry()(attemptError)
				return usage, attemptError
			}
			if upstreamOutput {
				types.ErrOptionWithSkipRetry()(attemptError)
			} else if upstreamFailure && !clientGone && !forwarding {
				helper.MarkResponsesUpstreamFailure(c)
			}
			return nil, attemptError
		}
	}
	// Settle only observed upstream facts before adding any gateway terminal event.
	usage := accumulator.Finish()
	if state := info.EarlyResponses; state != nil && !state.SentAt.IsZero() && !state.Finished {
		_ = helper.FailEarlyResponses(c, info, types.NewOpenAIError(fmt.Errorf("upstream stream ended before a terminal event"), types.ErrorCodeBadResponse, http.StatusBadGateway))
	}
	return usage, nil
}

// Keep only recognized, content-free reasoning frames private until an attempt
// produces output. An unknown extension could describe usage or tool work.
func emptyReasoningPrelude(data, kind string) bool {
	event := gjson.Parse(data)
	var field string
	switch kind {
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		field = "part"
		if !emptyReasoningPart(event.Get(field), "summary_text") {
			return false
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		field = "delta"
	case "response.reasoning_summary_text.done", "response.reasoning_text.done":
		field = "text"
	case dto.ResponsesOutputTypeItemAdded, dto.ResponsesOutputTypeItemDone:
		field = "item"
		item := event.Get(field)
		if !hasOnlyResponseFields(item, "type", "id", "status", "summary", "content", "encrypted_content") ||
			!emptyReasoningParts(item.Get("summary"), "summary_text") || !emptyReasoningParts(item.Get("content"), "reasoning_text") {
			return false
		}
	default:
		return false
	}
	if field == "delta" || field == "text" {
		value := event.Get(field)
		if value.Type != gjson.String || value.Str != "" {
			return false
		}
	}
	if padding := event.Get("obfuscation"); padding.Exists() && padding.Type != gjson.String {
		return false
	}
	return hasOnlyResponseFields(event, "type", "sequence_number", "response_id", "item_id", "output_index", "summary_index", "content_index", "obfuscation", field)
}

func emptyReasoningParts(parts gjson.Result, partType string) bool {
	if parts.Type == gjson.Null {
		return true
	}
	if !parts.IsArray() {
		return false
	}
	empty := true
	parts.ForEach(func(_, part gjson.Result) bool {
		empty = emptyReasoningPart(part, partType)
		return empty
	})
	return empty
}

func emptyReasoningPart(part gjson.Result, partType string) bool {
	value := part.Get("text")
	return part.Get("type").String() == partType && value.Type == gjson.String &&
		value.Str == "" && hasOnlyResponseFields(part, "type", "text")
}

func hasOnlyResponseFields(object gjson.Result, allowed ...string) bool {
	if !object.IsObject() {
		return false
	}
	known := true
	object.ForEach(func(key, _ gjson.Result) bool {
		known = slices.Contains(allowed, key.Str)
		return known
	})
	return known
}

func rewriteSGLangResponsesCreatedAt(info *relaycommon.RelayInfo, payload []byte, path string, createdAt dto.IntValue) []byte {
	if info.GetChannelType() != constant.ChannelTypeSGLang {
		return payload
	}
	if !gjson.GetBytes(payload, path).Exists() {
		return payload
	}
	patched, err := sjson.SetBytes(payload, path, int(createdAt))
	if err != nil {
		return payload
	}
	return patched
}
