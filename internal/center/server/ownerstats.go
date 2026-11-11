package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jooservices/speedtest/internal/center/stats"
)

type OwnerStatsStore interface {
	Summary(context.Context, stats.Query) ([]stats.SummaryComparison, error)
	Trend(context.Context, stats.Query) ([]stats.Series, error)
	HourOfDay(context.Context, stats.Query) ([]stats.HourSeries, error)
	Heatmap(context.Context, stats.Query, string) (stats.Heatmap, error)
	Distribution(context.Context, stats.Query) ([]stats.Distribution, error)
}

const maxStatsSpan = 366 * 24 * time.Hour

var statsRanges = map[string]time.Duration{
	"6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour,
}

type ownerStats struct {
	store   OwnerStatsStore
	limiter *nodeRateLimiter
	now     func() time.Time
}

func newOwnerStatsHandler(store OwnerStatsStore, auth *serverAuth) http.Handler {
	h := &ownerStats{store: store, limiter: newOwnerRateLimiter(), now: time.Now}
	return ownerReader(auth, h.limiter, h.serve)
}

func ownerStatsRoute(path string) bool {
	switch path {
	case "/api/stats/summary", "/api/stats/trend", "/api/stats/hour-of-day", "/api/stats/heatmap", "/api/stats/distribution":
		return true
	default:
		return false
	}
}

func (h *ownerStats) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/stats/summary":
		h.summary(w, r)
	case "/api/stats/trend":
		h.trend(w, r)
	case "/api/stats/hour-of-day":
		h.hourOfDay(w, r)
	case "/api/stats/heatmap":
		h.heatmap(w, r)
	case "/api/stats/distribution":
		h.distribution(w, r)
	default:
		writeNotFoundProblem(w, r)
	}
}

func (h *ownerStats) summary(w http.ResponseWriter, r *http.Request) {
	q, ok := h.window(w, r)
	if !ok {
		return
	}
	if q.NodeIDs, q.NodesSet, ok = parseStatsNodes(w, r); !ok {
		return
	}
	if q.ISPIDs, q.ISPsSet, ok = parseStatsISPs(w, r); !ok {
		return
	}
	items, err := h.store.Summary(r.Context(), q)
	if err != nil {
		writeStatsError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", items)
}

func (h *ownerStats) trend(w http.ResponseWriter, r *http.Request) {
	q, ok := h.window(w, r)
	if !ok {
		return
	}
	if q.NodeIDs, q.NodesSet, ok = parseStatsNodes(w, r); !ok {
		return
	}
	if q.ISPIDs, q.ISPsSet, ok = parseStatsISPs(w, r); !ok {
		return
	}
	if q.Metric, ok = parseStatsMetric(w, r); !ok {
		return
	}
	if q.Timezone, ok = parseStatsTimezone(w, r); !ok {
		return
	}
	q.Bucket = "day"
	if raw, present, valid := singleStatsParam(w, r, "bucket"); !valid {
		return
	} else if present {
		if raw != "hour" && raw != "day" {
			statsInvalid(w, r, "bucket must be hour or day.")
			return
		}
		q.Bucket = raw
	}
	if raw, present, valid := singleStatsParam(w, r, "combined"); !valid {
		return
	} else if present {
		if raw != "true" && raw != "false" {
			statsInvalid(w, r, "combined must be true or false.")
			return
		}
		q.Combined = raw == "true"
	}
	if q.Combined && q.Metric != stats.Download && q.Metric != stats.Upload {
		statsInvalid(w, r, "combined is available only for down and up metrics.")
		return
	}
	if countCalendarBuckets(q.From, q.To, q.Bucket, q.Timezone) > 300 {
		statsInvalid(w, r, "the requested trend contains more than 300 buckets.")
		return
	}
	items, err := h.store.Trend(r.Context(), q)
	if err != nil {
		writeStatsError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", items)
}

func (h *ownerStats) hourOfDay(w http.ResponseWriter, r *http.Request) {
	q, ok := h.window(w, r)
	if !ok {
		return
	}
	if q.NodeIDs, q.NodesSet, ok = parseStatsNodes(w, r); !ok {
		return
	}
	if q.ISPIDs, q.ISPsSet, ok = parseStatsISPs(w, r); !ok {
		return
	}
	if q.Metric, ok = parseStatsMetric(w, r); !ok {
		return
	}
	if q.Timezone, ok = parseStatsTimezone(w, r); !ok {
		return
	}
	items, err := h.store.HourOfDay(r.Context(), q)
	if err != nil {
		writeStatsError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", items)
}

func (h *ownerStats) heatmap(w http.ResponseWriter, r *http.Request) {
	q := stats.Query{Metric: stats.Download}
	var engineOK bool
	if q.Engine, engineOK = parseEngine(w, r); !engineOK {
		return
	}
	var ok bool
	if q.ISPIDs, q.ISPsSet, ok = parseStatsISPs(w, r); !ok {
		return
	}
	if q.Metric, ok = parseStatsMetric(w, r); !ok {
		return
	}
	if q.Timezone, ok = parseStatsTimezone(w, r); !ok {
		return
	}
	raw, present, valid := singleStatsParam(w, r, "node")
	if !valid {
		return
	}
	if !present || !uuidPattern.MatchString(raw) {
		statsInvalid(w, r, "node must be a valid node ID.")
		return
	}
	now := h.now().UTC()
	q.From, q.To = now.Add(-30*24*time.Hour), now
	item, err := h.store.Heatmap(r.Context(), q, strings.ToLower(raw))
	if err != nil {
		writeStatsError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", item)
}

func (h *ownerStats) distribution(w http.ResponseWriter, r *http.Request) {
	q, ok := h.window(w, r)
	if !ok {
		return
	}
	if q.NodeIDs, q.NodesSet, ok = parseStatsNodes(w, r); !ok {
		return
	}
	if q.ISPIDs, q.ISPsSet, ok = parseStatsISPs(w, r); !ok {
		return
	}
	if q.Metric, ok = parseStatsMetric(w, r); !ok {
		return
	}
	if q.Timezone, ok = parseStatsTimezone(w, r); !ok {
		return
	}
	q.Bins = 15
	if raw, present, valid := singleStatsParam(w, r, "bins"); !valid {
		return
	} else if present {
		bins, err := strconv.Atoi(raw)
		if err != nil || bins < 1 || bins > 100 {
			statsInvalid(w, r, "bins must be an integer between 1 and 100.")
			return
		}
		q.Bins = bins
	}
	items, err := h.store.Distribution(r.Context(), q)
	if err != nil {
		writeStatsError(w, r, err)
		return
	}
	writeEnvelope(w, http.StatusOK, "Success", items)
}

func (h *ownerStats) window(w http.ResponseWriter, r *http.Request) (stats.Query, bool) {
	q := stats.Query{}
	var engineOK bool
	if q.Engine, engineOK = parseEngine(w, r); !engineOK {
		return q, false
	}
	fromRaw, fromSet, valid := singleStatsParam(w, r, "from")
	if !valid {
		return q, false
	}
	toRaw, toSet, valid := singleStatsParam(w, r, "to")
	if !valid {
		return q, false
	}
	rangeRaw, rangeSet, valid := singleStatsParam(w, r, "range")
	if !valid {
		return q, false
	}
	var from, to time.Time
	switch {
	case fromSet || toSet:
		if rangeSet || !fromSet || !toSet {
			statsInvalid(w, r, "from and to must be supplied together, without range.")
			return q, false
		}
		parsedFrom, err := time.Parse(time.RFC3339Nano, fromRaw)
		if err != nil {
			statsInvalid(w, r, "from must be an RFC 3339 timestamp.")
			return q, false
		}
		parsedTo, err := time.Parse(time.RFC3339Nano, toRaw)
		if err != nil {
			statsInvalid(w, r, "to must be an RFC 3339 timestamp.")
			return q, false
		}
		from, to = parsedFrom, parsedTo
	case rangeSet:
		duration, ok := statsRanges[rangeRaw]
		if !ok {
			statsInvalid(w, r, "range must be 6h, 24h, 7d or 30d.")
			return q, false
		}
		to = h.now().UTC()
		from = to.Add(-duration)
	default:
		to = h.now().UTC()
		from = to.Add(-statsRanges["7d"])
	}
	if !to.After(from) {
		statsInvalid(w, r, "to must be later than from.")
		return q, false
	}
	duration := to.Sub(from)
	if duration > maxStatsSpan {
		statsInvalid(w, r, "the requested analytics range must not exceed 366 days.")
		return q, false
	}
	q.From, q.To = from.UTC(), to.UTC()
	q.PreviousFrom, q.PreviousTo = q.From.Add(-duration), q.From
	return q, true
}

func parseStatsNodes(w http.ResponseWriter, r *http.Request) ([]string, bool, bool) {
	return parseStatsIDList(w, r, "nodes", "nodes must be comma-separated node IDs.", "nodes accepts at most 100 IDs.")
}

func parseStatsISPs(w http.ResponseWriter, r *http.Request) ([]string, bool, bool) {
	return parseStatsIDList(w, r, "isps", "isps must be comma-separated ISP IDs.", "isps accepts at most 100 IDs.")
}

func parseStatsIDList(w http.ResponseWriter, r *http.Request, key, badID, tooMany string) ([]string, bool, bool) {
	raw, present, valid := singleStatsParam(w, r, key)
	if !valid {
		return nil, false, false
	}
	if !present {
		return nil, false, true
	}
	if raw == "" {
		return []string{}, true, true
	}
	seen := make(map[string]bool)
	ids := make([]string, 0)
	for _, part := range strings.Split(raw, ",") {
		id := strings.ToLower(strings.TrimSpace(part))
		if id == "" || seen[id] {
			continue
		}
		if !uuidPattern.MatchString(id) {
			statsInvalid(w, r, badID)
			return nil, true, false
		}
		seen[id] = true
		ids = append(ids, id)
		if len(ids) > 100 {
			statsInvalid(w, r, tooMany)
			return nil, true, false
		}
	}
	return ids, true, true
}

func parseStatsMetric(w http.ResponseWriter, r *http.Request) (stats.Metric, bool) {
	raw, present, valid := singleStatsParam(w, r, "metric")
	if !valid {
		return "", false
	}
	if !present {
		return stats.Download, true
	}
	metric := stats.Metric(raw)
	switch metric {
	case stats.Download, stats.Upload, stats.Ping, stats.Jitter, stats.Loss:
		return metric, true
	default:
		statsInvalid(w, r, "metric must be down, up, ping, jitter or loss.")
		return "", false
	}
}

func parseStatsTimezone(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw, present, valid := singleStatsParam(w, r, "timezone")
	if !valid {
		return "", false
	}
	if !present {
		return "UTC", true
	}
	if _, err := time.LoadLocation(raw); err != nil {
		statsInvalid(w, r, "timezone must be a valid IANA time zone.")
		return "", false
	}
	return canonicalZone(raw), true
}

// zoneAliases maps legacy IANA link names, which browsers still report (Asia/Saigon is what many Vietnamese
// systems return), to the canonical names PostgreSQL's time zone database knows. Go accepts both spellings,
// PostgreSQL only the canonical one, so an unmapped alias would turn into a 500 in the grouping queries.
var zoneAliases = map[string]string{
	"Asia/Saigon": "Asia/Ho_Chi_Minh", "Asia/Calcutta": "Asia/Kolkata", "Asia/Katmandu": "Asia/Kathmandu",
	"Asia/Rangoon": "Asia/Yangon", "Asia/Dacca": "Asia/Dhaka", "Asia/Thimbu": "Asia/Thimphu",
	"Asia/Ujung_Pandang": "Asia/Makassar", "Asia/Ulan_Bator": "Asia/Ulaanbaatar", "Asia/Macao": "Asia/Macau",
	"Asia/Chongqing": "Asia/Shanghai", "Asia/Chungking": "Asia/Shanghai", "Asia/Harbin": "Asia/Shanghai",
	"Asia/Istanbul": "Europe/Istanbul", "Asia/Ashkhabad": "Asia/Ashgabat", "Asia/Tel_Aviv": "Asia/Jerusalem",
	"Europe/Kiev": "Europe/Kyiv", "Europe/Belfast": "Europe/London", "Atlantic/Faeroe": "Atlantic/Faroe",
	"America/Buenos_Aires": "America/Argentina/Buenos_Aires", "America/Indianapolis": "America/Indiana/Indianapolis",
	"America/Louisville": "America/Kentucky/Louisville", "America/Godthab": "America/Nuuk", "Pacific/Samoa": "Pacific/Pago_Pago",
	"Australia/ACT": "Australia/Sydney", "Australia/NSW": "Australia/Sydney", "Australia/Victoria": "Australia/Melbourne",
	"US/Eastern": "America/New_York", "US/Central": "America/Chicago", "US/Mountain": "America/Denver", "US/Pacific": "America/Los_Angeles",
	"Etc/UTC": "UTC", "Etc/GMT": "UTC", "GMT": "UTC", "Zulu": "UTC", "Universal": "UTC",
}

func canonicalZone(name string) string {
	if canonical, ok := zoneAliases[name]; ok {
		return canonical
	}
	return name
}

func singleStatsParam(w http.ResponseWriter, r *http.Request, name string) (string, bool, bool) {
	items, present := r.URL.Query()[name]
	if !present {
		return "", false, true
	}
	if len(items) != 1 {
		statsInvalid(w, r, name+" must be supplied once.")
		return "", true, false
	}
	return items[0], true, true
}

func countCalendarBuckets(from, to time.Time, bucket, timezone string) int {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return 0
	}
	startLocal := from.In(location)
	endLocal := to.Add(-time.Nanosecond).In(location)
	start := time.Date(startLocal.Year(), startLocal.Month(), startLocal.Day(), startLocal.Hour(), 0, 0, 0, time.UTC)
	end := time.Date(endLocal.Year(), endLocal.Month(), endLocal.Day(), endLocal.Hour(), 0, 0, 0, time.UTC)
	step := time.Hour
	if bucket == "day" {
		start = time.Date(startLocal.Year(), startLocal.Month(), startLocal.Day(), 0, 0, 0, 0, time.UTC)
		end = time.Date(endLocal.Year(), endLocal.Month(), endLocal.Day(), 0, 0, 0, 0, time.UTC)
		step = 24 * time.Hour
	}
	count := 0
	for label := start; !label.After(end); label = label.Add(step) {
		local := time.Date(label.Year(), label.Month(), label.Day(), label.Hour(), 0, 0, 0, location)
		actual := local.In(location)
		if actual.Year() == label.Year() && actual.Month() == label.Month() && actual.Day() == label.Day() && actual.Hour() == label.Hour() {
			count++
		}
	}
	return count
}

func statsInvalid(w http.ResponseWriter, r *http.Request, detail string) {
	writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", detail)
}

func writeStatsError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, stats.ErrNotFound) {
		writeNotFoundProblem(w, r)
		return
	}
	writeAPIProblem(w, r, http.StatusInternalServerError, "Internal failure", "Internal failure; safe detail only.")
}
