package alerts

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/catalog"
	"github.com/jooservices/speedtest/internal/center/notify/template"
)

// QualityLimits are the configured thresholds used in every quality/success message.
type QualityLimits struct {
	DownloadPct float64
	UploadPct   float64
	PingMS      float64
	JitterMS    float64
	LossPct     float64
}

// DefaultQualityLimits matches the production owner defaults (JOOST-78).
func DefaultQualityLimits() QualityLimits {
	return QualityLimits{DownloadPct: 65, UploadPct: 40, PingMS: 30, JitterMS: 10, LossPct: 2}
}

// BuildPayload assembles template placeholders for one notification.
func BuildPayload(eventKey, nodeName, centerURL string, severity string, result agentapi.Result, expectedDown, expectedUp float64, limits QualityLimits, extra map[string]string) map[string]string {
	e, _ := catalog.LookupEvent(eventKey)
	title := e.Title
	if title == "" {
		title = eventKey
	}
	if severity == "" {
		severity = e.Severity
	}
	data := map[string]string{
		"node": nodeName, "label": "", "event": title, "severity": severity,
		"severity_icon":  template.SeverityIcon(severity),
		"download_limit": fmt.Sprintf("%.0f%% of expected", limits.DownloadPct),
		"upload_limit":   fmt.Sprintf("%.0f%% of expected", limits.UploadPct),
		"ping_limit":     formatNum(limits.PingMS),
		"jitter_limit":   formatNum(limits.JitterMS),
		"loss_limit":     formatNum(limits.LossPct),
		"link":           trimSlash(centerURL) + "/#/alerts",
		"center":         trimSlash(centerURL),
		"since":          time.Now().UTC().Format(time.RFC3339),
		"count":          "", "duration": "", "value": "", "expected": "", "pct": "", "threshold": "",
		"server": "", "isp": "", "ip": "", "old_ip": "", "city": "",
	}
	fillResult(data, result, expectedDown, expectedUp, limits)
	for k, v := range extra {
		data[k] = v
	}
	return data
}

func fillResult(data map[string]string, result agentapi.Result, expectedDown, expectedUp float64, limits QualityLimits) {
	if result.TS != "" {
		data["since"] = result.TS
	}
	data["download"] = mbps(result.Down)
	data["upload"] = mbps(result.Up)
	data["ping"] = optNum(result.Ping)
	data["jitter"] = optNum(result.Jitter)
	data["loss"] = optNum(result.Loss)
	data["download_ok"] = okFlag(result.Down != nil && expectedDown > 0 && *result.Down/expectedDown*100 >= limits.DownloadPct)
	data["upload_ok"] = okFlag(result.Up != nil && expectedUp > 0 && *result.Up/expectedUp*100 >= limits.UploadPct)
	data["ping_ok"] = okFlag(result.Ping != nil && *result.Ping <= limits.PingMS)
	data["jitter_ok"] = okFlag(result.Jitter != nil && *result.Jitter <= limits.JitterMS)
	data["loss_ok"] = okFlag(result.Loss != nil && *result.Loss <= limits.LossPct)
	if result.IP != nil {
		data["ip"] = *result.IP
	}
	if result.EngineISP != nil {
		data["isp"] = *result.EngineISP
	}
	if result.Server != nil {
		data["server"] = result.Server.Name
		if result.Server.City != "" {
			data["city"] = result.Server.City
		}
	}
	if expectedDown > 0 {
		data["expected"] = formatNum(expectedDown)
	}
}

// PayloadJSON marshals placeholder data for alert_events.payload.
func PayloadJSON(data map[string]string) json.RawMessage {
	raw, _ := json.Marshal(data) // map[string]string cannot fail to marshal
	return raw
}

func mbps(v *float64) string {
	if v == nil {
		return ""
	}
	return formatNum(*v) + " Mbps"
}

func optNum(v *float64) string {
	if v == nil {
		return ""
	}
	return formatNum(*v)
}

func formatNum(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func okFlag(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func metricValue(rule Rule, result agentapi.Result, expectedDown, expectedUp float64) (value, expected, pct string) {
	switch rule.Metric {
	case "down_pct":
		if result.Down != nil && expectedDown > 0 {
			p := *result.Down / expectedDown * 100
			return formatNum(p), formatNum(expectedDown), formatNum(p)
		}
	case "up_pct":
		if result.Up != nil && expectedUp > 0 {
			p := *result.Up / expectedUp * 100
			return formatNum(p), formatNum(expectedUp), formatNum(p)
		}
	case "ping":
		return optNum(result.Ping), "", optNum(result.Ping)
	case "jitter":
		return optNum(result.Jitter), "", optNum(result.Jitter)
	case "loss":
		return optNum(result.Loss), "", optNum(result.Loss)
	case "failed":
		return strconv.Itoa(rule.ForTests), "", ""
	}
	return "", "", ""
}
