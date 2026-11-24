package controller

import (
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func writeRegistrationInviteError(c *gin.Context) {
	c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "REGISTRATION_INVITE_INVALID", "message": i18n.T(c, "user.registration_invite_invalid")})
}

func ListRegistrationInvites(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("p"))
	page = max(1, min(page, 100000))
	var invites []model.RegistrationInvite
	if err := model.DB.Order("id desc").Offset((page - 1) * 50).Limit(50).Find(&invites).Error; err != nil {
		common.ApiErrorI18n(c, i18n.MsgDatabaseError)
		return
	}
	common.ApiSuccess(c, gin.H{"required": common.RegistrationInviteRequired.Load(), "items": invites})
}

func CreateRegistrationInvite(c *gin.Context) {
	var request struct {
		ExpiresAt int64 `json:"expires_at"`
	}
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		common.ApiErrorI18n(c, i18n.MsgInvalidParams)
		return
	}
	now := common.GetTimestamp()
	if request.ExpiresAt <= now || request.ExpiresAt > now+30*86400 {
		common.ApiErrorI18n(c, i18n.MsgInvalidParams)
		return
	}
	invite, code, err := model.CreateRegistrationInvite(c.GetInt("id"), request.ExpiresAt)
	if err != nil {
		common.ApiErrorI18n(c, i18n.MsgDatabaseError)
		return
	}
	// The plaintext code is returned once and never included in audit parameters.
	common.ApiSuccess(c, gin.H{"invitation": invite, "code": code})
}

func RevokeRegistrationInvite(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorI18n(c, i18n.MsgInvalidParams)
		return
	}
	result := model.DB.Model(&model.RegistrationInvite{}).Where("id = ? AND used_at = 0 AND revoked_at = 0", id).Update("revoked_at", common.GetTimestamp())
	if result.Error != nil {
		common.ApiErrorI18n(c, i18n.MsgDatabaseError)
		return
	}
	common.ApiSuccess(c, nil)
}
