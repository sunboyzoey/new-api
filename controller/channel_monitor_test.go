package controller

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChannelMonitorRejectsCrossOriginAndNonJSONWrites(t *testing.T) {
	for _, tc := range []struct {
		origin, content string
		status          int
	}{{"http://evil.invalid", "application/json", 403}, {"http://localhost", "application/json", 200}, {"", "text/plain", 403}, {"", "application/json", 200}} {
		r := gin.New()
		r.POST("/write", ChannelMonitorWriteGuard, func(c *gin.Context) { c.Status(200) })
		req := httptest.NewRequest("POST", "http://localhost/write", strings.NewReader(`{}`))
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("Content-Type", tc.content)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, tc.status, w.Code)
	}
}
func TestChannelMonitorAdminEndpointsRejectVisitorAndNormalUser(t *testing.T) {
	user, _ := setupSecurityEnrollmentTest(t)
	token := "temporary-monitor-test-token"
	require.NoError(t, model.DB.Model(user).Update("access_token", token).Error)
	r := gin.New()
	r.GET("/monitor-admin", middleware.AdminAuth(), middleware.RequirePermission(authz.ChannelRead), func(c *gin.Context) { c.Status(200) })
	for _, auth := range []string{"", "Bearer " + token} {
		req := httptest.NewRequest("GET", "/monitor-admin", nil)
		req.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Contains(t, []int{401, 403}, w.Code)
	}
	// Root access retains the existing administrator permission system.
	require.NoError(t, model.DB.Model(user).Update("role", common.RoleRootUser).Error)
	req := httptest.NewRequest("GET", "/monitor-admin", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
}

func TestChannelMonitorGrafanaCookieRevalidatesCredentialOnEveryRequest(t *testing.T) {
	user, _ := setupSecurityEnrollmentTest(t)
	token := "temporary-grafana-test-token"
	require.NoError(t, model.DB.Model(user).Updates(map[string]any{"access_token": token, "role": common.RoleRootUser}).Error)
	r := gin.New()
	r.POST("/issue", middleware.AdminAuth(), middleware.RequirePermission(authz.ChannelRead), IssueChannelGrafanaSession)
	r.GET("/bridge", ChannelGrafanaSessionAuth, middleware.AdminAuth(), middleware.RequirePermission(authz.ChannelRead), func(c *gin.Context) { c.Status(200) })
	req := httptest.NewRequest("POST", "/issue", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	cookies := w.Result().Cookies()
	require.Len(t, cookies, 1)
	require.True(t, cookies[0].HttpOnly)
	require.Equal(t, "/api/channel-monitor/grafana", cookies[0].Path)
	req = httptest.NewRequest("GET", "/bridge", nil)
	req.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	require.NoError(t, model.DB.Model(user).Update("access_token", nil).Error)
	req = httptest.NewRequest("GET", "/bridge", nil)
	req.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, 401, w.Code)
}
