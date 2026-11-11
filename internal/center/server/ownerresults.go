package server

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jooservices/speedtest/internal/center/ownerapi"
)

// OwnerResultsStore is the dashboard-side result API. All routes are read-only and need a session.
type OwnerResultsStore interface {
	ListResults(ctx context.Context, f ownerapi.ResultFilter, page, size int) ([]map[string]any, int, error)
	GetResult(ctx context.Context, id string) (map[string]any, error)
	ExportResults(ctx context.Context, f ownerapi.ResultFilter, emit func(ownerapi.ResultRow) error) error
}

const maxResultSpan = 366 * 24 * time.Hour

var resultRanges = map[string]time.Duration{"6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

// csvHeader lists the export columns; they match the dashboard's own export.
var csvHeader = []string{"time", "node", "status", "download_mbps", "upload_mbps", "ping_ms", "jitter_ms", "packet_loss_pct",
	"loaded_latency_down_ms", "loaded_latency_up_ms", "server", "public_ip", "error", "failure_class", "engine"}

type ownerResults struct {
	store   OwnerResultsStore
	limiter *nodeRateLimiter
	now     func() time.Time
}

func newOwnerResultsHandler(store OwnerResultsStore, auth *serverAuth) http.Handler {
	h := &ownerResults{store: store, limiter: newOwnerRateLimiter(), now: time.Now}
	return ownerReader(auth, h.limiter, h.serve)
}

// ownerResultsRoute reports whether the path belongs to the result API.
func ownerResultsRoute(path string) bool {
	return path == "/api/export/results.csv" || path == "/api/export/results.json" || path == "/api/results" || path == "/api/results/export.csv" ||
		(strings.HasPrefix(path, "/api/results/") && strings.Count(strings.Trim(path, "/"), "/") == 2)
}

func (h *ownerResults) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/export/results.csv":
		h.exportCSV(w, r, ownerapi.ResultFilter{Sort: "ts", Dir: "asc", AllEngines: true})
	case "/api/export/results.json":
		h.exportJSON(w, r)
	case "/api/results":
		h.list(w, r)
	case "/api/results/export.csv":
		filter, ok := h.filter(w, r)
		if !ok {
			return
		}
		h.exportCSV(w, r, filter)
	default:
		id := strings.TrimPrefix(r.URL.Path, "/api/results/")
		if !uuidPattern.MatchString(id) {
			writeNotFoundProblem(w, r)
			return
		}
		result, err := h.store.GetResult(r.Context(), id)
		if err != nil {
			writeOwnerError(w, r, err)
			return
		}
		writeEnvelope(w, http.StatusOK, "Success", result)
	}
}

func (h *ownerResults) list(w http.ResponseWriter, r *http.Request) {
	filter, ok := h.filter(w, r)
	if !ok {
		return
	}
	page, size, ok := paging(w, r)
	if !ok {
		return
	}
	items, total, err := h.store.ListResults(r.Context(), filter, page, size)
	if err != nil {
		writeOwnerError(w, r, err)
		return
	}
	writePage(w, items, page, size, total)
}

func (h *ownerResults) exportCSV(w http.ResponseWriter, r *http.Request, filter ownerapi.ResultFilter) {
	var out *csv.Writer
	flusher, _ := w.(http.Flusher)
	start := func() {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="speedtest-results-`+h.now().UTC().Format("2006-01-02")+`.csv"`)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		out = csv.NewWriter(w)
		_ = out.Write(csvHeader)
	}
	rows := 0
	err := h.store.ExportResults(r.Context(), filter, func(row ownerapi.ResultRow) error {
		if out == nil {
			start()
		}
		if err := out.Write(csvRecord(row)); err != nil {
			return err
		}
		rows++
		if rows%500 == 0 {
			out.Flush()
			if err := out.Error(); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		return nil
	})
	if out == nil && err == nil {
		start()
	}
	if out == nil {
		writeOwnerError(w, r, err)
		return
	}
	out.Flush()
	if err != nil {
		// The status line is already sent; cut the connection so the client sees an incomplete download.
		panic(http.ErrAbortHandler)
	}
}

// exportJSON streams every stored result, oldest first, as one JSON array without the response envelope.
func (h *ownerResults) exportJSON(w http.ResponseWriter, r *http.Request) {
	flusher, _ := w.(http.Flusher)
	started := false
	rows := 0
	enc := func(row ownerapi.ResultRow) error {
		if !started {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="speedtest-results-`+h.now().UTC().Format("2006-01-02")+`.json"`)
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte("[")); err != nil {
				return err
			}
			started = true
		} else if _, err := w.Write([]byte(",")); err != nil {
			return err
		}
		raw, err := json.Marshal(row.Result)
		if err != nil {
			return err
		}
		if _, err := w.Write(raw); err != nil {
			return err
		}
		rows++
		if rows%500 == 0 && flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	err := h.store.ExportResults(r.Context(), ownerapi.ResultFilter{Sort: "ts", Dir: "asc", AllEngines: true}, enc)
	if !started && err != nil {
		writeOwnerError(w, r, err)
		return
	}
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	if !started {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="speedtest-results-`+h.now().UTC().Format("2006-01-02")+`.json"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
		return
	}
	_, _ = w.Write([]byte("]"))
}

// csvRecord renders one result. Text cells that a spreadsheet would read as a formula are neutralised.
func csvRecord(row ownerapi.ResultRow) []string {
	res := row.Result
	text := func(v any) string {
		s, _ := v.(string)
		return neutralise(s)
	}
	num := func(v any) string {
		switch n := v.(type) {
		case float64:
			return strconv.FormatFloat(n, 'f', -1, 64)
		case int64:
			return strconv.FormatInt(n, 10)
		}
		return ""
	}
	node := strings.TrimSpace(row.NodeName + " " + row.NodeLabel)
	server := ""
	if s, ok := res["server"].(map[string]any); ok {
		name, _ := s["name"].(string)
		city, _ := s["city"].(string)
		server = strings.Trim(strings.TrimSpace(name)+" - "+strings.TrimSpace(city), " -")
	}
	class := text(res["failureClass"])
	if class == "" {
		class = "other"
	}
	return []string{text(res["ts"]), neutralise(node), text(res["status"]), num(res["down"]), num(res["up"]), num(res["ping"]),
		num(res["jitter"]), num(res["loss"]), num(res["dLat"]), num(res["uLat"]), neutralise(server), text(res["ip"]), text(res["error"]), class, text(res["engine"])}
}

func neutralise(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func invalid(w http.ResponseWriter, r *http.Request, detail string) bool {
	writeAPIProblem(w, r, http.StatusUnprocessableEntity, "Validation failed", detail)
	return false
}

// filter parses and validates the filter and sort parameters shared by the list and the export.
func (h *ownerResults) filter(w http.ResponseWriter, r *http.Request) (ownerapi.ResultFilter, bool) {
	q := r.URL.Query()
	var f ownerapi.ResultFilter
	var valid bool
	if f.Engine, valid = parseEngine(w, r); !valid {
		return f, false
	}
	if _, present := q["nodes"]; present {
		f.NodesSet = true
		seen := map[string]bool{}
		for _, id := range strings.Split(q.Get("nodes"), ",") {
			id = strings.ToLower(strings.TrimSpace(id))
			if id == "" || seen[id] {
				continue
			}
			if !uuidPattern.MatchString(id) {
				return f, invalid(w, r, "nodes must be comma-separated node IDs.")
			}
			seen[id] = true
			f.Nodes = append(f.Nodes, id)
		}
		if len(f.Nodes) > 100 {
			return f, invalid(w, r, "nodes accepts at most 100 IDs.")
		}
	}
	if _, present := q["isps"]; present {
		f.ISPsSet = true
		seen := map[string]bool{}
		for _, id := range strings.Split(q.Get("isps"), ",") {
			id = strings.ToLower(strings.TrimSpace(id))
			if id == "" || seen[id] {
				continue
			}
			if !uuidPattern.MatchString(id) {
				return f, invalid(w, r, "isps must be comma-separated ISP IDs.")
			}
			seen[id] = true
			f.ISPs = append(f.ISPs, id)
		}
		if len(f.ISPs) > 100 {
			return f, invalid(w, r, "isps accepts at most 100 IDs.")
		}
	}
	if !h.timeRange(w, r, q, &f) {
		return f, false
	}
	switch status := q.Get("status"); status {
	case "", "all":
	case "ok", "degraded", "failed":
		f.Status = status
	default:
		return f, invalid(w, r, "status must be all, ok, degraded or failed.")
	}
	if f.Query = strings.TrimSpace(q.Get("q")); len([]rune(f.Query)) > 200 {
		return f, invalid(w, r, "q must be at most 200 characters.")
	}
	for key, dest := range map[string]**float64{"minDown": &f.MinDown, "maxDown": &f.MaxDown} {
		if raw := q.Get(key); raw != "" {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
				return f, invalid(w, r, key+" must be a non-negative number.")
			}
			*dest = &v
		}
	}
	if f.MinDown != nil && f.MaxDown != nil && *f.MaxDown < *f.MinDown {
		return f, invalid(w, r, "maxDown must not be below minDown.")
	}
	if f.Sort = q.Get("sort"); f.Sort != "" && !ownerapi.ValidResultSort(f.Sort) {
		return f, invalid(w, r, "sort is not an allowed key.")
	}
	if f.Dir = q.Get("dir"); f.Dir != "" && f.Dir != "asc" && f.Dir != "desc" {
		return f, invalid(w, r, "dir must be asc or desc.")
	}
	return f, true
}

func (h *ownerResults) timeRange(w http.ResponseWriter, r *http.Request, q url.Values, f *ownerapi.ResultFilter) bool {
	fromRaw, toRaw, rangeRaw := q.Get("from"), q.Get("to"), q.Get("range")
	switch {
	case (fromRaw != "" || toRaw != "") && rangeRaw != "":
		return invalid(w, r, "Use either range or from and to, not both.")
	case fromRaw != "" || toRaw != "":
		if fromRaw == "" || toRaw == "" {
			return invalid(w, r, "from and to must be given together.")
		}
		from, err1 := time.Parse(time.RFC3339, fromRaw)
		to, err2 := time.Parse(time.RFC3339, toRaw)
		if err1 != nil || err2 != nil {
			return invalid(w, r, "from and to must be RFC 3339 timestamps.")
		}
		if !to.After(from) || to.Sub(from) > maxResultSpan {
			return invalid(w, r, "to must follow from by at most 366 days.")
		}
		f.From, f.To = from.UTC(), to.UTC()
	default:
		if rangeRaw == "" {
			rangeRaw = "7d"
		}
		span, ok := resultRanges[rangeRaw]
		if !ok {
			return invalid(w, r, "range must be 6h, 24h, 7d or 30d.")
		}
		now := h.now().UTC()
		f.From, f.To = now.Add(-span), now.Add(time.Second)
	}
	return true
}
