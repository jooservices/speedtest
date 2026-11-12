package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/center/alerts"
)

type fakeOwnerAlerts struct {
	err        error
	principal  string
	key        string
	input      alerts.RuleInput
	ifMatch    string
	filter     alerts.EventFilter
	page, size int
	acked      bool
	calls      []string
}

func (f *fakeOwnerAlerts) ListRules(_ context.Context, page, size int) ([]alerts.Rule, int, error) {
	f.calls = append(f.calls, "rules")
	f.page, f.size = page, size
	return []alerts.Rule{{ID: nodeID, Name: "slow", Nodes: "all"}}, 1, f.err
}
func (f *fakeOwnerAlerts) CreateRule(_ context.Context, principal, key string, in alerts.RuleInput) (json.RawMessage, bool, error) {
	f.calls = append(f.calls, "create")
	f.principal, f.key, f.input = principal, key, in
	return json.RawMessage(`{"id":"` + nodeID + `","name":"slow"}`), false, f.err
}
func (f *fakeOwnerAlerts) GetRule(context.Context, string) (alerts.Rule, string, error) {
	f.calls = append(f.calls, "get-rule")
	return alerts.Rule{ID: nodeID, Name: "slow", Nodes: "all"}, `"rule-1"`, f.err
}
func (f *fakeOwnerAlerts) ReplaceRule(_ context.Context, id string, in alerts.RuleInput, etag string) (alerts.Rule, string, error) {
	f.calls = append(f.calls, "replace-rule")
	f.input, f.ifMatch = in, etag
	return alerts.Rule{ID: id, Name: in.Name}, `"rule-2"`, f.err
}
func (f *fakeOwnerAlerts) DeleteRule(_ context.Context, _ string, etag string) error {
	f.calls = append(f.calls, "delete-rule")
	f.ifMatch = etag
	return f.err
}
func (f *fakeOwnerAlerts) ListEvents(_ context.Context, filter alerts.EventFilter, page, size int) ([]alerts.Event, int, error) {
	f.calls = append(f.calls, "events")
	f.filter, f.page, f.size = filter, page, size
	return []alerts.Event{{ID: nodeID, NodeID: stringAddress(nodeID), Title: "slow"}}, 1, f.err
}
func (f *fakeOwnerAlerts) GetEvent(context.Context, string) (alerts.Event, string, error) {
	f.calls = append(f.calls, "get-event")
	return alerts.Event{ID: nodeID, NodeID: stringAddress(nodeID), Title: "slow"}, `"event-1"`, f.err
}
func (f *fakeOwnerAlerts) Acknowledge(_ context.Context, _ string, acked bool, etag string) (alerts.Event, string, error) {
	f.calls = append(f.calls, "ack")
	f.acked, f.ifMatch = acked, etag
	return alerts.Event{ID: nodeID, NodeID: stringAddress(nodeID), Acked: acked}, `"event-2"`, f.err
}

func stringAddress(value string) *string { return &value }

func (f *fakeOwnerAlerts) AcknowledgeAll(context.Context) (string, error) {
	f.calls = append(f.calls, "ack-all")
	return commandID, f.err
}

const alertRuleJSON = `{"name":"Slow download","metric":"down_pct","op":"<","value":80,"forTests":3,"nodes":"all","severity":"warn","channels":["email"],"enabled":true}`

func alertsHandlerFor(store OwnerAlertsStore) http.Handler {
	return newNodeHandler(Options{OwnerAlerts: store})
}

func TestOwnerAlertsAuthenticationAndRoutes(t *testing.T) {
	store := &fakeOwnerAlerts{}
	h := alertsHandlerFor(store)
	for _, path := range []string{"/api/alerts/rules", "/api/alerts/rules/" + nodeID, "/api/alerts/events", "/api/alerts/events/" + nodeID} {
		if rec := do(h, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without session = %d", path, rec.Code)
		}
		if rec := do(h, http.MethodGet, path, "", ownerRead); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d (%s)", path, rec.Code, rec.Body.String())
		}
	}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/alerts/rules", alertRuleJSON},
		{http.MethodPut, "/api/alerts/rules/" + nodeID, alertRuleJSON},
		{http.MethodDelete, "/api/alerts/rules/" + nodeID, ""},
		{http.MethodPut, "/api/alerts/events/" + nodeID + "/ack", `{"acked":true}`},
	} {
		rec := do(h, tc.method, tc.path, tc.body, ownerRead)
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without write authorization = %d", tc.method, tc.path, rec.Code)
		}
	}
	if !ownerAlertsRoute("/api/alerts/rules") || !ownerAlertsRoute("/api/alerts/events/"+nodeID+"/ack") || ownerAlertsRoute("/api/alerts/settings") {
		t.Fatal("alert route detection does not match supported resources")
	}
}

func TestOwnerAlertRuleCRUD(t *testing.T) {
	store := &fakeOwnerAlerts{}
	h := alertsHandlerFor(store)
	list := do(h, http.MethodGet, "/api/alerts/rules?page=2&size=10", "", ownerRead)
	if list.Code != http.StatusOK || store.page != 2 || store.size != 10 || !strings.Contains(list.Body.String(), `"pagination"`) {
		t.Fatalf("rules list = %d %s", list.Code, list.Body.String())
	}

	create := do(h, http.MethodPost, "/api/alerts/rules", alertRuleJSON, ownerWriteWith(map[string]string{"Idempotency-Key": "alert-create-1"}))
	if create.Code != http.StatusCreated || store.principal != "owner-id" || store.key != "alert-create-1" ||
		create.Header().Get("Location") != "/api/alerts/rules/"+nodeID || !strings.Contains(create.Body.String(), `"id":"`+nodeID+`"`) {
		t.Fatalf("rule create = %d %s headers=%v", create.Code, create.Body.String(), create.Header())
	}
	if store.input.Name != "Slow download" || !store.input.AllNodes {
		t.Fatalf("create input = %+v", store.input)
	}

	path := "/api/alerts/rules/" + nodeID
	if rec := do(h, http.MethodGet, path, "", ownerRead); rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"rule-1"` {
		t.Fatalf("rule get = %d etag=%q", rec.Code, rec.Header().Get("ETag"))
	}
	replaced := do(h, http.MethodPut, path, alertRuleJSON, ownerWriteWith(map[string]string{"If-Match": `"old"`}))
	if replaced.Code != http.StatusOK || store.ifMatch != `"old"` || replaced.Header().Get("ETag") != `"rule-2"` {
		t.Fatalf("rule replace = %d %s", replaced.Code, replaced.Body.String())
	}
	if rec := do(h, http.MethodPut, path, alertRuleJSON, ownerWriteWith(nil)); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("rule replace without ETag = %d", rec.Code)
	}
	if rec := do(h, http.MethodDelete, path, "", ownerWriteWith(map[string]string{"If-Match": `"old"`})); rec.Code != http.StatusNoContent || store.ifMatch != `"old"` {
		t.Fatalf("rule delete = %d %s", rec.Code, rec.Body.String())
	}
	store.err = alerts.ErrStale
	if rec := do(h, http.MethodPut, path, alertRuleJSON, ownerWriteWith(map[string]string{"If-Match": `"old"`})); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale replace = %d", rec.Code)
	}
}

func TestOwnerAlertRuleValidationAndMethods(t *testing.T) {
	store := &fakeOwnerAlerts{}
	h := alertsHandlerFor(store)
	for _, body := range []string{
		`{"metric":"ping"}`,
		strings.Replace(alertRuleJSON, `"nodes":"all"`, `"nodes":null`, 1),
		strings.Replace(alertRuleJSON, `"metric":"down_pct"`, `"metric":"unknown"`, 1),
		strings.Replace(alertRuleJSON, `"channels":["email"]`, `"channels":[]`, 1),
	} {
		if rec := do(h, http.MethodPost, "/api/alerts/rules", body, ownerWriteWith(map[string]string{"Idempotency-Key": "key"})); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("invalid rule %s = %d", body, rec.Code)
		}
	}
	if rec := do(h, http.MethodPost, "/api/alerts/rules", alertRuleJSON, ownerWriteWith(nil)); rec.Code != http.StatusBadRequest {
		t.Fatalf("create without idempotency key = %d", rec.Code)
	}
	if rec := do(h, http.MethodPatch, "/api/alerts/rules/"+nodeID, alertRuleJSON, ownerWriteWith(nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unsupported method = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/alerts/rules/bad", "", ownerRead); rec.Code != http.StatusNotFound {
		t.Fatalf("invalid resource id = %d", rec.Code)
	}
	store.err = alerts.ErrInvalid
	if rec := do(h, http.MethodPost, "/api/alerts/rules", alertRuleJSON, ownerWriteWith(map[string]string{"Idempotency-Key": "key"})); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("store validation = %d", rec.Code)
	}
	store.err = errors.New("private db error")
	if rec := do(h, http.MethodGet, "/api/alerts/rules", "", ownerRead); rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "private db error") {
		t.Fatalf("internal error exposed = %d %s", rec.Code, rec.Body.String())
	}
}

func TestOwnerAlertEventsFiltersAndAcknowledgement(t *testing.T) {
	store := &fakeOwnerAlerts{}
	h := alertsHandlerFor(store)
	list := do(h, http.MethodGet, "/api/alerts/events?state=all&severity=crit&node="+nodeID+"&page=3&size=7", "", ownerRead)
	if list.Code != http.StatusOK || store.filter != (alerts.EventFilter{State: "all", Severity: "crit", NodeID: nodeID}) || store.page != 3 || store.size != 7 {
		t.Fatalf("events filter = %+v page=%d size=%d response=%d %s", store.filter, store.page, store.size, list.Code, list.Body.String())
	}
	if rec := do(h, http.MethodGet, "/api/alerts/events?state=bad", "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid event state = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/alerts/events?severity=urgent", "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid severity = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/alerts/events?node=bad", "", ownerRead); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid node filter = %d", rec.Code)
	}
	eventPath := "/api/alerts/events/" + nodeID
	if rec := do(h, http.MethodGet, eventPath, "", ownerRead); rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"event-1"` {
		t.Fatalf("event detail = %d etag=%q", rec.Code, rec.Header().Get("ETag"))
	}
	ackPath := eventPath + "/ack"
	if rec := do(h, http.MethodPut, ackPath, `{"acked":false}`, ownerWriteWith(map[string]string{"If-Match": `"event-1"`})); rec.Code != http.StatusOK || store.acked || store.ifMatch != `"event-1"` || rec.Header().Get("ETag") != `"event-2"` {
		t.Fatalf("event reopen = %d %s", rec.Code, rec.Body.String())
	}
	for _, tc := range []struct {
		body string
		want int
	}{
		{"{}", http.StatusUnprocessableEntity},
		{`{"acked":true,"extra":true}`, http.StatusBadRequest},
	} {
		if rec := do(h, http.MethodPut, ackPath, tc.body, ownerWriteWith(map[string]string{"If-Match": `"event-1"`})); rec.Code != tc.want {
			t.Errorf("invalid acknowledgement %s = %d, want %d", tc.body, rec.Code, tc.want)
		}
	}
	store.err = alerts.ErrNotFound
	if rec := do(h, http.MethodGet, eventPath, "", ownerRead); rec.Code != http.StatusNotFound {
		t.Fatalf("missing event = %d", rec.Code)
	}
}

func TestOwnerAlertsAcknowledgeAll(t *testing.T) {
	store := &fakeOwnerAlerts{}
	h := alertsHandlerFor(store)
	if rec := do(h, http.MethodPost, "/api/alerts/acknowledgements", "", nil); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("without credentials = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/api/alerts/acknowledgements", "", ownerWriteWith(nil)); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
		t.Fatalf("without an Idempotency-Key = %d", rec.Code)
	}
	rec := do(h, http.MethodPost, "/api/alerts/acknowledgements", "", ownerWriteWith(map[string]string{"Idempotency-Key": "ack-all-1"}))
	if rec.Code != http.StatusAccepted || rec.Header().Get("Location") != "/api/jobs/"+commandID || !strings.Contains(rec.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("ack all = %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	if rec := do(h, http.MethodGet, "/api/alerts/acknowledgements", "", ownerRead); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", rec.Code)
	}
	store.err = errors.New("db down")
	rec = do(h, http.MethodPost, "/api/alerts/acknowledgements", "", ownerWriteWith(map[string]string{"Idempotency-Key": "ack-all-2"}))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "db down") {
		t.Fatalf("store failure = %d %s", rec.Code, rec.Body.String())
	}
}

func TestOwnerAlertStorageFailuresAcrossEndpoints(t *testing.T) {
	h := alertsHandlerFor(&fakeOwnerAlerts{err: errors.New("private database detail")})
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/alerts/rules", ""}, {"GET", "/api/alerts/rules/" + nodeID, ""},
		{"PUT", "/api/alerts/rules/" + nodeID, alertRuleJSON}, {"DELETE", "/api/alerts/rules/" + nodeID, ""},
		{"POST", "/api/alerts/rules", alertRuleJSON}, {"GET", "/api/alerts/events", ""},
		{"GET", "/api/alerts/events/" + nodeID, ""}, {"PUT", "/api/alerts/events/" + nodeID + "/ack", `{"acked":true}`},
		{"POST", "/api/alerts/acknowledgements", ""},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			rec := do(h, tc.method, tc.path, tc.body, func(r *http.Request) {
				ownerWrite(r)
				r.Header.Set("If-Match", `"current"`)
				r.Header.Set("Idempotency-Key", "failure-key")
			})
			if rec.Code != 500 || strings.Contains(rec.Body.String(), "private database detail") {
				t.Fatalf("response %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAlertInvalidMethodsAndRequestBodies(t *testing.T) {
	h := alertsHandlerFor(&fakeOwnerAlerts{})
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/api/alerts/events", "", 405}, {"POST", "/api/alerts/events/" + nodeID, "", 405},
		{"GET", "/api/alerts/events/" + nodeID + "/ack", "", 405}, {"GET", "/api/alerts/events/invalid/ack", "", 404},
		{"GET", "/api/alerts/events/invalid", "", 404}, {"GET", "/api/alerts/rules/invalid", "", 404},
		{"POST", "/api/alerts/rules", `null`, 400}, {"POST", "/api/alerts/rules", `[]`, 400},
		{"POST", "/api/alerts/rules", `{"name":false}`, 400},
		{"POST", "/api/alerts/rules", strings.Replace(alertRuleJSON, `"nodes":"all"`, `"nodes":null`, 1), 422},
		{"POST", "/api/alerts/rules", strings.Replace(alertRuleJSON, `"nodes":"all"`, `"nodes":["invalid"]`, 1), 422},
	} {
		rec := do(h, tc.method, tc.path, tc.body, func(r *http.Request) { ownerWrite(r); r.Header.Set("Idempotency-Key", "invalid-input") })
		if rec.Code != tc.want {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
