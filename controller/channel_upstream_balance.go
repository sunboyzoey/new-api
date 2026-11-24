package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
)

const upstreamBalanceInterval = 10 * time.Minute

// -1 denotes an unlimited key/subscription, never a monetary amount.
const unlimitedUpstreamBalance = -1

func channelBalanceSystem(channel *model.Channel) string {
	system := channel.GetSetting().BalanceSystem
	if system == "" {
		switch channel.Type {
		case constant.ChannelTypeNewAPI:
			return "newapi"
		case constant.ChannelTypeSub2API:
			return "sub2api"
		}
	}
	return system
}

// fetchUpstreamBalance uses only the channel API key. New API returns key quota;
// Sub2API returns the available wallet, key quota, or subscription headroom.
func fetchUpstreamBalance(channel *model.Channel) (float64, error) {
	base, err := url.Parse(strings.TrimSpace(channel.GetBaseURL()))
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return 0, errors.New("invalid upstream balance base URL")
	}
	base.Path = strings.TrimSuffix(strings.TrimRight(base.Path, "/"), "/v1")
	base.RawPath = ""
	key := strings.TrimSpace(channel.Key)
	if key == "" {
		return 0, errors.New("upstream balance API key is required")
	}
	client, err := service.GetHttpClientWithProxySettings(channel.GetSetting().Proxy, channel.GetSetting())
	if err != nil {
		return 0, errors.New("invalid upstream balance proxy")
	}
	// Do not mutate the shared proxy client or forward credentials on redirects.
	queryClient := *client
	queryClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	get := func(path string, result any) error {
		endpoint := *base
		endpoint.Path += path
		if path == "/v1/usage" {
			day := time.Now().UTC().Format("2006-01-02")
			endpoint.RawQuery = url.Values{"days": {"1"}, "start_date": {day}, "end_date": {day}}.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return errors.New("cannot create upstream balance request")
		}
		req.Header.Set("Authorization", "Bearer "+key)
		response, err := queryClient.Do(req)
		if err != nil {
			return errors.New("upstream balance request failed")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("upstream balance HTTP status: %d", response.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, (256<<10)+1))
		if err != nil || len(body) > 256<<10 {
			return errors.New("cannot read upstream balance response")
		}
		if err := common.Unmarshal(body, result); err != nil {
			return errors.New("invalid upstream balance JSON")
		}
		return nil
	}
	var balance float64
	switch channelBalanceSystem(channel) {
	case "newapi":
		var usage struct {
			Code bool `json:"code"`
			Data struct {
				Object    string   `json:"object"`
				Available *float64 `json:"total_available"`
				Unlimited *bool    `json:"unlimited_quota"`
			} `json:"data"`
		}
		if err := get("/api/usage/token/", &usage); err != nil {
			return 0, err
		}
		if !usage.Code || usage.Data.Object != "token_usage" || usage.Data.Unlimited == nil || usage.Data.Available == nil {
			return 0, errors.New("invalid New API token quota response")
		}
		if *usage.Data.Unlimited {
			return unlimitedUpstreamBalance, nil
		}
		var status struct {
			Success bool `json:"success"`
			Data    struct {
				QuotaPerUnit float64 `json:"quota_per_unit"`
			} `json:"data"`
		}
		if err := get("/api/status", &status); err != nil {
			return 0, err
		}
		if !status.Success || status.Data.QuotaPerUnit <= 0 || math.IsNaN(status.Data.QuotaPerUnit) || math.IsInf(status.Data.QuotaPerUnit, 0) {
			return 0, errors.New("invalid New API quota conversion rate")
		}
		balance = *usage.Data.Available / status.Data.QuotaPerUnit
	case "sub2api":
		var usage struct {
			Valid        *bool          `json:"isValid"`
			Mode         string         `json:"mode"`
			Remaining    *float64       `json:"remaining"`
			Unit         string         `json:"unit"`
			Subscription map[string]any `json:"subscription"`
			RateLimits   []struct {
				Limit     *float64 `json:"limit"`
				Remaining *float64 `json:"remaining"`
			} `json:"rate_limits"`
		}
		if err := get("/v1/usage", &usage); err != nil {
			return 0, err
		}
		if usage.Valid == nil || !*usage.Valid || (usage.Unit != "USD" && !(usage.Mode == "quota_limited" && usage.Unit == "" && usage.Remaining == nil)) {
			return 0, errors.New("invalid Sub2API available quota response")
		}
		remaining := usage.Remaining
		// Rate-only keys omit top-level remaining/unit. Sub2API expresses these
		// windows in USD; the tightest window limits the currently usable quota.
		if usage.Mode == "quota_limited" {
			for _, window := range usage.RateLimits {
				if window.Limit == nil || *window.Limit <= 0 || window.Remaining == nil {
					return 0, errors.New("invalid Sub2API quota window")
				}
				if remaining == nil || *window.Remaining < *remaining {
					remaining = window.Remaining
				}
			}
		}
		if remaining == nil {
			return 0, errors.New("missing Sub2API available quota")
		}
		balance = *remaining
		if balance == unlimitedUpstreamBalance && usage.Subscription != nil && usage.Mode != "quota_limited" {
			return balance, nil
		}
	default:
		return 0, errors.New("unsupported upstream balance system")
	}
	if math.IsNaN(balance) || math.IsInf(balance, 0) {
		return 0, errors.New("invalid upstream balance amount")
	}
	// An overdrawn wallet/key has no available quota.
	return max(0, balance), nil
}

// Refresh only explicitly selected upstream systems (and native New API
// channels). Query failures preserve the last successful balance and timestamp.
func refreshUpstreamBalances() {
	channels, err := model.GetAllChannels(0, 0, true, false)
	if err != nil {
		common.SysError("failed to load channels for upstream balance refresh")
		return
	}
	var workers sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, channel := range channels {
		if channelBalanceSystem(channel) == "" || channel.Status != common.ChannelStatusEnabled || channel.ChannelInfo.IsMultiKey || channel.Type == constant.ChannelTypeTaskPlugin {
			continue
		}
		slots <- struct{}{}
		workers.Go(func() {
			defer func() { <-slots }()
			if _, err := updateChannelBalance(channel); err != nil {
				common.SysError(fmt.Sprintf("upstream balance refresh failed: channel_id=%d: %s", channel.Id, err.Error()))
			}
		})
	}
	workers.Wait()
}

func AutomaticallyUpdateUpstreamBalances() {
	if !common.IsMasterNode {
		return
	}
	ticker := time.NewTicker(upstreamBalanceInterval)
	defer ticker.Stop()
	for range ticker.C {
		refreshUpstreamBalances()
	}
}
