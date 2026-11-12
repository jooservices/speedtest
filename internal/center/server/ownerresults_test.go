package server

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/ownerapi"
)

const resultID = "00000000-0000-4000-8000-0000000000b1"

type fakeOwnerResults struct {
	filter    ownerapi.ResultFilter
	page      int
	size      int
	total     int
	listErr   error
	getErr    error
	rows      []ownerapi.ResultRow
	exportErr error
	failAfter int
}

func (f *fakeOwnerResults) ListResults(_ context.Context, filter ownerapi.ResultFilter, page, size int) ([]map[string]any, int, error) {
	f.filter, f.page, f.size = filter, page, size
	return []map[string]any{{"id": resultID}}, f.total, f.listErr
}
func (f *fakeOwnerResults) GetResult(_ context.Context, id string) (map[string]any, error) {
	return map[string]any{"id": id}, f.getErr
}
func (f *fakeOwnerResults) ExportResults(_ context.Context, filter ownerapi.ResultFilter, emit func(ownerapi.ResultRow) error) error {
	f.filter = filter
	for i, row := range f.rows {
		if f.failAfter > 0 && i == f.failAfter {
			return f.exportErr
		}
		if err := emit(row); err != nil {
			return err
		}
	}
	return f.exportErr
}

func resultsHandlerFor(store OwnerResultsStore) http.Handler {
	h := newNodeHandler(Options{OwnerResults: store, NodeCredentials: &fakeNodeCredentialService{}})
	return h
}

func TestOwnerResultsNeedASession(t *testing.T) {
	h := resultsHandlerFor(&fakeOwnerResults{})
	for _, path := range []string{"/api/results", "/api/results/" + resultID, "/api/results/export.csv"} {
		if rec := do(h, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d", path, rec.Code)
		}
	}
}

func TestOwnerResultsMethodsAndIDs(t *testing.T) {
	h := resultsHandlerFor(&fakeOwnerResults{})
	if rec := do(h, http.MethodPost, "/api/results", "", func(r *http.Request) { ownerRead(r); r.Header.Set("Origin", "http://center.test") }); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/results/not-a-uuid", "", ownerRead); rec.Code != http.StatusNotFound {
		t.Fatalf("bad id = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/results/"+resultID, "", ownerRead); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), resultID) {
		t.Fatalf("detail = %d %s", rec.Code, rec.Body.String())
	}
	missing := resultsHandlerFor(&fakeOwnerResults{getErr: ownerapi.ErrNotFound})
	if rec := do(missing, http.MethodGet, "/api/results/"+resultID, "", ownerRead); rec.Code != http.StatusNotFound {
		t.Fatalf("missing = %d", rec.Code)
	}
	broken := resultsHandlerFor(&fakeOwnerResults{listErr: errors.New("db down")})
	rec := do(broken, http.MethodGet, "/api/results", "", ownerRead)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "db down") {
		t.Fatalf("store failure = %d %s (must not leak the cause)", rec.Code, rec.Body.String())
	}
}

func TestOwnerResultsListPaginationAndFilters(t *testing.T) {
	store := &fakeOwnerResults{total: 51}
	h := resultsHandlerFor(store)
	rec := do(h, http.MethodGet, "/api/results?page=2&size=10&status=failed&q=%20hanoi%20&minDown=10&maxDown=300.5&sort=down&dir=asc&nodes="+nodeID+","+nodeID+"&range=24h", "", ownerRead)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"last_page":6`) || !strings.Contains(rec.Body.String(), `"total":51`) {
		t.Fatalf("pagination = %s", rec.Body.String())
	}
	f := store.filter
	if store.page != 2 || store.size != 10 || f.Status != "failed" || f.Query != "hanoi" || *f.MinDown != 10 || *f.MaxDown != 300.5 ||
		f.Sort != "down" || f.Dir != "asc" || !f.NodesSet || len(f.Nodes) != 1 || f.To.Sub(f.From) < 24*time.Hour {
		t.Fatalf("filter = %+v page %d size %d", f, store.page, store.size)
	}
}

func TestOwnerResultsDefaultsAndExplicitRange(t *testing.T) {
	store := &fakeOwnerResults{}
	h := resultsHandlerFor(store)
	do(h, http.MethodGet, "/api/results", "", ownerRead)
	if store.page != 1 || store.size != 25 || store.filter.NodesSet || store.filter.To.Sub(store.filter.From) < 7*24*time.Hour {
		t.Fatalf("defaults: %+v page %d size %d", store.filter, store.page, store.size)
	}
	do(h, http.MethodGet, "/api/results?nodes=&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z", "", ownerRead)
	if !store.filter.NodesSet || len(store.filter.Nodes) != 0 || store.filter.From.Format(time.RFC3339) != "2026-11-01T00:00:00Z" {
		t.Fatalf("explicit: %+v", store.filter)
	}
}

func TestOwnerResultsRejectInvalidParameters(t *testing.T) {
	h := resultsHandlerFor(&fakeOwnerResults{})
	for _, query := range []string{
		"nodes=nope", "status=great", "minDown=-1", "minDown=abc", "maxDown=NaN", "minDown=50&maxDown=10",
		"sort=password", "dir=up", "range=1y", "range=24h&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z",
		"from=2026-11-01T00:00:00Z", "from=yesterday&to=today", "from=2026-11-02T00:00:00Z&to=2026-11-01T00:00:00Z",
		"from=2024-01-01T00:00:00Z&to=2026-01-01T00:00:00Z", "q=" + strings.Repeat("x", 201),
	} {
		for _, path := range []string{"/api/results?", "/api/results/export.csv?"} {
			rec := do(h, http.MethodGet, path+query, "", ownerRead)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s%s = %d, want 422", path, query, rec.Code)
			}
		}
	}
	for _, query := range []string{"page=0", "size=101", "size=x"} {
		if rec := do(h, http.MethodGet, "/api/results?"+query, "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", query, rec.Code)
		}
	}
	var many []string
	for i := 0; i < 101; i++ {
		many = append(many, fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
	}
	if rec := do(h, http.MethodGet, "/api/results?nodes="+strings.Join(many, ","), "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("101 node IDs = %d, want 422", rec.Code)
	}
}

func TestOwnerResultsExportCSV(t *testing.T) {
	rows := []ownerapi.ResultRow{
		{NodeName: "VM1", NodeLabel: "Home", Result: map[string]any{"ts": "2026-11-03T01:00:00Z", "status": "ok", "down": 250.5, "up": 90.25, "ping": 12.0, "jitter": 1.5,
			"loss": 0.0, "bytesDown": int64(100), "bytesUp": int64(50), "dLat": 20.5, "uLat": 25.0, "ip": "203.0.113.5", "error": nil, "server": map[string]any{"name": "Viettel", "city": "Hanoi"}}},
		{NodeName: "=cmd|' /C calc'!A0", NodeLabel: "x", Result: map[string]any{"ts": "2026-11-03T01:30:00Z", "status": "failed", "down": nil, "ip": nil,
			"error": "@SUM(1,2), \"quoted\"\nnext", "server": map[string]any{"name": "", "city": "Da Nang"}}},
	}
	h := resultsHandlerFor(&fakeOwnerResults{rows: rows})
	rec := do(h, http.MethodGet, "/api/results/export.csv?status=all", "", ownerRead)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), `attachment; filename="speedtest-results-`) {
		t.Fatalf("export = %d %v", rec.Code, rec.Header())
	}
	records, err := csv.NewReader(rec.Body).ReadAll()
	if err != nil || len(records) != 3 {
		t.Fatalf("csv: %v %v", records, err)
	}
	if strings.Join(records[0], ",") != strings.Join(csvHeader, ",") {
		t.Fatalf("header = %v", records[0])
	}
	if got := strings.Join(records[1], "|"); got != "2026-11-03T01:00:00Z|VM1 Home|ok|250.5|90.25|12|1.5|0|100|50|150|20.5|25|Viettel - Hanoi|203.0.113.5||other|" {
		t.Fatalf("row 1 = %s", got)
	}
	second := records[2]
	if !strings.HasPrefix(second[1], "'=cmd") || second[15] != "'@SUM(1,2), \"quoted\"\nnext" || second[3] != "" || second[8] != "" || second[9] != "" || second[10] != "" || second[12] != "" || second[13] != "Da Nang" || second[16] != "other" {
		t.Fatalf("row 2 must neutralise formulas and keep quoting: %q", second)
	}
}

func TestOwnerResultsExportEmptyAndFailures(t *testing.T) {
	rec := do(resultsHandlerFor(&fakeOwnerResults{}), http.MethodGet, "/api/results/export.csv", "", ownerRead)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != strings.Join(csvHeader, ",") {
		t.Fatalf("empty export = %d %q", rec.Code, rec.Body.String())
	}
	early := do(resultsHandlerFor(&fakeOwnerResults{exportErr: errors.New("db down")}), http.MethodGet, "/api/results/export.csv", "", ownerRead)
	if early.Code != http.StatusInternalServerError || strings.Contains(early.Body.String(), "db down") {
		t.Fatalf("failure before the first row = %d %s", early.Code, early.Body.String())
	}
	rows := []ownerapi.ResultRow{{Result: map[string]any{"ts": "t", "status": "ok"}}, {Result: map[string]any{"ts": "t", "status": "ok"}}}
	h := resultsHandlerFor(&fakeOwnerResults{rows: rows, exportErr: errors.New("db down"), failAfter: 1})
	req := httptest.NewRequest(http.MethodGet, "http://center.test/api/results/export.csv", nil)
	ownerRead(req)
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("a failure mid-stream must abort the response, recovered %v", recovered)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), req)
	t.Fatal("handler returned normally")
}

func TestCompletedExportsPublishReadyEvents(t *testing.T) {
	store := &fakeOwnerResults{rows: []ownerapi.ResultRow{{Result: map[string]any{"status": "ok"}}, {Result: map[string]any{"status": "failed"}}}}
	events := &fakeNotificationEvents{}
	h := newNodeHandler(Options{OwnerResults: store, NotificationEvents: events, NodeCredentials: &fakeNodeCredentialService{}})
	for _, path := range []string{"/api/export/results.csv", "/api/export/results.json"} {
		if rec := do(h, http.MethodGet, path, "", ownerRead); rec.Code != http.StatusOK {
			t.Fatalf("%s export = %d", path, rec.Code)
		}
	}
	if len(events.published) != 2 || events.published[0].key != "platform.export_ready" || events.published[0].payload["format"] != "csv" ||
		events.published[1].payload["format"] != "json" || events.published[0].payload["count"] != "2" {
		t.Fatalf("export completion events = %+v", events.published)
	}
}

func TestOwnerResultsRouteMatching(t *testing.T) {
	for path, want := range map[string]bool{"/api/results": true, "/api/results/export.csv": true, "/api/results/" + resultID: true,
		"/api/results/" + resultID + "/x": false, "/api/resultsx": false, "/api/nodes": false} {
		if ownerResultsRoute(path) != want {
			t.Errorf("%s = %v", path, !want)
		}
	}
	if neutralise("-1") != "'-1" || neutralise("plain") != "plain" || neutralise("") != "" {
		t.Fatal("neutralise")
	}
}

func TestOwnerResultsFullExports(t *testing.T) {
	rows := []ownerapi.ResultRow{
		{NodeName: "VM1", Result: map[string]any{"id": "a", "ts": "2026-11-01T00:00:00Z", "status": "ok", "down": 10.5, "geo": nil, "raw": map[string]any{"k": "v"}}},
		{NodeName: "VM2", Result: map[string]any{"id": "b", "ts": "2026-11-02T00:00:00Z", "status": "failed", "down": nil, "error": "boom"}},
	}
	store := &fakeOwnerResults{rows: rows}
	h := resultsHandlerFor(store)
	for _, path := range []string{"/api/export/results.csv", "/api/export/results.json"} {
		if rec := do(h, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d", path, rec.Code)
		}
	}
	// Query parameters are ignored: the full export always covers every stored result, oldest first.
	rec := do(h, http.MethodGet, "/api/export/results.csv?range=6h&status=failed", "", ownerRead)
	records, err := csv.NewReader(rec.Body).ReadAll()
	if rec.Code != http.StatusOK || err != nil || len(records) != 3 || store.filter.Sort != "ts" || store.filter.Dir != "asc" ||
		!store.filter.From.IsZero() || store.filter.Status != "" {
		t.Fatalf("csv = %d %v %v filter %+v", rec.Code, records, err, store.filter)
	}
	rec = do(h, http.MethodGet, "/api/export/results.json", "", ownerRead)
	var decoded []map[string]any
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), `attachment; filename="speedtest-results-`) ||
		json.Unmarshal(rec.Body.Bytes(), &decoded) != nil || len(decoded) != 2 || decoded[0]["id"] != "a" || decoded[1]["error"] != "boom" {
		t.Fatalf("json = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"success"`) {
		t.Fatal("the export is a bare array, not an envelope")
	}
	empty := do(resultsHandlerFor(&fakeOwnerResults{}), http.MethodGet, "/api/export/results.json", "", ownerRead)
	if empty.Code != http.StatusOK || strings.TrimSpace(empty.Body.String()) != "[]" {
		t.Fatalf("empty json = %d %q", empty.Code, empty.Body.String())
	}
	failing := do(resultsHandlerFor(&fakeOwnerResults{exportErr: errors.New("db down")}), http.MethodGet, "/api/export/results.json", "", ownerRead)
	if failing.Code != http.StatusInternalServerError || strings.Contains(failing.Body.String(), "db down") {
		t.Fatalf("json failure before the first row = %d %s", failing.Code, failing.Body.String())
	}
	if rec := do(h, http.MethodPost, "/api/export/results.json", "", ownerWriteWith(nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
}

func TestOwnerResultsJSONExportAbortsOnMidStreamFailure(t *testing.T) {
	rows := []ownerapi.ResultRow{{Result: map[string]any{"id": "a"}}, {Result: map[string]any{"id": "b"}}}
	h := resultsHandlerFor(&fakeOwnerResults{rows: rows, exportErr: errors.New("db down"), failAfter: 1})
	req := httptest.NewRequest(http.MethodGet, "http://center.test/api/export/results.json", nil)
	ownerRead(req)
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("a truncated JSON array must abort the connection, recovered %v", recovered)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), req)
	t.Fatal("handler returned normally")
}
