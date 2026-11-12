package notify

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/catalog"
	"github.com/jooservices/speedtest/internal/center/notify/template"
)

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Store) renderDelivery(ctx context.Context, q rowQuerier, d Delivery, centerURL string) (string, error) {
	if d.EventID == nil {
		return Message("Test notification", "Command center", d.CreatedAt, centerURL), nil
	}
	var title, nodeName, eventKey string
	var since time.Time
	var payload []byte
	err := q.QueryRow(ctx, `SELECT e.title,COALESCE(n.name,e.payload->>'node','Command center'),e.ts,COALESCE(e.event_key,''),COALESCE(e.payload,'{}'::jsonb)
		FROM alert_events e LEFT JOIN nodes n ON n.id=e.node_id WHERE e.id=$1`, *d.EventID).
		Scan(&title, &nodeName, &since, &eventKey, &payload)
	if err != nil {
		return "", err
	}
	data := map[string]string{}
	_ = json.Unmarshal(payload, &data)
	if data["node"] == "" {
		data["node"] = nodeName
	}
	if data["event"] == "" {
		data["event"] = title
	}
	if data["since"] == "" {
		data["since"] = since.UTC().Format(time.RFC3339)
	}
	if data["link"] == "" {
		data["link"] = trimRightSlash(centerURL) + "/#/alerts"
	}
	if data["center"] == "" {
		data["center"] = trimRightSlash(centerURL)
	}
	if data["severity_icon"] == "" {
		data["severity_icon"] = template.SeverityIcon(data["severity"])
	}
	body := ""
	if eventKey != "" {
		body, err = s.templateBody(ctx, q, eventKey, d.Channel)
		if err != nil {
			return "", err
		}
	}
	if body == "" {
		return Message(title, nodeName, since, centerURL), nil
	}
	return template.Render(d.Channel, body, data)
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
