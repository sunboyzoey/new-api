package openai

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var errResponsesPrefixMismatch = errors.New("alternate channel did not reproduce the delivered response prefix")

type responseTextSlot struct {
	output, part int
	field        string
}

type responsesPrefixFrame struct {
	shape [sha256.Size]byte
	delta string
}

// One request owns a single public stream. Each new channel must reproduce all
// published semantic events in order before it may append any new output.
type responsesContinuation struct {
	ctx                         *gin.Context
	info                        *relaycommon.RelayInfo
	frames                      []responsesPrefixFrame
	publicItems                 map[int]string
	deliveredText               map[responseTextSlot]*strings.Builder
	attemptItems                map[int]string
	publicID, upstreamID        string
	createdAt                   int64
	hasCreatedAt                bool
	lifecycle                   bool
	replaying                   bool
	uncatchable                 bool
	tooLarge                    bool
	cursor, offset, limit, size int
}

func beginResponsesContinuation(c *gin.Context, info *relaycommon.RelayInfo) *responsesContinuation {
	s, _ := common.GetContextKeyType[*responsesContinuation](c, constant.ContextKeyResponsesPublicPrefix)
	if s == nil {
		s = &responsesContinuation{ctx: c, publicItems: make(map[int]string), deliveredText: make(map[responseTextSlot]*strings.Builder)}
		common.SetContextKey(c, constant.ContextKeyResponsesPublicPrefix, s)
	} else {
		s.replaying = s.lifecycle || len(s.frames) != 0 || s.tooLarge
	}
	s.uncatchable = s.tooLarge
	s.info = info
	s.upstreamID = ""
	s.cursor, s.offset, s.limit = 0, 0, len(s.frames)
	s.attemptItems = make(map[int]string)
	return s
}

// The reader calls this only after checking the request and observed events
// for external side effects and the client for disconnection.
func (s *responsesContinuation) canRetry() bool {
	if s == nil {
		return false
	}
	common.SetContextKey(s.ctx, constant.ContextKeyResponsesPrefixRecovery, true)
	return true
}

func (s *responsesContinuation) matched() bool { return s.cursor == s.limit && !s.uncatchable }

func (s *responsesContinuation) itemID(index int, upstream string) (string, error) {
	if upstream == "" {
		return "", nil
	}
	if seen := s.attemptItems[index]; seen != "" && seen != upstream {
		return "", errResponsesPrefixMismatch
	}
	if _, seen := s.attemptItems[index]; !seen {
		if len(upstream) > 512 || len(s.attemptItems) >= 8192 {
			if s.cursor < s.limit {
				return "", errResponsesPrefixMismatch
			}
			s.tooLarge = true
			return upstream, nil
		}
		s.attemptItems[index] = strings.Clone(upstream)
	}
	if public := s.publicItems[index]; public != "" {
		return public, nil
	}
	if s.cursor < s.limit {
		return "", errResponsesPrefixMismatch
	}
	s.publicItems[index] = s.attemptItems[index]
	return upstream, nil
}

// reconcile receives only frames intended for public delivery. Empty lifecycle
// envelopes carry channel-local metadata and are not part of the content prefix.
func (s *responsesContinuation) reconcile(data string) (string, bool, error) {
	if s == nil {
		return data, true, nil
	}
	root := gjson.Parse(data)
	typ := root.Get("type").String()
	terminal := typ == "response.completed" || typ == "response.done" || typ == "response.incomplete"
	lifecycle := (typ == "response.created" || typ == "response.in_progress" || typ == "response.queued") && len(root.Get("response.output").Array()) == 0
	if s.replaying && (s.uncatchable || (terminal && !s.matched())) {
		return "", false, errResponsesPrefixMismatch
	}
	state := s.info.EarlyResponses
	acked := state != nil && !state.SentAt.IsZero()
	var editErr error
	set := func(path string, value any) {
		if editErr == nil {
			data, editErr = sjson.Set(data, path, value)
		}
	}
	upstreamID := root.Get("response.id").String()
	if upstreamID == "" {
		upstreamID = root.Get("response_id").String()
	}
	if upstreamID != "" {
		if s.upstreamID != "" && s.upstreamID != upstreamID {
			return "", false, errResponsesPrefixMismatch
		}
		s.upstreamID = upstreamID
		if s.publicID == "" {
			s.publicID = upstreamID
		}
		if !acked && upstreamID != s.publicID {
			for _, path := range []string{"response.id", "response_id"} {
				if root.Get(path).Exists() {
					set(path, s.publicID)
				}
			}
		}
	}
	if date := root.Get("response.created_at"); date.Exists() {
		if !s.hasCreatedAt {
			s.createdAt, s.hasCreatedAt = date.Int(), true
		}
		if !acked && date.Int() != s.createdAt {
			set("response.created_at", s.createdAt)
		}
	}
	if lifecycle {
		if s.replaying && s.lifecycle {
			return "", false, nil
		}
		s.lifecycle = true
	}
	rewriteItem := func(index int, path string) error {
		id := root.Get(path).String()
		if id == "" {
			return nil
		}
		public, err := s.itemID(index, id)
		if err != nil {
			return err
		}
		if id != public {
			set(path, public)
		}
		return nil
	}
	index := int(root.Get("output_index").Int())
	for _, path := range []string{"item.id", "item_id"} {
		if err := rewriteItem(index, path); err != nil {
			return "", false, err
		}
	}
	for i := range root.Get("response.output").Array() {
		if err := rewriteItem(i, fmt.Sprintf("response.output.%d.id", i)); err != nil {
			return "", false, err
		}
	}
	if editErr != nil {
		return "", false, editErr
	}
	if s.replaying && !s.snapshotMatches(root, typ, index) {
		return "", false, errResponsesPrefixMismatch
	}
	if !lifecycle && !terminal {
		var fields map[string]common.RawMessage
		if err := common.UnmarshalJsonStr(data, &fields); err != nil {
			return "", false, err
		}
		for _, key := range []string{"sequence_number", "obfuscation", "response_id"} {
			delete(fields, key)
		}
		delta := ""
		isDelta := typ == "response.output_text.delta" || typ == "response.reasoning_text.delta" || typ == "response.reasoning_summary_text.delta"
		if isDelta && root.Get("delta").Type == gjson.String && len(root.Get("logprobs").Array()) == 0 {
			delta = root.Get("delta").String()
			if delta != "" {
				delete(fields, "delta")
			}
		}
		raw, err := common.Marshal(fields)
		if err != nil {
			return "", false, err
		}
		shape := sha256.Sum256(raw)
		for s.cursor < s.limit {
			old := s.frames[s.cursor]
			if shape != old.shape || (old.delta == "") != (delta == "") {
				return "", false, errResponsesPrefixMismatch
			}
			if old.delta == "" {
				s.cursor++
				return "", false, nil
			}
			remaining := old.delta[s.offset:]
			n := min(len(remaining), len(delta))
			if remaining[:n] != delta[:n] {
				return "", false, errResponsesPrefixMismatch
			}
			delta = delta[n:]
			s.offset += n
			if s.offset == len(old.delta) {
				s.cursor++
				s.offset = 0
			}
			if len(delta) == 0 {
				return "", false, nil
			}
		}
		if isDelta && delta != "" && root.Get("delta").String() != delta {
			set("delta", delta)
		}
		if !s.tooLarge {
			s.size += len(delta) + sha256.Size
			if len(s.frames) >= 8192 || s.size > 1<<20 {
				s.tooLarge = true
				s.frames = nil
				s.limit, s.cursor = 0, 0
			} else {
				s.frames = append(s.frames, responsesPrefixFrame{shape: shape, delta: strings.Clone(delta)})
				if delta != "" {
					slot := textSlot(typ, root)
					if s.deliveredText[slot] == nil {
						s.deliveredText[slot] = new(strings.Builder)
					}
					s.deliveredText[slot].WriteString(delta)
				}
			}
		}
	}
	if !terminal && !s.tooLarge {
		s.rememberSnapshots(root, typ, index)
	}
	if s.replaying && !acked && state != nil {
		set("sequence_number", state.Sequence)
	}
	if editErr != nil {
		return "", false, editErr
	}
	if terminal && s.info.ChannelMeta != nil && s.upstreamID != "" {
		public := s.publicID
		if acked {
			public = state.ID
			if err := service.SaveEarlyResponseAlias(s.info.UserId, s.info.ChannelId, public, s.upstreamID); err != nil {
				return "", false, err
			}
		}
		if !acked && public == s.upstreamID {
			service.RecordFallbackResponseOwner(s.ctx, s.info.ChannelId, s.upstreamID)
		} else if err := service.SaveFallbackResponseOwner(s.ctx, s.info.ChannelId, public, s.upstreamID); err != nil {
			return "", false, err
		}
	}
	return data, true, nil
}

func textSlot(typ string, root gjson.Result) responseTextSlot {
	slot := responseTextSlot{output: int(root.Get("output_index").Int()), part: int(root.Get("content_index").Int()), field: "content"}
	if strings.HasPrefix(typ, "response.reasoning_summary_") {
		slot.field, slot.part = "summary", int(root.Get("summary_index").Int())
	}
	return slot
}

// A replacement terminal or done snapshot must agree with actual bytes the
// client has seen. A provider may omit the output snapshot on its terminal.
func (s *responsesContinuation) snapshotMatches(root gjson.Result, typ string, index int) bool {
	switch typ {
	case "response.output_text.done", "response.reasoning_text.done", "response.reasoning_summary_text.done",
		"response.content_part.done", "response.reasoning_summary_part.done":
		buf := s.deliveredText[textSlot(typ, root)]
		if buf == nil {
			return true
		}
		value := root.Get("text")
		if strings.Contains(typ, "part.done") {
			value = root.Get("part.text")
		}
		return value.Type == gjson.String && strings.HasPrefix(value.String(), buf.String())
	case "response.output_item.done", "response.completed", "response.done", "response.incomplete":
		output := root.Get("response.output")
		if typ != "response.output_item.done" && !output.Exists() {
			return true
		}
		for slot, buf := range s.deliveredText {
			var item gjson.Result
			if typ == "response.output_item.done" {
				if index != slot.output {
					continue
				}
				item = root.Get("item")
			} else {
				item = output.Get(strconv.Itoa(slot.output))
			}
			value := item.Get(fmt.Sprintf("%s.%d.text", slot.field, slot.part))
			if value.Type != gjson.String || !strings.HasPrefix(value.String(), buf.String()) {
				return false
			}
		}
	}
	return true
}

// A done or item snapshot may carry text that was never sent as a delta.
// Remember its suffix too, so future completions cannot retract those bytes.
func (s *responsesContinuation) rememberSnapshots(root gjson.Result, typ string, index int) {
	remember := func(slot responseTextSlot, value gjson.Result) {
		if value.Type != gjson.String || s.tooLarge {
			return
		}
		text := value.String()
		if text == "" {
			return
		}
		buf := s.deliveredText[slot]
		if buf == nil {
			buf = new(strings.Builder)
			s.deliveredText[slot] = buf
		}
		if !strings.HasPrefix(text, buf.String()) {
			// Keep forwarding the original response, but no future generation
			// can transparently reproduce an internally inconsistent snapshot.
			s.tooLarge = true
			return
		}
		suffix := text[buf.Len():]
		s.size += len(suffix)
		if s.size > 1<<20 {
			s.tooLarge = true
			return
		}
		buf.WriteString(suffix)
	}
	switch typ {
	case "response.output_text.done", "response.reasoning_text.done", "response.reasoning_summary_text.done":
		remember(textSlot(typ, root), root.Get("text"))
	case "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		remember(textSlot(typ, root), root.Get("part.text"))
	case "response.output_item.added", "response.output_item.done":
		for _, field := range []string{"content", "summary"} {
			for part, value := range root.Get("item." + field).Array() {
				remember(responseTextSlot{index, part, field}, value.Get("text"))
			}
		}
	}
}
