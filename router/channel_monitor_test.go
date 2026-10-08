package router

import (
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"testing"
)

func TestChannelMonitoringRoutesArePrivate(t *testing.T) {
	r := gin.New()
	SetApiRouter(r)
	for _, path := range []string{"/api/channel-monitor", "/api/channel-monitor/admin", "/api/channel-monitor/grafana/d/newapi-channels"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 401 && w.Code != 403 {
				t.Fatalf("visitor could access %s: %d", path, w.Code)
			}
		})
	}
}
