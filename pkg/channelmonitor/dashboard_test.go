package channelmonitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSnapshotEmptyWindowIsAvailableButHealthUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer srv.Close()
	t.Setenv("CHANNEL_MONITOR_PROMETHEUS", srv.URL)
	data, err := Snapshot(context.Background(), []Channel{{ID: 128, Name: "channel", Models: []string{"mock"}}}, "invalid")
	if err != nil || !data.Available || data.Window != "1h" || len(data.Channels) != 1 || data.Channels[0].FailureRate != nil || data.Channels[0].P95 != nil {
		t.Fatalf("empty window cannot imply healthy: %+v %v", data, err)
	}
}
func TestSnapshotBackendFailureLeavesHealthUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	t.Setenv("CHANNEL_MONITOR_PROMETHEUS", srv.URL)
	data, err := Snapshot(context.Background(), []Channel{{ID: 128}}, "5m")
	if err == nil || data.Available || data.Channels[0].FailureRate != nil {
		t.Fatal("backend outage shown as healthy")
	}
}
