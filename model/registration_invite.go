package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var ErrRegistrationInviteInvalid = errors.New("registration invitation is required or unavailable")

// RegistrationInvite stores no usable invitation secret. Redemption and account
// creation must commit in the same transaction.
type RegistrationInvite struct {
	Id        int    `json:"id"`
	CodeHash  string `json:"-" gorm:"type:varchar(64);not null;uniqueIndex"`
	CreatedBy int    `json:"created_by"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	UsedBy    int    `json:"used_by"`
	UsedAt    int64  `json:"used_at"`
	RevokedAt int64  `json:"revoked_at"`
}

func RegistrationInviteHash(code string) string {
	code = strings.TrimSpace(code)
	if len(code) != 43 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(code)
	if err != nil || len(raw) != 32 {
		return ""
	}
	hash := sha256.Sum256([]byte(code))
	return hex.EncodeToString(hash[:])
}

func CreateRegistrationInvite(creator int, expiresAt int64) (*RegistrationInvite, string, error) {
	now := common.GetTimestamp()
	if creator <= 0 || expiresAt <= now || expiresAt > now+int64(30*24*time.Hour/time.Second) {
		return nil, "", errors.New("invitation expiry must be within 30 days")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, "", err
	}
	code := base64.RawURLEncoding.EncodeToString(secret[:])
	invite := &RegistrationInvite{CodeHash: RegistrationInviteHash(code), CreatedBy: creator, CreatedAt: now, ExpiresAt: expiresAt}
	if err := DB.Create(invite).Error; err != nil {
		return nil, "", err
	}
	return invite, code, nil
}

// ConsumeRegistrationInviteTx uses a conditional update, including on SQLite,
// so concurrent registrations cannot both consume the same invitation.
func ConsumeRegistrationInviteTx(tx *gorm.DB, hash string, userID int) error {
	if !common.RegistrationInviteRequired.Load() {
		return nil
	}
	if len(hash) != 64 || userID <= 0 {
		return ErrRegistrationInviteInvalid
	}
	result := tx.Model(&RegistrationInvite{}).
		Where("code_hash = ? AND used_at = 0 AND revoked_at = 0 AND expires_at > ?", hash, common.GetTimestamp()).
		Updates(map[string]any{"used_by": userID, "used_at": common.GetTimestamp()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrRegistrationInviteInvalid
	}
	return nil
}

// RegisterWithInvite retains the same account setup as password and legacy
// WeChat registration, while making invitation redemption atomic.
func (user *User) RegisterWithInvite(inviterID int, hash string) error {
	if common.RegistrationInviteRequired.Load() && len(hash) != 64 {
		return ErrRegistrationInviteInvalid
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := user.InsertWithTx(tx, inviterID); err != nil {
			return err
		}
		return ConsumeRegistrationInviteTx(tx, hash, user.Id)
	})
	if err != nil {
		return err
	}
	user.FinishInsert(inviterID)
	return nil
}
