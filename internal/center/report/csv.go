package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"
)

var csvHeader = []string{"month", "isp", "asn", "node", "plan", "advertised_down_mbps", "advertised_up_mbps", "tests", "failures",
	"avg_down_mbps", "p95_down_mbps", "avg_up_mbps", "p95_up_mbps", "delivered_percent", "worst_hours_utc", "downtime_minutes", "ip_changes"}

// safeCell stops a spreadsheet from running a name that starts with a formula character as a formula.
func safeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func num(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

func hours(h []int) string {
	parts := make([]string, len(h))
	for i, v := range h {
		parts[i] = fmt.Sprintf("%02d", v)
	}
	return strings.Join(parts, " ")
}

func planName(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// CSV renders the report as RFC 4180 CSV.
func CSV(r Report) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(csvHeader); err != nil {
		return nil, err
	}
	for _, row := range r.Rows {
		rec := []string{r.Month, safeCell(row.ISP), strconv.FormatInt(row.ASN, 10), safeCell(row.Node), safeCell(planName(row.Plan)),
			num(row.AdvertisedDown), num(row.AdvertisedUp), strconv.Itoa(row.Tests), strconv.Itoa(row.Failures),
			num(row.AvgDown), num(row.P95Down), num(row.AvgUp), num(row.P95Up), num(row.DeliveredPct),
			hours(row.WorstHours), strconv.Itoa(row.DowntimeMin), strconv.Itoa(row.IPChanges)}
		if err := w.Write(rec); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}
