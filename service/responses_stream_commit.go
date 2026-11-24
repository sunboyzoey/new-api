package service

import (
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

const responsesStreamCommitKey = "responses_stream_commit"

type ResponsesCommitReason string

const (
	ResponsesCommitEvent          ResponsesCommitReason = "event_forwarded"
	ResponsesCommitBufferLimit    ResponsesCommitReason = "prelude_buffer_limit"
	ResponsesCommitChatConversion ResponsesCommitReason = "chat_conversion"
)

// Capture only bounded protocol labels and counters from the event that first
// prevents replay. Other events may carry additional output later in the stream.
type responsesStreamCommit struct {
	EventType        string                `json:"event_type"`
	ItemType         string                `json:"item_type,omitempty"`
	Reason           ResponsesCommitReason `json:"reason"`
	BufferedBytes    int                   `json:"buffered_bytes"`
	EventBytes       int                   `json:"event_bytes"`
	HasOutputText    bool                  `json:"has_output_text"`
	HasReasoningText bool                  `json:"has_reasoning_text"`
	HasToolCall      bool                  `json:"has_tool_call"`
}

func MarkResponsesStreamCommit(c *gin.Context, event *dto.ResponsesStreamResponse, reason ResponsesCommitReason, bufferedBytes, eventBytes int) {
	if c.GetBool("responses_content_delivered") {
		return
	}
	commit := responsesStreamCommit{EventType: "other", Reason: reason, BufferedBytes: bufferedBytes, EventBytes: eventBytes}
	switch event.Type {
	case "response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done",
		"response.mcp_call_arguments.delta", "response.mcp_call_arguments.done",
		"response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed",
		"response.file_search_call.in_progress", "response.file_search_call.searching", "response.file_search_call.completed",
		"response.code_interpreter_call.in_progress", "response.code_interpreter_call.interpreting", "response.code_interpreter_call.completed",
		"response.mcp_call.in_progress", "response.mcp_call.completed", "response.mcp_call.failed":
		commit.EventType = event.Type
		commit.HasToolCall = true
	case "response.created", "response.in_progress", "response.queued", "response.completed", "response.done",
		"response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error",
		"response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done",
		"response.output_text.delta", "response.output_text.done", "response.output_text.annotation.added",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.reasoning_text.delta", "response.reasoning_text.done", "response.refusal.delta", "response.refusal.done",
		"response.audio.delta", "response.audio.done", "response.audio_transcript.delta", "response.audio_transcript.done":
		commit.EventType = event.Type
	}
	commit.HasOutputText = (event.Type == "response.output_text.delta" && event.Delta != "") ||
		(event.Type == "response.output_text.done" && event.Text != nil && *event.Text != "") ||
		(event.Part != nil && event.Part.Type == "output_text" && event.Part.Text != "")
	switch event.Type {
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		commit.HasReasoningText = event.Part != nil && event.Part.Text != ""
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		commit.HasReasoningText = event.Delta != ""
	case "response.reasoning_summary_text.done", "response.reasoning_text.done":
		commit.HasReasoningText = event.Text != nil && *event.Text != ""
	}
	if event.Item != nil {
		commit.ItemType = commit.observeOutput(event.Item)
	}
	if event.Response != nil {
		for i := range event.Response.Output {
			commit.observeOutput(&event.Response.Output[i])
		}
	}
	c.Set(responsesStreamCommitKey, commit)
	c.Set("responses_content_delivered", true)
}

func (commit *responsesStreamCommit) observeOutput(item *dto.ResponsesOutput) string {
	for _, part := range item.Content {
		if part.Type == "output_text" && part.Text != "" {
			commit.HasOutputText = true
		}
	}
	switch item.Type {
	case "function_call", "custom_tool_call", "web_search_call", "file_search_call", "mcp_call",
		"code_interpreter_call", "image_generation_call", "computer_call", "local_shell_call",
		"shell_call", "apply_patch_call", "tool_search_call":
		commit.HasToolCall = true
		return item.Type
	case "reasoning":
		for _, part := range item.Summary {
			commit.HasReasoningText = commit.HasReasoningText || part.Text != ""
		}
		for _, part := range item.Content {
			commit.HasReasoningText = commit.HasReasoningText || part.Text != ""
		}
		return item.Type
	case "message":
		return item.Type
	default:
		return "other"
	}
}
