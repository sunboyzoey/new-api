package controller

import (
	"github.com/QuantumNous/new-api/common"
	cm "github.com/QuantumNous/new-api/pkg/channelmonitor"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"testing"
)

func TestPublicGroupFrequencyCannotBeChangedByQuery(t *testing.T) {
	groupMonitorCache.Lock()
	old := groupMonitorCache.snapshot
	groupMonitorCache.snapshot = cm.GroupDashboard{Window: "1h", IntervalSeconds: 300, Groups: []cm.GroupHealth{}}
	groupMonitorCache.Unlock()
	defer func() { groupMonitorCache.Lock(); groupMonitorCache.snapshot = old; groupMonitorCache.Unlock() }()
	r := gin.New()
	r.GET("/group", GetGroupMonitor)
	for _, query := range []string{"", "?window=5m&interval=1", "?window=24h&frequency=1"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/group"+query, nil))
		var body struct {
			Data cm.GroupDashboard `json:"data"`
		}
		if err := common.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Data.Window != "1h" || body.Data.IntervalSeconds != 300 {
			t.Fatal("visitor can change frequency")
		}
	}
}
