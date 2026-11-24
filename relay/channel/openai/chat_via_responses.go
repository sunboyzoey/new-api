package openai

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func OaiResponsesToChatHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)

	var responsesResp dto.OpenAIResponsesResponse
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	if err := common.Unmarshal(body, &responsesResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	if oaiError := responsesResp.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	info.ObserveResponseModel(responsesResp.Model)
	responseValue, usage, err := convertResponsesResponseForClient(c, info, &responsesResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	responseBody, err := common.Marshal(responseValue)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

func OaiResponsesToChatBufferedStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)

	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.RequireTerminal()
	accumulator := relayconvert.NewResponsesBufferedAccumulator()
	var finalResponse *dto.OpenAIResponsesResponse
	var streamErr *types.NewAPIError

	scanner := helper.NewStreamScanner(resp.Body)
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 6 || line[:5] != "data:" {
			continue
		}
		data := line[5:]
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			if data == "[DONE]" {
				break
			}
			continue
		}

		var streamResp dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResp); err != nil {
			logger.LogError(c, "failed to unmarshal buffered responses stream event: "+err.Error())
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			break
		}
		if streamResp.Response != nil {
			info.ObserveResponseModel(streamResp.Response.Model)
		}
		service.ObserveResponsesOutcome(info, &streamResp)
		accumulator.ProcessEvent(&streamResp)
		switch streamResp.Type {
		case "response.completed", "response.done", "response.incomplete":
			finalResponse = streamResp.Response
			if streamResp.Type == "response.incomplete" {
				if finalResponse == nil {
					finalResponse = &dto.OpenAIResponsesResponse{}
				}
				if len(finalResponse.Status) == 0 {
					finalResponse.Status = []byte(`"incomplete"`)
				}
			}
		case "response.failed", "response.error":
			if streamResp.Response != nil {
				if oaiErr := streamResp.Response.GetOpenAIError(); oaiErr != nil && oaiErr.Type != "" {
					streamErr = types.WithOpenAIError(*oaiErr, http.StatusInternalServerError)
					break
				}
			}
			streamErr = types.NewOpenAIError(fmt.Errorf("responses stream error: %s", streamResp.Type), types.ErrorCodeBadResponse, http.StatusInternalServerError)
		}
		if streamErr != nil || finalResponse != nil {
			break
		}
	}
	if streamErr != nil {
		return nil, streamErr
	}
	if err := scanner.Err(); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	if finalResponse == nil {
		finalResponse = &dto.OpenAIResponsesResponse{
			ID:        helper.GetResponseID(c),
			CreatedAt: dto.IntValue(time.Now().Unix()),
			Model:     info.UpstreamModelName,
			Status:    []byte(`"completed"`),
		}
	}
	accumulator.SupplementResponseOutput(finalResponse)

	responseValue, usage, err := convertResponsesResponseForClient(c, info, finalResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	responseBody, err := common.Marshal(responseValue)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

func convertResponsesResponseForClient(c *gin.Context, info *relaycommon.RelayInfo, response *dto.OpenAIResponsesResponse) (any, *dto.Usage, error) {
	if responseID := helper.GetResponseID(c); responseID != "" {
		response.ID = responseID
	}

	usage := relayconvert.UsageFromResponsesUsage(response.Usage)
	if usage == nil || usage.TotalTokens == 0 {
		text := service.ExtractOutputTextFromResponses(response)
		usage = service.ResponseText2Usage(c, text, info.UpstreamModelName, info.GetEstimatePromptTokens())
		response.Usage = relayconvert.UsageFromChatUsage(usage)
	}

	result, err := service.ConvertResponse(c, info, info.RelayFormat, response)
	if err != nil {
		return nil, nil, err
	}
	if result.Usage != nil && result.Usage.TotalTokens != 0 {
		usage = result.Usage
	}
	return result.Value, usage, nil
}

func OaiResponsesToChatStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)

	responseId := helper.GetResponseID(c)
	createAt := time.Now().Unix()
	state, err := relayconvert.NewResponseStreamState(types.RelayFormatOpenAIResponses, info.RelayFormat, relayconvert.ResponseStreamOptions{
		ID:      responseId,
		Model:   info.UpstreamModelName,
		Created: createAt,
	})
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	streamErr := (*types.NewAPIError)(nil)
	strictChat := info.RelayFormat == types.RelayFormatOpenAI
	// upstreamFailed records a response.failed / response.error event and
	// failedUsage the usage it reported, if any.
	upstreamFailed := false
	var failedUsage *dto.Usage

	if info.RelayFormat == types.RelayFormatClaude && info.ClaudeConvertInfo == nil {
		info.ClaudeConvertInfo = &relaycommon.ClaudeConvertInfo{LastMessagesType: relaycommon.LastMessageTypeNone}
	}

	// writeFailed stops the stream after a failed client write. When the client
	// went away the upstream already produced output, so the request settles
	// normally instead of failing; any other write error stays a server error.
	writeFailed := func(err error) bool {
		if strictChat || c.Request.Context().Err() != nil {
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, err)
			return false
		}
		streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
		return false
	}

	sendGeminiResponse := func(geminiResponse *dto.GeminiChatResponse) bool {
		if geminiResponse == nil {
			return true
		}
		geminiResponseStr, err := common.Marshal(geminiResponse)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
			return false
		}
		c.Render(-1, common.CustomEvent{Data: "data: " + string(geminiResponseStr)})
		_ = helper.FlushWriter(c)
		return true
	}

	sendStreamResult := func(result relayconvert.ResponseResult) bool {
		// Converted chat chunks share the same acknowledgment identity.
		if info.EarlyResponses != nil && info.EarlyResponses.Chat {
			var chunk *dto.ChatCompletionsStreamResponse
			switch value := result.Value.(type) {
			case dto.ChatCompletionsStreamResponse:
				chunk = &value
			case *dto.ChatCompletionsStreamResponse:
				chunk = value
			}
			if chunk != nil {
				if len(chunk.Choices) == 0 && chunk.Usage == nil {
					return true
				}
				raw, err := common.Marshal(chunk)
				if err != nil {
					streamErr = types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusBadGateway)
					return false
				}
				data, err := helper.RewriteEarlyChatChunk(info, string(raw))
				if err != nil {
					streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusBadGateway)
					return false
				}
				if err := helper.ResponseChunkData(c, dto.ResponsesStreamResponse{}, data); err != nil {
					return writeFailed(err)
				}
				return true
			}
		}
		switch value := result.Value.(type) {
		case dto.ChatCompletionsStreamResponse:
			if len(value.Choices) == 0 && value.Usage == nil {
				return true
			}
			if err := helper.ObjectData(c, &value); err != nil {
				return writeFailed(err)
			}
			return true
		case *dto.ChatCompletionsStreamResponse:
			if value == nil || (len(value.Choices) == 0 && value.Usage == nil) {
				return true
			}
			if err := helper.ObjectData(c, value); err != nil {
				return writeFailed(err)
			}
			return true
		case dto.ClaudeResponse:
			if err := helper.ClaudeData(c, value); err != nil {
				return writeFailed(err)
			}
			return true
		case *dto.ClaudeResponse:
			if value == nil {
				return true
			}
			if err := helper.ClaudeData(c, *value); err != nil {
				return writeFailed(err)
			}
			return true
		case dto.GeminiChatResponse:
			return sendGeminiResponse(&value)
		case *dto.GeminiChatResponse:
			return sendGeminiResponse(value)
		default:
			streamErr = types.NewOpenAIError(fmt.Errorf("unsupported converted stream response type %T", result.Value), types.ErrorCodeBadResponse, http.StatusInternalServerError)
			return false
		}
	}

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if streamErr != nil {
			stopStream(sr, streamErr)
			return
		}

		var streamResp dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResp); err != nil {
			logger.LogError(c, "failed to unmarshal responses stream event: "+err.Error())
			if strictChat {
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusBadGateway)
				sr.Stop(streamErr)
				return
			}
			sr.Error(err)
			return
		}

		if streamResp.Response != nil {
			info.ObserveResponseModel(streamResp.Response.Model)
		}
		service.ObserveResponsesOutcome(info, &streamResp)
		if c.GetBool("chat_retry_managed") && !responsesEventReplaySafe(&streamResp) && !responsesTransportKeepalive(streamResp.Type, data) {
			c.Set("chat_retry_unsafe", true)
		}
		if info.FirstTextTime.IsZero() && streamResp.Type == "response.output_text.delta" && streamResp.Delta != "" {
			info.FirstTextTime = time.Now()
		}
		if streamResp.Type == "error" || streamResp.Type == "response.error" || streamResp.Type == "response.failed" || (strictChat && info.StreamStatus.ResponseFailed()) {
			upstreamFailed = true
			if strictChat {
				if streamResp.Response != nil {
					failedUsage = streamResp.Response.Usage
				}
				rawError := gjson.Get(data, "response.error")
				if !rawError.IsObject() {
					rawError = gjson.Get(data, "error")
				}
				flat := !rawError.IsObject()
				if flat {
					rawError = gjson.Parse(data)
				}
				wire := types.OpenAIError{}
				_ = common.UnmarshalJsonStr(rawError.Raw, &wire)
				if flat {
					wire.Type = ""
				}
				if wire.Code == nil && wire.Type == "" {
					wire.Code, wire.Type = "server_error", "server_error"
				}
				if wire.Message == "" {
					wire.Message = "upstream Responses stream failed"
				}
				status := int(gjson.Get(data, "status_code").Int())
				if status < 400 || status > 599 {
					status = http.StatusBadRequest
				}
				streamErr = service.NormalizeResponsesRetryError(types.WithOpenAIError(wire, status))
				stopStream(sr, streamErr)
				return
			}
			if streamResp.Response != nil {
				failedUsage = streamResp.Response.Usage
				if oaiErr := streamResp.Response.GetOpenAIError(); oaiErr != nil && oaiErr.Type != "" {
					streamErr = types.WithOpenAIError(*oaiErr, http.StatusInternalServerError)
					stopStream(sr, streamErr)
					return
				}
			}
			streamErr = types.NewOpenAIError(fmt.Errorf("responses stream error: %s", streamResp.Type), types.ErrorCodeBadResponse, http.StatusInternalServerError)
			stopStream(sr, streamErr)
			return
		}

		results, err := service.ConvertStreamResponseChunk(c, info, state, &streamResp)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
			stopStream(sr, streamErr)
			return
		}
		for _, result := range results {
			if !sendStreamResult(result) {
				stopStream(sr, streamErr)
				return
			}
		}
		if info.EarlyResponses != nil && info.StreamStatus.ResponseOutcome() != "" {
			sr.Done()
		}
	})

	if strictChat {
		info.StreamStatus.RequireTerminal()
		if c.Request.Context().Err() != nil || info.StreamStatus.EndReason == relaycommon.StreamEndReasonClientGone || info.StreamStatus.EndReason == relaycommon.StreamEndReasonPingFail {
			usage := state.Usage()
			if usage == nil {
				usage = service.ResponseText2Usage(c, state.UsageText(), info.UpstreamModelName, info.GetEstimatePromptTokens())
			}
			return usage, nil
		}
		if streamErr == nil && (!info.StreamStatus.IsNormalEnd() || info.StreamStatus.HasErrors() || info.StreamStatus.ResponseOutcome() == "") {
			streamErr = types.WithOpenAIError(types.OpenAIError{Code: "server_error", Type: "server_error", Message: "upstream Responses stream failed or ended before a terminal event"}, http.StatusBadGateway)
		}
	}

	if streamErr != nil {
		if strictChat {
			streamErr = service.NormalizeResponsesRetryError(streamErr)
			info.StreamStatus.MarkFailed(string(streamErr.GetErrorCode()), streamErr.ToOpenAIError().Type, streamErr.StatusCode)
		}
		if !strictChat && (!upstreamFailed || state.UsageText() == "") {
			return nil, streamErr
		}
		// The upstream failed after output reached the client: bill the
		// delivered output (upstream usage when reported, otherwise the
		// estimate) and send the client the same error the failed request
		// path writes. stream_status already records the failure.
		usage := relayconvert.UsageFromResponsesUsage(failedUsage)
		if usage.TotalTokens == 0 {
			usage = state.Usage()
		}
		if usage == nil || usage.TotalTokens == 0 {
			usage = &dto.Usage{}
			if state.UsageText() != "" || !strictChat {
				usage = service.ResponseText2Usage(c, state.UsageText(), info.UpstreamModelName, info.GetEstimatePromptTokens())
			}
		} else if (usage.CompletionTokens == 0 || usage.PromptTokens == 0) && (state.UsageText() != "" || !strictChat) {
			// As in the native Responses accumulator, reported usage that
			// omits the output or the prompt is completed from the estimate of
			// the delivered output and the prompt.
			estimate := service.ResponseText2Usage(c, state.UsageText(), info.UpstreamModelName, info.GetEstimatePromptTokens())
			if usage.CompletionTokens == 0 {
				usage.CompletionTokens = estimate.CompletionTokens
			}
			if usage.PromptTokens == 0 {
				usage.PromptTokens = estimate.PromptTokens
			}
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
			if usage.BillingUsage != nil {
				usage.BillingUsage = dto.CloneBillingUsageWithEstimatedCompletion(usage.BillingUsage, usage.CompletionTokens)
			}
		}
		if strictChat {
			if c.GetBool("chat_retry_managed") {
				return usage, streamErr
			}
			if !c.Writer.Written() && usage.TotalTokens == 0 {
				return nil, streamErr
			}
			_ = helper.FailChatStream(c, info, streamErr)
			if usage.TotalTokens == 0 && state.UsageText() == "" {
				types.ErrOptionWithSkipRetry()(streamErr)
				return nil, streamErr
			}
			return usage, nil
		}
		logger.LogError(c, "responses stream failed after output was delivered, billing the delivered output: "+streamErr.Error())
		streamErr.SetMessage(common.MessageWithRequestId(streamErr.Error(), c.GetString(common.RequestIdKey)))
		if info.EarlyResponses != nil && !info.EarlyResponses.SentAt.IsZero() {
			_ = helper.FailEarlyResponses(c, info, streamErr)
		} else if info.RelayFormat == types.RelayFormatClaude {
			c.JSON(streamErr.StatusCode, gin.H{"type": "error", "error": streamErr.ToClaudeError()})
		} else {
			c.JSON(streamErr.StatusCode, gin.H{"error": streamErr.ToOpenAIError()})
		}
		return usage, nil
	}

	usage := state.Usage()
	if usage == nil || usage.TotalTokens == 0 {
		usage = service.ResponseText2Usage(c, state.UsageText(), info.UpstreamModelName, info.GetEstimatePromptTokens())
		state.SetUsage(usage)
	}
	if info.StreamStatus.EndReason == relaycommon.StreamEndReasonClientGone {
		return usage, nil
	}
	if ack := info.EarlyResponses; ack != nil && ack.Chat {
		if !ack.SentAt.IsZero() {
			responseId, createAt = ack.ID, ack.CreatedAt
		}
	}

	if info.RelayFormat == types.RelayFormatClaude && info.ClaudeConvertInfo != nil {
		info.ClaudeConvertInfo.Usage = usage
	}
	finalResults, err := service.FinalizeStreamResponse(c, info, state)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	for _, result := range finalResults {
		if !sendStreamResult(result) {
			if streamErr != nil {
				return nil, streamErr
			}
			return usage, nil
		}
	}
	if info.RelayFormat == types.RelayFormatOpenAI && info.ShouldIncludeUsage && usage != nil {
		if err := helper.ObjectData(c, helper.GenerateFinalUsageResponse(responseId, createAt, info.UpstreamModelName, *usage)); err != nil {
			writeFailed(err)
			if streamErr != nil {
				return nil, streamErr
			}
			return usage, nil
		}
	}

	if info.RelayFormat == types.RelayFormatOpenAI {
		helper.Done(c)
	}
	if ack := info.EarlyResponses; ack != nil {
		ack.Finished = true
	}
	return usage, nil
}
