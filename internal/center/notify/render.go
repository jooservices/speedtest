package notify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/catalog"
	"github.com/jooservices/speedtest/internal/center/notify/template"
	"github.com/jooservices/speedtest/internal/center/report"
)

const (
	maxTelegramMessageUnits = 4096
	maxDigestPeriodUnits    = 256
	maxDigestSummaryUnits   = 512
)

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Store) renderDelivery(ctx context.Context, q rowQuerier, d Delivery, centerURL string) (string, error) {
	if d.EventID == nil {
		return Message("Test notification", "Command center", d.CreatedAt, centerURL), nil
	}
	delivery, err := loadDeliveryTemplateData(ctx, q, *d.EventID)
	if err != nil {
		return "", err
	}
	prepareDeliveryVariables(delivery.values, delivery.eventKey, d.Channel, centerURL)
	if delivery.values["severity_icon"] == "" {
		delivery.values["severity_icon"] = template.SeverityIcon(delivery.values["severity"])
	}
	body := ""
	if delivery.eventKey != "" {
		body, err = s.templateBody(ctx, q, delivery.eventKey, d.Channel)
		if err != nil {
			return "", err
		}
	}
	if body == "" && !isDigestEvent(delivery.eventKey) {
		return Message(delivery.title, delivery.nodeName, delivery.since, centerURL), nil
	}
	return renderDeliveryTemplate(d.Channel, delivery.eventKey, body, delivery.values)
}

type deliveryTemplateData struct {
	title, nodeName, eventKey string
	since                     time.Time
	values                    map[string]string
}

func loadDeliveryTemplateData(ctx context.Context, q rowQuerier, eventID string) (deliveryTemplateData, error) {
	var fields deliveryTemplateData
	var payload []byte
	err := q.QueryRow(ctx, `SELECT e.title,COALESCE(n.name,e.payload->>'node','Command center'),e.ts,COALESCE(e.event_key,''),COALESCE(e.payload,'{}'::jsonb)
		FROM alert_events e LEFT JOIN nodes n ON n.id=e.node_id WHERE e.id=$1`, eventID).
		Scan(&fields.title, &fields.nodeName, &fields.since, &fields.eventKey, &payload)
	if err != nil {
		return deliveryTemplateData{}, err
	}
	fields.values = map[string]string{}
	_ = json.Unmarshal(payload, &fields.values)
	if fields.values["node"] == "" {
		fields.values["node"] = fields.nodeName
	}
	if fields.values["event"] == "" {
		fields.values["event"] = fields.title
	}
	if fields.values["since"] == "" {
		fields.values["since"] = fields.since.UTC().Format(time.RFC3339)
	}
	return fields, nil
}

func prepareDeliveryVariables(data map[string]string, eventKey, channel, centerURL string) {
	if isDigestEvent(eventKey) {
		prepareDigestVariables(data, channel, centerURL)
		return
	}
	if data["link"] == "" {
		data["link"] = trimRightSlash(centerURL) + "/#/alerts"
	}
	if data["center"] == "" {
		data["center"] = trimRightSlash(centerURL)
	}
}

func prepareDigestVariables(data map[string]string, channel, centerURL string) {
	data["node"] = "Command center"
	data["center"] = ""
	if data["link"] != "" && !report.IsDashboardLinkFor(centerURL, data["link"]) {
		data["link"] = ""
	}
	if channel != "telegram" {
		return
	}
	data["summary"] = boundedDigestSummary(data)
	if telegramTextUnits(data["period"]) > maxDigestPeriodUnits {
		data["period"] = "Digest period"
	}
}

func renderDeliveryTemplate(channel, eventKey, body string, data map[string]string) (string, error) {
	digest := isDigestEvent(eventKey)
	if body == "" && digest {
		body = "{period}\n{summary}\n{link}"
	}
	rendered, err := template.Render(channel, body, data)
	if err != nil || channel != "telegram" || !digest || telegramTextUnits(rendered) <= maxTelegramMessageUnits {
		return rendered, err
	}
	return renderBoundedTelegramDigest(data)
}

func isDigestEvent(key string) bool {
	return key == "digest.daily" || key == "digest.weekly"
}

func boundedDigestSummary(data map[string]string) string {
	summary := data["telegram_summary"]
	if summary == "" && strings.HasPrefix(data["summary"], "No speed test results were recorded") {
		summary = data["summary"]
	}
	if summary == "" || telegramTextUnits(summary) > maxDigestSummaryUnits {
		return "Per-node counts and averages continue in the dashboard."
	}
	return summary
}

func renderBoundedTelegramDigest(data map[string]string) (string, error) {
	body := "{period}\n{summary}\nFull per-node details and averages continue in the dashboard. Open dashboard: {link}"
	if data["link"] == "" {
		body = "{period}\n{summary}"
	}
	rendered, err := template.Render("telegram", body, data)
	if err != nil {
		return "", err
	}
	if telegramTextUnits(rendered) > maxTelegramMessageUnits {
		return "", template.ErrInvalidTemplate
	}
	return rendered, nil
}

func telegramTextUnits(text string) int {
	units := 0
	for _, character := range text {
		units++
		if character > 0xFFFF {
			units++
		}
	}
	return units
}

func (s *Store) templateBody(ctx context.Context, q rowQuerier, eventKey, channel string) (string, error) {
	var body string
	err := q.QueryRow(ctx, `SELECT body FROM notification_templates WHERE event_key=$1 AND channel=$2`, eventKey, channel).Scan(&body)
	if err == nil {
		if body == "" {
			return "", template.ErrInvalidTemplate
		}
		return body, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if def, ok := template.DefaultBody(eventKey, channel); ok {
		return def, nil
	}
	if _, ok := catalog.LookupEvent(eventKey); ok {
		if def, ok := template.DefaultBody(eventKey, "email"); ok {
			return def, nil
		}
	}
	return "", nil
}

func trimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
