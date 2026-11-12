package stats

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type eventTestRows struct {
	items   []Event
	index   int
	err     error
	scanErr error
}

func (r *eventTestRows) Next() bool { return r.index < len(r.items) }
func (r *eventTestRows) Close()     {}
func (r *eventTestRows) Err() error { return r.err }
func (r *eventTestRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if r.index >= len(r.items) {
		return errors.New("scan past end")
	}
	item := r.items[r.index]
	r.index++
	*dest[0].(*string) = item.ID
	*dest[1].(*string) = item.NodeID
	*dest[2].(*time.Time) = item.TS
	*dest[3].(*string) = item.Type
	*dest[4].(*string) = item.Label
	return nil
}

func TestCollectEventsCapsNewestResultsAndReportsTruncation(t *testing.T) {
	rows := &eventTestRows{items: make([]Event, MaxEvents+1)}
	for i := range rows.items {
		rows.items[i] = Event{ID: fmt.Sprint(i), Label: "plain text"}
	}
	got, err := collectEvents(rows)
	if err != nil || !got.Truncated || len(got.Items) != MaxEvents || got.Items[0].ID != "0" || got.Items[MaxEvents-1].ID != "499" {
		t.Fatalf("bounded events = count:%d truncated:%t err:%v", len(got.Items), got.Truncated, err)
	}
	empty, err := collectEvents(&eventTestRows{})
	if err != nil || empty.Truncated || empty.Items == nil || len(empty.Items) != 0 {
		t.Fatalf("empty events = %+v err=%v", empty, err)
	}
	if _, err := collectEvents(&eventTestRows{items: []Event{{ID: "x"}}, scanErr: errors.New("scan failed")}); err == nil {
		t.Fatal("event scan failure was ignored")
	}
	if _, err := collectEvents(&eventTestRows{err: errors.New("rows failed")}); err == nil {
		t.Fatal("event row failure was ignored")
	}
}

func TestEventsQueryIsParameterizedAndEmptyNodeSelectionShortCircuits(t *testing.T) {
	store := &Store{DB: &engineQueryRecorder{}}
	if got, err := store.Events(t.Context(), Query{NodesSet: true, NodeIDs: []string{}}); err != nil || got.Items == nil || len(got.Items) != 0 {
		t.Fatalf("empty node selection = %+v err=%v", got, err)
	}
	query := Query{Engine: "ookla", ISP: "O'Reilly ISP", From: time.Now().Add(-time.Hour), To: time.Now()}
	_, err := store.Events(t.Context(), query)
	if err == nil || strings.Contains(eventsSQL, query.ISP) || !strings.Contains(eventsSQL, "ORDER BY ts DESC,type,id LIMIT 501") {
		t.Fatalf("events query was not bounded/parameterized: %v", err)
	}
}
