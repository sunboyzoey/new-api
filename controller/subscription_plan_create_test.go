package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestCreateSubscriptionPlanPreservesRequestedStatus(t *testing.T) {
	for _, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var dialector gorm.Dialector
			switch engine {
			case "sqlite":
				dialector = sqlite.Open(filepath.Join(t.TempDir(), "plans.db"))
			case "mysql":
				dsn := os.Getenv("TEST_SUBSCRIPTION_CONTROLLER_MYSQL_DSN")
				if dsn == "" {
					t.Skip("isolated controller MySQL DSN not configured")
				}
				dialector = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_SUBSCRIPTION_CONTROLLER_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("isolated controller PostgreSQL DSN not configured")
				}
				dialector = postgres.Open(dsn)
			}
			db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			previous := model.DB
			model.DB = db
			t.Cleanup(func() { model.DB = previous; sqlDB, _ := db.DB(); _ = sqlDB.Close() })
			require.NoError(t, db.Migrator().DropTable(&model.SubscriptionPlan{}))
			require.NoError(t, db.AutoMigrate(&model.SubscriptionPlan{}))
			for _, enabled := range []bool{false, true} {
				t.Run(map[bool]string{false: "disabled draft stays disabled", true: "enabled plan stays enabled"}[enabled], func(t *testing.T) {
					plan := model.SubscriptionPlan{Title: "Public daily draft", PriceAmount: 0, TotalAmount: 250000, DurationUnit: "day", DurationValue: 1, Enabled: enabled, QuotaResetPeriod: "never", DailyClaimLimit: 100, ClaimCycle: "daily", MaxClaimsPerCycle: 1, BlockActiveClaim: true}
					body, err := common.Marshal(AdminUpsertSubscriptionPlanRequest{Plan: plan})
					require.NoError(t, err)
					router := gin.New()
					router.POST("/plans", AdminCreateSubscriptionPlan)
					response := httptest.NewRecorder()
					request := httptest.NewRequest(http.MethodPost, "/plans", bytes.NewReader(body))
					request.Header.Set("Content-Type", "application/json")
					router.ServeHTTP(response, request)
					require.Equal(t, http.StatusOK, response.Code)
					var result struct {
						Success bool                   `json:"success"`
						Data    model.SubscriptionPlan `json:"data"`
					}
					require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
					require.True(t, result.Success, response.Body.String())
					assert.Equal(t, enabled, result.Data.Enabled)
					var stored model.SubscriptionPlan
					require.NoError(t, db.First(&stored, result.Data.Id).Error)
					assert.Equal(t, enabled, stored.Enabled)
					assert.Equal(t, 100, stored.DailyClaimLimit)
					assert.Equal(t, 1, stored.MaxClaimsPerCycle)
				})
			}
		})
	}
}
