package audit

import "testing"

func TestClassify(t *testing.T) {
	const id = "10000000-0000-4000-8000-000000000001"
	cases := []struct {
		method, path, action, target string
		ok                           bool
	}{
		{"POST", "/api/nodes", "node.create", "", true},
		{"PATCH", "/api/nodes/" + id, "node.update", id, true},
		{"DELETE", "/api/nodes/" + id, "node.delete", id, true},
		{"POST", "/api/nodes/" + id + "/run", "node.run", id, true},
		{"PUT", "/api/schedules/rules", "schedule.rules", "", true}, // the literal route wins over /api/schedules/{id}
		{"PUT", "/api/schedules/" + id, "schedule.update", id, true},
		{"POST", "/api/schedules/stagger", "schedule.stagger", "", true},
		{"PUT", "/api/settings", "settings.update", "", true},
		{"POST", "/api/settings/enrolment-key", "settings.enrolment_key", "", true},
		{"POST", "/api/auth/password", "auth.password", "", true},
		{"PUT", "/api/alerts/events/" + id + "/ack", "alert.ack", id, true},
		{"POST", "/api/notifications/test/webhook", "notification.test", "webhook", true},
		{"PATCH", "/api/notifications/events/test.success", "notification.event.update", "test.success", true},
		{"POST", "/api/notifications/events/test.success/tests/webhook", "notification.event.test", "test.success/webhook", true},
		{"PUT", "/api/notifications/templates/test.success/telegram", "notification.template.update", "test.success/telegram", true},
		{"POST", "/api/notifications/templates/test.success/telegram/resets", "notification.template.reset", "test.success/telegram", true},
		{"POST", "/api/notifications/templates/test.success/telegram/previews", "", "", false},
		{"POST", "/api/nodes/", "node.create", "", true}, // trailing slash
		{"GET", "/api/nodes", "", "", false},
		{"POST", "/api/agent/results", "", "", false},
		{"POST", "/api/auth/login", "", "", false},
		{"POST", "/somewhere/else", "", "", false},
		{"POST", "/api/unknown", "", "", false},
	}
	for _, c := range cases {
		got, ok := Classify(c.method, c.path)
		if ok != c.ok || got.Action != c.action || got.TargetID != c.target {
			t.Errorf("Classify(%s %s) = %+v, %v; want %s %q %v", c.method, c.path, got, ok, c.action, c.target, c.ok)
		}
		if ok && (got.Summary == "" || got.TargetType == "") {
			t.Errorf("Classify(%s %s) needs a summary and target type: %+v", c.method, c.path, got)
		}
	}
}

func TestEscapeLikeAndID(t *testing.T) {
	if got := escapeLike(`a%b_c\d`); got != `a\%b\_c\\d` {
		t.Fatalf("escapeLike = %q", got)
	}
	if id := newID(); len(id) != 36 || id[14] != '4' {
		t.Fatalf("newID = %q", id)
	}
}

func TestRecordRejectsIncompleteEntries(t *testing.T) {
	s := &Store{}
	if err := s.Record(t.Context(), Entry{Action: "x"}); err == nil {
		t.Fatal("an entry without an actor was accepted")
	}
	if err := s.Record(t.Context(), Entry{Actor: "a"}); err == nil {
		t.Fatal("an entry without an action was accepted")
	}
}
