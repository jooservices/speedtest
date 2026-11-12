package stats

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type engineQueryRecorder struct {
	sql  string
	args []any
}

func (r *engineQueryRecorder) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.sql, r.args = sql, args
	return nil, errors.New("query fixture")
}
func (*engineQueryRecorder) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("unexpected QueryRow")
}

func TestQueryScopesEngineBeforeAggregation(t *testing.T) {
	for _, name := range []string{"", "ookla", "librespeed"} {
		recorder := &engineQueryRecorder{}
		store := &Store{DB: recorder}
		_, err := store.query(t.Context(), Query{Engine: name, ISP: "Example ISP"}, "SELECT $2, $1 FROM nodes n LEFT JOIN results r ON r.node_id = n.id", 1, 2)
		if err == nil || !strings.Contains(recorder.sql, "r.engine = COALESCE(NULLIF($3::text,''),n.primary_engine)") || !strings.Contains(recorder.sql, "lower($4::text)") || strings.Contains(recorder.sql, "Example ISP") || len(recorder.args) != 4 || recorder.args[2] != name || recorder.args[3] != "Example ISP" {
			t.Fatal(recorder, err)
		}
	}
	recorder := &engineQueryRecorder{}
	_, err := (&Store{DB: recorder}).query(t.Context(), Query{}, "SELECT * FROM nodes n LEFT JOIN results r ON r.node_id = n.id")
	if err == nil || !strings.Contains(recorder.sql, "$1::text") || len(recorder.args) != 2 || recorder.args[1] != "" {
		t.Fatal(recorder, err)
	}
}
