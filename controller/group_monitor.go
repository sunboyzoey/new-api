package controller

import (
	"context"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	cm "github.com/QuantumNous/new-api/pkg/channelmonitor"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

var groupMonitorCache = struct {
	sync.RWMutex
	snapshot cm.GroupDashboard
}{snapshot: cm.GroupDashboard{Window: "1h", IntervalSeconds: 300, Groups: []cm.GroupHealth{}}}

func publicMonitorGroups() []cm.GroupHealth {
	usable := service.GetUserUsableGroups("")
	models := map[string]map[string]bool{}
	for _, item := range model.GetPricing() {
		for group := range usable {
			for _, enabledGroup := range item.EnableGroup {
				if enabledGroup == group || enabledGroup == "all" {
					if models[group] == nil {
						models[group] = map[string]bool{}
					}
					models[group][item.ModelName] = true
				}
			}
		}
	}
	groups := []cm.GroupHealth{}
	for group, names := range models {
		row := cm.GroupHealth{Name: group, Models: []string{}}
		for name := range names {
			row.Models = append(row.Models, name)
		}
		sort.Strings(row.Models)
		groups = append(groups, row)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	return groups
}
func monitorEvaluationChannels() ([]cm.EvaluationChannel, error) {
	channels, err := model.GetAllChannels(0, 1000, false, true)
	result := []cm.EvaluationChannel{}
	if err != nil {
		return result, err
	}
	for _, ch := range channels {
		if ch.Status == 1 {
			result = append(result, cm.EvaluationChannel{ID: ch.Id, Groups: cm.SplitMonitorConfiguration(ch.Group), Models: cm.SplitMonitorConfiguration(ch.Models)})
		}
	}
	return result, nil
}
func buildGroupMonitorSnapshot(ctx context.Context, at time.Time) cm.GroupDashboard {
	snapshot, _ := cm.GroupSnapshotAt(ctx, publicMonitorGroups(), "1h", at)
	channels, err := monitorEvaluationChannels()
	jobs := []cm.Evaluation{}
	if evaluator, e := getMonitorEvaluator(); e == nil {
		jobs = evaluator.List(true)
		snapshot.QualityAvailable = err == nil
	}
	cm.AttachGroupQuality(snapshot.Groups, channels, jobs)
	return snapshot
}
func startGroupMonitor() func() {
	ctx, cancel := context.WithCancel(context.Background())
	dir := os.Getenv("CHANNEL_MONITOR_DATA_DIR")
	if dir == "" {
		dir = "/root/new-api/data/channel-monitor"
	}
	path := filepath.Join(dir, "group-snapshot.json")
	if raw, err := os.ReadFile(path); err == nil {
		var snapshot cm.GroupDashboard
		if common.Unmarshal(raw, &snapshot) == nil && snapshot.IntervalSeconds == 300 && snapshot.Window == "1h" {
			if time.Since(snapshot.UpdatedAt) > 10*time.Minute {
				snapshot.Available = false
				snapshot.QualityAvailable = false
			}
			groupMonitorCache.Lock()
			groupMonitorCache.snapshot = snapshot
			groupMonitorCache.Unlock()
		}
	}
	go func() {
		for {
			at := time.Now().UTC().Truncate(5 * time.Minute)
			runCtx, stop := context.WithTimeout(ctx, 15*time.Second)
			snapshot := buildGroupMonitorSnapshot(runCtx, at)
			stop()
			if ctx.Err() != nil {
				return
			}
			groupMonitorCache.Lock()
			groupMonitorCache.snapshot = snapshot
			groupMonitorCache.Unlock()
			if raw, err := common.Marshal(snapshot); err == nil {
				if os.MkdirAll(dir, 0700) == nil && os.WriteFile(path+".tmp", raw, 0600) == nil {
					_ = os.Rename(path+".tmp", path)
				}
			}
			timer := time.NewTimer(time.Until(at.Add(5 * time.Minute)))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return cancel
}

// Public clients can only read the fixed background snapshot, not change its window/frequency.
func GetGroupMonitor(c *gin.Context) {
	groupMonitorCache.RLock()
	snapshot := groupMonitorCache.snapshot
	groupMonitorCache.RUnlock()
	message := ""
	if !snapshot.Available {
		message = "Monitoring backend unavailable"
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": snapshot, "message": message})
}
func GetGroupDrawings(c *gin.Context) {
	group, name := c.Query("group"), c.Query("model")
	allowed := false
	for _, row := range publicMonitorGroups() {
		if row.Name == group && slices.Contains(row.Models, name) {
			allowed = true
			break
		}
	}
	if !allowed {
		c.JSON(404, gin.H{"success": false, "message": "Group or model not found"})
		return
	}
	channels, err := monitorEvaluationChannels()
	evaluator, e := getMonitorEvaluator()
	if err != nil || e != nil {
		c.JSON(503, gin.H{"success": false, "message": "Test results unavailable"})
		return
	}
	drawings := cm.GroupDrawings(group, name, channels, evaluator.List(false))
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"success": true, "data": drawings})
}
