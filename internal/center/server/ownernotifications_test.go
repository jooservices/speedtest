package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/center/notify"
	"github.com/jooservices/speedtest/internal/center/notify/template"
)

type notificationStub struct {
	err        error
	event      string
	nodeEvent  string
	nodeID     string
	nodeOn     bool
	page, size int
	thresholds map[string]float64
}

func (f *notificationStub) Test(context.Context, string) (notify.Delivery, error) {
	return notify.Delivery{ID: nodeID, Channel: "webhook", Status: "pending"}, f.err
}
func (f *notificationStub) Delivery(context.Context, string) (notify.Delivery, error) {
	return notify.Delivery{ID: nodeID, Channel: "webhook", Status: "sent"}, f.err
}
func (f *notificationStub) Deliveries(_ context.Context, event string, page, size int) ([]notify.Delivery, int, error) {
	f.event, f.page, f.size = event, page, size
	return []notify.Delivery{}, 51, f.err
}
func (f *notificationStub) ListNotificationEvents(context.Context) ([]notify.EventView, error) {
	return []notify.EventView{{Key: "test.success", Title: "Test success", Enabled: true, Severity: "info", Channels: []string{"telegram"}}}, f.err
}
func (f *notificationStub) GetNotificationEvent(_ context.Context, key string) (notify.EventView, error) {
	if key == "missing" {
		return notify.EventView{}, notify.ErrEventNotFound
	}
	return notify.EventView{Key: key, Title: "Test success", Enabled: true, Severity: "info", Channels: []string{"telegram"}}, f.err
}
func (f *notificationStub) UpdateNotificationEvent(_ context.Context, key string, patch notify.EventPatch) (notify.EventView, error) {
	f.thresholds = patch.Thresholds
	if value, ok := patch.Thresholds["for_tests"]; ok && value != float64(int(value)) {
		return notify.EventView{}, notify.ErrInvalidEvent
	}
	return notify.EventView{Key: key, Enabled: false, Severity: "warn", Channels: []string{"email"}, Customized: true}, f.err
}
func (f *notificationStub) SetNotificationNodeEnabled(_ context.Context, event, node string, enabled bool) error {
	f.nodeEvent, f.nodeID, f.nodeOn = event, node, enabled
	return f.err
}
func (f *notificationStub) NotificationNodeEnabled(_ context.Context, event, node string) (bool, error) {
	f.nodeEvent, f.nodeID = event, node
	return f.nodeOn, f.err
}
func (f *notificationStub) GetNotificationTemplate(_ context.Context, key, channel string) (notify.TemplateView, error) {
	return notify.TemplateView{EventKey: key, Channel: channel, Body: "Hello {node}", IsDefault: true}, f.err
}
func (f *notificationStub) PutNotificationTemplate(_ context.Context, key, channel, body string) (notify.TemplateView, error) {
	if strings.Contains(body, "{secret}") {
		return notify.TemplateView{}, template.ErrUnknownPlaceholder
	}
	return notify.TemplateView{EventKey: key, Channel: channel, Body: body}, f.err
}
func (f *notificationStub) ResetNotificationTemplate(_ context.Context, key, channel string) (notify.TemplateView, error) {
	return notify.TemplateView{EventKey: key, Channel: channel, Body: "default", IsDefault: true}, f.err
}
func (f *notificationStub) PreviewNotificationTemplate(_ context.Context, key, channel, _ string) (notify.TemplatePreview, error) {
	return notify.TemplatePreview{Channel: channel, Body: "preview", Sample: map[string]string{"node": "Fiber"}}, f.err
}
func (f *notificationStub) TestNotificationEvent(_ context.Context, _, channel string) (notify.Delivery, error) {
	return notify.Delivery{ID: nodeID, Channel: channel, Status: "pending"}, f.err
}
func TestNotificationAPI(t *testing.T) {
	f := &notificationStub{}
	h := newNodeHandler(Options{OwnerNotifications: f})
	for _, path := range []string{"/api/notifications/deliveries", "/api/notifications/deliveries/" + nodeID} {
		if rec := do(h, "GET", path, "", nil); rec.Code != 401 {
			t.Fatal("session required", rec.Code)
		}
		if rec := do(h, "GET", path, "", ownerRead); rec.Code != 200 {
			t.Fatal("GET", rec.Code)
		}
	}
	path := "/api/notifications/test/webhook"
	if rec := do(h, "POST", path, "", ownerRead); rec.Code != 403 {
		t.Fatal("CSRF required", rec.Code)
	}
	rec := do(h, "POST", path, "", ownerWriteWith(nil))
	if rec.Code != 202 || rec.Header().Get("Location") != "/api/notifications/deliveries/"+nodeID || !strings.Contains(rec.Body.String(), `"meta":{}`) {
		t.Fatal("test POST", rec.Code, rec.Body.String())
	}
	if rec := do(h, "GET", "/api/notifications/deliveries?event="+nodeID+"&page=2&size=10", "", ownerRead); rec.Code != 200 || f.event != nodeID || f.page != 2 || f.size != 10 || !strings.Contains(rec.Body.String(), `"last_page":6`) {
		t.Fatal("paging")
	}
	for path, code := range map[string]int{"/api/notifications/test/nope": 404, "/api/notifications/deliveries/nope": 404, "/api/notifications/nope": 404, "/api/notifications/deliveries?event=nope": 422, "/api/notifications/deliveries?event=" + nodeID + "&event=" + nodeID: 422, "/api/notifications/deliveries?size=101": 422, "/api/notifications/deliveries?page=1000001": 422} {
		method := "GET"
		auth := ownerRead
		if strings.Contains(path, "/test/") {
			method = "POST"
			auth = ownerWriteWith(nil)
		}
		if rec := do(h, method, path, "", auth); rec.Code != code {
			t.Errorf("%s = %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/api/notifications/deliveries", "/api/notifications/deliveries/" + nodeID} {
		if rec := do(h, "POST", path, "", ownerWriteWith(nil)); rec.Code != 405 {
			t.Fatal("method", rec.Code)
		}
	}
	if rec := do(h, "GET", "/api/notifications/test/webhook", "", ownerRead); rec.Code != 405 {
		t.Fatal("method")
	}
	for _, test := range []struct {
		err  error
		code int
	}{{notify.ErrUnconfigured, 422}, {notify.ErrNotFound, 404}, {errors.New("SECRET URL"), 500}} {
		f.err = test.err
		rec := do(h, "POST", "/api/notifications/test/webhook", "", ownerWriteWith(nil))
		if rec.Code != test.code || strings.Contains(rec.Body.String(), "SECRET URL") {
			t.Fatal("error mapping", rec.Code)
		}
	}
	f.err = nil
	if rec := do(h, "GET", "/api/notifications/events", "", ownerRead); rec.Code != 200 || !strings.Contains(rec.Body.String(), "test.success") {
		t.Fatal("list events", rec.Code, rec.Body.String())
	}
	if rec := do(h, "PATCH", "/api/notifications/events/test.success", `{"enabled":false}`, ownerWriteWith(nil)); rec.Code != 200 {
		t.Fatal("patch event", rec.Code, rec.Body.String())
	}
	if rec := do(h, "PATCH", "/api/notifications/events/quality.ping_high", `{"thresholds":{"for_tests":3}}`, ownerWriteWith(nil)); rec.Code != 200 || f.thresholds["for_tests"] != 3 {
		t.Fatal("quality streak threshold patch", rec.Code, rec.Body.String())
	}
	if rec := do(h, "PATCH", "/api/notifications/events/quality.ping_high", `{"thresholds":{"for_tests":2.5}}`, ownerWriteWith(nil)); rec.Code != 422 {
		t.Fatal("fractional quality streak threshold", rec.Code, rec.Body.String())
	}
	nodeSetting := "/api/notifications/events/test.success/nodes/" + nodeID
	f.nodeOn = true
	if rec := do(h, "GET", nodeSetting, "", ownerRead); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":true`) || f.nodeEvent != "test.success" || f.nodeID != nodeID {
		t.Fatal("get node event setting", rec.Code, rec.Body.String())
	}
	if rec := do(h, "PUT", nodeSetting, `{"enabled":false}`, ownerRead); rec.Code != 403 {
		t.Fatal("node setting CSRF required", rec.Code)
	}
	if rec := do(h, "PUT", nodeSetting, `{"enabled":false}`, ownerWriteWith(nil)); rec.Code != 200 || f.nodeOn || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatal("put node event setting", rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/api/notifications/events/missing/nodes/" + nodeID, "/api/notifications/events/test.success/nodes/not-a-uuid"} {
		if rec := do(h, "GET", path, "", ownerRead); rec.Code != 404 {
			t.Errorf("invalid node event path %s = %d", path, rec.Code)
		}
	}
	if rec := do(h, "GET", "/api/notifications/templates/test.success/telegram", "", ownerRead); rec.Code != 200 {
		t.Fatal("get template", rec.Code)
	}
	if rec := do(h, "PUT", "/api/notifications/templates/test.success/telegram", `{"body":"Hi {node}"}`, ownerWriteWith(nil)); rec.Code != 200 {
		t.Fatal("put template", rec.Code, rec.Body.String())
	}
	if rec := do(h, "PUT", "/api/notifications/templates/test.success/telegram", `{"body":"Hi {secret}"}`, ownerWriteWith(nil)); rec.Code != 422 {
		t.Fatal("unknown placeholder", rec.Code)
	}
	if rec := do(h, "POST", "/api/notifications/templates/test.success/telegram/previews", `{"body":"Hi {node}"}`, ownerWriteWith(nil)); rec.Code != 200 {
		t.Fatal("preview", rec.Code, rec.Body.String())
	}
	if rec := do(h, "POST", "/api/notifications/templates/test.success/telegram/resets", "", ownerWriteWith(nil)); rec.Code != 200 {
		t.Fatal("reset", rec.Code, rec.Body.String())
	}
}

func TestNotificationCatalogBranches(t *testing.T) {
	f := &notificationStub{}
	h := newNodeHandler(Options{OwnerNotifications: f})

	if rec := do(h, "GET", "/api/notifications/events/test.success", "", ownerRead); rec.Code != 200 {
		t.Fatal("get event", rec.Code, rec.Body.String())
	}
	if rec := do(h, "GET", "/api/notifications/events/missing", "", ownerRead); rec.Code != 404 {
		t.Fatal("missing catalog key", rec.Code)
	}
	if rec := do(h, "GET", "/api/notifications/events/not.a.real.event", "", ownerRead); rec.Code != 404 {
		t.Fatal("unknown key", rec.Code)
	}
	if rec := do(h, "DELETE", "/api/notifications/events/test.success", "", ownerWriteWith(nil)); rec.Code != 405 {
		t.Fatal("event method", rec.Code)
	}
	if rec := do(h, "POST", "/api/notifications/events/test.success/tests/webhook", "", ownerWriteWith(nil)); rec.Code != http.StatusAccepted || !strings.Contains(rec.Header().Get("Location"), nodeID) {
		t.Fatal("event template test", rec.Code, rec.Body.String())
	}
	if rec := do(h, "GET", "/api/notifications/events/test.success/tests/webhook", "", ownerRead); rec.Code != http.StatusMethodNotAllowed {
		t.Fatal("event template test method", rec.Code)
	}
	if rec := do(h, "PATCH", "/api/notifications/events/test.success", `{`, ownerWriteWith(nil)); rec.Code != 422 {
		t.Fatal("bad json", rec.Code)
	}
	if rec := do(h, "PATCH", "/api/notifications/events/test.success", `{"enabled":false}{"x":1}`, ownerWriteWith(nil)); rec.Code != 422 {
		t.Fatal("two values", rec.Code)
	}
	if rec := do(h, "GET", "/api/notifications/templates/test.success/nope", "", ownerRead); rec.Code != 404 {
		t.Fatal("bad channel", rec.Code)
	}
	if rec := do(h, "GET", "/api/notifications/templates/test.success/telegram/extra", "", ownerRead); rec.Code != 404 {
		t.Fatal("extra segment", rec.Code)
	}
	if rec := do(h, "DELETE", "/api/notifications/templates/test.success/telegram", "", ownerWriteWith(nil)); rec.Code != 405 {
		t.Fatal("template method", rec.Code)
	}
	if rec := do(h, "GET", "/api/notifications/templates/test.success/telegram/previews", "", ownerRead); rec.Code != 405 {
		t.Fatal("preview method", rec.Code)
	}
	if rec := do(h, "GET", "/api/notifications/templates/test.success/telegram/resets", "", ownerRead); rec.Code != 405 {
		t.Fatal("reset method", rec.Code)
	}
	if rec := do(h, "POST", "/api/notifications/templates/test.success/telegram/previews", "", ownerWriteWith(nil)); rec.Code != 200 {
		t.Fatal("preview empty body", rec.Code, rec.Body.String())
	}
	if rec := do(h, "POST", "/api/notifications/nope", "", ownerWriteWith(nil)); rec.Code != 404 {
		t.Fatal("catalog default", rec.Code)
	}
	if rec := do(h, "POST", "/api/notifications/events", "", ownerWriteWith(nil)); rec.Code != 404 {
		t.Fatal("events collection write", rec.Code)
	}
	f.err = errors.New("list boom")
	if rec := do(h, "GET", "/api/notifications/deliveries", "", ownerRead); rec.Code != 500 {
		t.Fatal("deliveries failure", rec.Code)
	}
	f.err = notify.ErrNotFound
	if rec := do(h, "GET", "/api/notifications/deliveries/"+nodeID, "", ownerRead); rec.Code != 404 {
		t.Fatal("delivery not found", rec.Code)
	}
	f.err = nil

	f.err = notify.ErrEventNotFound
	if rec := do(h, "GET", "/api/notifications/events/test.success", "", ownerRead); rec.Code != 404 {
		t.Fatal("event not found", rec.Code)
	}
	f.err = notify.ErrTemplateNotFound
	if rec := do(h, "GET", "/api/notifications/templates/test.success/telegram", "", ownerRead); rec.Code != 404 {
		t.Fatal("template not found", rec.Code)
	}
	f.err = notify.ErrInvalidEvent
	if rec := do(h, "PATCH", "/api/notifications/events/test.success", `{"severity":"nope"}`, ownerWriteWith(nil)); rec.Code != 422 {
		t.Fatal("invalid event", rec.Code)
	}
	f.err = template.ErrInvalidTemplate
	if rec := do(h, "PUT", "/api/notifications/templates/test.success/telegram", `{"body":""}`, ownerWriteWith(nil)); rec.Code != 422 {
		t.Fatal("invalid template", rec.Code)
	}
	f.err = errors.New("db down")
	if rec := do(h, "GET", "/api/notifications/events", "", ownerRead); rec.Code != 500 {
		t.Fatal("list failure", rec.Code)
	}
	if rec := do(h, "POST", "/api/notifications/templates/test.success/telegram/resets", "", ownerWriteWith(nil)); rec.Code != 500 {
		t.Fatal("reset failure", rec.Code)
	}
	if rec := do(h, "POST", "/api/notifications/templates/test.success/telegram/previews", `{"body":"Hi {node}"}`, ownerWriteWith(nil)); rec.Code != 500 {
		t.Fatal("preview failure", rec.Code)
	}
	f.err = nil
}

func TestOwnerNotificationsServeGuards(t *testing.T) {
	f := &notificationStub{}
	h := &ownerNotifications{store: f, limiter: newNodeRateLimiter(60, 1)}
	rec := httptest.NewRecorder()
	h.serve(rec, httptest.NewRequest(http.MethodGet, "/api/notifications/events", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated serve = %d", rec.Code)
	}
	ok := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/notifications/events", nil)
	h.serve(ok, req.WithContext(withOwner(req)))
	if ok.Code != http.StatusOK {
		t.Fatalf("authenticated serve = %d %s", ok.Code, ok.Body.String())
	}
	limited := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/notifications/events", nil)
	h.serve(limited, req2.WithContext(withOwner(req2)))
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited serve = %d", limited.Code)
	}
}
