package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"encoding/base64"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const pelicanWheelFixture = `<svg viewBox='0 0 32 32'><g><circle cx='16' cy='16' r='12' fill='none' stroke='black'/><path d='M4 16H28' stroke='black'/><animateTransform attributeName='transform' type='rotate' from='0 16 16' to='360 16 16' dur='2s' repeatCount='indefinite'/></g></svg>`

const pelicanHTMLFixture = `<!DOCTYPE html><html><head><title>Pelican</title></head><body>` + pelicanWheelFixture + `</body></html>`

func TestPelicanHTMLDrawing(t *testing.T) {
	drawing, err := service.PelicanDrawingFromHTML(pelicanHTMLFixture)
	require.NoError(t, err)
	preview, err := service.PelicanHTMLPreview(drawing)
	require.NoError(t, err)
	assert.Contains(t, preview, "<!DOCTYPE html>")
	assert.Contains(t, preview, "default-src 'none'")
	assert.Contains(t, preview, "data:image/svg+xml;base64,"+base64.StdEncoding.EncodeToString([]byte(drawing)))
	assert.Contains(t, drawing, "<animateTransform")
	for _, tc := range []struct{ name, document, code string }{
		{"bare SVG", pelicanWheelFixture, "invalid_html"},
		{"incomplete HTML", strings.TrimSuffix(pelicanHTMLFixture, "</html>"), "invalid_html"},
		{"static HTML", strings.Replace(pelicanHTMLFixture, pelicanWheelFixture, "<svg><path/></svg>", 1), "missing_animation"},
		{"active HTML", strings.Replace(pelicanHTMLFixture, "</head>", "<script></script></head>", 1), "invalid_html"},
		{"embedded content", strings.Replace(pelicanHTMLFixture, "</svg>", "<foreignObject/></svg>", 1), "unsafe_html"},
		{"oversize document", strings.Repeat(" ", service.MaxPelicanSVGBytes) + pelicanHTMLFixture, "output_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drawing, err := service.PelicanDrawingFromHTML(tc.document)
			require.EqualError(t, err, tc.code)
			assert.Empty(t, drawing)
		})
	}
}

func TestPelicanAnimatedPathDrawing(t *testing.T) {
	const wing = `<path d="M0 0 Q1 2 3 1"><animate attributeName="d" dur="2s" repeatCount="indefinite" values="M0 0 Q1 2 3 1; M0 0 Q1 3 3 1; M0 0 Q1 4 3 1; M0 0 Q1 3 3 1; M0 0 Q1 2 3 1"/></path>`
	const leg = `<path d="M0 0 L1 2 L3 1"><animate attributeName="d" dur="2s" repeatCount="indefinite" values="M0 0 L1 2 L3 1; M0 0 L2 2 L3 2; M0 0 L3 2 L3 3; M0 0 L4 2 L3 4; M0 0 L5 2 L3 5; M0 0 L4 2 L3 4; M0 0 L3 2 L3 3; M0 0 L2 2 L3 2; M0 0 L1 2 L3 1"/></path>`
	document := strings.Replace(pelicanHTMLFixture, "</svg>", wing+leg+"</svg>", 1)
	drawing, err := service.PelicanDrawingFromHTML(document)
	require.NoError(t, err)
	assert.Contains(t, drawing, wing+leg)
	preview, err := service.PelicanHTMLPreview(drawing)
	require.NoError(t, err)
	assert.Contains(t, preview, "data:image/svg+xml;base64,"+base64.StdEncoding.EncodeToString([]byte(drawing)))

	_, err = service.ValidateAnimatedPelicanSVG("<svg>" + wing + leg + "</svg>")
	require.EqualError(t, err, "missing_animation")

	const frames = "M0,0 L1e1,-3 C1,2 3,4 5,6 Z; M0,0 L8,-1 C2,2 3,4 5,6 Z"
	const morph = `<path d="M0,0 L10,-3 C1,2 3,4 5,6 Z"><animate attributeName="d" dur="100ms" repeatCount="indefinite" calcMode="linear" values="` + frames + `"/></path>`
	_, err = service.ValidateAnimatedPelicanSVG(strings.Replace(pelicanWheelFixture, "</svg>", morph+"</svg>", 1))
	require.NoError(t, err)

	longPath := "M0 0" + strings.Repeat("L1 1", 32)
	for _, tc := range []struct{ name, path string }{
		{"wrong target property", strings.Replace(morph, `attributeName="d"`, `attributeName="fill"`, 1)},
		{"explicit target", strings.Replace(morph, "<animate ", `<animate href="#leg" `, 1)},
		{"qualified target property", strings.Replace(morph, "attributeName=", `xmlns:x="urn:test" x:attributeName=`, 1)},
		{"duplicate property", strings.Replace(morph, "<animate ", `<animate attributeName="d" `, 1)},
		{"wrong parent", strings.ReplaceAll(morph, "path", "g")},
		{"nested shape", strings.Replace(morph, "/></path>", "><circle/></animate></path>", 1)},
		{"single frame", strings.Replace(morph, frames, "M0 0L1 1", 1)},
		{"different structure", strings.Replace(morph, frames, "M0 0L1 1;M0 0Q1 2 3 4", 1)},
		{"wrong number of coordinates", strings.Replace(morph, frames, "M0 0L1;M0 0L2", 1)},
		{"nongeometry frame", strings.Replace(morph, frames, "initial;inherit", 1)},
		{"coordinate limit", strings.Replace(morph, frames, "M0 0L1 1;M0 0L1 1e9", 1)},
		{"frame limit", strings.Replace(morph, frames, strings.Repeat("M0 0L1 1;", 32)+"M0 0L2 2", 1)},
		{"command limit", strings.Replace(morph, frames, longPath+";"+longPath, 1)},
		{"too many animations", strings.Repeat(morph, 17)},
		{"duration outside bounds", strings.Replace(morph, "100ms", "1ms", 1)},
		{"unbounded duration", strings.Replace(morph, "100ms", "indefinite", 1)},
		{"event clock", strings.Replace(morph, "<animate ", `<animate begin="click" `, 1)},
		{"nonlinear interpolation", strings.Replace(morph, "linear", "paced", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drawing, err := service.PelicanDrawingFromHTML(strings.Replace(pelicanHTMLFixture, "</svg>", tc.path+"</svg>", 1))
			require.EqualError(t, err, "unsafe_html")
			assert.Empty(t, drawing)
		})
	}
}

func TestPelicanSVGValidation(t *testing.T) {
	_, err := service.ValidateAnimatedPelicanSVG("<svg><circle r='10'/></svg>")
	require.EqualError(t, err, "missing_animation")
	wheel, err := service.ValidateAnimatedPelicanSVG(pelicanWheelFixture)
	require.NoError(t, err)
	assert.Contains(t, wheel, "animateTransform")

	for _, tc := range []struct{ name, svg, errorCode string }{
		{"valid", `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><circle cx="50" cy="50" r="10"/></svg>`, ""},
		{"namespace supplied", `<svg><path d="M0 0L10 10"/></svg>`, ""},
		{"accessible drawing", `<svg role="img" aria-labelledby="art-title subtitle" aria-describedby="说明"><title id="art-title">Pelican</title><desc id="subtitle">On a bicycle</desc><desc id="说明">A drawing</desc><path/></svg>`, ""},
		{"external label reference", `<svg aria-labelledby="other.svg#title"><path/></svg>`, "unsafe_svg"},
		{"nonstandard ARIA namespace", `<svg xmlns:xlink="http://www.w3.org/1999/xlink" xlink:aria-describedby="summary"><desc id="summary">A drawing</desc></svg>`, "unsafe_svg"},
		{"numeric keyframes", strings.Replace(pelicanWheelFixture, "from='0 16 16' to='360 16 16'", "values='0,16,16;180,16,16;360,16,16'", 1), ""},
		{"no rotation", strings.Replace(pelicanWheelFixture, "360 16 16", "0 16 16", 1), "unsafe_svg"},
		{"non-numeric frame", strings.Replace(pelicanWheelFixture, "360 16 16", "one-turn", 1), "unsafe_svg"},
		{"animation restricted to transforms", strings.Replace(pelicanWheelFixture, "attributeName='transform'", "attributeName='fill'", 1), "unsafe_svg"},
		{"animation cannot retarget", strings.Replace(pelicanWheelFixture, "type='rotate'", "type='rotate' href='#other'", 1), "unsafe_svg"},
		{"animation clock must run", strings.Replace(pelicanWheelFixture, "dur='2s'", "dur='0s'", 1), "unsafe_svg"},
		{"local gradient", `<svg><defs><linearGradient id="g"><stop offset="0" stop-color="red"/></linearGradient></defs><rect fill="url(#g)"/></svg>`, ""},
		{"truncated", `<svg><path/>`, "invalid_svg"},
		{"malformed", `<svg><path></svg>`, "invalid_svg"},
		{"script", `<svg><script>alert(1)</script></svg>`, "unsafe_svg"},
		{"event", `<svg onload="alert(1)"><path/></svg>`, "unsafe_svg"},
		{"foreignObject", `<svg><foreignObject/></svg>`, "unsafe_svg"},
		{"external use", `<svg><use href="https://example.test/x.svg#id"/></svg>`, "unsafe_svg"},
		{"external paint", `<svg><rect fill="url(https://example.test/x)"/></svg>`, "unsafe_svg"},
		{"encoded external", `<svg><use href="&#104;ttps://example.test/x"/></svg>`, "unsafe_svg"},
		{"css", `<svg><style>@import url(https://example.test)</style></svg>`, "unsafe_svg"},
		{"multiple roots", `<svg><path/></svg><svg><path/></svg>`, "invalid_svg"},
		{"oversize", strings.Repeat("a", service.MaxPelicanSVGBytes+1), "output_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svg, err := service.ValidatePelicanSVG(tc.svg)
			if tc.errorCode != "" {
				require.EqualError(t, err, tc.errorCode)
				assert.Empty(t, svg)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, svg, `xmlns="http://www.w3.org/2000/svg"`)
		})
	}
}

func TestPelicanCacheAggregation(t *testing.T) {
	report := service.AggregateGroupCacheHealth([]model.Log{
		{Type: model.LogTypeConsume, Group: "openai", ChannelId: 1, ModelName: "gpt", PromptTokens: 2000, Other: `{"cache_tokens":1500}`},
		{Type: model.LogTypeConsume, Group: "claude", ChannelId: 1, ModelName: "claude", PromptTokens: 200, Other: `{"usage_semantic":"anthropic","cache_tokens":500,"cache_creation_tokens":300,"cache_creation_tokens_5m":100,"cache_creation_tokens_1h":200}`},
		{Type: model.LogTypeConsume, Group: "openai", ChannelId: 1, ModelName: "gpt", PromptTokens: 1000, Other: `{"cache_tokens":0,"cache_usage_reported":true}`},
		{Type: model.LogTypeConsume, Group: "openai", ChannelId: 1, ModelName: "gpt", PromptTokens: 3000},
		{Type: model.LogTypeConsume, Group: "openai", ChannelId: 1, PromptTokens: 99999, TokenName: "模型测试"},
		{Type: model.LogTypeConsume, Group: "claude", ChannelId: 1, PromptTokens: 99999, Other: `{"monitor_test":"pelican_svg_v1"}`},
		{Type: model.LogTypeError, Group: "openai", ChannelId: 1, ModelName: "gpt"},
		{Type: model.LogTypeConsume, Group: "unknown", PromptTokens: 100, Other: `{"cache_tokens":0}`},
		{Type: model.LogTypeConsume, Group: "normalized", PromptTokens: 500, Other: `{"input_tokens_total":1000,"cache_tokens":500,"cache_write_tokens":200,"cache_creation_tokens":100,"cache_creation_tokens_5m":100}`},
	})
	rows := map[string]service.GroupCacheHealth{}
	for _, row := range report.Groups {
		rows[row.Group] = row
	}
	require.Len(t, rows, 4)
	require.NotNil(t, rows["claude"].CacheHitRate)
	assert.Equal(t, float64(50), *rows["claude"].CacheHitRate)
	assert.Equal(t, int64(1000), rows["claude"].InputTokens)
	assert.Equal(t, int64(300), rows["claude"].CacheWriteTokens)
	assert.Equal(t, int64(4), rows["openai"].Requests)
	assert.Equal(t, int64(3000), rows["openai"].CacheInputTokens)
	assert.Equal(t, int64(6000), rows["openai"].InputTokens)
	assert.Equal(t, int64(1), rows["openai"].Errors)
	require.NotNil(t, rows["openai"].CacheHitRate)
	assert.Equal(t, float64(50), *rows["openai"].CacheHitRate)
	assert.Nil(t, rows["unknown"].CacheHitRate)
	assert.Equal(t, int64(200), rows["normalized"].CacheWriteTokens)
}

func TestPelicanDatabaseMatrix(t *testing.T) {
	for _, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var dialector gorm.Dialector
			switch engine {
			case "sqlite":
				dialector = sqlite.Open(filepath.Join(t.TempDir(), "pelican.db"))
			case "mysql":
				dsn := os.Getenv("TEST_PELICAN_MYSQL_DSN")
				if dsn == "" {
					t.Skip("isolated MySQL DSN not configured")
				}
				dialector = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_PELICAN_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("isolated PostgreSQL DSN not configured")
				}
				dialector = postgres.Open(dsn)
			}
			db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			if engine == "sqlite" {
				var version string
				require.NoError(t, db.Raw("SELECT sqlite_version()").Scan(&version).Error)
				t.Logf("SQLite engine version: %s", version)
			}
			oldMainType, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
			common.SetDatabaseTypes(common.DatabaseType(engine), common.DatabaseType(engine))
			t.Cleanup(func() { common.SetDatabaseTypes(oldMainType, oldLogType) })
			oldDB, oldLogs := model.DB, model.LOG_DB
			model.DB, model.LOG_DB = db, db
			t.Cleanup(func() { model.DB, model.LOG_DB = oldDB, oldLogs; sqlDB, _ := db.DB(); _ = sqlDB.Close() })
			// Only the explicitly isolated test schema is modified.
			require.NoError(t, db.Migrator().DropTable(&model.PelicanTestResult{}, &model.SystemTask{}, &model.SystemTaskLock{}, &model.Log{}))
			require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}, &model.Log{}, &model.Option{}))
			legacy := model.SystemTask{TaskID: "existing", Type: "channel_test", Status: model.SystemTaskStatusSucceeded, Result: "existing history"}
			require.NoError(t, db.Create(&legacy).Error)
			for range 2 {
				require.NoError(t, db.AutoMigrate(&model.PelicanTestResult{}))
			}
			var preserved model.SystemTask
			require.NoError(t, db.First(&preserved, legacy.ID).Error)
			assert.Equal(t, "existing history", preserved.Result)
			assert.True(t, db.Migrator().HasIndex(&model.PelicanTestResult{}, "idx_pelican_time"))
			// Test fresh migration and repeated migration with existing non-ASCII data.
			for i := range 23 {
				row := model.PelicanTestResult{TargetID: "fixture", TaskID: "probe", Group: "中文分组A", ChannelName: "private-channel-route", ChannelID: 7, Model: "test", StartedAt: int64(i / 2), Status: "failed", Error: "timeout", Output: "private upstream output", TemplateVersion: "pelican_group_svg_v2"}
				require.NoError(t, db.Create(&row).Error)
			}
			for range 2 {
				require.NoError(t, db.AutoMigrate(&model.PelicanTestResult{}))
			}
			var count int64
			require.NoError(t, db.Model(&model.PelicanTestResult{}).Count(&count).Error)
			assert.Equal(t, int64(23), count)
			// A reused target ID must not reveal the old group's history or accept its cursor.
			hidden := model.PelicanTestResult{TargetID: "fixture", Group: "中文分组a", Model: "old-group-secret-model", StartedAt: 100, TemplateVersion: "pelican_group_svg_v2"}
			require.NoError(t, db.Create(&hidden).Error)
			fixtureConfig, err := common.Marshal(pelicanGroupConfig{Targets: []pelicanGroupTarget{{ID: "fixture", Group: "中文分组A", Models: []string{"new-model"}}}})
			require.NoError(t, err)
			require.NoError(t, db.Save(&model.Option{Key: pelicanConfigKey, Value: string(fixtureConfig)}).Error)
			router := gin.New()
			router.GET("/results", GetPelicanResults)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/results", nil))
			require.Equal(t, http.StatusOK, recorder.Code)
			var page struct {
				Success bool
				Data    struct {
					Items   []model.PelicanTestResult `json:"items"`
					HasMore bool                      `json:"has_more"`
				}
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &page))
			require.True(t, page.Success)
			require.Len(t, page.Data.Items, 20)
			assert.True(t, page.Data.HasMore)
			assert.NotContains(t, recorder.Body.String(), "private upstream output")
			assert.NotContains(t, recorder.Body.String(), "private-channel-route")
			assert.Zero(t, page.Data.Items[0].ChannelID)
			assert.NotContains(t, recorder.Body.String(), "task_id")
			assert.Equal(t, int64(23), page.Data.Items[0].ID)
			assert.NotContains(t, recorder.Body.String(), hidden.Model)
			assert.Equal(t, "test", page.Data.Items[0].Model, "same-group historical models are retained")
			before := page.Data.Items[19].ID
			recorder = httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/results?before=%d", before), nil))
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &page))
			require.Len(t, page.Data.Items, 3)
			assert.False(t, page.Data.HasMore)
			recorder = httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/results?target=fixture&before=%d", hidden.ID), nil))
			assert.Equal(t, http.StatusBadRequest, recorder.Code)
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
				recorder = httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(method, "/results", nil))
				assert.Equal(t, http.StatusNotFound, recorder.Code)
			}
			require.NoError(t, db.Model(&model.PelicanTestResult{}).Count(&count).Error)
			assert.Equal(t, int64(24), count)
			require.NoError(t, db.Create(&model.Log{CreatedAt: time.Now().Unix(), Type: model.LogTypeConsume, Group: "route-group", ChannelId: 3, PromptTokens: 2000, Other: `{"cache_tokens":1500}`}).Error)
			// Use a canceled request first: failed reads must never poison the cache.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, _ = service.GetGroupCacheReport(ctx)
			report, err := service.GetGroupCacheReport(context.Background())
			require.NoError(t, err)
			require.NotEmpty(t, report.Groups)
			require.Len(t, report.Trends, 1)
			assert.Equal(t, "route-group", report.Trends[0].Group)
			assert.Equal(t, int64(300), report.BucketSeconds)
			require.Len(t, report.Trends[0].Points, 288)
			last := report.Trends[0].Points[287]
			require.NotNil(t, last.CacheHitRate)
			assert.Equal(t, float64(75), *last.CacheHitRate)
			assert.Equal(t, int64(2000), last.InputTokens)

			// Read reserved group column with each dialect (the cache may already be warm).
			var logs []model.Log
			require.NoError(t, db.Select("id", "group", "channel_id", "other").Where("type IN ?", []int{model.LogTypeConsume, model.LogTypeError}).Find(&logs).Error)
			require.Len(t, logs, 1)
			assert.Equal(t, "route-group", logs[0].Group)
			t.Setenv("CRYPTO_SECRET", "isolated-monitor-secret")
			configRouter := gin.New()
			configRouter.PUT("/config", UpdatePelicanConfig)
			configRouter.GET("/config", GetPelicanConfig)
			body := `{"enabled":true,"targets":[{"group":"group-A","base_url":"https://remote.example","api_key":"fixture-monitor-key","models":["selected-model"],"endpoint":"responses","enabled":true}]}`
			saved := httptest.NewRecorder()
			configRouter.ServeHTTP(saved, httptest.NewRequest("PUT", "/config", strings.NewReader(body)))
			require.Equal(t, 200, saved.Code, saved.Body.String())
			config, err := readPelicanConfig(db)
			require.NoError(t, err)
			require.Len(t, config.Targets, 1)
			assert.NotContains(t, config.Targets[0].APIKey, "fixture-monitor-key")
			plain, err := pelicanKey(config.Targets[0])
			require.NoError(t, err)
			assert.Equal(t, "fixture-monitor-key", plain)
			drawing, err := service.PelicanDrawingFromHTML(pelicanHTMLFixture)
			require.NoError(t, err)
			target := config.Targets[0]
			animation := model.PelicanTestResult{TargetID: target.ID, Group: target.Group, Model: target.Models[0], TemplateVersion: pelicanHTMLTemplate, Status: "pending_review", Output: pelicanHTMLFixture, SVG: drawing}
			require.NoError(t, db.Create(&animation).Error)
			recorder = httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/results", nil))
			require.Equal(t, http.StatusOK, recorder.Code)
			records := gjson.GetBytes(recorder.Body.Bytes(), "data.items").Array()
			require.Len(t, records, 1)
			assert.Equal(t, animation.ID, records[0].Get("id").Int())
			assert.Contains(t, records[0].Get("html").String(), "data:image/svg+xml;base64,"+base64.StdEncoding.EncodeToString([]byte(drawing)))
			assert.False(t, records[0].Get("svg").Exists())
			assert.False(t, records[0].Get("output").Exists())
			config.Targets[0].APIKey = ""
			config.Targets[0].Models = []string{"changed-model"}
			config.Targets[0].ReasoningEffort = "medium"
			bodyBytes, err := common.Marshal(config)
			require.NoError(t, err)
			saved = httptest.NewRecorder()
			configRouter.ServeHTTP(saved, httptest.NewRequest("PUT", "/config", strings.NewReader(string(bodyBytes))))
			require.Equal(t, 200, saved.Code, saved.Body.String())
			config, err = readPelicanConfig(db)
			require.NoError(t, err)
			assert.Equal(t, []string{"changed-model"}, config.Targets[0].Models)
			assert.Equal(t, "medium", config.Targets[0].ReasoningEffort)
			config.Targets[0].APIKey = ""
			config.Targets = append(config.Targets, config.Targets[0])
			bodyBytes, err = common.Marshal(config)
			require.NoError(t, err)
			saved = httptest.NewRecorder()
			configRouter.ServeHTTP(saved, httptest.NewRequest("PUT", "/config", strings.NewReader(string(bodyBytes))))
			assert.Equal(t, 400, saved.Code)
			config, err = readPelicanConfig(db)
			require.NoError(t, err)
			assert.Len(t, config.Targets, 1)
			t.Run("manual target queue", func(t *testing.T) { testManualPelicanQueue(t, db) })
		})
	}
}

// Two saved targets with deliberately unusable credentials let the real runner
// record its model selection without making an upstream request.
func testManualPelicanQueue(t *testing.T, db *gorm.DB) {
	t.Helper()
	t.Setenv("PELICAN_TEST_ENABLED", "true")
	config := pelicanGroupConfig{Targets: []pelicanGroupTarget{
		{ID: "A", Group: "group-a", Models: []string{"old-model"}},
		{ID: "B", Group: "group-b", Models: []string{"other-model"}},
	}}
	serialized, err := common.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, db.Save(&model.Option{Key: pelicanConfigKey, Value: string(serialized)}).Error)
	scheduled, err := model.CreateSystemTask(pelicanTaskType, nil, nil)
	require.NoError(t, err)
	router := gin.New()
	router.POST("/targets/:id/test", TestPelicanTarget)
	post := func(path, body string, want int) gjson.Result {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		require.Equal(t, want, recorder.Code, recorder.Body.String())
		return gjson.ParseBytes(recorder.Body.Bytes())
	}
	assert.Equal(t, "PELICAN_TARGET_NOT_FOUND", post("/targets/absent/test", "", 404).Get("code").String())
	assert.Equal(t, "INVALID_PELICAN_TEST_REQUEST", post("/targets/A/test", `{"model":"override"}`, 400).Get("code").String())
	assert.Equal(t, "INVALID_PELICAN_TEST_REQUEST", post("/targets/A/test?url=https://example.test", "", 400).Get("code").String())
	t.Setenv("PELICAN_TEST_ENABLED", "false")
	assert.Equal(t, "PELICAN_TEST_DISABLED", post("/targets/A/test", "", 503).Get("code").String())
	t.Setenv("PELICAN_TEST_ENABLED", "true")
	first := post("/targets/A/test", "", 200).Get("data")
	require.True(t, first.Get("created").Bool())
	require.Len(t, first.Map(), 2)
	id := first.Get("task_id").String()
	require.NotEmpty(t, id)
	duplicate := post("/targets/A/test", "", 200).Get("data")
	assert.False(t, duplicate.Get("created").Bool())
	assert.Equal(t, id, duplicate.Get("task_id").String())
	assert.Equal(t, "PELICAN_TEST_BUSY", post("/targets/B/test", "", 409).Get("code").String())

	manual := manualPelicanTestHandler{}
	active, err := model.GetActiveSystemTask(manual.Type())
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.JSONEq(t, `{"target_id":"A"}`, active.Payload)
	// Execution must re-read the configuration even when automatic tests are paused.
	config.Targets[0].Models = []string{"saved-model"}
	serialized, err = common.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, db.Save(&model.Option{Key: pelicanConfigKey, Value: string(serialized)}).Error)
	task, claimed, err := model.ClaimSystemTask(active.ID, manual.Type(), "test-runner", time.Now().Unix()+60)
	require.NoError(t, err)
	require.True(t, claimed)
	manual.Run(context.Background(), task, "test-runner")
	var rows []model.PelicanTestResult
	require.NoError(t, db.Where("task_id = ?", id).Find(&rows).Error)
	require.Len(t, rows, 1)
	assert.Equal(t, "A", rows[0].TargetID)
	assert.Equal(t, "saved-model", rows[0].Model)
	assert.Equal(t, "credential_error", rows[0].Error)
	stillScheduled, err := model.GetActiveSystemTask(pelicanTaskType)
	require.NoError(t, err)
	require.NotNil(t, stillScheduled)
	assert.Equal(t, scheduled.TaskID, stillScheduled.TaskID)
	assert.True(t, post("/targets/B/test", "", 200).Get("data.created").Bool())
}

func TestPelicanGroupProbeStatisticsUseCompletedTests(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.PelicanTestResult{}, &model.Option{}))
	target := pelicanGroupTarget{ID: "probe", Group: "remote-group", BaseURL: "https://remote.example", Models: []string{"model"}}
	config, err := common.Marshal(pelicanGroupConfig{Enabled: true, Targets: []pelicanGroupTarget{target}})
	require.NoError(t, err)
	require.NoError(t, db.Save(&model.Option{Key: pelicanConfigKey, Value: string(config)}).Error)
	now := time.Now().UnixMilli()
	for i, status := range []string{"failed", "pending_review", "running"} {
		row := model.PelicanTestResult{
			TargetID: target.ID, Group: target.Group, Model: "model", Status: status, ChannelName: "private-channel-route", ChannelID: 7,
			StartedAt: now - 2 + int64(i), TemplateVersion: "pelican_group_svg_v2",
			InputTokens: 100, CacheReported: status == "pending_review", CacheReadTokens: 75,
		}
		require.NoError(t, db.Create(&row).Error)
	}
	router := gin.New()
	router.GET("/groups", GetPelicanGroups)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/groups", nil))
	require.Equal(t, http.StatusOK, response.Code)
	group := gjson.GetBytes(response.Body.Bytes(), "data.groups.0")
	assert.Equal(t, "running", group.Get("model_results.0.status").String())
	assert.Zero(t, group.Get("model_results.0.channel_id").Int())
	assert.NotContains(t, response.Body.String(), "private-channel-route")
	assert.Equal(t, "probe", group.Get("cache_source").String())
	assert.Equal(t, int64(2), group.Get("cache.requests").Int())
	assert.Equal(t, int64(1), group.Get("cache.errors").Int())
	assert.Equal(t, float64(50), group.Get("error_rate").Float())
	points := group.Get("trend").Array()
	require.Len(t, points, 1)
	point := points[0]
	assert.Equal(t, int64(2), point.Get("requests").Int())
	assert.Equal(t, int64(1), point.Get("errors").Int())
	assert.Equal(t, int64(1), point.Get("cache_samples").Int())
	assert.Equal(t, int64(100), point.Get("cache_input_tokens").Int())
	assert.Equal(t, float64(75), point.Get("cache_hit_rate").Float())
	assert.Equal(t, float64(50), point.Get("error_rate").Float())
	assert.Equal(t, group.Get("finished_at").Int()-service.GroupCacheBucketSeconds, point.Get("timestamp").Int())
	assert.False(t, point.Get("channel_id").Exists())
	assert.False(t, point.Get("model").Exists())
}

func TestPelicanLocalErrorRateRequiresTrafficAndBothLogs(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.PelicanTestResult{}, &model.Option{}, &model.Log{}))
	originalAddress := system_setting.ServerAddress
	system_setting.ServerAddress = "https://gateway.example"
	t.Cleanup(func() { system_setting.ServerAddress = originalAddress })
	targets := []pelicanGroupTarget{
		{ID: "active", Group: "active", BaseURL: system_setting.ServerAddress, Models: []string{"model"}},
		{ID: "empty", Group: "empty", BaseURL: system_setting.ServerAddress, Models: []string{"model"}},
	}
	config, err := common.Marshal(pelicanGroupConfig{Enabled: true, Targets: targets})
	require.NoError(t, err)
	require.NoError(t, db.Save(&model.Option{Key: pelicanConfigKey, Value: string(config)}).Error)
	now := time.Now().Unix()
	require.NoError(t, db.Create(&[]model.Log{
		{CreatedAt: now, Type: model.LogTypeConsume, Group: "active", ChannelId: 8, PromptTokens: 1000, Other: `{"cache_tokens":600}`},
		{CreatedAt: now, Type: model.LogTypeConsume, Group: "active", ChannelId: 9, PromptTokens: 9000},
		{CreatedAt: now - 3600, Type: model.LogTypeError, Group: "private-group", ModelName: "private-model"},
	}).Error)
	router := gin.New()
	router.GET("/groups", GetPelicanGroups)
	originalErrorLogs, originalConsumeLogs := constant.ErrorLogEnabled, common.LogConsumeEnabled
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		constant.ErrorLogEnabled, common.LogConsumeEnabled = originalErrorLogs, originalConsumeLogs
		common.OptionMapRWMutex.Unlock()
	})
	for _, tc := range []struct {
		name                   string
		errorLogs, consumeLogs bool
		want                   string
	}{
		{"no error logs", false, true, "null"},
		{"no consumption logs", true, false, "null"},
		{"both logs enabled", true, true, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			common.OptionMapRWMutex.Lock()
			constant.ErrorLogEnabled, common.LogConsumeEnabled = tc.errorLogs, tc.consumeLogs
			common.OptionMapRWMutex.Unlock()
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/groups", nil))
			require.Equal(t, http.StatusOK, response.Code)
			groups := gjson.GetBytes(response.Body.Bytes(), "data.groups")
			assert.Equal(t, tc.want, groups.Get("0.error_rate").Raw)
			assert.Equal(t, "null", groups.Get("1.error_rate").Raw)
			points := groups.Get("0.trend").Array()
			require.Len(t, points, 1)
			assert.Equal(t, tc.want, points[0].Get("error_rate").Raw)
			assert.Equal(t, float64(60), points[0].Get("cache_hit_rate").Float())
			assert.Equal(t, int64(1000), points[0].Get("cache_input_tokens").Int())
			assert.Empty(t, groups.Get("1.trend").Array())
			assert.NotContains(t, response.Body.String(), "private-group")
			assert.NotContains(t, response.Body.String(), "private-model")
		})
	}
}

func TestPelicanGroupProbeRoutesThroughGateway(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.PelicanTestResult{}, &model.Option{}))
	t.Setenv("CRYPTO_SECRET", "fixture-stable-secret")
	service.InitHttpClient()
	protection := system_setting.GetFetchSetting()
	originalProtection := protection.EnableSSRFProtection
	protection.EnableSSRFProtection = false
	t.Cleanup(func() { protection.EnableSSRFProtection = originalProtection })
	var mode atomic.Int32
	requests := make(chan map[string]any, 1)
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &request))
		assert.Equal(t, "Bearer fixture-key", r.Header.Get("Authorization"))
		assert.NotContains(t, request, "group")
		assert.NotContains(t, request, "channel_id")
		if mode.Load() == 1 {
			close(requestStarted)
			select {
			case <-r.Context().Done():
			case <-releaseResponse:
			}
			return
		}
		if mode.Load() == 2 {
			http.Redirect(w, r, "/credential-leak", http.StatusTemporaryRedirect)
			return
		}
		if mode.Load() == 3 {
			w.WriteHeader(401)
			_, _ = w.Write([]byte("fixture-key private upstream failure"))
			return
		}
		if mode.Load() == 4 {
			_, _ = w.Write([]byte(`{"output":[{"content":[{"type":"output_text","text":"<svg><desc>fix&#116;ure-key</desc><path/></svg>"}]}]}`))
			return
		}
		if mode.Load() == 5 {
			_, _ = w.Write([]byte(`{"output":[{"content":[{"type":"output_text","text":"<!DOCTYPE html><html><body><svg><circle r='10'/></svg></body></html>"}]}]}`))
			return
		}
		requests <- request
		if r.URL.Path == "/v1/responses" {
			assert.Equal(t, pelicanPrompt, request["input"])
			_, _ = w.Write([]byte(`{"output":[{"content":[{"type":"output_text","text":"` + pelicanHTMLFixture + `"}]}],"usage":{"input_tokens":100,"output_tokens":50,"input_tokens_details":{"cached_tokens":0}}}`))
			return
		}
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		assert.Contains(t, fmt.Sprint(request["messages"]), "pelican riding a bicycle")
		assert.Equal(t, float64(8192), request["max_completion_tokens"])
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + pelicanHTMLFixture + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":75}}}`))
	}))
	defer server.Close()
	defer close(releaseResponse)
	client := service.GetHttpClient()
	originalClient := *client
	*client = *server.Client()
	t.Cleanup(func() { *client = originalClient })
	target := pelicanGroupTarget{Group: "group-A", BaseURL: server.URL, APIKey: "fixture-key", Models: []string{"gpt-6.2-astra"}, Endpoint: "chat/completions", Enabled: true}
	require.NoError(t, normalizePelicanTarget(&target, map[string]pelicanGroupTarget{}))
	for _, tc := range []struct {
		endpoint, effort string
		cacheReadTokens  int64
	}{
		{"chat/completions", "", 75},
		{"chat/completions", "medium", 75},
		{"responses", "", 0},
		{"responses", "medium", 0},
	} {
		target.Endpoint, target.ReasoningEffort = tc.endpoint, tc.effort
		record, err := runPelicanGroupProbe(context.Background(), "fixture", target, target.Models[0])
		require.NoError(t, err)
		require.Equal(t, "pending_review", record.Status, record.Error)
		assert.Contains(t, record.SVG, "animateTransform")
		assert.Equal(t, pelicanHTMLTemplate, record.TemplateVersion)
		assert.Equal(t, pelicanHTMLFixture, record.Output)
		assert.Equal(t, "group-A", record.Group)
		assert.Zero(t, record.ChannelID)
		assert.True(t, record.CacheReported)
		assert.Equal(t, tc.cacheReadTokens, record.CacheReadTokens)
		request := <-requests
		assert.Equal(t, "gpt-6.2-astra", request["model"])
		if tc.effort == "" {
			assert.NotContains(t, request, "reasoning")
			assert.NotContains(t, request, "reasoning_effort")
		} else if tc.endpoint == "responses" {
			assert.Equal(t, map[string]any{"effort": "medium"}, request["reasoning"])
			assert.NotContains(t, request, "reasoning_effort")
		} else {
			assert.Equal(t, "medium", request["reasoning_effort"])
			assert.NotContains(t, request, "reasoning")
		}
	}
	mode.Store(2)
	record, err := runPelicanGroupProbe(context.Background(), "fixture", target, target.Models[0])
	require.NoError(t, err)
	assert.Equal(t, 307, record.HTTPStatus)
	assert.Equal(t, "upstream_error", record.Error)
	mode.Store(3)
	record, err = runPelicanGroupProbe(context.Background(), "fixture", target, target.Models[0])
	require.NoError(t, err)
	assert.Equal(t, 401, record.HTTPStatus)
	assert.NotContains(t, record.Error, "fixture-key")
	mode.Store(4)
	record, err = runPelicanGroupProbe(context.Background(), "fixture", target, target.Models[0])
	require.NoError(t, err)
	var saved model.PelicanTestResult
	require.NoError(t, db.First(&saved, record.ID).Error)
	assert.Equal(t, "unsafe_html", saved.Error)
	assert.Empty(t, saved.Output)
	assert.Empty(t, saved.SVG)
	mode.Store(5)
	record, err = runPelicanGroupProbe(context.Background(), "fixture", target, target.Models[0])
	require.NoError(t, err)
	assert.Equal(t, "failed", record.Status)
	assert.Equal(t, "missing_animation", record.Error)
	assert.Empty(t, record.SVG)
	mode.Store(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *model.PelicanTestResult, 1)
	go func() {
		result, runErr := runPelicanGroupProbe(ctx, "fixture", target, target.Models[0])
		assert.NoError(t, runErr)
		done <- result
	}()
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("request not started")
	}
	cancel()
	select {
	case result := <-done:
		assert.Equal(t, "interrupted", result.Error)
	case <-time.After(5 * time.Second):
		t.Fatal("request not canceled")
	}
}

func TestPelicanConfigSecretsAndGroupBinding(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Option{}, &model.Token{}, &model.PelicanTestResult{}, &model.Log{}))
	t.Setenv("CRYPTO_SECRET", "fixture-stable-secret")
	originalAddress := system_setting.ServerAddress
	system_setting.ServerAddress = "https://gateway.example"
	t.Cleanup(func() { system_setting.ServerAddress = originalAddress })
	token := model.Token{UserId: 1, Key: "fixture-key", Group: "fixed", Status: common.TokenStatusEnabled}
	require.NoError(t, db.Create(&token).Error)
	target := pelicanGroupTarget{Group: "wrong", BaseURL: system_setting.ServerAddress, APIKey: "sk-fixture-key", Models: []string{"chosen"}, Endpoint: "responses"}
	assert.ErrorContains(t, normalizePelicanTarget(&target, map[string]pelicanGroupTarget{}), "fixed group")
	target.ID = ""
	target.Group = "fixed"
	target.APIKey = "sk-fixture-key"
	target.ReasoningEffort = "invalid"
	require.ErrorContains(t, normalizePelicanTarget(&target, nil), "reasoning effort")
	target.ReasoningEffort = "medium"
	require.NoError(t, normalizePelicanTarget(&target, map[string]pelicanGroupTarget{}))
	assert.NotContains(t, target.APIKey, "fixture-key")
	key, err := pelicanKey(target)
	require.NoError(t, err)
	assert.Equal(t, "sk-fixture-key", key)
	modified := target
	modified.Group = "other"
	_, err = pelicanKey(modified)
	require.Error(t, err)
	ciphertext, err := base64.StdEncoding.DecodeString(target.APIKey)
	require.NoError(t, err)
	ciphertext[len(ciphertext)-1] ^= 1
	modified = target
	modified.APIKey = base64.StdEncoding.EncodeToString(ciphertext)
	_, err = pelicanKey(modified)
	require.Error(t, err)
	retained := target
	retained.APIKey = ""
	require.NoError(t, normalizePelicanTarget(&retained, map[string]pelicanGroupTarget{target.ID: target}))
	assert.Equal(t, target.APIKey, retained.APIKey)
	retained.BaseURL = "https://other.example"
	retained.APIKey = ""
	assert.ErrorContains(t, normalizePelicanTarget(&retained, map[string]pelicanGroupTarget{target.ID: target}), "new key")
	for _, base := range []string{"http://gateway.example", "https://user:password@gateway.example", "https://gateway.example?key=private", "file:///tmp/key"} {
		bad := target
		bad.BaseURL = base
		bad.APIKey = "fixture"
		assert.Error(t, normalizePelicanTarget(&bad, map[string]pelicanGroupTarget{target.ID: target}))
	}
	config := pelicanGroupConfig{Enabled: true, Targets: []pelicanGroupTarget{target}}
	payload, err := common.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, db.Save(&model.Option{Key: pelicanConfigKey, Value: string(payload)}).Error)
	router := gin.New()
	router.GET("/config", GetPelicanConfig)
	router.GET("/groups", GetPelicanGroups)
	router.GET("/results", GetPelicanResults)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/config", nil))
	assert.Equal(t, 200, response.Code)
	assert.NotContains(t, response.Body.String(), `"api_key":`)
	assert.NotContains(t, response.Body.String(), target.APIKey)
	assert.Contains(t, response.Body.String(), `has_api_key":true`)
	assert.Equal(t, "medium", gjson.GetBytes(response.Body.Bytes(), "data.targets.0.reasoning_effort").String())
	// Historical provider text is not trusted, including content persisted before
	// publication filters were introduced or drawn using a previously rotated key.
	rows := []model.PelicanTestResult{
		{Status: "failed", Error: "upstream echoed " + key, ErrorCode: key, ChannelName: key, Output: key, TaskID: key, HTTPStatus: 401},
		{Status: "pending_review", SVG: `<svg><title>sk-fix<tspan>ture-key</tspan></title><path/></svg>`},
		{Status: "pending_review", SVG: `<svg aria-label="sk-previouscredential123456"><path/></svg>`},
		{Status: "pending_review", SVG: `<svg><title>Pelican</title><path/></svg>`},
		{Status: "pending_review", SVG: pelicanWheelFixture},
		{Status: "failed", Error: "missing_animation"},
		{Status: "pending_review", SVG: pelicanWheelFixture, Output: "private model response"},
		{Status: "pending_review", SVG: "<svg><path/></svg>"},
	}
	for i := range rows {
		rows[i].TargetID, rows[i].Group, rows[i].Model = target.ID, target.Group, target.Models[0]
		rows[i].StartedAt, rows[i].TemplateVersion = int64(len(rows)-i), "pelican_group_svg_v2"
	}
	rows[4].TemplateVersion, rows[5].TemplateVersion = "pelican_group_svg_v3", "pelican_group_svg_v3"
	rows[6].TemplateVersion, rows[7].TemplateVersion = pelicanHTMLTemplate, pelicanHTMLTemplate
	require.NoError(t, db.Create(&rows).Error)
	for _, endpoint := range []string{"/groups", "/results"} {
		response = httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, endpoint, nil))
		require.Equal(t, 200, response.Code)
		for _, private := range []string{key, target.APIKey, "previouscredential", "api_key", "gateway.example", "upstream echoed"} {
			assert.NotContains(t, response.Body.String(), private)
		}
		records := gjson.GetBytes(response.Body.Bytes(), "data.items").Array()
		if endpoint == "/groups" {
			group := gjson.GetBytes(response.Body.Bytes(), "data.groups.0")
			assert.Equal(t, "fixed", group.Get("group").String())
			records = append(group.Get("model_results").Array(), group.Get("latest"))
			require.Len(t, records, 2)
		} else {
			require.Len(t, records, 8)
			for _, row := range records[1:3] {
				assert.False(t, row.Get("svg").Exists())
				assert.Equal(t, "unsafe_svg", row.Get("error").String())
			}
			assert.Contains(t, records[3].Get("svg").String(), "<title>Pelican</title>")
			assert.Contains(t, records[4].Get("svg").String(), "<animateTransform")
			assert.Equal(t, "missing_animation", records[5].Get("error").String())
			assert.False(t, records[4].Get("html").Exists())
			assert.False(t, records[6].Get("svg").Exists())
			assert.Contains(t, records[6].Get("html").String(), "<!DOCTYPE html>")
			assert.NotContains(t, response.Body.String(), "private model response")
			assert.False(t, records[7].Get("html").Exists())
			assert.Equal(t, "unsafe_html", records[7].Get("error").String())
		}
		assert.Equal(t, "upstream_error", records[0].Get("error").String())
		assert.Equal(t, int64(401), records[0].Get("http_status").Int())
		for _, row := range records {
			for _, private := range []string{"error_code", "channel_id", "channel_name", "task_id", "output"} {
				assert.False(t, row.Get(private).Exists(), private)
			}
		}
	}
}

func TestGroupCacheCountsBilledFinalFailuresOnce(t *testing.T) {
	logs := []model.Log{
		{CreatedAt: 901, RequestId: "paid", Group: "auto", ChannelId: 2, Type: model.LogTypeError, Other: `{"admin_info":{"final_request_error":true}}`},
		{CreatedAt: 899, RequestId: "paid", Group: "g", ChannelId: 2, Type: model.LogTypeConsume, PromptTokens: 100, CompletionTokens: 5, Other: `{"cache_tokens":40}`},
		{CreatedAt: 902, RequestId: "unpaid", Group: "g", ChannelId: 2, Type: model.LogTypeError, Other: `{"admin_info":{"final_request_error":true}}`},
		{CreatedAt: 903, RequestId: "old", Group: "g", ChannelId: 2, Type: model.LogTypeError},
		{CreatedAt: 904, RequestId: "old", Group: "g", ChannelId: 2, Type: model.LogTypeConsume},
	}
	report := service.AggregateGroupCacheHealth(logs)
	require.Len(t, report.Groups, 1)
	require.Len(t, report.Channels, 1)
	for _, row := range []service.GroupCacheHealth{report.Groups[0], report.Channels[0]} {
		assert.EqualValues(t, 4, row.Requests)
		assert.EqualValues(t, 3, row.Errors)
		assert.EqualValues(t, 100, row.InputTokens)
		assert.EqualValues(t, 5, row.OutputTokens)
		assert.EqualValues(t, 40, row.CacheReadTokens)
	}
	trends := service.AggregateGroupCacheTrends(logs, 600, 1200)
	require.Len(t, trends, 1)
	require.Len(t, trends[0].Points, 2)
	assert.EqualValues(t, 1, trends[0].Points[0].Requests)
	assert.EqualValues(t, 1, trends[0].Points[0].Errors)
	assert.EqualValues(t, 3, trends[0].Points[1].Requests)
	assert.EqualValues(t, 2, trends[0].Points[1].Errors)
}

func TestPelicanCacheTrendsRespectWindowAndUnknownData(t *testing.T) {
	logs := []model.Log{
		{CreatedAt: 601, Type: model.LogTypeConsume, Group: "A", PromptTokens: 1000, Other: `{"cache_tokens":900}`},
		{CreatedAt: 899, Type: model.LogTypeConsume, Group: "A", PromptTokens: 9000, Other: `{"cache_tokens":900}`},
		{CreatedAt: 603, Type: model.LogTypeConsume, Group: "A", PromptTokens: 5000},
		{CreatedAt: 602, Type: model.LogTypeError, Group: "A"},
		{CreatedAt: 901, Type: model.LogTypeConsume, Group: "A", PromptTokens: 100, Other: `{"cache_tokens":0}`},
		{CreatedAt: 1201, Type: model.LogTypeConsume, Group: "B", PromptTokens: 200, Other: `{"cache_tokens":0,"cache_usage_reported":true}`},
		{CreatedAt: 1800, Type: model.LogTypeConsume, Group: "A", PromptTokens: 100, Other: `{"cache_tokens":50}`},
		{CreatedAt: 599, Type: model.LogTypeConsume, Group: "outside", PromptTokens: 99999},
		{CreatedAt: 1801, Type: model.LogTypeConsume, Group: "outside", PromptTokens: 99999},
		{CreatedAt: 1000, Type: model.LogTypeConsume, Group: "synthetic", TokenName: "模型测试", PromptTokens: 99999},
	}
	trends := service.AggregateGroupCacheTrends(logs, 600, 1800)
	require.Len(t, trends, 2)
	require.Equal(t, "A", trends[0].Group)
	require.Len(t, trends[0].Points, 4)
	points := trends[0].Points
	assert.Equal(t, []int64{600, 900, 1200, 1500}, []int64{points[0].Timestamp, points[1].Timestamp, points[2].Timestamp, points[3].Timestamp})
	require.NotNil(t, points[0].CacheHitRate)
	assert.Equal(t, float64(18), *points[0].CacheHitRate)
	assert.Equal(t, int64(4), points[0].Requests)
	assert.Equal(t, int64(10000), points[0].CacheInputTokens)
	assert.Equal(t, int64(15000), points[0].InputTokens)
	assert.Equal(t, int64(1), points[0].Errors)
	assert.Nil(t, points[1].CacheHitRate)
	assert.Equal(t, int64(100), points[1].InputTokens)
	assert.Nil(t, points[2].CacheHitRate)
	assert.Zero(t, points[2].Requests)
	require.NotNil(t, points[3].CacheHitRate)
	assert.Equal(t, float64(50), *points[3].CacheHitRate)
	require.NotNil(t, trends[1].Points[2].CacheHitRate)
	assert.Zero(t, *trends[1].Points[2].CacheHitRate)
	assert.Empty(t, service.AggregateGroupCacheTrends(logs, 1800, 1800))
}
