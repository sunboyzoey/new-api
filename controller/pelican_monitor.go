package controller

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

const pelicanTaskType = "pelican_group_svg"
const pelicanHTMLTemplate = "pelican_group_html_v4"

// Public history includes results from the original still-image prompt.
var pelicanGroupTemplates = []string{"pelican_group_svg_v2", "pelican_group_svg_v3", pelicanHTMLTemplate}

const pelicanPrompt = `Draw an animated pelican riding a bicycle in a complete, self-contained HTML document. Use this document structure: <!DOCTYPE html><html><head><title>Pelican</title></head><body><svg viewBox="0 0 320 240">YOUR DRAWING</svg></body></html>. Show its long bill and throat pouch, its feet on the pedals, and a bicycle with two visible spoked wheels. Make the spokes and pedals visibly turn in a continuous loop. Put each rotating part in a local <g>. Inside that group use <animateTransform attributeName="transform" type="rotate" from="0 CX CY" to="360 CX CY" dur="2s" repeatCount="indefinite"/> with numeric coordinates for the center. Keep the frame and background still. If you animate a leg or foot by changing its outline, put animate directly inside that path, target only d, and use a short loop of matching absolute M/L/Q/C path commands in values. Use no other animation types. Return only the HTML document. Use SVG shapes and presentation attributes for the drawing. Use no scripts, CSS, external resources or embedded images.`

// Manual runs use their own active key and lease so they never postpone the
// next scheduled pass over all enabled targets.
type manualPelicanTestHandler struct{}

type pelicanTargetTestPayload struct {
	TargetID string `json:"target_id"`
}

func (manualPelicanTestHandler) Type() string { return "pelican_target_test" }

func TestPelicanTarget(c *gin.Context) {
	if c.Request.ContentLength != 0 || c.Request.URL.RawQuery != "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "INVALID_PELICAN_TEST_REQUEST", "message": "Only the saved target can be tested"})
		return
	}
	if !common.GetEnvOrDefaultBool("PELICAN_TEST_ENABLED", true) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "code": "PELICAN_TEST_DISABLED", "message": "Group tests are disabled on this server"})
		return
	}
	config, err := readPelicanConfig(model.DB.WithContext(c.Request.Context()))
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "code": "PELICAN_TEST_UNAVAILABLE", "message": "Monitor configuration unavailable"})
		return
	}
	target, ok := config.targetByID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "code": "PELICAN_TARGET_NOT_FOUND", "message": "Monitoring group not found"})
		return
	}
	payload := pelicanTargetTestPayload{TargetID: target.ID}
	task, created, err := service.EnqueueSystemTask(manualPelicanTestHandler{}.Type(), payload)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "code": "PELICAN_TEST_UNAVAILABLE", "message": "Test queue unavailable"})
		return
	}
	if !created {
		var active pelicanTargetTestPayload
		if err := task.DecodePayload(&active); err != nil || active.TargetID != target.ID {
			c.JSON(http.StatusConflict, gin.H{"success": false, "code": "PELICAN_TEST_BUSY", "message": "Another group test is already queued or running"})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"task_id": task.TaskID, "created": created}})
}

func (manualPelicanTestHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	var payload pelicanTargetTestPayload
	if err := task.DecodePayload(&payload); err != nil || payload.TargetID == "" {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, errors.New("invalid test target"))
		return
	}
	if !common.GetEnvOrDefaultBool("PELICAN_TEST_ENABLED", true) {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, errors.New("group tests disabled"))
		return
	}
	config, err := readPelicanConfig(model.DB.WithContext(ctx))
	if err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, errors.New("monitor configuration unavailable"))
		return
	}
	target, ok := config.targetByID(payload.TargetID)
	if !ok {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, errors.New("monitoring group not found"))
		return
	}
	runPelicanTests(ctx, task, runnerID, []pelicanGroupTarget{target})
}

type pelicanTestHandler struct{}

func (pelicanTestHandler) Type() string { return pelicanTaskType }
func (pelicanTestHandler) Enabled() bool {
	config, err := readPelicanConfig(model.DB)
	return err == nil && config.Enabled && common.GetEnvOrDefaultBool("PELICAN_TEST_ENABLED", true)
}
func (pelicanTestHandler) Interval() time.Duration { return 10 * time.Minute }
func (pelicanTestHandler) NewPayload() any         { return nil }

func (pelicanTestHandler) ScheduleFromStart() bool { return true }

func (pelicanTestHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	config, err := readPelicanConfig(model.DB)
	if err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, err)
		return
	}
	if !config.Enabled || !common.GetEnvOrDefaultBool("PELICAN_TEST_ENABLED", true) {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusSucceeded, nil, nil)
		return
	}
	err = model.DB.Model(&model.PelicanTestResult{}).Where("status = ? AND started_at < ?", "running", time.Now().Add(-5*time.Minute).UnixMilli()).Updates(map[string]any{"status": "failed", "error": "interrupted", "finished_at": time.Now().UnixMilli()}).Error
	if err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, err)
		return
	}
	targets := slices.DeleteFunc(config.Targets, func(t pelicanGroupTarget) bool { return !t.Enabled })
	runPelicanTests(ctx, task, runnerID, targets)
}

func runPelicanTests(ctx context.Context, task *model.SystemTask, runnerID string, selected []pelicanGroupTarget) {
	type probe struct {
		target pelicanGroupTarget
		name   string
	}
	targets := make(chan probe)
	var workers sync.WaitGroup
	var mu sync.Mutex
	tested, failed := 0, 0
	var persistErr error
	for range 4 {
		workers.Go(func() {
			for item := range targets {
				if ctx.Err() != nil {
					continue
				}
				record, runErr := runPelicanGroupProbe(ctx, task.TaskID, item.target, item.name)
				mu.Lock()
				if runErr != nil {
					persistErr = runErr
				} else {
					tested++
					if record.Status == "failed" {
						failed++
					}
				}
				mu.Unlock()
			}
		})
	}
	for _, target := range selected {
		for _, name := range target.Models {
			select {
			case targets <- probe{target, name}:
			case <-ctx.Done():
			}
		}
	}
	close(targets)
	workers.Wait()
	err := errors.Join(persistErr, ctx.Err())
	status := model.SystemTaskStatusSucceeded
	if err != nil || failed > 0 {
		status = model.SystemTaskStatusFailed
	}
	finishSystemTaskHandler(task, runnerID, status, map[string]int{"tested": tested, "failed": failed}, err)
}

func runPelicanGroupProbe(ctx context.Context, taskID string, target pelicanGroupTarget, modelName string) (record *model.PelicanTestResult, err error) {
	record = &model.PelicanTestResult{TaskID: taskID, TargetID: target.ID, Group: target.Group, Model: modelName, StartedAt: time.Now().UnixMilli(), Status: "running", TemplateVersion: pelicanHTMLTemplate}
	if err := model.DB.Create(record).Error; err != nil {
		return nil, err
	}
	defer func() {
		if recover() != nil {
			record.Status = "failed"
			record.Error = "internal_error"
			record.SVG = ""
		}
		record.FinishedAt = time.Now().UnixMilli()
		record.DurationMS = record.FinishedAt - record.StartedAt
		// UPDATE only: deletion while a request is in flight cannot resurrect it.
		err = model.DB.Model(record).Select("status", "error", "svg", "output", "finished_at", "duration_ms", "error_code", "http_status", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "cache_reported").Updates(record).Error
	}()
	record.Status = "failed"
	key, keyErr := pelicanKey(target)
	if keyErr != nil {
		record.Error = "credential_error"
		return record, nil
	}
	if err := validatePelicanTokenGroup(target, key); err != nil {
		record.Error = "group_key_mismatch"
		return record, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	body := map[string]any{"model": modelName, "stream": false}
	if target.Endpoint == "responses" {
		body["input"] = pelicanPrompt
		body["max_output_tokens"] = 8192
		if target.ReasoningEffort != "" {
			body["reasoning"] = map[string]string{"effort": target.ReasoningEffort}
		}
	} else {
		body["messages"] = []map[string]string{{"role": "user", "content": pelicanPrompt}}
		body["max_completion_tokens"] = 8192
		if target.ReasoningEffort != "" {
			body["reasoning_effort"] = target.ReasoningEffort
		}
	}
	payload, marshalErr := common.Marshal(body)
	if marshalErr != nil {
		record.Error = "internal_error"
		return record, nil
	}
	base := strings.TrimRight(target.BaseURL, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	req, requestErr := http.NewRequestWithContext(probeCtx, http.MethodPost, base+"/"+target.Endpoint, bytes.NewReader(payload))
	if requestErr != nil {
		record.Error = "configuration_error"
		return record, nil
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	// Reuse DNS-pinned SSRF protection; never follow redirects with credentials.
	client := *service.GetSSRFProtectedHTTPClient()
	client.Timeout = 120 * time.Second
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, requestErr := client.Do(req)
	if requestErr != nil {
		record.Error = "upstream_error"
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			record.Error = "timeout"
		}
		if errors.Is(probeCtx.Err(), context.Canceled) {
			record.Error = "interrupted"
		}
		return record, nil
	}
	defer response.Body.Close()
	record.HTTPStatus = response.StatusCode
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		record.Error = "upstream_error"
		return record, nil
	}
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if readErr != nil {
		record.Error = "upstream_error"
		if probeCtx.Err() != nil {
			record.Error = "interrupted"
			if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
				record.Error = "timeout"
			}
		}
		return record, nil
	}
	if len(responseBody) > 1<<20 {
		record.Error = "output_too_large"
		return record, nil
	}
	parsed := gjson.ParseBytes(responseBody)
	usage := parsed.Get("usage")
	input := usage.Get("prompt_tokens")
	output := usage.Get("completion_tokens")
	cached := usage.Get("prompt_tokens_details.cached_tokens")
	if target.Endpoint == "responses" {
		input = usage.Get("input_tokens")
		output = usage.Get("output_tokens")
		cached = usage.Get("input_tokens_details.cached_tokens")
	}
	// These are observational response counts, never billing quantities. Invalid
	// or missing counters remain unknown, including missing cache metadata.
	if input.Type == gjson.Number && input.Int() >= 0 && input.Int() <= 1_000_000_000 {
		record.InputTokens = input.Int()
	}
	if output.Type == gjson.Number && output.Int() >= 0 && output.Int() <= 1_000_000_000 {
		record.OutputTokens = output.Int()
	}
	if cached.Type == gjson.Number && cached.Int() >= 0 && cached.Int() <= record.InputTokens {
		record.CacheReadTokens = cached.Int()
		record.CacheReported = true
	}
	write := usage.Get("cache_creation_input_tokens")
	if write.Type == gjson.Number && write.Int() >= 0 && write.Int() <= record.InputTokens {
		record.CacheWriteTokens = write.Int()
	}
	outputText := pelicanResponseText(responseBody)
	if pelicanCredentialEcho(outputText, key) {
		record.Error = "unsafe_html"
		return record, nil
	}
	record.Output = strings.ToValidUTF8(outputText[:min(len(outputText), service.MaxPelicanSVGBytes)], "")
	svg, validationErr := service.PelicanDrawingFromHTML(outputText)
	if parsed.Get("choices.0.finish_reason").String() == "length" || parsed.Get("status").String() == "incomplete" {
		validationErr = errors.New("truncated_output")
	}
	if validationErr != nil {
		record.Error = validationErr.Error()
		return record, nil
	}
	record.SVG = svg
	record.Status = "pending_review"
	return record, nil
}

func pelicanResponseText(body []byte) string {
	response := gjson.ParseBytes(body)
	if text := response.Get("choices.0.message.content"); text.Type == gjson.String {
		return text.String()
	}
	var output strings.Builder
	for _, item := range response.Get("output").Array() {
		for _, content := range item.Get("content").Array() {
			if content.Get("type").String() == "output_text" {
				output.WriteString(content.Get("text").String())
			}
		}
	}
	if output.Len() > 0 {
		return output.String()
	}
	for _, content := range response.Get("content").Array() {
		if content.Get("type").String() == "text" {
			output.WriteString(content.Get("text").String())
		}
	}
	if output.Len() > 0 {
		return output.String()
	}
	for _, part := range response.Get("candidates.0.content.parts").Array() {
		if !part.Get("thought").Bool() {
			output.WriteString(part.Get("text").String())
		}
	}
	return output.String()
}

var pelicanTokenPattern = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)

// A gateway can echo its Authorization value even inside a valid SVG drawing.
// Check literal output, decoded attributes, and text split across SVG elements.
func pelicanCredentialEcho(output, key string) bool {
	secret := strings.TrimPrefix(key, "sk-")
	containsCredential := func(value string) bool {
		return (secret != "" && strings.Contains(value, secret)) || pelicanTokenPattern.MatchString(value)
	}
	if containsCredential(output) || containsCredential(html.UnescapeString(output)) {
		return true
	}
	start := strings.Index(output, "<svg")
	if start < 0 {
		return false
	}
	decoder := xml.NewDecoder(strings.NewReader(output[start:]))
	var visibleText strings.Builder
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch node := token.(type) {
		case xml.StartElement:
			for _, attr := range node.Attr {
				if containsCredential(attr.Value) {
					return true
				}
			}
		case xml.CharData:
			visibleText.Write(node)
		}
	}
	return containsCredential(visibleText.String())
}

// Public results use an explicit field allowlist. Stored errors and upstream
// output from earlier versions can contain diagnostics supplied by a provider.
type publicPelicanResult struct {
	ID         int64  `json:"id"`
	TargetID   string `json:"target_id"`
	Group      string `json:"group"`
	Model      string `json:"model"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
	DurationMS int64  `json:"duration_ms"`
	Status     string `json:"status"`
	Error      string `json:"error"`
	HTTPStatus int    `json:"http_status,omitempty"`
	SVG        string `json:"svg,omitempty"`
	HTML       string `json:"html,omitempty"`
}

func publicPelicanMetadata(row *model.PelicanTestResult) publicPelicanResult {
	result := publicPelicanResult{
		ID: row.ID, TargetID: row.TargetID, Group: row.Group, Model: row.Model,
		StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, DurationMS: row.DurationMS,
		Status: row.Status,
	}
	switch result.Status {
	case "pending_review", "running":
	case "failed":
		switch row.Error {
		case "credential_error", "group_key_mismatch", "timeout", "interrupted", "unsupported_model",
			"invalid_html", "unsafe_html", "invalid_svg", "unsafe_svg", "output_too_large", "truncated_output", "missing_animation",
			"internal_error", "configuration_error":
			result.Error = row.Error
		default:
			result.Error = "upstream_error"
		}
	default:
		result.Status, result.Error = "failed", "upstream_error"
	}
	if row.HTTPStatus >= 100 && row.HTTPStatus <= 599 {
		result.HTTPStatus = row.HTTPStatus
	}
	return result
}

// Public read-only API: no trigger, no credentials, no raw upstream errors/output.
func GetPelicanResults(c *gin.Context) {
	before, err := strconv.ParseInt(c.DefaultQuery("before", "0"), 10, 64)
	if err != nil || before < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid cursor"})
		return
	}
	db := model.DB.WithContext(c.Request.Context())
	config, configErr := readPelicanConfig(db)
	if configErr != nil {
		c.JSON(503, gin.H{"success": false, "message": "Monitor configuration unavailable"})
		return
	}
	allowed := db.Where("1 = 0")
	requested := c.Query("target")
	targets := make(map[string]pelicanGroupTarget, len(config.Targets))
	for _, target := range config.Targets {
		if requested != "" && target.ID != requested {
			continue
		}
		binding := map[string]any{"target_id": target.ID, "group": target.Group}
		if common.UsingMainDatabase(common.DatabaseTypeMySQL) {
			// The public group binding is exact even on a case/accent-insensitive database.
			for column, value := range binding {
				binding[column] = gorm.Expr("CAST(? AS BINARY)", value)
			}
		}
		allowed = allowed.Or(binding)
		targets[target.ID] = target
	}
	query := db.Model(&model.PelicanTestResult{}).Omit("output", "task_id", "channel_id", "channel_name").Where("template_version IN ?", pelicanGroupTemplates).Where(allowed)
	if before > 0 {
		var anchor model.PelicanTestResult
		if err := query.Session(&gorm.Session{}).Select("id", "started_at").First(&anchor, before).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid cursor"})
			return
		}
		query = query.Where("started_at < ? OR (started_at = ? AND id < ?)", anchor.StartedAt, anchor.StartedAt, anchor.ID)
	}
	var results []model.PelicanTestResult
	if err := query.Order("started_at desc").Order("id desc").Limit(21).Find(&results).Error; err != nil {
		common.ApiError(c, fmt.Errorf("test results unavailable"))
		return
	}
	hasMore := len(results) > 20
	if hasMore {
		results = results[:20]
	}
	items := make([]publicPelicanResult, 0, len(results))
	keys := make(map[string]string)
	for i := range results {
		row := &results[i]
		item := publicPelicanMetadata(row)
		if item.Status == "pending_review" && row.SVG != "" {
			key, loaded := keys[row.TargetID]
			if !loaded {
				key, _ = pelicanKey(targets[row.TargetID])
				keys[row.TargetID] = key
			}
			switch {
			case key == "":
				item.Status, item.Error = "failed", "credential_error"
			case pelicanCredentialEcho(row.SVG, key):
				item.Status, item.Error = "failed", "unsafe_svg"
				if row.TemplateVersion == pelicanHTMLTemplate {
					item.Error = "unsafe_html"
				}
			case row.TemplateVersion == pelicanHTMLTemplate:
				if document, err := service.PelicanHTMLPreview(row.SVG); err == nil {
					item.HTML = document
				} else {
					item.Status, item.Error = "failed", "unsafe_html"
				}
			default:
				if svg, err := service.ValidatePelicanSVG(row.SVG); err == nil {
					item.SVG = svg
				} else {
					item.Status, item.Error = "failed", "unsafe_svg"
				}
			}
		}
		items = append(items, item)
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"items": items, "has_more": hasMore, "interval_minutes": 10, "enabled": config.Enabled && common.GetEnvOrDefaultBool("PELICAN_TEST_ENABLED", true)}})
}

type pelicanTrendPoint struct {
	service.GroupRatePoint
	ErrorRate *float64 `json:"error_rate"`
}

func pelicanTrafficPoint(point service.GroupRatePoint, errorRateAvailable bool) pelicanTrendPoint {
	public := pelicanTrendPoint{GroupRatePoint: point}
	if errorRateAvailable {
		public.ErrorRate = service.SamplePercentage(point.Errors, point.Requests)
	}
	return public
}

// Public summaries expose only groups deliberately configured for monitoring.
// Local cache statistics use gateway logs; remote credentials expose probe
// usage only and must never be represented as all traffic at the remote site.
func GetPelicanGroups(c *gin.Context) {
	config, err := readPelicanConfig(model.DB.WithContext(c.Request.Context()))
	if err != nil {
		c.JSON(503, gin.H{"success": false, "message": "Monitor configuration unavailable"})
		return
	}
	type groupSummary struct {
		ID            string                   `json:"id"`
		Group         string                   `json:"group"`
		Models        []string                 `json:"models"`
		Enabled       bool                     `json:"enabled"`
		GroupVerified bool                     `json:"group_verified"`
		Latest        *publicPelicanResult     `json:"latest"`
		ModelResults  []publicPelicanResult    `json:"model_results"`
		Cache         service.GroupCacheHealth `json:"cache"`
		CacheSource   string                   `json:"cache_source"`
		ErrorRate     *float64                 `json:"error_rate"`
		Trend         []pelicanTrendPoint      `json:"trend"`
		FinishedAt    int64                    `json:"finished_at"`
		Truncated     bool                     `json:"truncated"`
	}
	items := make([]groupSummary, 0, len(config.Targets))
	ids := make([]string, 0, len(config.Targets))
	for _, target := range config.Targets {
		ids = append(ids, target.ID)
	}
	var rows []model.PelicanTestResult
	if len(ids) > 0 {
		err = model.DB.WithContext(c.Request.Context()).Omit("svg", "output", "task_id", "channel_id", "channel_name").Where("target_id IN ? AND template_version IN ?", ids, pelicanGroupTemplates).Order("started_at desc").Order("id desc").Limit(50001).Find(&rows).Error
		if err != nil {
			c.JSON(503, gin.H{"success": false, "message": "Test results unavailable"})
			return
		}
	}
	truncated := len(rows) > 50000
	if truncated {
		rows = rows[:50000]
	}
	var localReport *service.GroupCacheReport
	for _, target := range config.Targets {
		if localPelicanTarget(target) {
			localReport, _ = service.GetGroupCacheReport(c.Request.Context())
			break
		}
	}
	rowsByTarget := make(map[string][]*model.PelicanTestResult, len(ids))
	for i := range rows {
		rowsByTarget[rows[i].TargetID] = append(rowsByTarget[rows[i].TargetID], &rows[i])
	}
	const daySeconds = int64(24 * 60 * 60)
	finishedAt := time.Now().Unix()
	startedAt := finishedAt - daySeconds
	common.OptionMapRWMutex.RLock()
	localErrorRateAvailable := constant.ErrorLogEnabled && common.LogConsumeEnabled
	common.OptionMapRWMutex.RUnlock()
	for _, target := range config.Targets {
		summary := groupSummary{
			ID: target.ID, Group: target.Group, Models: target.Models, Enabled: target.Enabled,
			ModelResults: []publicPelicanResult{}, Cache: service.GroupCacheHealth{Group: target.Group},
			CacheSource: "probe", Truncated: truncated, FinishedAt: finishedAt, Trend: []pelicanTrendPoint{},
		}
		isLocal := localPelicanTarget(target)
		seen := make(map[string]bool, len(target.Models))
		probeBuckets := make(map[int64]*service.GroupCacheHealth)
		for _, row := range rowsByTarget[target.ID] {
			if row.Group != target.Group || !slices.Contains(target.Models, row.Model) {
				continue
			}
			if !seen[row.Model] {
				seen[row.Model] = true
				result := publicPelicanMetadata(row)
				summary.ModelResults = append(summary.ModelResults, result)
				if summary.Latest == nil {
					summary.Latest = &result
				}
			}
			stamp := row.StartedAt / 1000
			if isLocal || row.Status == "running" || stamp < startedAt || stamp > finishedAt {
				continue
			}
			// The inclusive right endpoint belongs to the last five-minute bucket.
			index := (min(stamp, finishedAt-1) - startedAt) / service.GroupCacheBucketSeconds
			if probeBuckets[index] == nil {
				probeBuckets[index] = &service.GroupCacheHealth{}
			}
			for _, totals := range [2]*service.GroupCacheHealth{&summary.Cache, probeBuckets[index]} {
				totals.Requests++
				if row.Status == "failed" {
					totals.Errors++
				}
				totals.InputTokens += row.InputTokens
				totals.OutputTokens += row.OutputTokens
				if row.CacheReported {
					totals.CacheSamples++
					totals.CacheInputTokens += row.InputTokens
					totals.CacheReadTokens += row.CacheReadTokens
					totals.CacheWriteTokens += row.CacheWriteTokens
				}
			}
		}
		if isLocal {
			summary.CacheSource = "unavailable"
			if localReport != nil {
				summary.CacheSource = "local"
				summary.Truncated = localReport.Truncated
				summary.FinishedAt = localReport.FinishedAt
				for _, row := range localReport.Groups {
					if row.Group == target.Group {
						summary.Cache = row
						break
					}
				}
				for _, series := range localReport.Trends {
					if series.Group != target.Group {
						continue
					}
					for _, point := range series.Points {
						if point.Requests > 0 {
							summary.Trend = append(summary.Trend, pelicanTrafficPoint(point.GroupRatePoint, localErrorRateAvailable))
						}
					}
					break
				}
			}
			if key, err := pelicanKey(target); err == nil {
				summary.GroupVerified = validatePelicanTokenGroup(target, key) == nil
			}
		} else {
			for i := range daySeconds / service.GroupCacheBucketSeconds {
				if bucket := probeBuckets[i]; bucket != nil {
					point := bucket.RatePoint(startedAt + i*service.GroupCacheBucketSeconds)
					summary.Trend = append(summary.Trend, pelicanTrafficPoint(point, true))
				}
			}
			summary.Cache.CacheHitRate = service.SamplePercentage(summary.Cache.CacheReadTokens, summary.Cache.CacheInputTokens)
		}
		if summary.CacheSource == "probe" || (summary.CacheSource == "local" && localErrorRateAvailable) {
			summary.ErrorRate = service.SamplePercentage(summary.Cache.Errors, summary.Cache.Requests)
		}
		items = append(items, summary)
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"success": true, "data": gin.H{"groups": items, "enabled": config.Enabled && common.GetEnvOrDefaultBool("PELICAN_TEST_ENABLED", true), "interval_minutes": 10}})
}
