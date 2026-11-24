package helper

import (
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
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
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// MarkResponsesUpstreamFailure allows another native channel before delivery
// or while a managed Responses handler can verify the published prefix.
func MarkResponsesUpstreamFailure(c *gin.Context) {
	prefixRecovery := c.GetBool("responses_retry_managed") && common.GetContextKeyBool(c, constant.ContextKeyResponsesPrefixRecovery)
	if !common.GetContextKeyBool(c, constant.ContextKeyResponsesChannelFailover) || c.Request.Context().Err() != nil || (c.GetBool("responses_content_delivered") && !prefixRecovery) {
		return
	}
	if c.Writer.Written() && !prefixRecovery && !(c.GetBool("responses_retry_managed") && c.GetBool("responses_early_ack_committed")) {
		return
	}
	common.SetContextKey(c, constant.ContextKeyResponsesUpstreamRetryable, true)
}

// PrepareEarlyResponses prepares a content-free acknowledgment in the client's
// protocol. Tools, structured output and history are forwarded unchanged.
func PrepareEarlyResponses(info *relaycommon.RelayInfo, body common.ReplayableBody) {
	info.EarlyResponses.Stop()
	if info.ResponsesRetryManaged && info.EarlyResponses != nil && (info.EarlyResponses.FirstEvent || !info.EarlyResponses.SentAt.IsZero()) {
		// Keep an upstream-first response public across attempts too. Only a
		// previously acknowledged lifecycle may produce synthetic progress.
		info.EarlyResponses.FirstEvent = info.EarlyResponses.SentAt.IsZero()
		info.EarlyResponses.UpstreamID = ""
		info.EarlyResponses.FailureEvent = ""
		return
	}
	info.EarlyResponses = nil
	info.EarlyAckReason = "disabled"
	settings := operation_setting.GetGeneralSetting()
	if !settings.ResponsesEarlyAckEnabled {
		return
	}
	info.EarlyAckReason = "nonstream"
	if !info.IsStream {
		return
	}
	chat := info.RelayFormat == types.RelayFormatOpenAI && (info.RelayMode == relayconstant.RelayModeChatCompletions || info.RelayMode == relayconstant.RelayModeResponses)
	info.EarlyAckReason = "unsupported_protocol"
	if !chat && (info.RelayMode != relayconstant.RelayModeResponses || info.RelayFormat != types.RelayFormatOpenAIResponses) {
		return
	}
	info.EarlyAckReason = "unsupported_channel"
	if info.ChannelType != constant.ChannelTypeOpenAI && info.ChannelType != constant.ChannelTypeAdvancedCustom {
		return
	}
	// Advanced Custom can also target Claude or Gemini handlers, which do not
	// participate in the OpenAI early-ack identity and terminal lifecycle.
	info.EarlyAckReason = "unsupported_upstream_protocol"
	upstreamFormat := info.GetFinalRequestRelayFormat()
	if upstreamFormat != types.RelayFormatOpenAI && upstreamFormat != types.RelayFormatOpenAIResponses {
		return
	}
	info.EarlyAckReason = "body_too_large"
	if body.Size() > 8<<20 {
		return
	}
	info.EarlyAckReason = "body_unreadable"
	reader, err := body.NewReader()
	if err != nil {
		return
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, (8<<20)+1))
	if err != nil || len(raw) > 8<<20 || !gjson.ValidBytes(raw) {
		return
	}
	req := gjson.ParseBytes(raw)
	info.EarlyAckReason = "upstream_nonstream"
	if !req.Get("stream").Bool() {
		return
	}
	info.EarlyAckReason = "eligible"
	if info.EarlyAckDeadline.IsZero() {
		delay := settings.ResponsesEarlyAckDelayMS
		if delay <= 0 {
			delay = operation_setting.DefaultResponsesEarlyAckDelayMS
		}
		delay = min(delay, 1500)
		if upper := min(settings.ResponsesEarlyAckMaxDelayMS, 1500); upper > delay {
			delay += rand.IntN(upper - delay + 1)
		}
		info.EarlyAckDeadline = info.StartTime.Add(time.Duration(delay) * time.Millisecond)
	}
	remaining := time.Until(info.EarlyAckDeadline)
	info.EarlyResponses = &relaycommon.EarlyResponsesState{
		Timer: time.NewTimer(max(0, remaining)), ID: service.EarlyResponseIDPrefix + strings.ReplaceAll(uuid.NewString(), "-", ""),
		CreatedAt: time.Now().Unix(), Chat: chat,
	}
	if chat {
		info.EarlyResponses.ID = "chatcmpl-newapi-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	response := map[string]any{
		"id": info.EarlyResponses.ID, "object": "response", "created_at": info.EarlyResponses.CreatedAt,
		"status": "in_progress", "error": nil, "incomplete_details": nil, "output": []any{}, "usage": nil,
		"model": info.OriginModelName, "tools": []any{}, "tool_choice": "auto", "parallel_tool_calls": true,
		"store": true, "temperature": 1, "top_p": 1, "truncation": "disabled", "metadata": map[string]any{},
		"text":         map[string]any{"format": map[string]any{"type": "text"}},
		"instructions": nil, "max_output_tokens": nil, "previous_response_id": nil, "reasoning": nil, "user": nil,
	}
	for _, key := range []string{"tools", "conversation", "prompt", "background", "service_tier", "instructions", "max_output_tokens", "previous_response_id", "reasoning", "store", "temperature", "top_p", "text", "tool_choice", "parallel_tool_calls", "truncation", "metadata", "user"} {
		if value := req.Get(key); value.Exists() {
			response[key] = value.Value()
		}
	}
	if info.ResponsePreviousAlias != "" {
		response["previous_response_id"] = info.ResponsePreviousAlias
	}
	info.EarlyResponses.Response = response
}

func SendEarlyResponsesAck(c *gin.Context, info *relaycommon.RelayInfo) error {
	state := info.EarlyResponses
	if state == nil || state.Finished || state.FirstEvent || !state.SentAt.IsZero() {
		return nil
	}
	state.Stop()
	SetEventStreamHeaders(c)
	eventType := "response.created"
	payload := map[string]any{"type": eventType, "sequence_number": 0, "response": state.Response}
	if state.Chat {
		eventType = ""
		payload = map[string]any{"id": state.ID, "object": "chat.completion.chunk", "created": state.CreatedAt, "model": info.OriginModelName,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}}}
		c.Writer.Header().Set("X-Newapi-Early-Ack", "chat.completion.chunk")
	} else {
		c.Writer.Header().Set("X-Newapi-Early-Ack", eventType)
	}
	ExtendWriteDeadline(c)
	data, err := common.Marshal(payload)
	if err != nil {
		return err
	}
	// Even partial delivery commits HTTP status. Only the native Responses
	// gate may retry internally while no attempt content has been exposed.
	state.SentAt = time.Now()
	info.EarlyAckReason = "sent"
	state.Sequence = 1
	c.Set("responses_early_ack_committed", true)
	if err := ResponseChunkData(c, dto.ResponsesStreamResponse{Type: eventType}, string(data)); err != nil {
		return service.NewDownstreamDisconnectError()
	}
	return nil
}

// SendBufferedResponsesProgress exposes only the existing public lifecycle.
// Call it only on actual upstream progress while the attempt remains buffered.
// It does not bind the public response to an attempt or commit retryable content.
func SendBufferedResponsesProgress(c *gin.Context, info *relaycommon.RelayInfo) error {
	state := info.EarlyResponses
	if state == nil || state.Chat || state.Finished || state.FirstEvent || state.SentAt.IsZero() || c.GetBool("responses_content_delivered") {
		return nil
	}
	last := state.LastProgressAt
	if last.IsZero() {
		last = state.SentAt
	}
	now := time.Now()
	if now.Sub(last) < 15*time.Second {
		return nil
	}
	payload := map[string]any{
		"type": "response.in_progress", "sequence_number": state.Sequence,
		"response": map[string]any{
			"id": state.ID, "object": "response", "created_at": state.CreatedAt,
			"model": info.OriginModelName, "status": "in_progress", "output": []any{},
		},
	}
	raw, err := common.Marshal(payload)
	if err != nil {
		return err
	}
	if err := ResponseChunkData(c, dto.ResponsesStreamResponse{Type: "response.in_progress"}, string(raw)); err != nil {
		return err
	}
	state.Sequence++
	state.LastProgressAt = now
	return nil
}

// RewriteEarlyResponsesEvent preserves unknown fields and text/usage verbatim.
// Upstream created becomes in_progress, carrying the upstream metadata.
func RewriteEarlyResponsesEvent(c *gin.Context, info *relaycommon.RelayInfo, event dto.ResponsesStreamResponse, data string) (string, string, error) {
	state := info.EarlyResponses
	eventType := event.Type
	updates := map[string]any{}
	if info.ResponsePreviousAlias != "" && gjson.Get(data, "response.previous_response_id").Exists() {
		updates["response.previous_response_id"] = info.ResponsePreviousAlias
	}
	if state != nil {
		state.FirstEvent = true
		state.Stop()
		if state.SentAt.IsZero() {
			info.EarlyAckReason = "upstream_first"
		}
		if !state.SentAt.IsZero() {
			if event.Response != nil && event.Response.ID != "" {
				if state.UpstreamID == "" {
					// A progressive response can change channels after publication.
					// Its continuation records the winning owner at the terminal.
					if _, guarded := c.Get(string(constant.ContextKeyResponsesPublicPrefix)); !guarded {
						if err := service.SaveEarlyResponseAlias(info.UserId, info.ChannelId, state.ID, event.Response.ID); err != nil {
							return "", "", err
						}
						if err := service.SaveFallbackResponseOwner(c, info.ChannelId, state.ID, event.Response.ID); err != nil {
							return "", "", err
						}
					}
					state.UpstreamID = event.Response.ID
				} else if event.Response.ID != state.UpstreamID {
					return "", "", fmt.Errorf("upstream changed response identity")
				}
			}
			if event.Response != nil {
				updates["response.id"] = state.ID
				updates["response.created_at"] = state.CreatedAt
			}
			if gjson.Get(data, "response_id").Exists() {
				updates["response_id"] = state.ID
			}
			if eventType == "response.created" {
				eventType = "response.in_progress"
				updates["type"] = eventType
			}
			updates["sequence_number"] = state.Sequence
			state.Sequence++
		}
	}
	for path, value := range updates {
		var err error
		data, err = sjson.Set(data, path, value)
		if err != nil {
			return "", "", err
		}
	}
	return eventType, data, nil
}

// RewriteEarlyChatChunk keeps the chat identity consistent without changing
// text, tool arguments, finish reasons or usage. Called only on outbound chunks.
func RewriteEarlyChatChunk(info *relaycommon.RelayInfo, data string) (string, error) {
	state := info.EarlyResponses
	if state == nil || !state.Chat {
		return data, nil
	}
	state.FirstEvent = true
	state.Stop()
	if state.SentAt.IsZero() {
		info.EarlyAckReason = "upstream_first"
		return data, nil
	}
	if !gjson.Valid(data) {
		return "", fmt.Errorf("invalid chat stream JSON")
	}
	if gjson.Get(data, "error").Exists() {
		return data, nil
	}
	var err error
	data, err = sjson.Set(data, "id", state.ID)
	if err != nil {
		return "", err
	}
	return sjson.Set(data, "created", state.CreatedAt)
}

// FailEarlyResponses terminates the committed stream without inventing usage or
// success. The original error still drives normal refunds and error logging.
func FailEarlyResponses(c *gin.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError) error {
	state := info.EarlyResponses
	if state == nil || (state.SentAt.IsZero() && !(info.ResponsesRetryManaged && c.Writer.Written())) || state.Finished {
		return nil
	}
	state.Finished = true
	state.Stop()
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	info.StreamStatus.RequireTerminal()
	info.StreamStatus.MarkFailed(string(apiErr.GetErrorCode()), apiErr.ToOpenAIError().Type, apiErr.StatusCode)
	if state.Chat {
		data, err := common.Marshal(map[string]any{"error": apiErr.ToOpenAIError()})
		if err != nil {
			return err
		}
		ExtendWriteDeadline(c)
		err = ResponseChunkData(c, dto.ResponsesStreamResponse{}, string(data))
		if err != nil {
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, err)
		}
		return err
	}
	response := make(map[string]any, len(state.Response))
	for key, value := range state.Response {
		response[key] = value
	}
	envelope := map[string]any{}
	guardedPrefix := common.GetContextKeyBool(c, constant.ContextKeyResponsesPrefixRecovery) && c.GetBool("responses_content_delivered")
	if state.FailureEvent != "" && !guardedPrefix {
		_ = common.UnmarshalJsonStr(state.FailureEvent, &envelope)
		if upstream, ok := envelope["response"].(map[string]any); ok {
			for key, value := range upstream {
				response[key] = value
			}
		}
	}
	if !state.SentAt.IsZero() {
		response["id"], response["created_at"] = state.ID, state.CreatedAt
	}
	response["status"] = "failed"
	wire := apiErr.ToOpenAIError()
	errorFields, _ := response["error"].(map[string]any)
	if errorFields == nil {
		errorFields, _ = envelope["error"].(map[string]any)
	}
	if errorFields == nil {
		errorFields = map[string]any{}
	}
	// Retain the upstream failure details, including provider extensions.
	// Only fill absent fields for gateway-generated or incomplete errors.
	for key, fallback := range map[string]any{"code": wire.Code, "type": wire.Type, "message": wire.Message} {
		if value, exists := errorFields[key]; !exists || value == nil || value == "" {
			errorFields[key] = fallback
		}
	}
	if _, exists := errorFields["param"]; !exists && wire.Param != "" {
		errorFields["param"] = wire.Param
	}
	response["error"] = errorFields
	delete(envelope, "error")
	delete(envelope, "code")
	delete(envelope, "message")
	if upstreamID := c.GetString(common.UpstreamRequestIdKey); upstreamID != "" {
		envelope["upstream_request_id"] = upstreamID
	}
	envelope["type"], envelope["sequence_number"], envelope["response"] = "response.failed", state.Sequence, response
	data, err := common.Marshal(envelope)
	if err != nil {
		return err
	}
	ExtendWriteDeadline(c)
	err = ResponseChunkData(c, dto.ResponsesStreamResponse{Type: "response.failed"}, string(data))
	if err != nil {
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, err)
	}
	return err
}

// FailChatStream sends one OpenAI-compatible error without fabricating a finish
// reason or [DONE]. Callers retain observed usage for the existing settlement.
func FailChatStream(c *gin.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError) error {
	if apiErr == nil || c.GetBool("chat_stream_error_sent") {
		return nil
	}
	if c.Request.Context().Err() != nil || (info.StreamStatus != nil && (info.StreamStatus.EndReason == relaycommon.StreamEndReasonClientGone || info.StreamStatus.EndReason == relaycommon.StreamEndReasonPingFail)) {
		return nil
	}
	apiErr = service.NormalizeResponsesRetryError(apiErr)
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	info.StreamStatus.RequireTerminal()
	info.StreamStatus.MarkFailed(string(apiErr.GetErrorCode()), apiErr.ToOpenAIError().Type, apiErr.StatusCode)
	c.Set("chat_stream_error_sent", true)
	if state := info.EarlyResponses; state != nil && state.Chat {
		state.Finished = true
		state.Stop()
	}
	payload := map[string]any{"error": apiErr.ToOpenAIError()}
	if upstreamID := c.GetString(common.UpstreamRequestIdKey); upstreamID != "" {
		payload["upstream_request_id"] = upstreamID
	}
	if !c.Writer.Written() {
		c.Header("Content-Type", "application/json")
		c.JSON(apiErr.StatusCode, payload)
		return nil
	}
	raw, err := common.Marshal(payload)
	if err != nil {
		return err
	}
	ExtendWriteDeadline(c)
	if err := ResponseChunkData(c, dto.ResponsesStreamResponse{}, string(raw)); err != nil {
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, err)
		return err
	}
	return nil
}
