package model

import (
	"errors"
	"math"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// Claims survive subscription deletion so stock and lifetime limits cannot be reset.
type SubscriptionClaim struct {
	Id             int    `json:"id"`
	UserId         int    `json:"user_id" gorm:"uniqueIndex:idx_claim_request,priority:1;index:idx_claim_user_period,priority:2"`
	PlanId         int    `json:"plan_id" gorm:"index:idx_claim_day,priority:1;index:idx_claim_user_period,priority:1"`
	RequestKey     string `json:"-" gorm:"type:varchar(64);not null;uniqueIndex:idx_claim_request,priority:2"`
	SubscriptionId int    `json:"subscription_id" gorm:"uniqueIndex;not null"`
	ClaimedAt      int64  `json:"claimed_at" gorm:"type:bigint;not null;index:idx_claim_day,priority:2;index:idx_claim_user_period,priority:3"`
	CycleStart     int64  `json:"cycle_start" gorm:"type:bigint;not null"`
}

type SubscriptionClaimResult struct {
	Subscription *UserSubscription `json:"subscription"`
	Replayed     bool              `json:"replayed"`
}

type SubscriptionClaimAvailability struct {
	DailyRemaining *int64 `json:"daily_remaining"`
	DailyRefreshAt int64  `json:"daily_refresh_at"`
	CycleRefreshAt int64  `json:"cycle_refresh_at"`
	LifetimeCount  int64  `json:"lifetime_count"`
	CycleCount     int64  `json:"cycle_count"`
	CanClaim       bool   `json:"can_claim"`
	Reason         string `json:"reason,omitempty"`
}

func ValidateSubscriptionClaimRules(plan *SubscriptionPlan) error {
	if math.IsNaN(plan.PriceAmount) || math.IsInf(plan.PriceAmount, 0) {
		return errors.New("Plan price must be finite")
	}
	if plan.DailyClaimLimit < 0 || plan.MaxClaimsPerCycle < 0 || plan.DailyClaimLimit > 1_000_000_000 || plan.MaxClaimsPerCycle > 1_000_000_000 {
		return errors.New("Claim limits must be integers between 0 and 1000000000")
	}
	if plan.ClaimCycle == "" {
		plan.ClaimCycle = SubscriptionResetDaily
	}
	if plan.ClaimCycle != SubscriptionResetDaily && plan.ClaimCycle != SubscriptionResetWeekly && plan.ClaimCycle != SubscriptionResetMonthly {
		return errors.New("Claim cycle must be daily, weekly or monthly")
	}
	if plan.PriceAmount == 0 {
		if plan.QuotaResetPeriod == SubscriptionResetCustom && plan.QuotaResetCustomSeconds > int64(math.MaxInt64/int64(time.Second)) {
			return errors.New("Free subscription quota reset period is too large")
		}
		// Guard the duration arithmetic used by the existing subscription constructor.
		maxValue := int64(0)
		switch plan.DurationUnit {
		case SubscriptionDurationYear:
			maxValue = 100
		case SubscriptionDurationMonth:
			maxValue = 1200
		case SubscriptionDurationDay:
			maxValue = int64(math.MaxInt64 / int64(24*time.Hour))
		case SubscriptionDurationHour:
			maxValue = int64(math.MaxInt64 / int64(time.Hour))
		case SubscriptionDurationCustom:
			if plan.CustomSeconds > int64(math.MaxInt64/int64(time.Second)) {
				return errors.New("Free subscription validity period is too large")
			}
		}
		if maxValue > 0 && int64(plan.DurationValue) > maxValue {
			return errors.New("Free subscription validity period is too large")
		}
		now := time.Now()
		end, err := calcPlanEndTime(now, plan)
		if err != nil {
			return err
		}
		if end <= now.Unix() {
			return errors.New("Free subscriptions must have a positive validity period")
		}
	}
	return nil
}

// Shanghai calendar periods are independent of quota reset periods and host timezone.
func subscriptionClaimPeriod(now int64, period string) (int64, int64) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	} // tzdata is embedded above.
	date := time.Unix(now, 0).In(location)
	start := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, location)
	switch period {
	case SubscriptionResetWeekly:
		start = start.AddDate(0, 0, -(int(start.Weekday())+6)%7)
		return start.Unix(), start.AddDate(0, 0, 7).Unix()
	case SubscriptionResetMonthly:
		start = time.Date(date.Year(), date.Month(), 1, 0, 0, 0, 0, location)
		return start.Unix(), start.AddDate(0, 1, 0).Unix()
	default:
		return start.Unix(), start.AddDate(0, 0, 1).Unix()
	}
}

func GetSubscriptionClaimAvailability(tx *gorm.DB, userId int, plan *SubscriptionPlan, now int64) (*SubscriptionClaimAvailability, error) {
	if plan.IsAutomaticPublicReward() {
		return &SubscriptionClaimAvailability{CanClaim: false, Reason: "Free plan is unavailable"}, nil
	}
	dayStart, dayEnd := subscriptionClaimPeriod(now, SubscriptionResetDaily)
	cycleStart, cycleEnd := subscriptionClaimPeriod(now, plan.ClaimCycle)
	result := &SubscriptionClaimAvailability{DailyRefreshAt: dayEnd, CycleRefreshAt: cycleEnd, CanClaim: true}
	var dayCount, subscriptionCount int64
	if err := tx.Model(&SubscriptionClaim{}).Where("plan_id = ? AND claimed_at >= ? AND claimed_at < ?", plan.Id, dayStart, dayEnd).Count(&dayCount).Error; err != nil {
		return nil, err
	}
	if plan.DailyClaimLimit > 0 {
		remaining := max(0, int64(plan.DailyClaimLimit)-dayCount)
		result.DailyRemaining = &remaining
	}
	if err := tx.Model(&SubscriptionClaim{}).Where("plan_id = ? AND user_id = ?", plan.Id, userId).Count(&result.LifetimeCount).Error; err != nil {
		return nil, err
	}
	if err := tx.Model(&UserSubscription{}).Where("plan_id = ? AND user_id = ? AND (source <> ? OR source IS NULL)", plan.Id, userId, "free_claim").Count(&subscriptionCount).Error; err != nil {
		return nil, err
	}
	// Preserve legacy purchase/grant counts without counting free claim subscriptions twice.
	result.LifetimeCount += subscriptionCount
	if err := tx.Model(&SubscriptionClaim{}).Where("plan_id = ? AND user_id = ? AND claimed_at >= ? AND claimed_at < ?", plan.Id, userId, cycleStart, cycleEnd).Count(&result.CycleCount).Error; err != nil {
		return nil, err
	}
	switch {
	case !plan.Enabled || plan.PriceAmount != 0:
		result.Reason = "Free plan is unavailable"
	case plan.MaxPurchasePerUser > 0 && result.LifetimeCount >= int64(plan.MaxPurchasePerUser):
		result.Reason = "Lifetime claim limit reached"
	case plan.MaxClaimsPerCycle > 0 && result.CycleCount >= int64(plan.MaxClaimsPerCycle):
		result.Reason = "Activity cycle claim limit reached"
	case result.DailyRemaining != nil && *result.DailyRemaining == 0:
		result.Reason = "Today's free subscriptions are fully claimed"
	}
	if result.Reason == "" && plan.BlockActiveClaim {
		var active int64
		if err := tx.Model(&UserSubscription{}).Where("plan_id = ? AND user_id = ? AND status = ? AND end_time > ?", plan.Id, userId, "active", now).Count(&active).Error; err != nil {
			return nil, err
		}
		if active > 0 {
			result.Reason = "You already have an active subscription to this plan"
		}
	}
	result.CanClaim = result.Reason == ""
	return result, nil
}

func ClaimFreeSubscription(userId, planId int, requestKey string) (*SubscriptionClaimResult, error) {
	return claimFreeSubscription(userId, planId, requestKey, GetDBTimestamp)
}

func claimFreeSubscription(userId, planId int, requestKey string, clock func() int64) (*SubscriptionClaimResult, error) {
	if userId <= 0 || planId <= 0 || len(requestKey) < 16 || len(requestKey) > 64 || strings.TrimSpace(requestKey) != requestKey {
		return nil, errors.New("Invalid free subscription claim")
	}
	for _, char := range requestKey {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return nil, errors.New("Invalid claim request key")
		}
	}
	result := &SubscriptionClaimResult{}
	err := DB.Transaction(func(tx *gorm.DB) error {
		// SQLite has no SELECT FOR UPDATE. Acquire its writer lock before any reads.
		if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			if err := tx.Model(&SubscriptionPlan{}).Where("id = ?", planId).UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
				return err
			}
		}
		var plan SubscriptionPlan
		if err := lockForUpdate(tx).First(&plan, planId).Error; err != nil {
			return err
		}
		if plan.IsAutomaticPublicReward() {
			return errors.New("Free plan is unavailable")
		}
		var user User
		if err := lockForUpdate(tx).First(&user, userId).Error; err != nil {
			return err
		}
		if user.Status != common.UserStatusEnabled {
			return errors.New("User is disabled")
		}
		var previous SubscriptionClaim
		query := tx.Where("user_id = ? AND request_key = ?", userId, requestKey).Limit(1).Find(&previous)
		if query.Error != nil {
			return query.Error
		}
		if query.RowsAffected > 0 {
			if previous.PlanId != planId {
				return errors.New("Claim request key was already used for another plan")
			}
			var subscription UserSubscription
			if err := tx.First(&subscription, previous.SubscriptionId).Error; err != nil {
				return err
			}
			result.Subscription = &subscription
			result.Replayed = true
			return nil
		}
		now := clock()
		available, err := GetSubscriptionClaimAvailability(tx, userId, &plan, now)
		if err != nil {
			return err
		}
		if !available.CanClaim {
			return errors.New(available.Reason)
		}
		if err := ValidateSubscriptionClaimRules(&plan); err != nil {
			return err
		}
		subscription, err := CreateUserSubscriptionFromPlanTx(tx, userId, &plan, "free_claim")
		if err != nil {
			return err
		}
		cycleStart, _ := subscriptionClaimPeriod(now, plan.ClaimCycle)
		claim := SubscriptionClaim{UserId: userId, PlanId: planId, RequestKey: requestKey, SubscriptionId: subscription.Id, ClaimedAt: now, CycleStart: cycleStart}
		if err := tx.Create(&claim).Error; err != nil {
			return err
		}
		result.Subscription = subscription
		return nil
	})
	if err != nil {
		return nil, err
	}
	if result.Subscription.PrevUserGroup != "" && !result.Replayed {
		refreshSubscriptionUserGroupCache(userId, "free subscription claim")
	}
	return result, nil
}
