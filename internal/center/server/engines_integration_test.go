//go:build integration

package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/dashboard"
	"github.com/jooservices/speedtest/internal/center/stats"
)

func TestDashboardAndSeriesEngineIsolationHTTPIntegration(t *testing.T) {
	handler, session, exec := dashboardTestHandler(t)
	exec(`INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES ($1,'Engine fixture',1000,100)`, dashAlpha)
	exec(`INSERT INTO schedules(node_id) VALUES ($1)`, dashAlpha)
	exec(`INSERT INTO engine_schedules(node_id,engine) VALUES ($1,'ookla'),($1,'librespeed')`, dashAlpha)
	exec(`INSERT INTO ip_addresses(ip) VALUES ('203.0.113.71')`)
	for i, name := range []string{"ookla", "librespeed"} {
		exec(`INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,threads,engine,ip,server) VALUES (gen_random_uuid(),$1,$2,'ok',$3,10,5,4,$4,'203.0.113.71','{"name":"Fixture"}')`, dashAlpha, time.Now().Add(-time.Minute), 100*(i+1), name)
	}
	for _, name := range []string{"", "ookla", "librespeed"} {
		suffix := ""
		want := 100.0
		if name != "" {
			suffix = "&engine=" + name
		}
		if name == "librespeed" {
			want = 200
		}
		rec := dashboardGet(handler, session, "/api/dashboard?range=24h"+suffix)
		var snap struct {
			Data dashboard.Snapshot `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil || rec.Code != 200 || snap.Data.Current.Count != 1 || len(snap.Data.LatestResults) != 1 || snap.Data.Nodes[0].Summary.Down.Mean == nil || *snap.Data.Nodes[0].Summary.Down.Mean != want {
			t.Fatalf("dashboard %q: %d %s %v", name, rec.Code, rec.Body, err)
		}
		rec = dashboardGet(handler, session, "/api/series?range=24h&maxPoints=1"+suffix)
		var series struct {
			Data []stats.Series `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &series); err != nil || rec.Code != 200 || len(series.Data) != 1 || series.Data[0].Points[0].Value == nil || *series.Data[0].Points[0].Value != want {
			t.Fatalf("series %q: %d %s %v", name, rec.Code, rec.Body, err)
		}
	}
	t.Log("PASS: authenticated HTTP dashboard and series isolate Ookla/LibreSpeed; omitted filter selects primary")
}
