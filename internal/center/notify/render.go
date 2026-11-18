package notify

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/catalog"
	"github.com/jooservices/speedtest/internal/center/notify/template"
	"github.com/jooservices/speedtest/internal/center/report"
)

const (
	maxTelegramMessageUnits        = 4096
	maxDigestPeriodUnits           = 256
	maxDigestDashboardLinkRunes    = 512
	maxDigestDashboardLinkUnits    = maxDigestDashboardLinkRunes * 6
	maxDigestPartHeaderUnits       = 47
	digestDashboardLinePrefixUnits = 12 // newline plus "Dashboard: "
	maxDigestChunkTextUnits        = maxTelegramMessageUnits - maxDigestDashboardLinkUnits - maxDigestPeriodUnits - maxDigestPartHeaderUnits - 2 - digestDashboardLinePrefixUnits
	emptyDigestSummary             = "No speed test results were recorded for this period."
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
	if err := prepareDeliveryVariables(delivery.values, delivery.eventKey, d.Channel, centerURL); err != nil {
		return "", err
	}
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

func prepareDeliveryVariables(data map[string]string, eventKey, channel, centerURL string) error {
	if isDigestEvent(eventKey) {
		return prepareDigestVariables(data, channel, centerURL)
	}
	if data["link"] == "" {
		data["link"] = trimRightSlash(centerURL) + "/#/alerts"
	}
	if data["center"] == "" {
		data["center"] = trimRightSlash(centerURL)
	}
	return nil
}

func prepareDigestVariables(data map[string]string, channel, centerURL string) error {
	data["node"] = "Command center"
	data["center"] = ""
	if digestHasResults(data["summary"]) {
		link, err := report.DashboardLink(centerURL)
		if err != nil {
			return err
		}
		data["link"] = link
	} else {
		data["link"] = ""
	}
	data["digest_link_safe"] = "true"
	if channel != "telegram" {
		return nil
	}
	if data["telegram_summary"] != "" {
		data["summary"] = data["telegram_summary"]
	}
	if telegramEscapedTextUnits(data["period"]) > maxDigestPeriodUnits {
		data["period"] = "Digest period"
	}
	return nil
}

func renderDeliveryTemplate(channel, eventKey, body string, data map[string]string) (string, error) {
	digest := isDigestEvent(eventKey)
	if digest {
		var err error
		body, err = digestTemplateBody(body, data)
		if err != nil {
			return "", err
		}
	}
	rendered, err := template.Render(channel, body, data)
	if err != nil || channel != "telegram" || !digest {
		return rendered, err
	}
	if part := digestPartHeader(data); part != "" {
		rendered = part + "\n" + rendered
	}
	if telegramTextUnits(rendered) <= maxTelegramMessageUnits {
		return rendered, nil
	}
	return renderBoundedTelegramDigest(data)
}

func digestTemplateBody(body string, data map[string]string) (string, error) {
	if body == "" {
		body = "{period}\n{summary}\n{link}"
	}
	populated := digestHasResults(data["summary"])
	if populated && (data["digest_link_safe"] != "true" || data["link"] == "") {
		return "", report.ErrInvalidDashboardURL
	}
	return includeDigestRequirements(body, populated), nil
}

func isDigestEvent(key string) bool {
	return key == "digest.daily" || key == "digest.weekly"
}

func digestHasResults(summary string) bool {
	return strings.TrimSpace(summary) != emptyDigestSummary
}

func includeDigestRequirements(body string, populated bool) string {
	placeholders := template.PlaceholdersIn(body)
	if !slices.Contains(placeholders, "summary") {
		body += "\n{summary}"
	}
	if populated && !slices.Contains(placeholders, "link") {
		body += "\nDashboard: {link}"
	}
	return body
}

func renderBoundedTelegramDigest(data map[string]string) (string, error) {
	bounded := mapsClone(data)
	body := "{period}\n{summary}"
	if digestHasResults(bounded["summary"]) {
		if bounded["digest_link_safe"] != "true" || bounded["link"] == "" {
			return "", report.ErrInvalidDashboardURL
		}
		body += "\nDashboard: {link}"
	}
	rendered, err := renderBoundedBody(body, bounded)
	if err != nil || telegramTextUnits(rendered) > maxTelegramMessageUnits {
		return "", template.ErrInvalidTemplate
	}
	return rendered, nil
}

func renderBoundedBody(body string, data map[string]string) (string, error) {
	rendered, err := template.Render("telegram", body, data)
	if err != nil {
		return "", err
	}
	if part := digestPartHeader(data); part != "" {
		rendered = part + "\n" + rendered
	}
	return rendered, nil
}

func digestPartHeader(data map[string]string) string {
	part, err := strconv.Atoi(data["telegram_part"])
	if err != nil || part < 1 {
		return ""
	}
	total, err := strconv.Atoi(data["telegram_parts"])
	if err != nil || total < part {
		return ""
	}
	return "Part " + strconv.Itoa(part) + " of " + strconv.Itoa(total)
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

func telegramEscapedTextUnits(text string) int {
	units := 0
	for _, character := range text {
		units += telegramEscapedRuneUnits(character)
	}
	return units
}

func (s *Store) templateBody(ctx context.Context, q rowQuerier, eventKey, channel string) (string, error) {
	var body string
	err := q.QueryRow(ctx, `SELECT body FROM notification_templates WHERE event_key=$1 AND channel=$2`, eventKey, channel).Scan(&body)
	if err == nil {
		if strings.TrimSpace(body) == "" {
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
