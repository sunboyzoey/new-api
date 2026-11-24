package model

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestFreeSubscriptionClaimDatabaseMatrix(t *testing.T) {
	for _, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var dialector gorm.Dialector
			kind := common.DatabaseTypeSQLite
			switch engine {
			case "sqlite":
				dialector = sqlite.Open(filepath.Join(t.TempDir(), "claims.db") + "?_pragma=busy_timeout(20000)&_pragma=journal_mode(WAL)")
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN not configured")
				}
				dialector = mysql.Open(dsn)
				kind = common.DatabaseTypeMySQL
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN not configured")
				}
				dialector = postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
				kind = common.DatabaseTypePostgreSQL
			}
			db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			versionQuery := "SELECT version()"
			if engine == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("Database engine %s: %s", engine, version)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(16)
			previousDB, previousLog := DB, LOG_DB
			oldMain, oldLog := common.MainDatabaseType(), common.LogDatabaseType()
			DB, LOG_DB = db, db
			common.SetDatabaseTypes(kind, kind)
			initCol()
			t.Cleanup(func() {
				DB, LOG_DB = previousDB, previousLog
				common.SetDatabaseTypes(oldMain, oldLog)
				initCol()
				_ = sqlDB.Close()
			})
			models := []any{&SubscriptionClaim{}, &SubscriptionPreConsumeRecord{}, &UserSubscription{}, &SubscriptionOrder{}, &SubscriptionPlan{}, &User{}, &Option{}, &Log{}}
			require.NoError(t, db.Migrator().DropTable(models...))
			for range 2 {
				require.NoError(t, db.AutoMigrate(models...))
			}
			newUser := func(t *testing.T, name string) User {
				u := User{Username: name, AffCode: name, Password: "unused", Status: common.UserStatusEnabled, Group: "default", Quota: 0}
				require.NoError(t, db.Create(&u).Error)
				return u
			}
			newPlan := func(t *testing.T, title string) SubscriptionPlan {
				p := SubscriptionPlan{Title: title, Enabled: true, DurationUnit: SubscriptionDurationDay, DurationValue: 1, TotalAmount: 100, ClaimCycle: SubscriptionResetDaily, QuotaResetPeriod: SubscriptionResetNever, AllowBalancePay: common.GetPointer(false), AllowWalletOverflow: common.GetPointer(false)}
				require.NoError(t, db.Create(&p).Error)
				return p
			}

			t.Run("public rewards cannot be claimed as ordinary plans", func(t *testing.T) {
				u := newUser(t, "public-reward-recipient")
				require.NoError(t, db.AutoMigrate(&Checkin{}))
				require.NoError(t, GrantPublicRewardSubscription(u.Id, 5000000, PublicRewardRegistration))
				var registration SubscriptionPlan
				require.NoError(t, db.Where("reward_type = ?", PublicRewardRegistration).First(&registration).Error)
				require.NoError(t, db.Model(&registration).Update("claim_cycle", SubscriptionResetDaily).Error)
				availability, err := GetSubscriptionClaimAvailability(db, u.Id, &registration, GetDBTimestamp())
				require.NoError(t, err)
				assert.False(t, availability.CanClaim)
				_, err = ClaimFreeSubscription(u.Id, registration.Id, "public-reward-direct-claim")
				require.ErrorContains(t, err, "Free plan is unavailable")
				original := *operation_setting.GetCheckinSetting()
				*operation_setting.GetCheckinSetting() = operation_setting.CheckinSetting{Enabled: true, MinQuota: 2500000, MaxQuota: 2500000}
				defer func() { *operation_setting.GetCheckinSetting() = original }()
				reward, err := UserCheckin(u.Id)
				require.NoError(t, err)
				assert.Equal(t, 2500000, reward.QuotaAwarded)
				_, err = UserCheckin(u.Id)
				require.Error(t, err)
				var subs []UserSubscription
				require.NoError(t, db.Where("user_id = ?", u.Id).Order("id").Find(&subs).Error)
				require.Len(t, subs, 2)
				assert.EqualValues(t, 5000000, subs[0].AmountTotal)
				assert.EqualValues(t, 2500000, subs[1].AmountTotal)
				for _, sub := range subs {
					assert.Equal(t, "public", sub.UsageGroup)
					assert.Zero(t, sub.NextResetTime)
					assert.False(t, sub.AllowWalletOverflow)
				}
				var fresh User
				require.NoError(t, db.First(&fresh, u.Id).Error)
				assert.Zero(t, fresh.Quota)
			})

			t.Run("public quota stays scoped without affecting paid subscriptions or membership", func(t *testing.T) {
				u := newUser(t, "scope-user")
				p := newPlan(t, "public-scoped")
				p.UsageGroup = "public"
				require.NoError(t, db.Save(&p).Error)
				claimed, err := ClaimFreeSubscription(u.Id, p.Id, "public-scope-claim-key")
				require.NoError(t, err)
				assert.Equal(t, "public", claimed.Subscription.UsageGroup)
				var userAfter User
				require.NoError(t, db.First(&userAfter, u.Id).Error)
				assert.Equal(t, "default", userAfter.Group)
				has, err := HasActiveUserSubscription(u.Id, "GPT_Pro")
				require.NoError(t, err)
				assert.False(t, has)
				overflow, err := UserActiveSubscriptionsAllowWalletOverflow(u.Id, "GPT_Pro")
				require.NoError(t, err)
				assert.True(t, overflow)
				overflow, err = UserActiveSubscriptionsAllowWalletOverflow(u.Id, "public")
				require.NoError(t, err)
				assert.False(t, overflow)
				_, err = PreConsumeUserSubscription("scope-denied", u.Id, "test", 0, 1, "GPT_Pro")
				assert.Error(t, err)
				used, err := PreConsumeUserSubscription("scope-allowed", u.Id, "test", 0, 25, "public")
				require.NoError(t, err)
				assert.Equal(t, claimed.Subscription.Id, used.UserSubscriptionId)
				_, err = PreConsumeUserSubscription("scope-allowed", u.Id, "test", 0, 25, "GPT_Pro")
				assert.Error(t, err)
				require.NoError(t, db.Model(&p).Update("usage_group", "GPT_Pro").Error)
				used, err = PreConsumeUserSubscription("scope-snapshot", u.Id, "test", 0, 75, "public")
				require.NoError(t, err)
				assert.Equal(t, claimed.Subscription.Id, used.UserSubscriptionId)
				_, err = PreConsumeUserSubscription("scope-exhausted", u.Id, "test", 0, 1, "public")
				assert.ErrorContains(t, err, "insufficient")
				paid := newPlan(t, "scope-paid")
				paid.PriceAmount = 1
				paid.DurationValue = 2
				paid.AllowWalletOverflow = common.GetPointer(true)
				require.NoError(t, db.Save(&paid).Error)
				var paidSub *UserSubscription
				require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
					var err error
					paidSub, err = CreateUserSubscriptionFromPlanTx(tx, u.Id, &paid, "order")
					return err
				}))
				used, err = PreConsumeUserSubscription("scope-paid-gpt", u.Id, "test", 0, 10, "GPT_Pro")
				require.NoError(t, err)
				assert.Equal(t, paidSub.Id, used.UserSubscriptionId)
				require.NoError(t, db.Model(claimed.Subscription).Update("end_time", GetDBTimestamp()-1).Error)
				used, err = PreConsumeUserSubscription("scope-after-expiry", u.Id, "test", 0, 10, "public")
				require.NoError(t, err)
				assert.Equal(t, paidSub.Id, used.UserSubscriptionId)
			})
			t.Run("zero wallet and idempotent retry", func(t *testing.T) {
				u := newUser(t, "zero-wallet")
				p := newPlan(t, "free")
				first, err := ClaimFreeSubscription(u.Id, p.Id, "zero-wallet-request-0001")
				require.NoError(t, err)
				retry, err := ClaimFreeSubscription(u.Id, p.Id, "zero-wallet-request-0001")
				require.NoError(t, err)
				assert.True(t, retry.Replayed)
				assert.Equal(t, first.Subscription.Id, retry.Subscription.Id)
				var persisted User
				require.NoError(t, db.First(&persisted, u.Id).Error)
				assert.Zero(t, persisted.Quota)
				assert.Equal(t, "free_claim", first.Subscription.Source)
				assert.EqualValues(t, 100, first.Subscription.AmountTotal)
				var count int64
				require.NoError(t, db.Model(&SubscriptionClaim{}).Where("plan_id = ?", p.Id).Count(&count).Error)
				assert.EqualValues(t, 1, count)
				err = PurchaseSubscriptionWithBalance(u.Id, p.Id)
				assert.ErrorContains(t, err, "claim endpoint")
			})
			t.Run("request keys cannot create subscriptions for another plan", func(t *testing.T) {
				u := newUser(t, "key-owner")
				first := newPlan(t, "key-first")
				second := newPlan(t, "key-second")
				_, err := ClaimFreeSubscription(u.Id, first.Id, "same-request-key-across-plans")
				require.NoError(t, err)
				_, err = ClaimFreeSubscription(u.Id, second.Id, "same-request-key-across-plans")
				assert.ErrorContains(t, err, "another plan")
				var count int64
				require.NoError(t, db.Model(&UserSubscription{}).Where("plan_id = ?", second.Id).Count(&count).Error)
				assert.Zero(t, count)
			})
			t.Run("same request concurrent replay", func(t *testing.T) {
				u := newUser(t, "concurrent-retry")
				p := newPlan(t, "retry")
				var wg sync.WaitGroup
				ids := make(chan int, 12)
				failures := make(chan error, 12)
				for range 12 {
					wg.Go(func() {
						result, err := ClaimFreeSubscription(u.Id, p.Id, "concurrent-retry-request")
						if err != nil {
							failures <- err
							return
						}
						ids <- result.Subscription.Id
					})
				}
				wg.Wait()
				close(ids)
				close(failures)
				for err := range failures {
					assert.NoError(t, err)
				}
				unique := map[int]bool{}
				for id := range ids {
					unique[id] = true
				}
				assert.Len(t, unique, 1)
				var count int64
				require.NoError(t, db.Model(&UserSubscription{}).Where("plan_id = ?", p.Id).Count(&count).Error)
				assert.EqualValues(t, 1, count)
			})
			t.Run("daily inventory and unique users under contention", func(t *testing.T) {
				p := newPlan(t, "daily-stock")
				require.NoError(t, db.Model(&p).Update("daily_claim_limit", 3).Error)
				users := make([]User, 12)
				for i := range users {
					users[i] = newUser(t, fmt.Sprintf("stock-%d", i))
				}
				var wg sync.WaitGroup
				success := make(chan int, 12)
				for i := range users {
					wg.Go(func() {
						result, err := ClaimFreeSubscription(users[i].Id, p.Id, fmt.Sprintf("stock-request-%04d", i))
						if err == nil {
							success <- result.Subscription.Id
						}
					})
				}
				wg.Wait()
				close(success)
				assert.Len(t, success, 3)
				var claims, subs int64
				require.NoError(t, db.Model(&SubscriptionClaim{}).Where("plan_id = ?", p.Id).Count(&claims).Error)
				require.NoError(t, db.Model(&UserSubscription{}).Where("plan_id = ?", p.Id).Count(&subs).Error)
				assert.EqualValues(t, 3, claims)
				assert.Equal(t, claims, subs)
			})
			t.Run("active subscriptions prevent concurrent distinct requests", func(t *testing.T) {
				u := newUser(t, "active-lock")
				p := newPlan(t, "active-block")
				require.NoError(t, db.Model(&p).Update("block_active_claim", true).Error)
				var wg sync.WaitGroup
				success := make(chan int, 8)
				for i := range 8 {
					wg.Go(func() {
						r, err := ClaimFreeSubscription(u.Id, p.Id, fmt.Sprintf("active-request-%04d", i))
						if err == nil {
							success <- r.Subscription.Id
						}
					})
				}
				wg.Wait()
				close(success)
				assert.Len(t, success, 1)
			})
			t.Run("separate cycle and lifetime limits with Shanghai midnight", func(t *testing.T) {
				u := newUser(t, "cycle-limits")
				p := newPlan(t, "cycle")
				p.DailyClaimLimit = 1
				p.MaxPurchasePerUser = 2
				p.MaxClaimsPerCycle = 1
				require.NoError(t, db.Save(&p).Error)
				before := time.Date(2026, 10, 8, 15, 59, 59, 0, time.UTC).Unix()
				after := before + 1
				r, err := claimFreeSubscription(u.Id, p.Id, "cycle-request-day-one", func() int64 { return before })
				require.NoError(t, err)
				_, err = claimFreeSubscription(u.Id, p.Id, "cycle-request-same-day", func() int64 { return before })
				assert.ErrorContains(t, err, "cycle")
				a, err := GetSubscriptionClaimAvailability(db, u.Id, &p, after)
				require.NoError(t, err)
				assert.EqualValues(t, 1, *a.DailyRemaining)
				assert.Zero(t, a.CycleCount)
				assert.EqualValues(t, 1, a.LifetimeCount)
				_, err = claimFreeSubscription(u.Id, p.Id, "cycle-request-day-two", func() int64 { return after })
				require.NoError(t, err)
				require.NoError(t, db.Delete(&UserSubscription{}, r.Subscription.Id).Error)
				_, err = claimFreeSubscription(u.Id, p.Id, "cycle-request-day-three", func() int64 { return after + 86400 })
				assert.ErrorContains(t, err, "Lifetime")
			})
			t.Run("quota depletion expiry and disabled plans", func(t *testing.T) {
				u := newUser(t, "quota-expiry")
				p := newPlan(t, "quota")
				r, err := ClaimFreeSubscription(u.Id, p.Id, "quota-expiry-request-01")
				require.NoError(t, err)
				require.NoError(t, db.Model(&p).Update("enabled", false).Error)
				_, err = ClaimFreeSubscription(u.Id, p.Id, "disabled-plan-request-01")
				assert.ErrorContains(t, err, "unavailable")
				_, err = PreConsumeUserSubscription("consume-free-all", u.Id, "test-model", 0, 100)
				require.NoError(t, err)
				_, err = PreConsumeUserSubscription("consume-free-empty", u.Id, "test-model", 0, 1)
				assert.ErrorContains(t, err, "insufficient")
				require.NoError(t, db.Model(r.Subscription).Updates(map[string]any{"amount_used": 0, "end_time": GetDBTimestamp() - 1}).Error)
				_, err = PreConsumeUserSubscription("consume-free-expired", u.Id, "test-model", 0, 1)
				assert.ErrorContains(t, err, "no active")
			})
			t.Run("paid subscriptions preserve balance charging", func(t *testing.T) {
				u := newUser(t, "paid")
				p := newPlan(t, "paid-plan")
				p.PriceAmount = 1
				p.AllowBalancePay = common.GetPointer(true)
				require.NoError(t, db.Save(&p).Error)
				_, err := ClaimFreeSubscription(u.Id, p.Id, "paid-cannot-free-claim")
				assert.ErrorContains(t, err, "unavailable")
				assert.ErrorContains(t, PurchaseSubscriptionWithBalance(u.Id, p.Id), "余额不足")
				required, err := calcSubscriptionBalanceQuota(1)
				require.NoError(t, err)
				require.NoError(t, db.Model(&u).Update("quota", required+10).Error)
				require.NoError(t, PurchaseSubscriptionWithBalance(u.Id, p.Id))
				var paid User
				require.NoError(t, db.First(&paid, u.Id).Error)
				assert.Equal(t, 10, paid.Quota)
				var order SubscriptionOrder
				require.NoError(t, db.Where("user_id = ?", u.Id).First(&order).Error)
				assert.Equal(t, PaymentProviderBalance, order.PaymentProvider)
				assert.EqualValues(t, 1, order.Money)
			})
			t.Run("claim ledger failure rolls back subscription and stock", func(t *testing.T) {
				u := newUser(t, "rollback")
				p := newPlan(t, "rollback")
				p.UpgradeGroup = "benefit-test"
				p.DailyClaimLimit = 1
				require.NoError(t, db.Save(&p).Error)
				const callback = "test:reject_claim_ledger"
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "subscription_claims" {
						tx.AddError(errors.New("claim ledger unavailable"))
					}
				}))
				_, err := ClaimFreeSubscription(u.Id, p.Id, "rollback-request-key")
				require.NoError(t, db.Callback().Create().Remove(callback))
				assert.ErrorContains(t, err, "ledger unavailable")
				var count int64
				require.NoError(t, db.Model(&UserSubscription{}).Where("plan_id = ?", p.Id).Count(&count).Error)
				assert.Zero(t, count)
				var userAfter User
				require.NoError(t, db.First(&userAfter, u.Id).Error)
				assert.Equal(t, "default", userAfter.Group)
				availability, err := GetSubscriptionClaimAvailability(db, u.Id, &p, GetDBTimestamp())
				require.NoError(t, err)
				assert.EqualValues(t, 1, *availability.DailyRemaining)
				_, err = ClaimFreeSubscription(u.Id, p.Id, "rollback-request-key")
				require.NoError(t, err)
			})
			t.Run("calendar cycle limits reset independently of lifetime", func(t *testing.T) {
				for _, cycle := range []string{SubscriptionResetWeekly, SubscriptionResetMonthly} {
					u := newUser(t, "calendar-"+cycle)
					p := newPlan(t, "calendar-"+cycle)
					p.ClaimCycle = cycle
					p.MaxClaimsPerCycle = 1
					require.NoError(t, db.Save(&p).Error)
					now := time.Date(2026, 10, 8, 16, 0, 0, 0, time.UTC).Unix()
					_, boundary := subscriptionClaimPeriod(now, cycle)
					_, err := claimFreeSubscription(u.Id, p.Id, "calendar-first-request", func() int64 { return now })
					require.NoError(t, err)
					_, err = claimFreeSubscription(u.Id, p.Id, "calendar-second-request", func() int64 { return boundary - 1 })
					assert.ErrorContains(t, err, "cycle")
					_, err = claimFreeSubscription(u.Id, p.Id, "calendar-next-request", func() int64 { return boundary })
					require.NoError(t, err)
				}
			})
			t.Run("unlimited quota remains distinct from free price", func(t *testing.T) {
				u := newUser(t, "unlimited")
				p := newPlan(t, "unlimited")
				require.NoError(t, db.Model(&p).Update("total_amount", 0).Error)
				_, err := ClaimFreeSubscription(u.Id, p.Id, "unlimited-request-key-01")
				require.NoError(t, err)
				_, err = PreConsumeUserSubscription("unlimited-consume", u.Id, "test", 0, 101)
				require.NoError(t, err)
			})
		})
	}
}

func TestSubscriptionClaimShanghaiCalendarPeriods(t *testing.T) {
	now := time.Date(2026, 10, 8, 16, 0, 0, 0, time.UTC).Unix()
	for _, tc := range []struct {
		period     string
		start, end string
	}{
		{"daily", "2026-10-08T16:00:00Z", "2026-10-09T16:00:00Z"},
		{"weekly", "2026-10-04T16:00:00Z", "2026-10-11T16:00:00Z"},
		{"monthly", "2026-09-30T16:00:00Z", "2026-10-31T16:00:00Z"},
	} {
		t.Run(tc.period, func(t *testing.T) {
			start, end := subscriptionClaimPeriod(now, tc.period)
			assert.Equal(t, tc.start, time.Unix(start, 0).UTC().Format(time.RFC3339))
			assert.Equal(t, tc.end, time.Unix(end, 0).UTC().Format(time.RFC3339))
		})
	}
}

func TestSubscriptionClaimRulesValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan SubscriptionPlan
	}{
		{"no validity", SubscriptionPlan{DurationUnit: SubscriptionDurationCustom}},
		{"overflowing days", SubscriptionPlan{DurationUnit: SubscriptionDurationDay, DurationValue: 1_000_000_000}},
		{"overflowing quota reset", SubscriptionPlan{DurationUnit: SubscriptionDurationDay, DurationValue: 1, QuotaResetPeriod: SubscriptionResetCustom, QuotaResetCustomSeconds: 1_000_000_000_000}},
		{"negative stock", SubscriptionPlan{DailyClaimLimit: -1}},
		{"invalid cycle", SubscriptionPlan{ClaimCycle: "never"}},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Error(t, ValidateSubscriptionClaimRules(&tc.plan)) })
	}
}
