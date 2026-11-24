package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// DecideRelayRetry is the single retry decision for relay attempts. The reason
// is recorded in the request policy decision events of the log details.
func DecideRelayRetry(c *gin.Context, err *types.NewAPIError, retryTimes int) PolicyDecision {
	if err == nil {
		return PolicyDecision{Action: "stop", Reason: "request_completed", Source: "system"}
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		if c.Request.Context().Err() == context.DeadlineExceeded && common.GetContextKeyBool(c, constant.ContextKeyResponsesChannelFailover) {
			return PolicyDecision{Action: "stop", Reason: "downstream_deadline", Source: "system"}
		}
		return PolicyDecision{Action: "stop", Reason: "client_cancelled", Source: "system"}
	}
	if c.GetBool("responses_content_delivered") {
		return PolicyDecision{Action: "stop", Reason: "content_delivered", Source: "system"}
	}
	if c.GetBool("responses_early_ack_committed") && !c.GetBool("responses_retry_managed") {
		return PolicyDecision{Action: "stop", Reason: "early_response_committed", Source: "system"}
	}
	// Non-managed streams cannot be replayed once downstream bytes were sent.
	if c.Writer.Written() && common.GetContextKeyBool(c, constant.ContextKeyIsStream) && !c.GetBool("responses_retry_managed") {
		return PolicyDecision{Action: "stop", Reason: "stream_committed", Source: "system"}
	}
	failover := common.GetContextKeyBool(c, constant.ContextKeyResponsesChannelFailover)
	if common.GetContextKeyInt(c, constant.ContextKeyResponsesOwnerChannel) > 0 || (failover && common.GetContextKeyBool(c, constant.ContextKeyResponsesBoundHistory)) {
		return PolicyDecision{Action: "stop", Reason: "channel_bound_history", Source: "routing"}
	}
	if failover {
		if _, pinned, _ := GetChannelConstraints(c).ResolvedPin(); pinned {
			return PolicyDecision{Action: "stop", Reason: "pinned_channel", Source: "channel_constraint"}
		}
	}
	if ShouldSkipRetryAfterChannelAffinityFailure(c) {
		meta, _ := getChannelAffinityMeta(c)
		if !failover || meta.KeySourceType != "gjson" || meta.KeySourcePath != "prompt_cache_key" {
			source := RequestPolicy(c).SessionModeSource
			if source == "" {
				source = "session_rule"
			}
			return PolicyDecision{Action: "stop", Reason: "strict_session", Source: source}
		}
	}
	if failover {
		if !common.GetContextKeyBool(c, constant.ContextKeyResponsesUpstreamRetryable) || IsViolationFeeCode(err.GetErrorCode()) {
			return PolicyDecision{Action: "stop", Reason: "non_retryable_error", Source: "system"}
		}
		if retryTimes <= 0 {
			return PolicyDecision{Action: "stop", Reason: "attempt_budget_exhausted", Source: "global"}
		}
		return PolicyDecision{Action: "retry", Reason: "upstream_channel_failed", Source: "upstream"}
	}
	if GetChannelConstraints(c).SuppressesRetry() {
		return PolicyDecision{Action: "stop", Reason: "pinned_channel", Source: "channel_constraint"}
	}
	if types.IsChannelError(err) && !c.GetBool("responses_retry_managed") {
		return PolicyDecision{Action: "retry", Reason: "channel_error", Source: "system"}
	}
	if types.IsSkipRetryError(err) {
		return PolicyDecision{Action: "stop", Reason: "non_retryable_error", Source: "system"}
	}
	if retryTimes <= 0 {
		return PolicyDecision{Action: "stop", Reason: "attempt_budget_exhausted", Source: "global"}
	}
	if types.IsChannelError(err) {
		return PolicyDecision{Action: "retry", Reason: "channel_error", Source: "system"}
	}
	code := err.StatusCode
	if code >= 200 && code < 300 {
		return PolicyDecision{Action: "stop", Reason: "system_retry_exclusion", Source: "system"}
	}
	if code < 100 || code > 599 {
		return PolicyDecision{Action: "retry", Reason: "unrecognized_status", Source: "system"}
	}
	if operation_setting.IsAlwaysSkipRetryCode(err.GetErrorCode()) || operation_setting.IsAlwaysSkipRetryStatusCode(code) {
		return PolicyDecision{Action: "stop", Reason: "system_retry_exclusion", Source: "system"}
	}
	if operation_setting.ShouldRetryByStatusCode(code) {
		return PolicyDecision{Action: "retry", Reason: "retry_status_matched", Source: "global"}
	}
	return PolicyDecision{Action: "stop", Reason: "status_not_retryable", Source: "global"}
}

func ShouldRetryRelayError(c *gin.Context, openaiErr *types.NewAPIError, retryTimes int) bool {
	return DecideRelayRetry(c, openaiErr, retryTimes).Action == "retry"
}

func ProcessChannelError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError, relayInfo *relaycommon.RelayInfo) {
	ProcessChannelAttemptError(c, channelError, err)
	RecordRelayErrorLog(c, channelError.ChannelId, err, relayInfo)
}

// ProcessChannelAttemptError applies health decisions as soon as an attempt fails.
func ProcessChannelAttemptError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError) {
	if err == nil {
		return
	}
	logger.LogError(c, fmt.Sprintf("channel error (channel #%d, status code: %d): %s", channelError.ChannelId, err.StatusCode, common.LocalLogPreview(err.MaskSensitiveErrorWithStatusCode())))
	if ShouldDisableChannel(err) && channelError.AutoBan {
		reason := err.MaskSensitiveErrorWithStatusCode()
		gopool.Go(func() {
			DisableChannel(channelError, reason)
		})
	}
}

// RecordRelayErrorLog writes the error shown in usage history once the request ends.
func RecordRelayErrorLog(c *gin.Context, channelID int, err *types.NewAPIError, relayInfo *relaycommon.RelayInfo) {
	if constant.ErrorLogEnabled && types.IsRecordErrorLog(err) {
		userId := c.GetInt("id")
		tokenName := c.GetString("token_name")
		modelName := c.GetString("original_model")
		tokenId := c.GetInt("token_id")
		userGroup := c.GetString("group")
		other := model.NewLogOther()
		if c.Request != nil && c.Request.URL != nil {
			other.SetPublic("request_path", c.Request.URL.Path)
		}
		other.SetPublic("error_type", err.GetErrorType())
		other.SetPublic("error_code", err.GetErrorCode())
		other.SetPublic("status_code", err.StatusCode)
		if relayInfo != nil {
			other.SetAdmin("final_request_error", true)
		}
		AppendRelayLogAdminInfo(c, relayInfo, other)
		AppendResponseModelLogInfo(relayInfo, other)
		AppendTaskPluginContextAuditInfo(c, other)
		startTime := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
		if startTime.IsZero() {
			startTime = time.Now()
		}
		useTimeSeconds := int(time.Since(startTime).Seconds())
		model.RecordErrorLog(c, userId, channelID, modelName, tokenName, err.MaskSensitiveErrorWithStatusCode(), tokenId, useTimeSeconds, common.GetContextKeyBool(c, constant.ContextKeyIsStream), userGroup, other)
	}
}

// NormalizeResponsesRetryError distinguishes transport/server faults from
// permanent input/authentication failures. Unknown 4xx codes stay intact.
func NormalizeResponsesRetryError(err *types.NewAPIError) *types.NewAPIError {
	if err == nil {
		return nil
	}
	wire := err.ToOpenAIError()
	code := strings.ToLower(fmt.Sprint(wire.Code))
	kind := strings.ToLower(wire.Type)
	status := err.StatusCode
	permanent := status == http.StatusUnauthorized || status == http.StatusForbidden || kind == "authentication_error" || kind == "permission_error" ||
		code == "thinking_signature_invalid" || code == "invalid_encrypted_content" ||
		code == "invalid_api_key" || code == "auth_unavailable" || code == "upstream_authentication_required" || code == "context_length_exceeded" || code == "context_too_large" || code == "previous_response_not_found"
	if permanent {
		if status >= 500 || status < 400 {
			status = http.StatusBadRequest
		}
		if kind == "authentication_error" || code == "invalid_api_key" || code == "auth_unavailable" || code == "upstream_authentication_required" {
			status = http.StatusUnauthorized
		}
		if kind == "permission_error" {
			status = http.StatusForbidden
		}
		return types.WithOpenAIError(wire, status, types.ErrOptionWithSkipRetry())
	}
	if status == http.StatusTooManyRequests || code == "rate_limit_exceeded" || code == "rate_limit_error" || code == "turn_rate_limit_exceeded" || kind == "rate_limit_error" {
		normalized := types.WithOpenAIError(wire, http.StatusTooManyRequests)
		if types.IsSkipRetryError(err) {
			types.ErrOptionWithSkipRetry()(normalized)
		}
		return normalized
	}
	transient := code == "server_error" || code == "internal_server_error" || code == "upstream_error" || code == "service_unavailable" || code == "overloaded_error" || code == "tool_protocol_error" || code == "incomplete_stream" || code == "upstream_stream_read_error" || kind == "server_error"
	if kind == "invalid_request_error" && !transient {
		if status >= 500 || status < 400 {
			status = http.StatusBadRequest
		}
		return types.WithOpenAIError(wire, status, types.ErrOptionWithSkipRetry())
	}
	if transient || status >= 500 || status == http.StatusRequestTimeout {
		// HTTP classification must not erase the upstream error identity.
		// Clients use the original code to decide whether recovery is possible.
		if wire.Code == nil || fmt.Sprint(wire.Code) == "" {
			wire.Code = "server_error"
		}
		// Keep health accounting from mistaking a transport failure reported
		// as invalid_request_error for a client/business rejection.
		wire.Type = "server_error"
		options := []types.NewAPIErrorOptions{}
		if types.IsSkipRetryError(err) {
			options = append(options, types.ErrOptionWithSkipRetry())
		}
		return types.WithOpenAIError(wire, http.StatusBadGateway, options...)
	}
	if status >= 400 && status < 500 {
		return types.WithOpenAIError(wire, status, types.ErrOptionWithSkipRetry())
	}
	return err
}

// FinalRelayStreamError describes a terminal failure for the request log when
// the adaptor already finished the response and settled any delivered usage.
func FinalRelayStreamError(stream *relaycommon.StreamStatus) *types.NewAPIError {
	outcome := stream.OutcomeSnapshot()
	if outcome.Response == relaycommon.ResponseOutcomeFailed {
		code := outcome.ErrorCode
		if code == "" {
			code = "upstream_error"
		}
		wire := types.OpenAIError{Code: code, Type: outcome.ErrorType, Message: "The upstream reported a failed stream."}
		if outcome.ErrorStatus >= 400 && outcome.ErrorStatus <= 599 {
			return types.WithOpenAIError(wire, outcome.ErrorStatus)
		}
		return NormalizeResponsesRetryError(types.WithOpenAIError(wire, http.StatusBadGateway))
	}
	if outcome.Response != relaycommon.ResponseOutcomeUnknown {
		return nil
	}
	switch outcome.EndReason {
	case relaycommon.StreamEndReasonTimeout:
		return types.WithOpenAIError(types.OpenAIError{Code: "stream_timeout", Message: "The response stream timed out."}, http.StatusGatewayTimeout)
	case relaycommon.StreamEndReasonClientGone, relaycommon.StreamEndReasonPingFail:
		return types.WithOpenAIError(types.OpenAIError{Code: "client_disconnected", Message: "The downstream connection closed."}, 499)
	case relaycommon.StreamEndReasonScannerErr, relaycommon.StreamEndReasonPanic:
		return types.WithOpenAIError(types.OpenAIError{Code: "stream_interrupted", Message: "The response stream was interrupted."}, http.StatusBadGateway)
	}
	if outcome.ExpectsTerminal && outcome.EndReason != relaycommon.StreamEndReasonNone {
		return types.WithOpenAIError(types.OpenAIError{Code: "incomplete_stream", Message: "The stream ended without its terminal response."}, http.StatusBadGateway)
	}
	return nil
}
