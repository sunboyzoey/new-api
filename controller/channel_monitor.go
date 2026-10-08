package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/QuantumNous/new-api/model"
	cm "github.com/QuantumNous/new-api/pkg/channelmonitor"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var monitorEvaluator *cm.Evaluator
var monitorOnce sync.Once
var monitorInitErr error

func getMonitorEvaluator() (*cm.Evaluator, error) {
	monitorOnce.Do(func() {
		if os.Getenv("CHANNEL_MONITOR_ENABLED") != "true" {
			monitorInitErr = errors.New("channel monitoring is disabled")
			return
		}
		dir := os.Getenv("CHANNEL_MONITOR_DATA_DIR")
		if dir == "" {
			dir = "/root/new-api/data/channel-monitor"
		}
		monitorEvaluator, monitorInitErr = cm.NewEvaluator(filepath.Join(dir, "evaluations.json"), resolveMonitorUpstream)
	})
	return monitorEvaluator, monitorInitErr
}
func resolveMonitorUpstream(input cm.EvaluationInput) (cm.Upstream, error) {
	ch, err := model.GetChannelById(input.ChannelID, true)
	if err != nil || ch == nil {
		return cm.Upstream{}, errors.New("channel not found")
	}
	if ch.Status != 1 || ch.Type != 1 {
		return cm.Upstream{}, errors.New("test requires an enabled OpenAI-compatible channel")
	}
	allowed := false
	for _, m := range strings.Split(ch.Models, ",") {
		if strings.TrimSpace(m) == input.Model {
			allowed = true
			break
		}
	}
	if !allowed {
		return cm.Upstream{}, errors.New("model is not configured for this channel")
	}
	keys := ch.GetKeys()
	if len(keys) != 1 || strings.TrimSpace(keys[0]) == "" {
		return cm.Upstream{}, errors.New("test requires a channel with one API key")
	}
	if ch.ModelMapping != nil && strings.TrimSpace(*ch.ModelMapping) != "" && strings.TrimSpace(*ch.ModelMapping) != "{}" {
		return cm.Upstream{}, errors.New("tests on model-mapped channels are currently unsupported")
	}
	setting := ch.GetSetting()
	client, err := service.GetHttpClientWithProxySettings(setting.Proxy, setting)
	if err != nil {
		return cm.Upstream{}, errors.New("invalid channel transport configuration")
	}
	clone := *client
	clone.Timeout = 10 * time.Minute
	return cm.Upstream{BaseURL: ch.GetBaseURL(), Key: keys[0], Client: &clone}, nil
}
func channelMonitorSnapshot(c *gin.Context) {
	channels, err := model.GetAllChannels(0, 1000, false, true)
	if err != nil {
		c.JSON(503, gin.H{"success": false, "message": "channel metadata unavailable"})
		return
	}
	list := make([]cm.Channel, 0, len(channels))
	for _, ch := range channels {
		name := ch.Name
		models := []string{}
		for _, m := range strings.Split(ch.Models, ",") {
			if strings.TrimSpace(m) != "" {
				models = append(models, strings.TrimSpace(m))
			}
		}
		list = append(list, cm.Channel{ID: ch.Id, Name: name, Status: ch.Status, Models: models})
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 6*time.Second)
	defer cancel()
	snapshot, err := cm.Snapshot(ctx, list, c.Query("window"))
	message := ""
	if err != nil {
		message = "monitoring backend unavailable"
	}
	jobs := []cm.Evaluation{}
	schedules := []cm.Schedule{}
	evaluator, e := getMonitorEvaluator()
	if e == nil {
		jobs = evaluator.List(false)
		schedules = evaluator.Schedules()
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"success": true, "data": snapshot, "evaluations": jobs, "schedules": schedules, "evaluation_defaults": gin.H{"candy": cm.DefaultEvaluationPrompt("candy"), "pelican": cm.DefaultEvaluationPrompt("pelican")}, "message": message})
}
func GetChannelMonitorAdmin(c *gin.Context) { channelMonitorSnapshot(c) }

// JSON-only writes and same-origin checks also cover installations using HTTP cookies.
func ChannelMonitorWriteGuard(c *gin.Context) {
	if origin := c.GetHeader("Origin"); origin != "" {
		u, e := url.Parse(origin)
		if e != nil || !strings.EqualFold(u.Host, c.Request.Host) {
			c.AbortWithStatusJSON(403, gin.H{"success": false, "message": "cross-origin request rejected"})
			return
		}
	}
	if c.GetHeader("Sec-Fetch-Site") == "cross-site" || !strings.HasPrefix(c.GetHeader("Content-Type"), "application/json") {
		c.AbortWithStatusJSON(403, gin.H{"success": false, "message": "same-origin JSON request required"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	c.Next()
}
func SubmitChannelEvaluation(c *gin.Context) {
	var in cm.EvaluationInput
	if c.ShouldBindJSON(&in) != nil {
		c.JSON(400, gin.H{"success": false, "message": "invalid test input"})
		return
	}
	e, err := getMonitorEvaluator()
	if err != nil {
		c.JSON(503, gin.H{"success": false, "message": "evaluation service unavailable"})
		return
	}
	job, err := e.Submit(in)
	if err != nil {
		c.JSON(400, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.JSON(202, gin.H{"success": true, "data": job})
}
func ReviewChannelEvaluation(c *gin.Context) {
	var in struct {
		Score  int    `json:"score"`
		Review string `json:"review"`
	}
	if c.ShouldBindJSON(&in) != nil {
		c.JSON(400, gin.H{"success": false, "message": "invalid review"})
		return
	}
	e, err := getMonitorEvaluator()
	if err != nil {
		c.JSON(503, gin.H{"success": false})
		return
	}
	if err = e.Review(c.Param("id"), in.Score, in.Review); err != nil {
		c.JSON(400, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.JSON(200, gin.H{"success": true})
}
func SaveChannelEvaluationSchedule(c *gin.Context) {
	var in cm.Schedule
	if c.ShouldBindJSON(&in) != nil {
		c.JSON(400, gin.H{"success": false, "message": "invalid schedule"})
		return
	}
	e, err := getMonitorEvaluator()
	if err != nil {
		c.JSON(503, gin.H{"success": false})
		return
	}
	err = e.SaveSchedule(in)
	if err != nil {
		c.JSON(400, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.JSON(200, gin.H{"success": true, "data": e.Schedules()})
}
func ChannelMonitorGrafana(c *gin.Context) {
	target, _ := url.Parse("http://127.0.0.1:3030")
	proxy := httputil.NewSingleHostReverseProxy(target)
	old := proxy.Director
	proxy.Director = func(req *http.Request) {
		old(req)
		req.Host = target.Host
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
		for key := range req.Header {
			if strings.HasPrefix(strings.ToLower(key), "x-webauth") {
				req.Header.Del(key)
			}
		}
		req.Header.Set("X-WEBAUTH-USER", "channel-monitor-admin")
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) {
		http.Error(w, "monitoring dashboard unavailable", 503)
	}
	proxy.ServeHTTP(c.Writer, c.Request)
}

func InitChannelMonitor() (func(), error) {
	if os.Getenv("CHANNEL_MONITOR_ENABLED") != "true" {
		return func() {}, nil
	}
	e, err := getMonitorEvaluator()
	if err != nil {
		return nil, err
	}
	closeGroups := startGroupMonitor()
	return func() { closeGroups(); e.Close() }, nil
}

// Grafana navigation uses a short-lived opaque, HttpOnly cookie. The original
// dashboard credential stays in memory and is revalidated by AdminAuth on
// every proxied request, including revocation and permission changes.
type grafanaSession struct {
	Authorization string
	Expires       time.Time
}

var grafanaSessions = struct {
	sync.Mutex
	tickets map[[32]byte]grafanaSession
}{tickets: map[[32]byte]grafanaSession{}}

func IssueChannelGrafanaSession(c *gin.Context) {
	credential := c.GetHeader("Authorization")
	if credential == "" {
		c.JSON(401, gin.H{"success": false})
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		c.JSON(503, gin.H{"success": false})
		return
	}
	value := hex.EncodeToString(b)
	key := sha256.Sum256([]byte(value))
	grafanaSessions.Lock()
	for k, s := range grafanaSessions.tickets {
		if !time.Now().Before(s.Expires) {
			delete(grafanaSessions.tickets, k)
		}
	}
	if len(grafanaSessions.tickets) >= 128 {
		grafanaSessions.Unlock()
		c.JSON(429, gin.H{"success": false, "message": "too many monitoring sessions"})
		return
	}
	grafanaSessions.tickets[key] = grafanaSession{Authorization: credential, Expires: time.Now().Add(15 * time.Minute)}
	grafanaSessions.Unlock()
	http.SetCookie(c.Writer, &http.Cookie{Name: "newapi_channel_grafana", Value: value, Path: "/api/channel-monitor/grafana", MaxAge: 900, HttpOnly: true, Secure: c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https", SameSite: http.SameSiteStrictMode})
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"success": true})
}
func ChannelGrafanaSessionAuth(c *gin.Context) {
	if c.GetHeader("Authorization") == "" {
		value, err := c.Cookie("newapi_channel_grafana")
		if err == nil && len(value) == 64 {
			key := sha256.Sum256([]byte(value))
			grafanaSessions.Lock()
			s, ok := grafanaSessions.tickets[key]
			if ok && !time.Now().Before(s.Expires) {
				delete(grafanaSessions.tickets, key)
				ok = false
			}
			grafanaSessions.Unlock()
			if ok {
				c.Request.Header.Set("Authorization", s.Authorization)
			}
		}
	}
	if c.Request.Method != "GET" && c.Request.Method != "HEAD" {
		if origin := c.GetHeader("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(u.Host, c.Request.Host) {
				c.AbortWithStatus(403)
				return
			}
		}
		if c.GetHeader("Sec-Fetch-Site") == "cross-site" {
			c.AbortWithStatus(403)
			return
		}
	}
	c.Next()
}
