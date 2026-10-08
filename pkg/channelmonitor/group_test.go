package channelmonitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGroupSnapshotUsesActualGroupAndHidesUnpublishedGroups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		if !strings.Contains(query, "by(group)") && !strings.Contains(query, "by(le,group)") {
			t.Error("group query is not grouped by actual request group")
		}
		if !strings.Contains(query, `traffic="business",group!=""`) {
			t.Error("legacy or probe metrics included")
		}
		n := "20"
		if strings.Contains(query, `outcome="failure"`) {
			n = "2"
		}
		if strings.Contains(query, "histogram_quantile") {
			n = "1.5"
		}
		w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"group":"public"},"value":[1,"` + n + `"]},{"metric":{"group":"private"},"value":[1,"999"]}]}}`))
	}))
	defer srv.Close()
	t.Setenv("CHANNEL_MONITOR_PROMETHEUS", srv.URL)
	result, err := GroupSnapshot(context.Background(), []GroupHealth{{Name: "public", Models: []string{"model"}}, {Name: "empty", Models: []string{}}}, "5m")
	if err != nil || !result.Available || len(result.Groups) != 2 {
		t.Fatalf("unexpected response: %+v %v", result, err)
	}
	row := result.Groups[0]
	if row.Requests != 20 || row.Failures != 2 || row.FailureRate == nil || *row.FailureRate != 10 {
		t.Fatalf("incorrect weighted failure rate: %+v", row)
	}
	if result.Groups[1].FailureRate != nil || result.Groups[1].P95 != nil {
		t.Fatal("empty group incorrectly shown as healthy")
	}
}
func TestGroupSnapshotOutageIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	t.Setenv("CHANNEL_MONITOR_PROMETHEUS", srv.URL)
	result, err := GroupSnapshot(context.Background(), []GroupHealth{{Name: "public"}}, "1h")
	if err == nil || result.Available || result.Groups[0].FailureRate != nil {
		t.Fatal("outage shown as healthy")
	}
}
func TestGroupFiveMinuteHistoryIsAlignedAndAverageIsWeighted(t *testing.T) {
	at := time.Unix(1790762400, 0).UTC().Truncate(5 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("time") != strconv.FormatInt(at.Unix(), 10) {
			t.Error("query is not pinned to completed interval")
		}
		query := r.URL.Query().Get("query")
		if strings.Contains(r.URL.Path, "query_range") {
			if r.URL.Query().Get("step") != "300" {
				t.Error("history is not fixed to 5 minutes")
			}
			n := "10"
			if strings.Contains(query, `outcome="failure"`) {
				n = "2"
			}
			w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"group":"g"},"values":[[` + strconv.FormatInt(at.Unix(), 10) + `,"` + n + `"]]}]}}`))
			return
		}
		n := "10"
		if strings.Contains(query, `outcome="failure"`) {
			n = "2"
		}
		if strings.Contains(query, "_sum{") {
			n = "1.25"
		}
		w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"group":"g"},"value":[1,"` + n + `"]}]}}`))
	}))
	defer srv.Close()
	t.Setenv("CHANNEL_MONITOR_PROMETHEUS", srv.URL)
	result, err := GroupSnapshotAt(context.Background(), []GroupHealth{{Name: "g"}}, "1h", at)
	if err != nil {
		t.Fatal(err)
	}
	row := result.Groups[0]
	if row.Average == nil || *row.Average != 1.25 || len(row.History) != 12 || row.History[11].Requests != 10 || row.History[11].FailureRate == nil || *row.History[11].FailureRate != 20 || row.History[0].FailureRate != nil {
		t.Fatalf("bad snapshot: %+v", row)
	}
}
