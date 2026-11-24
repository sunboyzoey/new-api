package router

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// accessTokenExemptRoutes are /api routes that never reach dashboard
// authentication, so a personal access token has no rule to satisfy there.
var accessTokenExemptRoutes = []string{
	// Anonymous pages and setup.
	"GET /api/setup",
	"POST /api/setup",
	"GET /api/status",
	"GET /api/uptime/status",
	"GET /api/notice",
	"GET /api/user-agreement",
	"GET /api/privacy-policy",
	"GET /api/about",
	"GET /api/home_page_content",
	"GET /api/ratio_config",
	"GET /api/user/groups",

	// Anonymous account entry points.
	"GET /api/verification",
	"GET /api/reset_password",
	"POST /api/user/reset",
	"POST /api/user/register",
	"GET /api/user/login/encryption-key",
	"POST /api/user/login",
	"POST /api/user/login/2fa",
	"POST /api/user/login/verify",
	"POST /api/user/login/passkey/begin",
	"POST /api/user/login/passkey/finish",
	"POST /api/user/passkey/login/begin",
	"POST /api/user/passkey/login/finish",
	"GET /api/oauth/wechat",
	"GET /api/oauth/telegram/login",
	"GET /api/oauth/telegram/bind/:flow_token",

	// Browser session cookie endpoints.
	"POST /api/user/auth/refresh",
	"POST /api/user/auth/logout",

	// Payment callbacks.
	"POST /api/stripe/webhook",
	"POST /api/creem/webhook",
	"POST /api/waffo/webhook",
	"POST /api/waffo-pancake/webhook/:env",
	"GET /api/user/epay/notify",
	"POST /api/user/epay/notify",
	"GET /api/subscription/epay/notify",
	"POST /api/subscription/epay/notify",
	"GET /api/subscription/epay/return",
	"POST /api/subscription/epay/return",

	// API key authentication (TokenAuthReadOnly).
	"GET /api/usage/token/",
	"GET /api/log/token",
}

// accessTokenHelperRoutes are declared through handlePermissionRoute outside
// channelPermissionRoutes.
var accessTokenHelperRoutes = map[string]authz.Permission{
	"GET /api/task_plugin_options": authz.TaskPluginBind,
	"GET /api/audit":               authz.AuditRead,
}

func newAccessTokenScopeTestEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	// A permission route that is also in the static table panics here.
	require.NotPanics(t, func() {
		SetApiRouter(engine)
		SetDashboardRouter(engine)
		SetRelayRouter(engine)
		SetTaskPluginProtocolRouter(engine)
		SetVideoRouter(engine)
		SetTaskRouter(engine)
	})
	return engine
}

func TestAccessTokenRouteRulesCoverEveryDashboardRoute(t *testing.T) {
	engine := newAccessTokenScopeTestEngine(t)
	exempt := map[string]bool{}
	for _, key := range accessTokenExemptRoutes {
		exempt[key] = true
	}
	registered := map[string]bool{}
	panel := map[string]bool{}
	for _, route := range engine.Routes() {
		if !strings.HasPrefix(route.Path, "/api/") && !strings.HasPrefix(route.Path, "/pg/") {
			continue
		}
		key := route.Method + " " + route.Path
		registered[key] = true
		if !exempt[key] {
			panel[key] = true
		}
	}
	for _, key := range accessTokenExemptRoutes {
		assert.True(t, registered[key], "exempt route %s is no longer registered", key)
	}

	declared := map[string]int{}
	for _, key := range middleware.AccessTokenRouteRuleKeys() {
		declared[key]++
	}
	for key := range panel {
		assert.Equal(t, 1, declared[key], "panel route %s must be declared exactly once", key)
	}
	for key := range declared {
		assert.True(t, panel[key], "declared route %s is not a registered panel route", key)
	}

	permissionRoutes := map[string]authz.Permission{}
	for _, route := range channelPermissionRoutes {
		permissionRoutes[route.method+" "+joinPaths("/api/channel", route.path)] = route.permission
	}
	for key, permission := range accessTokenHelperRoutes {
		permissionRoutes[key] = permission
	}
	for key, permission := range permissionRoutes {
		rule, ok := middleware.AccessTokenRouteRule(key)
		require.True(t, ok, key)
		assert.Equal(t, "scope", rule.Kind(), key)
		assert.Equal(t, service.AccessTokenScopeOf(permission), rule.Scope(), key)
	}
}

func TestAccessTokenCatalogCoversEveryPermission(t *testing.T) {
	catalog := service.AccessTokenCatalog(1, common.RoleRootUser)
	scopes := map[string]bool{}
	resources := map[string]int{}
	for _, group := range catalog {
		for _, resource := range group.Resources {
			resources[resource.Resource]++
			for _, action := range resource.Actions {
				scopes[resource.Resource+":"+action.Action] = true
			}
		}
	}
	for _, permission := range authz.AllPermissions() {
		assert.True(t, scopes[service.AccessTokenScopeOf(permission)], "root catalog lacks %s", service.AccessTokenScopeOf(permission))
	}
	for resource, count := range resources {
		assert.Equal(t, 1, count, "resource %s appears in more than one catalog entry", resource)
	}
}

func TestAccessTokenVerificationScopesAreAllListed(t *testing.T) {
	files, err := filepath.Glob("../service/*.go")
	require.NoError(t, err)
	fileSet := token.NewFileSet()
	var scopes []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, file, nil, 0)
		require.NoError(t, err)
		for _, declaration := range parsed.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if !strings.HasPrefix(name.Name, "VerificationScope") || i >= len(value.Values) {
						continue
					}
					literal, ok := value.Values[i].(*ast.BasicLit)
					require.True(t, ok, name.Name)
					scope, err := strconv.Unquote(literal.Value)
					require.NoError(t, err)
					scopes = append(scopes, scope)
				}
			}
		}
	}
	require.NotEmpty(t, scopes)
	for _, scope := range scopes {
		_, listed := service.AccessTokenVerificationScope(scope)
		assert.True(t, listed, "verification scope %s has no access token rule", scope)
	}
}

func TestManualPelicanRouteRequiresSameOriginAdmin(t *testing.T) {
	setupRelayRouterTestDB(t)
	require.NoError(t, model.DB.AutoMigrate(&model.UserSession{}, &model.Option{}, &model.AuditLog{}, &model.UserAccessToken{}, &model.SystemTask{}))
	require.NoError(t, model.EnsureLegacyAccessTokenRetireAt(common.GetTimestamp()))
	t.Setenv("PELICAN_TEST_ENABLED", "true")
	const origin = "https://dashboard.example"
	secure, origins := common.SessionCookieSecure, common.SessionCookieTrustedURLs
	common.SessionCookieSecure, common.SessionCookieTrustedURLs = true, []string{origin}
	t.Cleanup(func() { common.SessionCookieSecure, common.SessionCookieTrustedURLs = secure, origins })

	// The runner stays stopped. Only the local task queue can be affected.
	config := model.Option{Key: "PelicanGroupMonitorSecret", Value: `{"enabled":true,"targets":[{"id":"selected","group":"group-A","models":["example"],"enabled":true}]}`}
	require.NoError(t, model.DB.Create(&config).Error)
	sessions := make(map[int]*service.AuthBundle)
	const legacyToken = "fixture-legacy-monitor-token"
	scopedToken := model.AccessTokenPrefix + strings.Repeat("a", 43)
	for _, role := range []int{common.RoleCommonUser, common.RoleAdminUser, common.RoleRootUser} {
		name := "monitor-" + strconv.Itoa(role)
		user := model.User{Username: name, Role: role, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: name}
		if role == common.RoleAdminUser {
			user.SetAccessToken(legacyToken)
		}
		require.NoError(t, model.DB.Create(&user).Error)
		if role == common.RoleAdminUser {
			token := &model.UserAccessToken{Name: "test fixture", TokenHash: model.AccessTokenFingerprint(scopedToken), TokenHint: model.AccessTokenHint(scopedToken)}
			require.NoError(t, token.SetScopes([]string{"ops:write"}))
			require.NoError(t, model.CreateUserAccessToken(user.Id, token, service.AccessTokenMaxPerUser))
		}
		session, err := service.CreateLoginSession(user.Id, "password", "127.0.0.1", "monitor-test")
		require.NoError(t, err)
		sessions[role] = session
	}

	engine := gin.New()
	SetApiRouter(engine)
	for _, tc := range []struct {
		name       string
		header     string
		cookie     string
		origin     string
		secure     bool
		wantStatus int
		wantCount  int64
		wantCode   string
	}{
		{"anonymous", "", "", origin, true, http.StatusUnauthorized, 0, "AUTH_UNAUTHORIZED"},
		{"refresh cookie alone even in development mode", "", sessions[common.RoleAdminUser].RefreshToken, origin, false, http.StatusUnauthorized, 0, "AUTH_UNAUTHORIZED"},
		{"ordinary user", sessions[common.RoleCommonUser].AccessToken, "", origin, true, http.StatusForbidden, 0, "AUTH_INSUFFICIENT_PRIVILEGE"},
		{"legacy admin token", legacyToken, "", origin, true, http.StatusForbidden, 0, "AUTH_SESSION_REQUIRED"},
		{"scoped admin token", scopedToken, "", origin, true, http.StatusForbidden, 0, "AUTH_SESSION_REQUIRED"},
		{"untrusted origin", sessions[common.RoleAdminUser].AccessToken, "", "https://other.example", true, http.StatusForbidden, 0, "AUTH_ORIGIN_FORBIDDEN"},
		{"administrator", sessions[common.RoleAdminUser].AccessToken, "", origin, true, http.StatusOK, 1, ""},
		{"root administrator", sessions[common.RoleRootUser].AccessToken, "", origin, true, http.StatusOK, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			common.SessionCookieSecure = tc.secure
			req := httptest.NewRequest(http.MethodPost, origin+"/api/group-health/targets/selected/test", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", "Bearer "+tc.header)
			}
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: service.RefreshCookieName, Value: tc.cookie})
			}
			req.Header.Set("Origin", tc.origin)
			out := httptest.NewRecorder()
			engine.ServeHTTP(out, req)
			require.Equal(t, tc.wantStatus, out.Code)
			if tc.wantCode != "" {
				assert.Contains(t, out.Body.String(), tc.wantCode)
			}
			var count int64
			require.NoError(t, model.DB.Model(&model.SystemTask{}).Count(&count).Error)
			assert.Equal(t, tc.wantCount, count)
		})
	}
}
