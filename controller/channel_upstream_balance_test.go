package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestUpstreamBalanceQueries(t *testing.T) {
	service.InitHttpClient()
	for _, tc := range []struct {
		name, system, body, status string
		want                       float64
		fail                       bool
	}{
		{name: "New API key quota uses upstream conversion", system: "newapi", body: `{"code":true,"data":{"object":"token_usage","total_available":2500000,"unlimited_quota":false}}`, status: `{"success":true,"data":{"quota_per_unit":1000000}}`, want: 2.5},
		{name: "New API unlimited", system: "newapi", body: `{"code":true,"data":{"object":"token_usage","total_available":0,"unlimited_quota":true}}`, want: -1},
		{name: "New API rejects error envelope", system: "newapi", body: `{"success":false,"message":"test-secret"}`, fail: true},
		{name: "New API rejects missing quota", system: "newapi", body: `{"code":true,"data":{"object":"token_usage","unlimited_quota":false}}`, fail: true},
		{name: "New API rejects zero conversion", system: "newapi", body: `{"code":true,"data":{"object":"token_usage","total_available":25,"unlimited_quota":false}}`, status: `{"success":true,"data":{"quota_per_unit":0}}`, fail: true},
		{name: "Sub2API wallet", system: "sub2api", body: `{"isValid":true,"remaining":12.5,"balance":12.5,"unit":"USD"}`, want: 12.5},
		{name: "Sub2API limited key", system: "sub2api", body: `{"isValid":true,"mode":"quota_limited","remaining":7,"unit":"USD","quota":{"remaining":7}}`, want: 7},
		{name: "Sub2API rate-only key", system: "sub2api", body: `{"isValid":true,"mode":"quota_limited","rate_limits":[{"limit":10,"remaining":3},{"limit":50,"remaining":8}]}`, want: 3},
		{name: "Sub2API tightest quota window", system: "sub2api", body: `{"isValid":true,"mode":"quota_limited","remaining":20,"unit":"USD","rate_limits":[{"limit":10,"remaining":2}]}`, want: 2},
		{name: "Sub2API missing quota window amount", system: "sub2api", body: `{"isValid":true,"mode":"quota_limited","rate_limits":[{"limit":10}]}`, fail: true},
		{name: "Sub2API subscription", system: "sub2api", body: `{"isValid":true,"remaining":3,"unit":"USD","subscription":{"daily_limit_usd":10}}`, want: 3},
		{name: "Sub2API unlimited subscription", system: "sub2api", body: `{"isValid":true,"remaining":-1,"unit":"USD","subscription":{}}`, want: -1},
		{name: "Sub2API depleted wallet", system: "sub2api", body: `{"isValid":true,"remaining":-1,"unit":"USD","balance":-1}`, want: 0},
		{name: "Sub2API missing amount", system: "sub2api", body: `{"isValid":true,"unit":"USD"}`, fail: true},
		{name: "Sub2API invalid key", system: "sub2api", body: `{"isValid":false,"remaining":5,"unit":"USD"}`, fail: true},
		{name: "Sub2API wrong currency", system: "sub2api", body: `{"isValid":true,"remaining":5,"unit":"CNY"}`, fail: true},
		{name: "Sub2API rejects arbitrary JSON", system: "sub2api", body: `{}`, fail: true},
		{name: "Sub2API rejects overflow", system: "sub2api", body: `{"isValid":true,"remaining":1e999,"unit":"USD"}`, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				assert.Equal(t, "Bearer test-secret", r.Header.Get("Authorization"))
				assert.Equal(t, http.MethodGet, r.Method)
				switch r.URL.Path {
				case "/tenant/api/usage/token/":
					assert.Equal(t, "newapi", tc.system)
					_, _ = w.Write([]byte(tc.body))
				case "/tenant/api/status":
					_, _ = w.Write([]byte(tc.status))
				case "/tenant/v1/usage":
					assert.Equal(t, "sub2api", tc.system)
					assert.Equal(t, "1", r.URL.Query().Get("days"))
					assert.Equal(t, r.URL.Query().Get("start_date"), r.URL.Query().Get("end_date"))
					_, _ = w.Write([]byte(tc.body))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			base := server.URL + "/tenant/v1/"
			setting := fmt.Sprintf(`{"balance_system":%q}`, tc.system)
			channel := &model.Channel{Type: constant.ChannelTypeOpenAI, BaseURL: &base, Key: "test-secret", Setting: &setting}
			balance, err := fetchUpstreamBalance(channel)
			if tc.fail {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "test-secret")
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, balance)
			}
			if tc.want == -1 {
				assert.Equal(t, 1, requests)
			}
		})
	}
}

func TestUpstreamBalanceRejectsRedirectsAndMalformedResponses(t *testing.T) {
	service.InitHttpClient()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("credentials must not follow a redirect") }))
	defer target.Close()
	for _, code := range []int{http.StatusFound, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusOK} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(code)
				_, _ = w.Write([]byte(strings.Repeat("x", (256<<10)+1)))
			}))
			defer server.Close()
			setting := `{"balance_system":"sub2api"}`
			_, err := fetchUpstreamBalance(&model.Channel{BaseURL: &server.URL, Key: "secret", Setting: &setting})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "secret")
		})
	}
	for _, base := range []string{"", "file:///etc/passwd", "https://user:secret@example.com", "https://example.com?key=secret"} {
		_, err := fetchUpstreamBalance(&model.Channel{Type: 60, BaseURL: &base, Key: "secret"})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "secret")
	}
}

func TestUpstreamBalanceRefreshPersistence(t *testing.T) {
	service.InitHttpClient()
	for _, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var driver gorm.Dialector
			switch engine {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "balance.db"))
			case "mysql":
				dsn := os.Getenv("TEST_BALANCE_MYSQL_DSN")
				if dsn == "" {
					t.Skip("isolated MySQL DSN not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_BALANCE_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("isolated PostgreSQL DSN not configured")
				}
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			old := model.DB
			model.DB = db
			t.Cleanup(func() { model.DB = old; sqlDB, _ := db.DB(); _ = sqlDB.Close() })
			require.NoError(t, db.AutoMigrate(&model.Channel{}))
			requests := 0
			fail := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if fail {
					w.WriteHeader(502)
					return
				}
				_, _ = w.Write([]byte(`{"isValid":true,"remaining":0,"unit":"USD"}`))
			}))
			defer server.Close()
			setting := `{"balance_system":"sub2api","proxy":""}`
			configured := model.Channel{Type: 1, Status: common.ChannelStatusEnabled, BaseURL: &server.URL, Key: "fake-key", Setting: &setting, Balance: 42, BalanceUpdatedTime: 10}
			require.NoError(t, configured.ValidateSettings())
			require.NoError(t, db.Create(&configured).Error)
			t.Cleanup(func() { db.Delete(&model.Channel{}, configured.Id) })
			// Disabled, unconfigured, and multi-key channels are not queried.
			for i := range 3 {
				other := configured
				other.Id = 0
				switch i {
				case 0:
					other.Status = common.ChannelStatusManuallyDisabled
				case 1:
					other.Setting = nil
				case 2:
					other.ChannelInfo.IsMultiKey = true
				}
				require.NoError(t, db.Create(&other).Error)
				t.Cleanup(func() { db.Delete(&model.Channel{}, other.Id) })
			}
			refreshUpstreamBalances()
			assert.Equal(t, 1, requests)
			var saved model.Channel
			require.NoError(t, db.First(&saved, configured.Id).Error)
			assert.Equal(t, "sub2api", saved.GetSetting().BalanceSystem)
			assert.Equal(t, 0.0, saved.Balance)
			assert.Equal(t, common.ChannelStatusEnabled, saved.Status)
			assert.Greater(t, saved.BalanceUpdatedTime, int64(10))
			timestamp := saved.BalanceUpdatedTime
			fail = true
			refreshUpstreamBalances()
			require.NoError(t, db.First(&saved, configured.Id).Error)
			assert.Equal(t, 0.0, saved.Balance)
			assert.Equal(t, timestamp, saved.BalanceUpdatedTime)
			assert.Equal(t, 2, requests)
		})
	}
	invalid := `{"balance_system":"unsupported"}`
	require.Error(t, (&model.Channel{Setting: &invalid}).ValidateSettings())
	assert.Equal(t, "newapi", channelBalanceSystem(&model.Channel{Type: 60}))
	assert.Equal(t, "sub2api", channelBalanceSystem(&model.Channel{Type: 59}))
}
