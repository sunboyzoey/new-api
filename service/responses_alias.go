package service

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/pkg/cachex"
	"github.com/gin-gonic/gin"
	"github.com/samber/hot"
)

const EarlyResponseIDPrefix = "resp_newapi_ack_"

var responseAliasesOnce sync.Once
var responseAliases *cachex.HybridCache[string]

// Response aliases are scoped to the requesting user and originating channel.
// Redis survives process restarts; without Redis the bounded cache is local.
func responsesAliasCache() *cachex.HybridCache[string] {
	responseAliasesOnce.Do(func() {
		responseAliases = cachex.NewHybridCache(cachex.HybridCacheConfig[string]{
			Namespace: cachex.Namespace("responses_early_ack"), Redis: common.RDB,
			RedisEnabled: func() bool { return common.RedisEnabled && common.RDB != nil },
			RedisCodec:   cachex.StringCodec{},
			Memory: func() *hot.HotCache[string, string] {
				return hot.NewHotCache[string, string](hot.LRU, 10000).WithTTL(30 * 24 * time.Hour).WithJanitor().Build()
			},
		})
	})
	return responseAliases
}

func SaveEarlyResponseAlias(userID, channelID int, alias, upstreamID string) error {
	if upstreamID == "" || len(upstreamID) > 512 || !strings.HasPrefix(alias, EarlyResponseIDPrefix) {
		return fmt.Errorf("invalid early response identity")
	}
	return responsesAliasCache().SetWithTTL(fmt.Sprintf("%d:%d:%s", userID, channelID, alias), upstreamID, 30*24*time.Hour)
}

// SaveFallbackResponseOwner records an identity only on a successful
// fallback or its continuations. The public ID and channel-scoped upstream
// identity resolve together even when other turns share a prompt cache.
func SaveFallbackResponseOwner(c *gin.Context, channelID int, publicID, upstreamID string) error {
	if channelID <= 0 || upstreamID == "" || len(upstreamID) > 512 {
		return nil
	}
	fallback := common.GetContextKeyBool(c, constant.ContextKeyResponsesChannelFailover) && len(c.GetStringSlice("use_channel")) > 1
	if !fallback && common.GetContextKeyInt(c, constant.ContextKeyResponsesOwnerChannel) != channelID {
		return nil
	}
	key := fmt.Sprintf("owner:%d:%s", common.GetContextKeyInt(c, constant.ContextKeyUserId), publicID)
	return responsesAliasCache().SetWithTTL(key, fmt.Sprintf("%d:%s", channelID, upstreamID), 30*24*time.Hour)
}

// RecordFallbackResponseOwner preserves a provider's own ID when a fallback
// or a later turn on its channel completes without a gateway acknowledgment.
func RecordFallbackResponseOwner(c *gin.Context, channelID int, upstreamID string) {
	if err := SaveFallbackResponseOwner(c, channelID, upstreamID, upstreamID); err != nil {
		logger.LogWarn(c, "could not save fallback Responses channel ownership")
	}
}

func responseOwner(userID int, id string) (channelID int, upstreamID string, err error) {
	if id == "" || len(id) > 512 {
		return 0, "", nil
	}
	record, found, err := responsesAliasCache().Get(fmt.Sprintf("owner:%d:%s", userID, id))
	if err != nil {
		return 0, "", fmt.Errorf("response owner lookup unavailable")
	}
	if !found {
		return 0, "", nil
	}
	channel, upstreamID, _ := strings.Cut(record, ":")
	channelID, err = strconv.Atoi(channel)
	if err != nil || channelID <= 0 || upstreamID == "" {
		return 0, "", fmt.Errorf("response owner unavailable")
	}
	return channelID, upstreamID, nil
}

func PreviousResponseOwnerChannel(userID int, id string) (int, error) {
	channelID, _, err := responseOwner(userID, id)
	return channelID, err
}

func ResolveEarlyResponseAlias(userID, channelID int, id string) (string, error) {
	if !strings.HasPrefix(id, EarlyResponseIDPrefix) {
		return id, nil
	}
	owner, upstreamID, err := responseOwner(userID, id)
	if err != nil {
		return "", err
	}
	if owner > 0 && owner == channelID {
		return upstreamID, nil
	}
	if owner > 0 {
		return "", fmt.Errorf("previous_response_id alias belongs to another channel; resend conversation input")
	}
	// Aliases created before owner routing used the selected channel in the key.
	resolved, found, err := responsesAliasCache().Get(fmt.Sprintf("%d:%d:%s", userID, channelID, id))
	if err != nil {
		return "", fmt.Errorf("response alias lookup unavailable")
	}
	if !found {
		return "", fmt.Errorf("previous_response_id alias expired, unavailable, or belongs to another user/channel; resend conversation input")
	}
	return resolved, nil
}
