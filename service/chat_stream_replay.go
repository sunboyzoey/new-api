package service

import (
	"unsafe"

	"github.com/tidwall/gjson"
)

type ChatStreamReplayPolicy int

const (
	ChatStreamUnmanaged ChatStreamReplayPolicy = iota
	ChatStreamReplayable
	ChatStreamExternalWork
)

// ClassifyChatStreamReplay keeps only an enum; even disk-backed prompts need
// no extra body reader or retained prompt data when the request is forwarded.
func ClassifyChatStreamReplay(raw []byte) ChatStreamReplayPolicy {
	var stream, external bool
	var seenStream, seenBackground, seenTools bool
	// The bytes are owned by the current request and remain read-only throughout
	// this synchronous walk. No key or value from the view escapes the function.
	gjson.Parse(unsafe.String(unsafe.SliceData(raw), len(raw))).ForEach(func(k, v gjson.Result) bool {
		switch k.String() {
		case "stream":
			if seenStream {
				external = true
			}
			seenStream = true
			stream = stream || v.Bool()
		case "background":
			if seenBackground || v.Bool() {
				external = true
			}
			seenBackground = true
		case "tools":
			if seenTools || !OpenAIToolsReplaySafe(v) {
				external = true
			}
			seenTools = true
		}
		return true
	})
	if !stream {
		return ChatStreamUnmanaged
	}
	if external {
		return ChatStreamExternalWork
	}
	return ChatStreamReplayable
}

// OpenAIToolsReplaySafe recognizes client proposals and read-only hosted tools.
func OpenAIToolsReplaySafe(tools gjson.Result) bool {
	if tools.Type == gjson.Null {
		return true
	}
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		switch tool.Get("type").String() {
		case "function", "custom", "web_search", "web_search_preview", "web_search_preview_2025_03_11", "file_search":
		case "namespace":
			// Namespaced tools remain client-dispatched when every member is a
			// function or custom proposal. Check members before allowing replay.
			members := tool.Get("tools")
			if !members.IsArray() {
				return false
			}
			for _, member := range members.Array() {
				switch member.Get("type").String() {
				case "function", "custom":
				default:
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}
