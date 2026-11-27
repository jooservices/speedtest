package catalog

import "slices"

// Event is one notification kind with defaults owned in this package.
// Owner overrides live in PostgreSQL; code defaults are the single source for reset.
type Event struct {
	Key         string
	Category    string
	Title       string
	Description string
	Enabled     bool
	Severity    string
	Channels    []string
	// AlertMetric links legacy alert_rules.metric rows to this catalog event when non-empty.
	AlertMetric string
	// Thresholds are the in-code defaults for this event; owner values can override them.
	Thresholds map[string]float64
	// RecoveryKey is the catalog event emitted when this alert clears (empty = none).
	RecoveryKey     string
	CooldownSeconds int
	RepeatSeconds   int
	// NodeScoped allows per-node enable overrides (test.success).
	NodeScoped bool
	// Placeholders lists every token the default templates may use.
	Placeholders []string
	// Templates holds one default body per channel (email, telegram, slack, webhook).
	Templates map[string]string
	// EngineScoped is reserved for JOOST-71 engine-dimension emitters; catalog only.
	EngineScoped bool
}

// Shared quality measurement tokens appear on every quality and test-success message.
var qualityPlaceholders = []string{
	"node", "label", "event", "severity", "value", "expected", "pct", "threshold",
	"download", "upload", "ping", "jitter", "loss",
	"download_limit", "upload_limit", "ping_limit", "jitter_limit", "loss_limit",
	"download_ok", "upload_ok", "ping_ok", "jitter_ok", "loss_ok",
	"server", "isp", "ip", "old_ip", "city", "since", "duration", "count", "failure_reason", "link", "center",
	"engine", "old_interval", "new_interval",
}

func basePlaceholders(extra ...string) []string {
	out := slices.Clone(qualityPlaceholders)
	for _, p := range extra {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func defaultTemplates(title, body string) map[string]string {
	plain := "{severity_icon} {event}\n{node}\n" + body + "\nSince {since}\n{link}"
	html := "{severity_icon} <b>{event}</b>\n{node}\n" + body + "\nSince {since}\n{link}"
	return map[string]string{
		"email":    plain,
		"telegram": html,
		"slack":    plain,
		"webhook":  `{"event":"{event}","severity":"{severity}","node":"{node}","text":"` + title + ` — {node}. ` + body + ` Since {since}. {link}"}`,
	}
}

func digestTemplates() map[string]string {
	plain := "{severity_icon} {event}\n{period}\n{summary}\n{link}"
	html := "{severity_icon} <b>{event}</b>\n{period}\n{summary}\n{link}"
	webhook := `{"event":"{event}","severity":"{severity}","period":"{period}","summary":"{summary}","link":"{link}"}`
	return map[string]string{"email": plain, "telegram": html, "slack": plain, "webhook": webhook}
}

func qualityBody(metric string) string {
	trigger := map[string]string{
		"down_pct": "Download {value} vs expected {expected} ({pct}% of expected; threshold {threshold}). ",
		"up_pct":   "Upload {value} vs expected {expected} ({pct}% of expected; threshold {threshold}). ",
		"ping":     "Ping {value} ms vs limit {threshold}. ",
		"jitter":   "Jitter {value} ms vs limit {threshold}. ",
		"loss":     "Packet loss {value}% vs limit {threshold}. ",
		// Recovery and the median drop have no single trigger metric: say the value against the expected value and its percentage.
		"":     "Download {download} vs expected {expected} ({pct}% of expected). ",
		"drop": "Speed {value}% of the {expected} (alert below {threshold}%). ",
	}
	return trigger[metric] + "Download {download} (limit {download_limit}, ok={download_ok}); upload {upload} (limit {upload_limit}, ok={upload_ok}); ping {ping} ms (limit {ping_limit}, ok={ping_ok}); jitter {jitter} ms (limit {jitter_limit}, ok={jitter_ok}); loss {loss}% (limit {loss_limit}, ok={loss_ok})."
}

func successBody() string {
	return "Download {download} ({pct}% of expected; limit {download_limit}, ok={download_ok}); upload {upload} (limit {upload_limit}, ok={upload_ok}); ping {ping} ms (limit {ping_limit}, ok={ping_ok}); jitter {jitter} ms (limit {jitter_limit}, ok={jitter_ok}); loss {loss}% (limit {loss_limit}, ok={loss_ok}). Server {server}; ISP {isp}. {failure_reason}"
}

var events = []Event{
	{
		Key: "node.offline", Category: "availability", Title: "Node offline",
		Description: "No heartbeat for three scheduled intervals.", Enabled: true, Severity: "crit",
		Channels: []string{"telegram"}, AlertMetric: "offline", RecoveryKey: "node.online", RepeatSeconds: 3600,
		Placeholders: basePlaceholders(), Templates: defaultTemplates("Node offline", "No heartbeat for three scheduled intervals."),
	},
	{
		Key: "node.online", Category: "availability", Title: "Node back online",
		Description: "Recovery after offline with downtime length.", Enabled: true, Severity: "info",
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Node back online", "Back online after {duration}."),
	},
	{
		Key: "agent.update_available", Category: "availability", Title: "Agent update available",
		Description: "A newer agent binary is available.", Enabled: true, Severity: "info",
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Agent update available", "A newer agent is available."),
	},
	{
		Key: "agent.update_applied", Category: "availability", Title: "Agent update applied",
		Description: "The agent reported a successful update.", Enabled: true, Severity: "info",
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Agent update applied", "Agent update applied."),
	},
	{
		Key: "agent.update_failed", Category: "availability", Title: "Agent update failed",
		Description: "The agent could not apply an update.", Enabled: true, Severity: "warn",
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Agent update failed", "Agent update failed."),
	},
	{
		Key: "agent.version_older", Category: "availability", Title: "Agent version older than center",
		Description: "The node agent is behind the center release.", Enabled: true, Severity: "warn",
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Agent version older", "Agent version is older than the center."),
	},
	{
		Key: "test.failed", Category: "tests", Title: "Test failed",
		Description: "N consecutive failed tests.", Enabled: true, Severity: "crit",
		Channels: []string{"telegram"}, AlertMetric: "failed", Thresholds: map[string]float64{"for_tests": 2}, RecoveryKey: "test.recovered",
		Placeholders: basePlaceholders(), Templates: defaultTemplates("Test failed", "{count} failed tests in a row. Classified reason: {failure_reason}."),
	},
	{
		Key: "test.recovered", Category: "tests", Title: "Tests recovered",
		Description: "First success after failures.", Enabled: true, Severity: "info",
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Tests recovered", "Tests recovered after {duration}."),
	},
	{
		Key: "test.success", Category: "tests", Title: "Test success",
		Description: "Successful test with measurements versus limits.", Enabled: true, Severity: "info",
		Channels: []string{"telegram"}, NodeScoped: true, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Test success", successBody()),
	},
	{
		Key: "test.manual_finished", Category: "tests", Title: "Manual test finished",
		Description: "An owner-triggered run finished.", Enabled: true, Severity: "info",
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Manual test finished", successBody()),
	},
	{
		Key: "quality.download_low", Category: "quality", Title: "Download below limit",
		Description: "Download below the configured percent of expected.", Enabled: true, Severity: "warn",
		Channels: []string{"telegram"}, AlertMetric: "down_pct", Thresholds: map[string]float64{"down_pct": 65, "for_tests": 2}, RecoveryKey: "quality.recovered",
		Placeholders: basePlaceholders(), Templates: defaultTemplates("Download below limit", qualityBody("down_pct")),
	},
	{
		Key: "quality.upload_low", Category: "quality", Title: "Upload below limit",
		Description: "Upload below the configured percent of expected.", Enabled: true, Severity: "warn",
		Channels: []string{"telegram"}, AlertMetric: "up_pct", Thresholds: map[string]float64{"up_pct": 40, "for_tests": 2}, RecoveryKey: "quality.recovered",
		Placeholders: basePlaceholders(), Templates: defaultTemplates("Upload below limit", qualityBody("up_pct")),
	},
	{
		Key: "quality.ping_high", Category: "quality", Title: "Ping above limit",
		Description: "Ping above the configured limit.", Enabled: true, Severity: "warn",
		Channels: []string{"telegram"}, AlertMetric: "ping", Thresholds: map[string]float64{"ping": 30, "for_tests": 2}, RecoveryKey: "quality.recovered",
		Placeholders: basePlaceholders(), Templates: defaultTemplates("Ping above limit", qualityBody("ping")),
	},
	{
		Key: "quality.jitter_high", Category: "quality", Title: "Jitter above limit",
		Description: "Jitter above the configured limit.", Enabled: true, Severity: "warn",
		Channels: []string{"telegram"}, AlertMetric: "jitter", Thresholds: map[string]float64{"jitter": 10, "for_tests": 2}, RecoveryKey: "quality.recovered",
		Placeholders: basePlaceholders(), Templates: defaultTemplates("Jitter above limit", qualityBody("jitter")),
	},
	{
		Key: "quality.loss_high", Category: "quality", Title: "Packet loss above limit",
		Description: "Packet loss above the configured limit.", Enabled: true, Severity: "warn",
		Channels: []string{"telegram"}, AlertMetric: "loss", Thresholds: map[string]float64{"loss": 2, "for_tests": 2}, RecoveryKey: "quality.recovered",
		Placeholders: basePlaceholders(), Templates: defaultTemplates("Packet loss above limit", qualityBody("loss")),
	},
	{
		Key: "quality.drop_vs_median", Category: "quality", Title: "Drop versus 7-day median",
		Description: "Speed dropped against the node's 7-day median.", Enabled: false, Severity: "warn", Thresholds: map[string]float64{"drop_pct": 30},
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(), EngineScoped: true,
		Templates: defaultTemplates("Drop versus median", qualityBody("drop")),
	},
	{
		Key: "quality.recovered", Category: "quality", Title: "Speed recovered",
		Description: "Quality alert cleared.", Enabled: true, Severity: "info",
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Speed recovered", "Recovered after {duration}. "+qualityBody("")),
	},
	{
		Key: "network.ip_changed", Category: "network", Title: "Public IP changed",
		Description: "The node's public IP changed.", Enabled: true, Severity: "info",
		Channels: []string{"telegram"}, AlertMetric: "ip_changed",
		Placeholders: basePlaceholders(), Templates: defaultTemplates("Public IP changed", "Now {ip} (was {old_ip}) in {city}. ISP {isp}."),
	},
	{
		Key: "network.isp_changed", Category: "network", Title: "ISP changed",
		Description: "The observed ISP changed.", Enabled: true, Severity: "info",
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("ISP changed", "ISP is now {isp}."),
	},
	{
		Key: "network.server_differs", Category: "network", Title: "Server differs from pin",
		Description: "The test server differs from the pinned server.", Enabled: false, Severity: "info",
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(), EngineScoped: true,
		Templates: defaultTemplates("Server differs", "Used server {server}."),
	},
	{
		Key: "bandwidth.budget_80", Category: "bandwidth", Title: "Bandwidth budget 80%",
		Description: "Bandwidth used this month reached the configured share of the monthly budget (default 80%).", Enabled: true, Severity: "warn", Thresholds: map[string]float64{"monthly_gb": 0, "percent": 80},
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Bandwidth budget 80%", "Bandwidth usage is {value} GB of {expected} GB ({pct}%)."),
	},
	{
		Key: "bandwidth.budget_100", Category: "bandwidth", Title: "Bandwidth budget 100%",
		Description: "Bandwidth used this month reached the configured share of the monthly budget (default 100%).", Enabled: true, Severity: "crit", Thresholds: map[string]float64{"monthly_gb": 0, "percent": 100},
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Bandwidth budget 100%", "Bandwidth usage is {value} GB of {expected} GB ({pct}%)."),
	},
	{
		Key: "bandwidth.projection_over", Category: "bandwidth", Title: "Projection over budget",
		Description: "Projected usage will exceed the monthly budget.", Enabled: true, Severity: "warn", Thresholds: map[string]float64{"monthly_gb": 0},
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Projection over budget", "Current usage is {value} GB of {expected} GB ({pct}%)."),
	},
	{
		Key: "bandwidth.interval_widened", Category: "bandwidth", Title: "Interval widened or paused",
		Description: "The schedule interval was widened or tests were paused for budget.", Enabled: true, Severity: "info",
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Interval widened", "Schedule for {engine} changed from {old_interval} to {new_interval}."),
	},
	{
		Key: "security.signin_new", Category: "security", Title: "Sign-in from new location",
		Description: "A sign-in came from a new IP or country.", Enabled: true, Severity: "warn",
		Channels: []string{"telegram", "email"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("New sign-in", "Sign-in from {ip} ({city})."),
	},
	{
		Key: "security.login_burst", Category: "security", Title: "Failed-login burst",
		Description: "Too many failed sign-in attempts.", Enabled: true, Severity: "crit",
		Channels: []string{"telegram", "email"}, Thresholds: map[string]float64{"attempts": 5}, CooldownSeconds: 300, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Failed-login burst", "{count} failed sign-in attempts."),
	},
	{
		Key: "security.password_changed", Category: "security", Title: "Password changed",
		Description: "The owner password was changed.", Enabled: true, Severity: "info",
		Channels: []string{"telegram", "email"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Password changed", "The owner password was changed."),
	},
	{
		Key: "security.api_token_created", Category: "security", Title: "API token created",
		Description: "A new API token was created.", Enabled: true, Severity: "info",
		Channels: []string{"telegram", "email"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("API token created", "A new API token was created."),
	},
	{
		Key: "security.2fa_changed", Category: "security", Title: "Two-factor changed",
		Description: "Two-factor sign-in was enabled or disabled.", Enabled: true, Severity: "info",
		Channels: []string{"telegram", "email"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Two-factor changed", "Two-factor sign-in was {value}."),
	},
	{
		Key: "platform.delivery_failing", Category: "platform", Title: "Notification delivery failing",
		Description: "A channel is failing; sent through another channel.", Enabled: true, Severity: "warn",
		Channels: []string{"telegram", "email"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Delivery failing", "Notification delivery is failing on a channel."),
	},
	{
		Key: "platform.retention_ran", Category: "platform", Title: "Retention completed",
		Description: "Nightly retention finished with counts.", Enabled: false, Severity: "info",
		Channels: []string{"telegram"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Retention completed", "Retention removed {count} rows."),
	},
	{
		Key: "platform.export_ready", Category: "platform", Title: "Data export ready",
		Description: "An export file is ready to download.", Enabled: true, Severity: "info",
		Channels: []string{"telegram", "email"}, Placeholders: basePlaceholders("format"),
		Templates: defaultTemplates("Export ready", "Data export completed ({format}, {count} rows)."),
	},
	{
		Key: "digest.daily", Category: "digests", Title: "Daily summary",
		Description: "Daily digest at 08:00 owner-local time for the preceding local day.", Enabled: true, Severity: "info",
		Channels: []string{"telegram", "email"}, Placeholders: basePlaceholders("period", "summary"),
		Templates: digestTemplates(),
	},
	{
		Key: "digest.weekly", Category: "digests", Title: "Weekly summary",
		Description: "Weekly digest on Monday at 08:00 owner-local time for the preceding Monday-Sunday week.", Enabled: true, Severity: "info",
		Channels: []string{"telegram", "email"}, Placeholders: basePlaceholders("period", "summary"),
		Templates: digestTemplates(),
	},
	{
		Key: "digest.monthly_isp", Category: "digests", Title: "Monthly ISP report",
		Description: "Monthly ISP report (JOOST-79).", Enabled: true, Severity: "info",
		Channels: []string{"telegram", "email"}, Placeholders: basePlaceholders(),
		Templates: defaultTemplates("Monthly ISP report", successBody()),
	},
}

// Events returns a deep copy of the catalog so callers cannot mutate defaults.
func Events() []Event {
	out := make([]Event, len(events))
	for i, e := range events {
		out[i] = cloneEvent(e)
	}
	return out
}

// LookupEvent finds one catalog event by key.
func LookupEvent(key string) (Event, bool) {
	for _, e := range events {
		if e.Key == key {
			return cloneEvent(e), true
		}
	}
	return Event{}, false
}

// EventForAlertMetric maps a legacy alert rule metric to its catalog event.
func EventForAlertMetric(metric string) (Event, bool) {
	for _, e := range events {
		if e.AlertMetric == metric {
			return cloneEvent(e), true
		}
	}
	return Event{}, false
}

// ValidEventKey reports whether key is in the catalog.
func ValidEventKey(key string) bool {
	_, ok := LookupEvent(key)
	return ok
}

// ValidEventPlaceholder reports whether name is allowed for the event.
func ValidEventPlaceholder(key, name string) bool {
	e, ok := LookupEvent(key)
	if !ok {
		return false
	}
	if name == "severity_icon" {
		return true
	}
	return slices.Contains(e.Placeholders, name)
}

func cloneEvent(e Event) Event {
	e.Channels = slices.Clone(e.Channels)
	e.Placeholders = slices.Clone(e.Placeholders)
	if e.Templates != nil {
		cp := make(map[string]string, len(e.Templates))
		for k, v := range e.Templates {
			cp[k] = v
		}
		e.Templates = cp
	}
	if e.Thresholds != nil {
		cp := make(map[string]float64, len(e.Thresholds))
		for k, v := range e.Thresholds {
			cp[k] = v
		}
		e.Thresholds = cp
	}
	return e
}
