package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/oauth"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRegistrationInvitation(t *testing.T) {
	existing, identity := setupSecurityEnrollmentTest(t)
	oldRequired, oldRegister, oldPassword, oldEmail, oldQuota, oldToken := common.RegistrationInviteRequired.Load(), common.RegisterEnabled, common.PasswordRegisterEnabled, common.EmailVerificationEnabled, common.QuotaForNewUser, constant.GenerateDefaultToken
	common.RegistrationInviteRequired.Store(true)
	common.RegisterEnabled, common.PasswordRegisterEnabled, common.EmailVerificationEnabled, common.QuotaForNewUser, constant.GenerateDefaultToken = true, true, false, 0, false
	t.Cleanup(func() {
		common.RegistrationInviteRequired.Store(oldRequired)
		common.RegisterEnabled, common.PasswordRegisterEnabled, common.EmailVerificationEnabled, common.QuotaForNewUser, constant.GenerateDefaultToken = oldRegister, oldPassword, oldEmail, oldQuota, oldToken
	})
	// Upgrade an existing user database and rerun migration with persisted invites.
	require.NoError(t, model.DB.AutoMigrate(&model.RegistrationInvite{}))
	invite, code, err := model.CreateRegistrationInvite(existing.Id, common.GetTimestamp()+3600)
	require.NoError(t, err)
	require.NoError(t, model.DB.AutoMigrate(&model.RegistrationInvite{}))
	require.NoError(t, model.DB.AutoMigrate(&model.RegistrationInvite{}))
	var preserved model.User
	require.NoError(t, model.DB.First(&preserved, existing.Id).Error)
	assert.Equal(t, existing.Username, preserved.Username)
	var stored model.RegistrationInvite
	require.NoError(t, model.DB.First(&stored, invite.Id).Error)
	assert.Equal(t, model.RegistrationInviteHash(code), stored.CodeHash)
	assert.NotEqual(t, code, stored.CodeHash)
	duplicate := model.RegistrationInvite{CodeHash: stored.CodeHash}
	assert.Error(t, model.DB.Create(&duplicate).Error)

	t.Run("password rejects missing expired revoked and replayed invitations", func(t *testing.T) {
		for i, state := range []string{"missing", "expired", "revoked", "valid", "replayed"} {
			candidate := code
			if state == "missing" {
				candidate = ""
			}
			if state == "expired" || state == "revoked" {
				row, secret, err := model.CreateRegistrationInvite(existing.Id, common.GetTimestamp()+3600)
				require.NoError(t, err)
				candidate = secret
				if state == "expired" {
					require.NoError(t, model.DB.Model(row).Update("expires_at", common.GetTimestamp()-1).Error)
				}
				if state == "revoked" {
					require.NoError(t, model.DB.Model(row).Update("revoked_at", common.GetTimestamp()).Error)
				}
			}
			body, err := common.Marshal(map[string]any{"username": fmt.Sprintf("invite-password-%d", i), "password": "valid-password-123", "registration_invite": candidate})
			require.NoError(t, err)
			response := securityEnrollmentRequest(http.MethodPost, "/api/user/register", string(body), "", identity, Register)
			var result struct {
				Success bool
				Code    string
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			assert.Equal(t, state == "valid", result.Success, state)
			var count int64
			require.NoError(t, model.DB.Model(&model.User{}).Where("username = ?", fmt.Sprintf("invite-password-%d", i)).Count(&count).Error)
			if state == "valid" {
				assert.EqualValues(t, 1, count)
			} else {
				assert.Equal(t, "REGISTRATION_INVITE_INVALID", result.Code, state)
				assert.Zero(t, count, state)
			}
		}
	})

	t.Run("rollback preserves invitation", func(t *testing.T) {
		row, secret, err := model.CreateRegistrationInvite(existing.Id, common.GetTimestamp()+3600)
		require.NoError(t, err)
		rollback := errors.New("abort account creation")
		err = model.DB.Transaction(func(tx *gorm.DB) error {
			require.NoError(t, model.ConsumeRegistrationInviteTx(tx, model.RegistrationInviteHash(secret), existing.Id))
			return rollback
		})
		require.ErrorIs(t, err, rollback)
		var untouched model.RegistrationInvite
		require.NoError(t, model.DB.First(&untouched, row.Id).Error)
		assert.Zero(t, untouched.UsedAt)
	})
	t.Run("single use under concurrent registration", func(t *testing.T) {
		row, secret, err := model.CreateRegistrationInvite(existing.Id, common.GetTimestamp()+3600)
		require.NoError(t, err)
		start := make(chan struct{})
		results := make(chan error, 2)
		var workers sync.WaitGroup
		for i := range 2 {
			workers.Go(func() {
				<-start
				user := model.User{Username: fmt.Sprintf("invite-race-%d", i), Role: common.RoleCommonUser}
				results <- user.RegisterWithInvite(0, model.RegistrationInviteHash(secret))
			})
		}
		close(start)
		workers.Wait()
		close(results)
		successes := 0
		for err := range results {
			if err == nil {
				successes++
			}
		}
		assert.Equal(t, 1, successes)
		var count int64
		require.NoError(t, model.DB.Model(&model.User{}).Where("username LIKE ?", "invite-race-%").Count(&count).Error)
		assert.EqualValues(t, 1, count)
		var consumed model.RegistrationInvite
		require.NoError(t, model.DB.First(&consumed, row.Id).Error)
		assert.NotZero(t, consumed.UsedBy)
	})

	t.Run("OAuth gates new users but allows existing users without consuming invitations", func(t *testing.T) {
		provider := &oauth.GitHubProvider{}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/api/oauth/github", nil)
		remote := &oauth.OAuthUser{ProviderUserID: "7712345", Username: "invite-github"}
		_, _, err := findOrCreateOAuthUser(c, provider, remote, nil, "")
		require.ErrorIs(t, err, model.ErrRegistrationInviteInvalid)
		_, secret, err := model.CreateRegistrationInvite(existing.Id, common.GetTimestamp()+3600)
		require.NoError(t, err)
		created, _, err := findOrCreateOAuthUser(c, provider, remote, nil, "", model.RegistrationInviteHash(secret))
		require.NoError(t, err)
		again, _, err := findOrCreateOAuthUser(c, provider, remote, nil, "")
		require.NoError(t, err)
		assert.Equal(t, created.Id, again.Id)
		spare, secret, err := model.CreateRegistrationInvite(existing.Id, common.GetTimestamp()+3600)
		require.NoError(t, err)
		_, _, err = findOrCreateOAuthUser(c, provider, remote, nil, "", model.RegistrationInviteHash(secret))
		require.NoError(t, err)
		var untouched model.RegistrationInvite
		require.NoError(t, model.DB.First(&untouched, spare.Id).Error)
		assert.Zero(t, untouched.UsedAt)
	})

	t.Run("custom OAuth registration requires an invitation", func(t *testing.T) {
		provider := oauth.NewGenericOAuthProvider(&model.CustomOAuthProvider{Id: 99, Name: "Invitation test", Slug: "invite-test"})
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/api/oauth/invite-test", nil)
		remote := &oauth.OAuthUser{ProviderUserID: "custom-invite-user", Username: "invite-custom"}
		_, _, err := findOrCreateOAuthUser(c, provider, remote, nil, "")
		require.ErrorIs(t, err, model.ErrRegistrationInviteInvalid)
		_, secret, err := model.CreateRegistrationInvite(existing.Id, common.GetTimestamp()+3600)
		require.NoError(t, err)
		created, _, err := findOrCreateOAuthUser(c, provider, remote, nil, "", model.RegistrationInviteHash(secret))
		require.NoError(t, err)
		var binding model.UserOAuthBinding
		require.NoError(t, model.DB.Where("user_id = ?", created.Id).First(&binding).Error)
		assert.Equal(t, "custom-invite-user", binding.ProviderUserId)
	})
	t.Run("WeChat cannot bypass invitation requirement", func(t *testing.T) {
		oldEnabled, oldAddress := common.WeChatAuthEnabled, common.WeChatServerAddress
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":"invite-wechat-user"}`))
		}))
		defer server.Close()
		common.WeChatAuthEnabled, common.WeChatServerAddress = true, server.URL
		defer func() { common.WeChatAuthEnabled, common.WeChatServerAddress = oldEnabled, oldAddress }()
		result := securityEnrollmentRequest(http.MethodGet, "/api/oauth/wechat?code=test", "", "", identity, WeChatAuth)
		assert.Equal(t, http.StatusBadRequest, result.Code)
		assert.Contains(t, result.Body.String(), "REGISTRATION_INVITE_INVALID")
		var count int64
		require.NoError(t, model.DB.Model(&model.User{}).Where("wechat_id = ?", "invite-wechat-user").Count(&count).Error)
		assert.Zero(t, count)
	})

	t.Run("list hides secrets and revoke prevents use", func(t *testing.T) {
		row, secret, err := model.CreateRegistrationInvite(existing.Id, common.GetTimestamp()+3600)
		require.NoError(t, err)
		result := securityEnrollmentRequest(http.MethodGet, "/api/option/registration_invites", "", "", identity, ListRegistrationInvites)
		assert.NotContains(t, result.Body.String(), secret)
		assert.NotContains(t, result.Body.String(), row.CodeHash)
		securityEnrollmentRequest(http.MethodDelete, "/api/option/registration_invites/1", "", "", identity, withAccessTokenID(RevokeRegistrationInvite, row.Id))
		user := model.User{Username: "invite-revoked"}
		assert.ErrorIs(t, user.RegisterWithInvite(0, model.RegistrationInviteHash(secret)), model.ErrRegistrationInviteInvalid)
	})
	t.Run("admin routes reject unauthenticated and ordinary users", func(t *testing.T) {
		engine := gin.New()
		group := engine.Group("/api/option", middleware.RootAuth())
		group.GET("/registration_invites", ListRegistrationInvites)
		group.POST("/registration_invites", CreateRegistrationInvite)
		group.DELETE("/registration_invites/:id", RevokeRegistrationInvite)
		bundle, err := service.CreateLoginSession(existing.Id, "password", "127.0.0.1", "invite-test")
		require.NoError(t, err)
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			path := "/api/option/registration_invites"
			if method == http.MethodDelete {
				path += "/1"
			}
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader("{}")))
			assert.Equal(t, http.StatusUnauthorized, recorder.Code)
			request := httptest.NewRequest(method, path, strings.NewReader("{}"))
			request.Header.Set("Authorization", "Bearer "+bundle.AccessToken)
			denied := httptest.NewRecorder()
			engine.ServeHTTP(denied, request)
			assert.Equal(t, http.StatusForbidden, denied.Code)
		}
	})
}

func TestRegistrationInvitationOAuthState(t *testing.T) {
	setupAuthFlowControllerTest(t)
	code := strings.Repeat("A", 43)
	body, err := common.Marshal(map[string]any{"provider": "auth-flow-test", "intent": "login", "registration_invite": code})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/oauth/state", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	GenerateOAuthCode(c)
	var response struct {
		Success bool
		Data    struct {
			FlowToken string `json:"flow_token"`
		}
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	flow, err := model.GetAuthFlow(response.Data.FlowToken, model.AuthFlowMatch{Purpose: model.AuthFlowPurposeOAuth, Provider: "auth-flow-test", Intent: model.AuthFlowIntentLogin})
	require.NoError(t, err)
	assert.NotContains(t, flow.Payload, code)
	var payload oauthFlowPayload
	require.NoError(t, common.UnmarshalJsonStr(flow.Payload, &payload))
	assert.Equal(t, model.RegistrationInviteHash(code), payload.RegistrationInviteHash)
	assert.NotContains(t, recorder.Body.String(), code)
}

func TestPublicRegistrationRewards(t *testing.T) {
	owner, identity := setupSecurityEnrollmentTest(t)
	oldQuota, oldRequired, oldRegister := common.QuotaForNewUser, common.RegistrationInviteRequired.Load(), common.RegisterEnabled
	common.QuotaForNewUser = 5000000
	common.RegistrationInviteRequired.Store(true)
	common.RegisterEnabled = true
	defer func() {
		common.QuotaForNewUser = oldQuota
		common.RegistrationInviteRequired.Store(oldRequired)
		common.RegisterEnabled = oldRegister
	}()
	require.NoError(t, model.DB.AutoMigrate(&model.RegistrationInvite{}, &model.SubscriptionPlan{}, &model.UserSubscription{}, &model.SubscriptionClaim{}))
	require.NoError(t, model.LOG_DB.AutoMigrate(&model.Log{}))
	for i, transport := range []string{"password", "github"} {
		_, secret, err := model.CreateRegistrationInvite(owner.Id, common.GetTimestamp()+3600)
		require.NoError(t, err)
		user := &model.User{Username: "reward-password", Status: common.UserStatusEnabled, Role: common.RoleCommonUser}
		if i == 0 {
			require.NoError(t, user.RegisterWithInvite(0, model.RegistrationInviteHash(secret)))
		} else {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/api/oauth/github", nil)
			user, _, err = findOrCreateOAuthUser(c, &oauth.GitHubProvider{}, &oauth.OAuthUser{ProviderUserID: "reward-identity", Username: "reward-github"}, nil, "", model.RegistrationInviteHash(secret))
			require.NoError(t, err)
		}
		var subscriptions []model.UserSubscription
		require.NoError(t, model.DB.Where("user_id = ?", user.Id).Find(&subscriptions).Error)
		require.Len(t, subscriptions, 1, transport)
		assert.Equal(t, "free_registration", subscriptions[0].Source)
		assert.Equal(t, "public", subscriptions[0].UsageGroup)
		assert.EqualValues(t, 5000000, subscriptions[0].AmountTotal)
		assert.Zero(t, user.Quota)
	}
	response := securityEnrollmentRequest(http.MethodGet, "/api/subscription/plans", "", "", identity, GetSubscriptionPlans)
	var result struct {
		Success bool
		Data    []SubscriptionPlanDTO
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success)
	assert.Empty(t, result.Data, "automatic rewards must not be offered for free claims")
}
