package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const (
	PublicRewardRegistration = "registration"
	PublicRewardCheckin      = "checkin"
)

// IsAutomaticPublicReward identifies rewards issued by account events, not claims.
func (plan *SubscriptionPlan) IsAutomaticPublicReward() bool {
	return plan.IsReward && (plan.RewardType == PublicRewardRegistration || plan.RewardType == PublicRewardCheckin)
}

// GrantPublicRewardSubscription grants quota scoped to the public routing group.
// Reward quota is intentionally stored as a subscription, not in User.Quota, so
// it cannot be spent through paid or private groups.
func GrantPublicRewardSubscription(userID, amount int, rewardType string) error {
	if userID <= 0 || amount <= 0 {
		return errors.New("invalid public reward")
	}
	if rewardType != PublicRewardRegistration && rewardType != PublicRewardCheckin {
		return errors.New("invalid public reward type")
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		return grantPublicRewardSubscriptionTx(tx, userID, amount, rewardType)
	})
}

func grantPublicRewardSubscriptionTx(tx *gorm.DB, userID, amount int, rewardType string) error {
	plan, err := ensurePublicRewardPlanTx(tx, amount, rewardType)
	if err != nil {
		return err
	}
	_, err = CreateUserSubscriptionFromPlanTx(tx, userID, plan, "free_"+rewardType)
	return err
}

func ensurePublicRewardPlanTx(tx *gorm.DB, amount int, rewardType string) (*SubscriptionPlan, error) {
	var plan SubscriptionPlan
	query := tx.Where("is_reward = ? AND reward_type = ? AND usage_group = ?", true, rewardType, "public").First(&plan)
	if query.Error != nil && !errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return nil, query.Error
	}
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		plan = SubscriptionPlan{
			Title:               "公益" + map[string]string{PublicRewardRegistration: "注册额度", PublicRewardCheckin: "签到额度"}[rewardType],
			PriceAmount:         0,
			Currency:            "USD",
			DurationUnit:        SubscriptionDurationYear,
			DurationValue:       100,
			Enabled:             true,
			AllowBalancePay:     common.GetPointer(false),
			AllowWalletOverflow: common.GetPointer(false),
			IsReward:            true,
			RewardType:          rewardType,
			UsageGroup:          "public",
			TotalAmount:         int64(amount),
			QuotaResetPeriod:    SubscriptionResetNever,
			ClaimCycle:          SubscriptionResetNever,
		}
		if err := tx.Create(&plan).Error; err != nil {
			return nil, err
		}
		return &plan, nil
	}
	// The plan amount is only used when issuing a new subscription. Existing
	// subscriptions retain their original amount snapshots.
	if plan.TotalAmount != int64(amount) {
		plan.TotalAmount = int64(amount)
		if err := tx.Save(&plan).Error; err != nil {
			return nil, err
		}
	}
	return &plan, nil
}

// PublicRewardSubscriptionExists reports whether a user has an active reward
// subscription for the requested public reward type.
func PublicRewardSubscriptionExists(userID int, rewardType string) (bool, error) {
	var count int64
	err := DB.Model(&UserSubscription{}).
		Joins("JOIN subscription_plans ON subscription_plans.id = user_subscriptions.plan_id").
		Where("user_subscriptions.user_id = ? AND user_subscriptions.status = ? AND user_subscriptions.end_time > ? AND user_subscriptions.usage_group = ? AND subscription_plans.reward_type = ?", userID, "active", common.GetTimestamp(), "public", rewardType).
		Count(&count).Error
	return count > 0, err
}
