//go:build integration

package alerts_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/alerts"
)

func TestMeasurementRulesKeepEngineStreaksIndependent(t *testing.T) {
	pool, ctx := integrationDB(t)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	addNode(t, ctx, pool, testNodeID, "engines", now, 30)
	store := &alerts.Store{DB: pool, AppKey: []byte(appKey)}
	primary := addRule(t, ctx, store, "primary", alerts.RuleInput{Name: "Primary failures", Metric: "failed", Op: "=", Value: 1, ForTests: 2, AllNodes: true, Severity: "warn", Channels: []string{"email"}, Enabled: true})
	libre := addRule(t, ctx, store, "libre", alerts.RuleInput{Engine: "librespeed", Name: "Libre failures", Metric: "failed", Op: "=", Value: 1, ForTests: 2, AllNodes: true, Severity: "warn", Channels: []string{"email"}, Enabled: true})
	agent := &agentapi.Postgres{Pool: pool, Hook: store}
	for i, name := range []string{"ookla", "librespeed", "ookla", "librespeed"} {
		item := testResult(fmt.Sprintf("71000000-0000-4000-8000-%012x", i+1), now.Add(time.Duration(i)*time.Minute), "failed", nil, 0)
		item.Engine = name
		if _, err := agent.SaveResult(ctx, testNodeID, item); err != nil {
			t.Fatal(err)
		}
		if i == 1 && scalar(t, ctx, pool, `SELECT count(*) FROM alert_events`) != 0 {
			t.Fatal("engines advanced each other's streak")
		}
	}
	for _, id := range []string{primary, libre} {
		if scalar(t, ctx, pool, `SELECT count(*) FROM alert_events WHERE rule_id=$1`, id) != 1 {
			t.Fatal("missing independent event", id)
		}
	}
	t.Log("PASS: omitted-engine rule follows primary; explicit LibreSpeed rule has an independent consecutive-failure streak")
}
