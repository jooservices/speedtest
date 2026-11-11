package alerts

import (
	"encoding/json"
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
	DownloadSet bool
	UploadSet   bool
	PingSet     bool
	JitterSet   bool
	LossSet     bool
}

// DefaultQualityLimits matches the production owner defaults (JOOST-78).
func DefaultQualityLimits() QualityLimits {
	return QualityLimits{DownloadPct: catalogLimit("down_pct"), UploadPct: catalogLimit("up_pct"),
		PingMS: catalogLimit("ping"), JitterMS: catalogLimit("jitter"), LossPct: catalogLimit("loss"),
		DownloadSet: true, UploadSet: true, PingSet: true, JitterSet: true, LossSet: true}
}

func catalogLimit(metric string) float64 {
	event, ok := catalog.EventForAlertMetric(metric)
	if !ok {
		return 0
	}
	return event.Thresholds[metric]
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
	centerURL = trimSlash(centerURL)
	since := result.TS
	if since == "" {
		since = time.Now().UTC().Format(time.RFC3339)
	}
	data := make(map[string]string, 32)
	data["node"] = nodeName
	data["event"] = title
	data["severity"] = severity
	data["severity_icon"] = template.SeverityIcon(severity)
	data["download_limit"] = configuredLimit(limits.DownloadSet, strconv.FormatFloat(limits.DownloadPct, 'f', 0, 64)+"% of expected")
	data["upload_limit"] = configuredLimit(limits.UploadSet, strconv.FormatFloat(limits.UploadPct, 'f', 0, 64)+"% of expected")
	data["ping_limit"] = configuredLimit(limits.PingSet, formatNum(limits.PingMS))
	data["jitter_limit"] = configuredLimit(limits.JitterSet, formatNum(limits.JitterMS))
	data["loss_limit"] = configuredLimit(limits.LossSet, formatNum(limits.LossPct))
	data["link"] = centerURL + "/#/alerts"
	data["center"] = centerURL
	data["since"] = since
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
	data["download_ok"] = configuredOK(limits.DownloadSet, result.Down != nil && expectedDown > 0 && *result.Down/expectedDown*100 >= limits.DownloadPct)
	data["upload_ok"] = configuredOK(limits.UploadSet, result.Up != nil && expectedUp > 0 && *result.Up/expectedUp*100 >= limits.UploadPct)
	data["ping_ok"] = configuredOK(limits.PingSet, result.Ping != nil && *result.Ping <= limits.PingMS)
	data["jitter_ok"] = configuredOK(limits.JitterSet, result.Jitter != nil && *result.Jitter <= limits.JitterMS)
	data["loss_ok"] = configuredOK(limits.LossSet, result.Loss != nil && *result.Loss <= limits.LossPct)
	if result.Down != nil && expectedDown > 0 {
		data["pct"] = formatNum(*result.Down / expectedDown * 100)
	}
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

func configuredLimit(configured bool, value string) string {
	if !configured {
		return ""
	}
	return value
}

func configuredOK(configured, ok bool) string {
	if !configured {
		return ""
	}
	return okFlag(ok)
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
