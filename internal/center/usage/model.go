// Package usage accounts for measured test traffic and optional monthly budgets.
package usage

import (
	"errors"
	"math"
	"strings"
	"time"
)

var ErrInvalid = errors.New("invalid usage request")

const (
	DefaultGroup     = "day"
	MaxRange         = 366 * 24 * time.Hour
	BudgetAlertRatio = 0.8
)

// Query selects usage totals without combining engine measurements.
type Query struct {
	NodeID string
	Engine string
	ISPID  string
	From   time.Time
	To     time.Time
	Group  string
}

// Bytes keeps directions separate and exposes their sum as actual traffic.
type Bytes struct {
	Down  int64 `json:"bytesDown"`
	Up    int64 `json:"bytesUp"`
	Total int64 `json:"totalBytes"`
}

// Bucket represents one UTC calendar bucket.
type Bucket struct {
	NodeID  string    `json:"nodeId"`
	Engine  string    `json:"engine"`
	ISPID   string    `json:"ispId,omitempty"`
	ISPName string    `json:"ispName,omitempty"`
	Start   time.Time `json:"start"`
	Bytes
}

// NodeEngine is the per-node, per-engine and recorded-ISP table row.
type NodeEngine struct {
	NodeID   string `json:"nodeId"`
	NodeName string `json:"nodeName"`
	Engine   string `json:"engine"`
	ISPID    string `json:"ispId,omitempty"`
	ISPName  string `json:"ispName,omitempty"`
	Bytes
	MonthToDate       Bytes `json:"monthToDate"`
	ProjectedMonthEnd Bytes `json:"projectedMonthEnd"`
	PreviousMonth     Bytes `json:"previousMonth"`
}

// Comparison compares month-to-date and the projected month with the full prior month.
type Comparison struct {
	CurrentMonthToDate int64    `json:"currentMonthToDateBytes"`
	PreviousMonth      int64    `json:"previousMonthBytes"`
	ProjectedMonthEnd  int64    `json:"projectedMonthEndBytes"`
	ChangePercent      *float64 `json:"changePercent"`
}

// Budget is either the global budget or one node's optional budget.
type Budget struct {
	ScopeKey        string `json:"scopeKey"`
	NodeID          string `json:"nodeId,omitempty"`
	MonthlyBytes    int64  `json:"monthlyBytes"`
	Action          string `json:"action"`
	WidenMultiplier int    `json:"widenMultiplier"`
	Revision        int64  `json:"revision"`
}

// Event is the stable payload for the JOOST-78 notification extension point.
type Event struct {
	ID          string    `json:"id"`
	ScopeKey    string    `json:"scopeKey"`
	NodeID      string    `json:"nodeId,omitempty"`
	PeriodStart time.Time `json:"periodStart"`
	Type        string    `json:"type"`
	UsedBytes   int64     `json:"usedBytes"`
	BudgetBytes int64     `json:"budgetBytes"`
	Ratio       float64   `json:"ratio"`
}

// Advice estimates bytes from recent real results and the proposed interval.
type Advice struct {
	NodeID                      string   `json:"nodeId"`
	Engine                      string   `json:"engine"`
	IntervalMinutes             int      `json:"intervalMinutes"`
	AverageBytesPerTest         *float64 `json:"averageBytesPerTest"`
	EstimatedBytesPerDay        *float64 `json:"estimatedBytesPerDay"`
	EstimatedBytesPerMonth      *float64 `json:"estimatedBytesPerMonth"`
	BudgetBytes                 int64    `json:"budgetBytes,omitempty"`
	BudgetScope                 string   `json:"budgetScope,omitempty"`
	RemainingBudgetBytes        int64    `json:"remainingBudgetBytes,omitempty"`
	MeasurementAvailable        bool     `json:"measurementAvailable"`
	SharedMeasurementsAvailable bool     `json:"sharedMeasurementsAvailable"`
	RecommendedInterval         *int     `json:"recommendedIntervalMinutes"`
	RecommendationStatus        string   `json:"recommendationStatus"`
}

// BudgetStatus is the current utilization of one configured budget.
type BudgetStatus struct {
	ScopeKey          string  `json:"scopeKey"`
	NodeID            string  `json:"nodeId,omitempty"`
	MonthlyBytes      int64   `json:"monthlyBytes"`
	UsedBytes         int64   `json:"usedBytes"`
	Ratio             float64 `json:"ratio"`
	Action            string  `json:"action"`
	AtMaximumInterval bool    `json:"atMaximumInterval"`
	Alert             bool    `json:"alert"`
}

// Response keeps aggregate totals and engine-separated rows together.
type Response struct {
	From              time.Time      `json:"from"`
	To                time.Time      `json:"to"`
	Group             string         `json:"group"`
	Totals            Bytes          `json:"totals"`
	MonthToDate       Bytes          `json:"monthToDate"`
	ProjectedMonthEnd Bytes          `json:"projectedMonthEnd"`
	PreviousMonth     Bytes          `json:"previousMonth"`
	Comparison        Comparison     `json:"comparison"`
	ByNodeEngine      []NodeEngine   `json:"byNodeEngine"`
	Series            []Bucket       `json:"series"`
	Budgets           []BudgetStatus `json:"budgets"`
}

// Normalize validates caller-controlled filters and supplies the default grouping.
func (q Query) Normalize(now time.Time) (Query, error) {
	q.NodeID = strings.ToLower(q.NodeID)
	q.ISPID = strings.ToLower(q.ISPID)
	if q.Group == "" {
		q.Group = DefaultGroup
	}
	if q.Group != "day" && q.Group != "week" && q.Group != "month" {
		return Query{}, ErrInvalid
	}
	if q.Engine != "" && q.Engine != "ookla" && q.Engine != "librespeed" {
		return Query{}, ErrInvalid
	}
	if q.To.IsZero() {
		q.To = now.UTC()
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-30 * 24 * time.Hour)
	}
	q.From, q.To = q.From.UTC(), q.To.UTC()
	if !q.From.Before(q.To) || q.To.Sub(q.From) > MaxRange {
		return Query{}, ErrInvalid
	}
	return q, nil
}

// MonthStart returns the first UTC instant of the containing month.
func MonthStart(now time.Time) time.Time {
	year, month, _ := now.UTC().Date()
	return time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
}

// ProjectMonthEnd estimates the month's total from elapsed calendar time.
func ProjectMonthEnd(used int64, now time.Time) int64 {
	start := MonthStart(now)
	end := start.AddDate(0, 1, 0)
	elapsed := now.UTC().Sub(start).Seconds()
	if elapsed <= 0 || used <= 0 {
		return 0
	}
	projected := float64(used) * end.Sub(start).Seconds() / elapsed
	if projected >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(math.Round(projected))
}

// RecommendedInterval returns the minimum interval that keeps a 30-day estimate within budget.
func RecommendedInterval(avgBytes float64, budget int64) *int {
	if avgBytes < 0 || math.IsNaN(avgBytes) || math.IsInf(avgBytes, 0) || budget <= 0 {
		return nil
	}
	minutesFloat := math.Ceil(avgBytes * 30 * 24 * 60 / float64(budget))
	if math.IsInf(minutesFloat, 0) || minutesFloat > 1440 {
		return nil
	}
	minutes := max(5, int(minutesFloat))
	return &minutes
}

func recommendationStatus(avgBytes float64, budget int64, interval *int) string {
	if interval != nil {
		return "feasible"
	}
	if avgBytes > 0 && budget > 0 && !math.IsNaN(avgBytes) && !math.IsInf(avgBytes, 0) {
		return "infeasible"
	}
	return "unavailable"
}

// ValidateBudget enforces positive monthly limits and the supported automatic action.
func ValidateBudget(b Budget) error {
	if b.MonthlyBytes <= 0 || b.Action != "widen" && b.Action != "pause" {
		return ErrInvalid
	}
	if b.WidenMultiplier != 0 && (b.WidenMultiplier < 2 || b.WidenMultiplier > 10) || b.Action == "widen" && b.WidenMultiplier == 0 {
		return ErrInvalid
	}
	return nil
}
