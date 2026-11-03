package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/update"
)

const releaseSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type updateCenter struct {
	mu         sync.Mutex
	heartbeats []Heartbeat
	hbStatus   int
	config     string
	commands   string
	acks       []CommandAck
}

func (c *updateCenter) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/agent/heartbeat":
			var hb Heartbeat
			_ = json.NewDecoder(r.Body).Decode(&hb)
			c.heartbeats = append(c.heartbeats, hb)
			if c.hbStatus != 0 {
				w.WriteHeader(c.hbStatus)
				_, _ = w.Write([]byte(`{"type":"/problems/servererror","title":"x","status":500}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"code":200,"message":"ok","data":{},"meta":{},"errors":null}`))
		case r.URL.Path == "/api/agent/config":
			if r.Header.Get("If-None-Match") == `"u1"` {
				w.Header().Set("ETag", `"u1"`)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"u1"`)
			_, _ = w.Write([]byte(`{"success":true,"code":200,"message":"ok","data":` + c.config + `,"meta":{},"errors":null}`))
		case r.URL.Path == "/api/agent/commands":
			_, _ = w.Write([]byte(`{"success":true,"code":200,"message":"ok","data":` + c.commands + `,"meta":{},"errors":null}`))
		case strings.HasSuffix(r.URL.Path, "/ack"):
			var ack CommandAck
			_ = json.NewDecoder(r.Body).Decode(&ack)
			c.acks = append(c.acks, ack)
			_, _ = w.Write([]byte(`{"success":true,"code":200,"message":"ok","data":{},"meta":{},"errors":null}`))
		default:
			http.NotFound(w, r)
		}
	})
}

func updateConfigJSON(auto bool) string {
	b, _ := json.Marshal(auto)
	return `{"revision":"1","name":"n","expectedDown":1,"expectedUp":1,"schedule":{"interval":30,"offset":0,"enabled":true},` +
		`"quiet":{"quietStart":"","quietEnd":"","timezone":"UTC"},"autoUpdate":` + string(b) +
		`,"release":{"version":"2.0.0","url":"https://center.example/agent","sha256":"` + releaseSHA + `"}}`
}

func newUpdateController(t *testing.T, center *updateCenter) *Controller {
	t.Helper()
	s := httptest.NewServer(center.handler())
	t.Cleanup(s.Close)
	return New(Client{BaseURL: baseURL(t, s), Credential: "secret", Agent: "speedtest-agent/1.0.0"}, &fakeScheduler{})
}

func TestHeartbeatCarriesTheUpdateResultAndConfirms(t *testing.T) {
	center := &updateCenter{}
	c := newUpdateController(t, center)
	confirmed := 0
	c.HeartbeatOK = func() { confirmed++ }
	c.UpdateResult = func() *UpdateResult {
		return &update.Result{Version: "2.0.0", Status: update.StatusRolledBack, TS: "2026-11-03T12:00:00Z", Error: strPtr("no heartbeat")}
	}
	c.heartbeat(context.Background())
	if confirmed != 1 || len(center.heartbeats) != 1 {
		t.Fatalf("confirmed %d, heartbeats %d", confirmed, len(center.heartbeats))
	}
	got := center.heartbeats[0].UpdateResult
	if got == nil || got.Version != "2.0.0" || got.Status != "rolled_back" || got.Error == nil || *got.Error != "no heartbeat" {
		t.Fatalf("update result = %+v", got)
	}
	center.hbStatus = http.StatusInternalServerError
	c.heartbeat(context.Background())
	if confirmed != 1 {
		t.Fatal("a failed heartbeat must not confirm an update")
	}
}

func TestHeartbeatWithoutAnUpdateResultOmitsIt(t *testing.T) {
	center := &updateCenter{}
	c := newUpdateController(t, center)
	c.heartbeat(context.Background())
	if center.heartbeats[0].UpdateResult != nil {
		t.Fatal("no result when there has been no update")
	}
}

func TestConfigOffersTheAdvertisedRelease(t *testing.T) {
	center := &updateCenter{config: updateConfigJSON(true)}
	c := newUpdateController(t, center)
	var offered []AgentConfig
	c.OfferUpdate = func(cfg AgentConfig) { offered = append(offered, cfg) }
	c.syncConfig(context.Background())
	c.syncConfig(context.Background()) // 304: the last configuration is offered again, so a later retry is possible
	if len(offered) != 2 || !offered[0].AutoUpdate || offered[0].Release == nil || offered[0].Release.Version != "2.0.0" || offered[0].Release.SHA256 != releaseSHA {
		t.Fatalf("offered = %+v", offered)
	}
}

func TestUpdateAgentCommand(t *testing.T) {
	version := "2.0.0"
	center := &updateCenter{config: updateConfigJSON(false)}
	c := newUpdateController(t, center)
	var gotVersion string
	var gotRelease *update.Release
	c.UpdateAgent = func(cmd Command, cfg AgentConfig) CommandAck {
		gotVersion = *cmd.Version
		gotRelease = cfg.Release
		return CommandAck{Status: "succeeded", Phase: "restarting to install", Pct: 100}
	}
	ack := c.execute(context.Background(), Command{ID: "c1", Type: "update_agent", Version: &version})
	if ack.Status != "succeeded" || gotVersion != "2.0.0" || gotRelease == nil || gotRelease.Version != "2.0.0" {
		t.Fatalf("ack %+v version %q release %+v", ack, gotVersion, gotRelease)
	}
	c.UpdateAgent = nil
	if ack := c.execute(context.Background(), Command{ID: "c2", Type: "update_agent", Version: &version}); ack.Status != "failed" || ack.Error == nil {
		t.Fatalf("an agent without self-update must refuse: %+v", ack)
	}
	c.UpdateAgent = func(Command, AgentConfig) CommandAck { return CommandAck{Status: "succeeded"} }
	center.config = "{broken"
	if ack := c.execute(context.Background(), Command{ID: "c3", Type: "update_agent", Version: &version}); ack.Status != "failed" || ack.Error == nil {
		t.Fatalf("an unreadable configuration must fail the command: %+v", ack)
	}
}

func TestPollCadenceIsShorterThanTheHeartbeatByDefaultOnly(t *testing.T) {
	c := &Controller{interval: HeartbeatInterval}
	if c.pollEvery() != CommandPollInterval || CommandPollInterval >= HeartbeatInterval {
		t.Fatalf("production default = %v", c.pollEvery())
	}
	c = &Controller{interval: 40 * time.Millisecond}
	if c.pollEvery() != 40*time.Millisecond {
		t.Fatalf("a shortened heartbeat keeps one pass per interval: %v", c.pollEvery())
	}
	c = &Controller{interval: HeartbeatInterval, poll: time.Second}
	if c.pollEvery() != time.Second {
		t.Fatalf("explicit poll interval: %v", c.pollEvery())
	}
	if opt := WithPollInterval(time.Minute); opt == nil {
		t.Fatal("option")
	}
}
