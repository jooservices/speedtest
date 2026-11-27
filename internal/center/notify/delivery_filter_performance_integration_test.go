//go:build integration

package notify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const deliveryFilterBenchmarkRows = 5001

type deliveryPlanNode struct {
	NodeType         string             `json:"Node Type"`
	Relation         string             `json:"Relation Name"`
	IndexName        string             `json:"Index Name"`
	ActualRows       float64            `json:"Actual Rows"`
	ActualLoops      float64            `json:"Actual Loops"`
	SharedHitBlocks  int                `json:"Shared Hit Blocks"`
	SharedReadBlocks int                `json:"Shared Read Blocks"`
	Plans            []deliveryPlanNode `json:"Plans"`
}

type deliveryPlan struct {
	ExecutionTime float64          `json:"Execution Time"`
	Plan          deliveryPlanNode `json:"Plan"`
}

func TestDeliveryFilterQueryPlansStayBoundedOnLargeTableIntegration(t *testing.T) {
	pool, ctx := database(t)
	seedDeliveryFilterBenchmark(t, pool, ctx)

	cases := []struct {
		name, event, status, channel, indexName string
		wantRows                                int
	}{
		{"event", "a0000000-0000-4000-8000-000000001388", "", "", "event_id_channel", 1},
		{"status-only", "", "failed", "", "status_created", 50},
		{"channel-only", "", "", "telegram", "channel_created", 50},
		{"status-and-channel", "", "failed", "telegram", "status_channel_created", 50},
		{"event-status-channel", "a0000000-0000-4000-8000-000000001388", "failed", "email", "event_id_channel", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count := explainDeliveryPlan(t, pool, ctx, "count", deliveryCountSQL, tc.event, tc.status, tc.channel)
			assertDeliveryCountPlan(t, tc.name, tc.indexName, count)
			page := explainDeliveryPlan(t, pool, ctx, "page", deliveryPageSQL, tc.event, tc.status, tc.channel, tc.wantRows, 0)
			assertDeliveryPagePlan(t, tc.name, tc.indexName, tc.wantRows, page)
		})
	}
}

func seedDeliveryFilterBenchmark(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	const nodeID = "10000000-0000-4000-8000-000000000124"
	execute(t, pool, `INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'delivery-plan-edge',100,50)`, nodeID)
	execute(t, pool, `INSERT INTO alert_events(id,node_id,severity,ts,title)
SELECT ('a0000000-0000-4000-8000-'||lpad(to_hex(n),12,'0'))::uuid,$1,'warn','2026-11-01T00:00:00Z','Delivery plan fixture'
FROM generate_series(1,$2::int) n`, nodeID, deliveryFilterBenchmarkRows)
	execute(t, pool, `INSERT INTO notification_deliveries(id,event_id,channel,status,attempts,created_at,next_attempt_at)
SELECT ('f0000000-0000-4000-8000-'||lpad(to_hex(n),12,'0'))::uuid,
       ('a0000000-0000-4000-8000-'||lpad(to_hex(n),12,'0'))::uuid,
       (ARRAY['email','telegram','slack','webhook'])[(n%4)+1],
       (ARRAY['pending','sent','failed'])[(n%3)+1],
       0,
       '2026-11-01T00:00:00Z'::timestamptz + (n||' seconds')::interval,
       '2026-11-01T00:00:00Z'::timestamptz + (n||' seconds')::interval
FROM generate_series(1,$1::int) n`, deliveryFilterBenchmarkRows)
	if _, err := pool.Exec(ctx, "VACUUM (ANALYZE) notification_deliveries"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries`).Scan(&count); err != nil || count != deliveryFilterBenchmarkRows {
		t.Fatalf("benchmark rows = %d, want %d: %v", count, deliveryFilterBenchmarkRows, err)
	}
}

func explainDeliveryPlan(t *testing.T, pool *pgxpool.Pool, ctx context.Context, name, sql string, args ...any) deliveryPlan {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("could not explain the %s query: %v", name, err)
	}
	var plans []deliveryPlan
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("invalid %s EXPLAIN JSON: %v", name, err)
	}
	return plans[0]
}

func deliveryPlanNodes(node deliveryPlanNode, nodes *[]deliveryPlanNode) {
	*nodes = append(*nodes, node)
	for _, child := range node.Plans {
		deliveryPlanNodes(child, nodes)
	}
}

func assertDeliveryCountPlan(t *testing.T, path, indexName string, plan deliveryPlan) {
	t.Helper()
	nodes := deliveryPlanNodesOf(plan)
	assertNoSequentialScan(t, path, nodes)
	index := deliveryPlanIndex(t, path, nodes)
	if !strings.Contains(index, indexName) {
		t.Fatalf("%s count uses index %q, want an index containing %q", path, index, indexName)
	}
	if blocks := plan.Plan.SharedHitBlocks + plan.Plan.SharedReadBlocks; blocks > 64 {
		t.Fatalf("%s count read %d shared blocks, want at most 64", path, blocks)
	}
	t.Logf("EXPLAIN %s count: %.2f ms, index=%s, shared blocks=%d", path, plan.ExecutionTime, index, plan.Plan.SharedHitBlocks+plan.Plan.SharedReadBlocks)
}

func assertDeliveryPagePlan(t *testing.T, path, indexName string, wantRows int, plan deliveryPlan) {
	t.Helper()
	nodes := deliveryPlanNodesOf(plan)
	assertNoSequentialScan(t, path, nodes)
	index := deliveryPlanIndex(t, path, nodes)
	if !strings.Contains(index, indexName) {
		t.Fatalf("%s page uses index %q, want an index containing %q", path, index, indexName)
	}
	if plan.Plan.NodeType != "Limit" || plan.Plan.ActualRows > float64(wantRows) {
		t.Fatalf("%s page plan root = %s rows=%.0f, want Limit at most %d", path, plan.Plan.NodeType, plan.Plan.ActualRows, wantRows)
	}
	if blocks := plan.Plan.SharedHitBlocks + plan.Plan.SharedReadBlocks; blocks > 32 {
		t.Fatalf("%s page read %d shared blocks, want at most 32", path, blocks)
	}
	t.Logf("EXPLAIN %s page: %.2f ms, index=%s, rows=%.0f, shared blocks=%d", path, plan.ExecutionTime, index, plan.Plan.ActualRows, plan.Plan.SharedHitBlocks+plan.Plan.SharedReadBlocks)
}

func deliveryPlanNodesOf(plan deliveryPlan) []deliveryPlanNode {
	var nodes []deliveryPlanNode
	deliveryPlanNodes(plan.Plan, &nodes)
	return nodes
}

func assertNoSequentialScan(t *testing.T, path string, nodes []deliveryPlanNode) {
	t.Helper()
	for _, node := range nodes {
		if node.NodeType == "Seq Scan" && node.Relation == "notification_deliveries" {
			t.Fatalf("%s plan contains a sequential scan: %+v", path, node)
		}
	}
}

func deliveryPlanIndex(t *testing.T, path string, nodes []deliveryPlanNode) string {
	t.Helper()
	for _, node := range nodes {
		if node.Relation == "notification_deliveries" && (node.NodeType == "Index Scan" || node.NodeType == "Index Only Scan") {
			return node.IndexName
		}
	}
	t.Fatalf("%s plan does not use an index on notification_deliveries: %+v", path, nodes)
	return ""
}
