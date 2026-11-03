package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/center/notify"
)

type notificationStub struct {
	err        error
	event      string
	page, size int
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
}
