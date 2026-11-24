package openai

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func OaiChatToResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	var chatResp dto.OpenAITextResponse
	if err := common.Unmarshal(body, &chatResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := chatResp.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	info.ObserveResponseModel(chatResp.Model)
	if responseID := helper.GetResponseID(c); responseID != "" {
		chatResp.Id = responseID
	}
	convertResult, err := service.ConvertResponse(c, info, types.RelayFormatOpenAIResponses, &chatResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	responsesResp, ok := convertResult.Value.(*dto.OpenAIResponsesResponse)
	if !ok {
		return nil, types.NewOpenAIError(fmt.Errorf("expected OpenAI responses response, got %T", convertResult.Value), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	usage := convertResult.Usage
	if usage == nil || usage.TotalTokens == 0 {
		text := service.ExtractOutputTextFromResponses(responsesResp)
		usage = service.ResponseText2Usage(c, text, info.UpstreamModelName, info.GetEstimatePromptTokens())
		responsesResp.Usage = relayconvert.UsageFromChatUsage(usage)
	}

	responseBody, err := common.Marshal(responsesResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

func OaiChatToResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, service.NormalizeResponsesRetryError(types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusBadGateway))
	}
	defer service.CloseResponseBodyGracefully(resp)
	state, err := relayconvert.NewResponseStreamState(types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses, relayconvert.ResponseStreamOptions{
		ID: helper.GetResponseID(c), Model: info.UpstreamModelName, EmitSequenceNumber: true,
	})
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusBadGateway)
	}
	var streamErr *types.NewAPIError
	var finished bool
	sendEvent := func(event relayconvert.ChatToResponsesStreamEvent) bool {
		data, err := common.Marshal(event.Payload)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusBadGateway)
			return false
		}
		if event.Type == "response.failed" || event.Type == "error" {
			if upstreamID := c.GetString(common.UpstreamRequestIdKey); upstreamID != "" {
				data, err = sjson.SetBytes(data, "upstream_request_id", upstreamID)
				if err != nil {
					streamErr = types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusBadGateway)
					return false
				}
			}
		}
		// This conversion has no lifecycle buffering. Once an event is exposed,
		// another upstream attempt cannot safely reuse its public response identity.
		service.MarkResponsesStreamCommit(c, &event.Payload, service.ResponsesCommitChatConversion, 0, len(data))
		wireData, eventType := string(data), event.Type
		if info.EarlyResponses != nil {
			eventType, wireData, err = helper.RewriteEarlyResponsesEvent(c, info, event.Payload, wireData)
			if err != nil {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway)
				return false
			}
		}
		if event.Type == "response.output_text.delta" && event.Payload.Delta != "" && info.FirstTextTime.IsZero() {
			info.FirstTextTime = time.Now()
		}
		if err := helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: eventType}, wireData); err != nil {
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, err)
			return false
		}
		if ack := info.EarlyResponses; ack != nil {
			switch event.Type {
			case "response.completed", "response.incomplete", "response.failed", "error":
				ack.Finished = true
			}
		}
		return true
	}
	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		var chunk dto.ChatCompletionsStreamResponse
		if err := common.UnmarshalJsonStr(data, &chunk); err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway)
			sr.Stop(streamErr)
			return
		}
		if chunk.Usage != nil {
			state.SetUsage(chunk.Usage)
		}
		if raw := gjson.Get(data, "error"); raw.Exists() && raw.Type != gjson.Null {
			wire := types.OpenAIError{}
			_ = common.UnmarshalJsonStr(raw.Raw, &wire)
			if wire.Code == nil && wire.Type == "" {
				wire.Code, wire.Type = "server_error", "server_error"
			}
			if wire.Message == "" {
				wire.Message = "upstream chat stream failed"
			}
			status := int(gjson.Get(data, "status_code").Int())
			if status < 400 || status > 599 {
				status = http.StatusBadRequest
			}
			streamErr = service.NormalizeResponsesRetryError(types.WithOpenAIError(wire, status))
			sr.Stop(streamErr)
			return
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finished = true
			}
		}
		info.ObserveResponseModel(chunk.Model)
		results, err := service.ConvertStreamResponseChunk(c, info, state, &chunk)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusBadGateway)
			sr.Stop(streamErr)
			return
		}
		for _, result := range results {
			event, ok := result.Value.(relayconvert.ChatToResponsesStreamEvent)
			if !ok {
				streamErr = types.NewOpenAIError(fmt.Errorf("expected OAI responses stream event, got %T", result.Value), types.ErrorCodeBadResponse, http.StatusBadGateway)
				sr.Stop(streamErr)
				return
			}
			if !sendEvent(event) {
				stopStream(sr, streamErr)
				return
			}
		}
	})
	info.StreamStatus.RequireTerminal()
	usage := state.Usage()
	if usage == nil || usage.TotalTokens == 0 {
		usage = &dto.Usage{}
		if state.UsageText() != "" || (finished && streamErr == nil) {
			usage = service.ResponseText2Usage(c, state.UsageText(), info.UpstreamModelName, info.GetEstimatePromptTokens())
		}
		state.SetUsage(usage)
	}
	if c.Request.Context().Err() != nil || info.StreamStatus.EndReason == relaycommon.StreamEndReasonClientGone || info.StreamStatus.EndReason == relaycommon.StreamEndReasonPingFail {
		return usage, nil
	}
	if streamErr == nil && (!finished || !info.StreamStatus.IsNormalEnd() || info.StreamStatus.HasErrors()) {
		streamErr = types.WithOpenAIError(types.OpenAIError{Code: "server_error", Type: "server_error", Message: "upstream chat stream failed or ended before a finish reason"}, http.StatusBadGateway)
	}
	var finalResults []relayconvert.ResponseResult
	if streamErr == nil {
		finalResults, err = service.FinalizeStreamResponse(c, info, state)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusBadGateway)
		}
	}
	if streamErr != nil {
		streamErr = service.NormalizeResponsesRetryError(streamErr)
		wire := streamErr.ToOpenAIError()
		info.StreamStatus.MarkFailed(string(streamErr.GetErrorCode()), wire.Type, streamErr.StatusCode)
		if !c.Writer.Written() {
			if usage.TotalTokens == 0 {
				return nil, streamErr
			}
			c.Header("Content-Type", "application/json")
			c.JSON(streamErr.StatusCode, gin.H{"error": wire})
			return usage, nil
		}
		c.Set("responses_stream_error_sent", true)
		failureResults, _ := state.FailResponsesStream(string(streamErr.GetErrorCode()), wire.Message, "")
		for _, result := range failureResults {
			event, ok := result.Value.(relayconvert.ChatToResponsesStreamEvent)
			if !ok {
				break
			}
			if event.Type == "response.failed" && event.Payload.Response != nil {
				event.Payload.Response.Error = wire
			}
			if !sendEvent(event) {
				break
			}
		}
		if usage.TotalTokens == 0 && state.UsageText() == "" {
			types.ErrOptionWithSkipRetry()(streamErr)
			return nil, streamErr
		}
		return usage, nil
	}
	for _, result := range finalResults {
		event, ok := result.Value.(relayconvert.ChatToResponsesStreamEvent)
		if !ok {
			return usage, types.NewOpenAIError(fmt.Errorf("expected OAI responses stream event, got %T", result.Value), types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		}
		if !sendEvent(event) {
			return usage, nil
		}
		service.ObserveResponsesOutcome(info, &event.Payload)
	}
	return usage, nil
}

// stopStream ends a converted stream from the scanner callback. A nil
// streamErr (the stream stops because the client went away) must reach
// StreamResult.Stop as an untyped nil; a typed nil *NewAPIError is a non-nil
// error that StreamResult would record as a failure and dereference.
func stopStream(sr *helper.StreamResult, streamErr *types.NewAPIError) {
	if streamErr == nil {
		sr.Stop(nil)
		return
	}
	sr.Stop(streamErr)
}
