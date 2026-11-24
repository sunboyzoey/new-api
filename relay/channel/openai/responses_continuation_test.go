package openai

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponsesContinuationMatchesPublishedBytesAcrossChunks(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	info := &relaycommon.RelayInfo{EarlyResponses: &relaycommon.EarlyResponsesState{}}
	delta := func(id, text string) string {
		return fmt.Sprintf(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":%q,"delta":%q}`, id, text)
	}
	s := beginResponsesContinuation(c, info)
	for _, text := range []string{"One ", "two"} {
		_, emit, err := s.reconcile(delta("first", text))
		require.NoError(t, err)
		require.True(t, emit)
	}
	s = beginResponsesContinuation(c, info)
	_, emit, err := s.reconcile(delta("second", "One"))
	require.NoError(t, err)
	assert.False(t, emit)
	_, _, err = s.reconcile(`{"type":"response.completed","response":{"id":"second" , "output":[]}}`)
	assert.ErrorIs(t, err, errResponsesPrefixMismatch)
	s = beginResponsesContinuation(c, info)
	frame, emit, err := s.reconcile(delta("third", "One two three"))
	require.NoError(t, err)
	require.True(t, emit)
	assert.Equal(t, " three", gjson.Get(frame, "delta").String())
	assert.Equal(t, "first", gjson.Get(frame, "item_id").String())
	_, _, err = s.reconcile(`{"type":"response.completed","response":{"id":"third","output":[{"id":"third","type":"message","content":[{"type":"output_text","text":"wrong"}]}]}}`)
	assert.ErrorIs(t, err, errResponsesPrefixMismatch)
}

func TestResponsesContinuationHistoryLimitAllowsCurrentAttemptToFinish(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	info := &relaycommon.RelayInfo{}
	s := beginResponsesContinuation(c, info)
	before := `{"type":"response.output_text.delta","delta":"a"}`
	_, _, err := s.reconcile(before)
	require.NoError(t, err)
	s = beginResponsesContinuation(c, info)
	_, emit, err := s.reconcile(before)
	require.NoError(t, err)
	assert.False(t, emit)
	_, emit, err = s.reconcile(`{"type":"response.output_text.delta","delta":"` + strings.Repeat("b", 1<<20) + `"}`)
	require.NoError(t, err)
	assert.True(t, emit)
	terminal := `{"type":"response.completed","response":{"id":"winner"}}`
	_, emit, err = s.reconcile(terminal)
	require.NoError(t, err)
	assert.True(t, emit)
	s = beginResponsesContinuation(c, info)
	_, emit, err = s.reconcile(before)
	assert.ErrorIs(t, err, errResponsesPrefixMismatch)
	assert.False(t, emit)
}
