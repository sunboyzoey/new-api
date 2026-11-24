package relay

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	openaichannel "github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

func ResponsesHelper(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {
	common.SetContextKey(c, constant.ContextKeyResponsesUpstreamRetryable, false)
	var responsesReq *dto.OpenAIResponsesRequest
	switch req := info.Request.(type) {
	case *dto.OpenAIResponsesRequest:
		responsesReq = req
	case *dto.OpenAIResponsesCompactionRequest:
		// Only fields documented for POST /v1/responses/compact are forwarded:
		// model, input, instructions, previous_response_id, prompt_cache_key,
		// prompt_cache_options, prompt_cache_retention, service_tier.
		// Undocumented Codex-parity fields (tools, reasoning, text) are parsed
		// for client compatibility but intentionally not sent upstream.
		responsesReq = &dto.OpenAIResponsesRequest{
			Model:                req.Model,
			Input:                req.Input,
			Instructions:         req.Instructions,
			PreviousResponseID:   req.PreviousResponseID,
			ParallelToolCalls:    req.ParallelToolCalls,
			ServiceTier:          req.ServiceTier,
			PromptCacheKey:       req.PromptCacheKey,
			PromptCacheOptions:   req.PromptCacheOptions,
			PromptCacheRetention: req.PromptCacheRetention,
		}
	default:
		return types.NewErrorWithStatusCode(
			fmt.Errorf("invalid request type, expected dto.OpenAIResponsesRequest or dto.OpenAIResponsesCompactionRequest, got %T", info.Request),
			types.ErrorCodeInvalidRequest,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}

	adaptor, requestBody, closer, apiErr := PrepareResponsesRequest(c, info, responsesReq)
	if apiErr != nil {
		return apiErr
	}
	defer closer.Close()
	nativeResponses := info.RelayMode == relayconstant.RelayModeResponses &&
		info.RelayFormat == types.RelayFormatOpenAIResponses &&
		model.SupportsManagedResponses(info.ChannelType, info.ApiType, info.ChannelOtherSettings.AdvancedCustom, c.Request.URL.Path, info.OriginModelName)
	if nativeResponses && info.ChannelType == constant.ChannelTypeAdvancedCustom && (!info.IsStream || info.ResponsesRetryManaged) {
		common.SetContextKey(c, constant.ContextKeyResponsesChannelFailover, true)
	}
	if info.ResponsesRetryManaged {
		if !nativeResponses {
			// A converting adaptor cannot continue an already public native Responses lifecycle.
			if c.Writer.Written() {
				return types.NewOpenAIError(fmt.Errorf("fallback adaptor cannot continue the committed Responses stream"), types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
			}
			info.ResponsesRetryManaged = false
			c.Set("responses_retry_managed", false)
			defer func() { info.ResponsesRetryManaged = true }()
		}
		if info.ResponsesRetryManaged {
			c.Set("responses_retry_managed", true)
		}
	}
	helper.PrepareEarlyResponses(info, requestBody)
	defer func() {
		info.EarlyResponses.Stop()
		if info.ResponsesRetryManaged && newAPIError != nil {
			newAPIError = service.NormalizeResponsesRetryError(newAPIError)
		}
		if !info.ResponsesRetryManaged && newAPIError != nil && info.EarlyResponses != nil && !info.EarlyResponses.SentAt.IsZero() {
			types.ErrOptionWithSkipRetry()(newAPIError)
			_ = helper.FailEarlyResponses(c, info, newAPIError)
		}
	}()

	var httpResp *http.Response
	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		return types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}

	statusCodeMappingStr := c.GetString("status_code_mapping")

	if resp != nil {
		httpResp = resp.(*http.Response)

		if httpResp.StatusCode != http.StatusOK {
			var errorBody []byte
			newAPIError, errorBody = service.RelayErrorHandlerWithBody(c.Request.Context(), httpResp, false)
			if nativeResponses && common.GetContextKeyBool(c, constant.ContextKeyResponsesChannelFailover) {
				handleResponsesHTTPFailure(c, info, newAPIError, errorBody)
			}
			// reset status code 重置状态码
			service.ResetStatusCode(newAPIError, statusCodeMappingStr)
			return newAPIError
		}
	}

	if (info.EarlyResponses != nil || info.ResponsesRetryManaged) && (httpResp == nil || !strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream")) {
		newAPIError = types.NewOpenAIError(fmt.Errorf("expected upstream Responses event stream"), types.ErrorCodeBadResponse, http.StatusBadGateway)
		if httpResp != nil {
			defer service.CloseResponseBodyGracefully(httpResp)
		}
		if nativeResponses && common.GetContextKeyBool(c, constant.ContextKeyResponsesChannelFailover) {
			if httpResp != nil && strings.Contains(strings.ToLower(httpResp.Header.Get("Content-Type")), "json") {
				// A provider ignoring stream=true can return an entire paid result.
				// Limit inspection of this unexpected body and avoid replay if its
				// completion or usage cannot be determined within the bound.
				const maxBody = 1 << 20
				body, readErr := io.ReadAll(io.LimitReader(httpResp.Body, maxBody+1))
				if readErr != nil || len(body) > maxBody {
					types.ErrOptionWithSkipRetry()(newAPIError)
				} else {
					handleResponsesHTTPFailure(c, info, newAPIError, body)
				}
			} else {
				helper.MarkResponsesUpstreamFailure(c)
			}
		}
		return newAPIError
	}

	usage, newAPIError := adaptor.DoResponse(c, httpResp, info)
	if newAPIError != nil {
		if info.ResponsesRetryManaged || (nativeResponses && common.GetContextKeyBool(c, constant.ContextKeyResponsesChannelFailover)) {
			// Delivered output and observed upstream usage settle once; the
			// router must never replay an attempt after either boundary.
			if observed, ok := usage.(*dto.Usage); ok && observed != nil {
				ConsumeResponsesQuota(c, info, observed)
				types.ErrOptionWithSkipRetry()(newAPIError)
			}
		}
		// reset status code 重置状态码
		service.ResetStatusCode(newAPIError, statusCodeMappingStr)
		return newAPIError
	}

	usageDto := usage.(*dto.Usage)
	if info.RelayMode == relayconstant.RelayModeResponsesCompact {
		originModelName := info.OriginModelName
		originPriceData := info.PriceData

		_, err := helper.ModelPriceHelper(c, info, info.GetEstimatePromptTokens(), &types.TokenCountMeta{})
		if err != nil {
			info.OriginModelName = originModelName
			info.PriceData = originPriceData
			return types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithSkipRetry(), types.ErrOptionWithStatusCode(http.StatusBadRequest))
		}
		service.PostTextConsumeQuota(c, info, usageDto, nil)

		info.OriginModelName = originModelName
		info.PriceData = originPriceData
		return nil
	}

	ConsumeResponsesQuota(c, info, usageDto)
	return nil
}

func handleResponsesHTTPFailure(c *gin.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError, body []byte) {
	usage, observedWork := openaichannel.ResponsesHTTPFailureUsage(info, body)
	if observedWork {
		if usage != nil {
			ConsumeResponsesQuota(c, info, usage)
		}
		types.ErrOptionWithSkipRetry()(apiErr)
		return
	}
	helper.MarkResponsesUpstreamFailure(c)
}

// ConsumeResponsesQuota applies the same settlement dispatch to HTTP and
// WebSocket Responses usage. Compact requests keep their separate repricing.
func ConsumeResponsesQuota(c *gin.Context, info *relaycommon.RelayInfo, usage *dto.Usage) {
	if strings.HasPrefix(info.OriginModelName, "gpt-4o-audio") {
		service.PostAudioConsumeQuota(c, info, usage, "")
		return
	}
	service.PostTextConsumeQuota(c, info, usage, nil)
}
