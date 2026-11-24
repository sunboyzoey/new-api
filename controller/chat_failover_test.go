package controller

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatHTTPRetryAfterFirstText(t *testing.T) {
	for _, mode := range []string{"native", "acknowledged", "via responses", "large passthrough", "prefix mismatch", "client cancel"} {
		t.Run(mode, func(t *testing.T) {
			if runResponsesAliasFixtureInChild(t) {
				return
			}
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(*websocket.Conn, *http.Request) {})
			oldRetries, oldLogs := common.RetryTimes, constant.ErrorLogEnabled
			common.RetryTimes, constant.ErrorLogEnabled = 1, true
			t.Cleanup(func() { common.RetryTimes, constant.ErrorLogEnabled = oldRetries, oldLogs })
			if mode == "acknowledged" {
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
					"general_setting.responses_early_ack_enabled":  "true",
					"general_setting.responses_early_ack_delay_ms": "1",
				}))
			}
			primary := &model.Channel{
				Name: "chat-primary", Key: "primary-fixture", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeOpenAI,
				Group: "default", Models: "ws-billing", BaseURL: fixture.channel.BaseURL, Priority: common.GetPointer(int64(5)),
			}
			if mode == "via responses" {
				primary.Type = constant.ChannelTypeAdvancedCustom
				primary.SetOtherSettings(dto.ChannelOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{
					IncomingPath: "/v1/chat/completions", UpstreamPath: "/v1/responses", Converter: relayconvert.ConverterOpenAIChatToOpenAIResponses,
				}}}})
			}
			if mode == "large passthrough" {
				primary.SetSetting(dto.ChannelSettings{PassThroughBodyEnabled: true})
			}
			require.NoError(t, primary.Insert())
			t.Cleanup(func() {
				require.NoError(t, model.DB.Where("channel_id = ?", primary.Id).Delete(&model.Ability{}).Error)
				require.NoError(t, model.DB.Delete(primary).Error)
			})

			var attempts atomic.Int32
			release := make(chan struct{})
			allowFirstText := make(chan struct{})
			releasePrimary := sync.OnceFunc(func() { close(release) })
			defer releasePrimary()
			fixture.httpUpstream = func(w http.ResponseWriter, r *http.Request) {
				number := attempts.Add(1)
				isPrimary := r.Header.Get("Authorization") == "Bearer primary-fixture"
				assert.Equal(t, number == 1, isPrimary)
				if isPrimary && mode == "large passthrough" {
					n, err := io.Copy(io.Discard, r.Body)
					assert.NoError(t, err)
					assert.Greater(t, n, int64(8<<20))
				}
				w.Header().Set("Content-Type", "text/event-stream")
				sendText := func(id, text string) {
					_, _ = fmt.Fprintf(w, "data: "+`{"id":"%s","created":1,"object":"chat.completion.chunk","model":"ws-billing","choices":[{"index":0,"delta":{"content":"%s"},"finish_reason":null}]}`+"\n\n", id, text)
				}
				if isPrimary {
					if mode == "acknowledged" {
						select {
						case <-allowFirstText:
						case <-r.Context().Done():
							return
						}
					}
					if mode == "via responses" {
						assert.Equal(t, "/v1/responses", r.URL.Path)
						_, _ = io.WriteString(w, "data: "+`{"type":"response.created","response":{"id":"resp_primary","status":"in_progress","model":"ws-billing","output":[]}}`+"\n\n")
						_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","item_id":"msg_primary","output_index":0,"content_index":0,"delta":"visible prefix"}`+"\n\n")
					} else {
						sendText("chat-primary-id", "visible prefix")
					}
					w.(http.Flusher).Flush()
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					// EOF without a terminal must reach the router.
					return
				}
				assert.Equal(t, "/v1/chat/completions", r.URL.Path)
				if mode == "prefix mismatch" {
					sendText("chat-alternate-id", "conflicting text")
				} else {
					sendText("chat-alternate-id", "visible ")
					sendText("chat-alternate-id", "prefix plus suffix")
				}
				_, _ = io.WriteString(w, "data: "+`{"id":"chat-alternate-id","created":2,"object":"chat.completion.chunk","model":"ws-billing","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`+"\n\ndata: [DONE]\n\n")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			payload := `{"model":"ws-billing","stream":true,"messages":[{"role":"user","content":"hello"}]}`
			if mode == "large passthrough" {
				payload = strings.TrimSuffix(payload, "}") + `,"padding":"` + strings.Repeat("x", 8<<20) + `"}`
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.gatewayURL+"/v1/chat/completions", strings.NewReader(payload))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			start := time.Now()
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			require.Equal(t, http.StatusOK, response.StatusCode)
			if mode == "acknowledged" {
				require.Equal(t, "chat.completion.chunk", response.Header.Get("X-Newapi-Early-Ack"))
				close(allowFirstText)
			}
			reader := bufio.NewReader(response.Body)
			var body strings.Builder
			for !strings.Contains(body.String(), "visible prefix") {
				line, err := reader.ReadString('\n')
				require.NoError(t, err)
				body.WriteString(line)
			}
			firstText := time.Since(start)
			t.Logf("client received first answer text in %s", firstText)
			require.Less(t, firstText, time.Second)
			if mode == "client cancel" {
				cancel()
				require.NoError(t, response.Body.Close())
			} else {
				releasePrimary()
				_, err = io.Copy(&body, reader)
				require.NoError(t, err)
			}
			select {
			case <-fixture.httpDone:
			case <-time.After(5 * time.Second):
				t.Fatal("chat relay did not finish")
			}
			if mode == "client cancel" {
				assert.Equal(t, int32(1), attempts.Load())
				return
			}
			assert.Equal(t, int32(2), attempts.Load())
			out := body.String()
			assert.Equal(t, 1, strings.Count(out, "visible prefix"))
			assert.NotContains(t, out, "chat-alternate-id")
			assert.NotContains(t, out, "conflicting text")
			if mode == "prefix mismatch" {
				assert.Equal(t, 1, strings.Count(out, `"error":`))
				assert.NotContains(t, out, "[DONE]")
				return
			}
			assert.Contains(t, out, "plus suffix")
			assert.NotContains(t, out, `"error":`)
			assert.Equal(t, 1, strings.Count(out, "[DONE]"))
			assertResponsesWSAccounting(t, fixture, []int{1000})
			var logged model.Log
			require.NoError(t, model.LOG_DB.Where("token_id = ? AND type = ?", fixture.token.Id, model.LogTypeConsume).First(&logged).Error)
			assert.Equal(t, fixture.channel.Id, logged.ChannelId)
			assert.Equal(t, 7, logged.PromptTokens)
			assert.Equal(t, 3, logged.CompletionTokens)
		})
	}
}
