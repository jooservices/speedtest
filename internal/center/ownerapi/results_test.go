package ownerapi

import (
	"strings"
	"testing"
	"time"
)

func TestLikePatternEscapesWildcards(t *testing.T) {
	if got := likePattern(`50%_off\`); got != `%50\%\_off\\%` {
		t.Fatalf("likePattern = %q", got)
	}
}

func TestWhereBuildsPlaceholdersOnly(t *testing.T) {
	min, max := 10.0, 500.0
	f := ResultFilter{NodesSet: true, Nodes: []string{"a", "b"}, From: time.Unix(1, 0), To: time.Unix(2, 0), Status: "ok",
		MinDown: &min, MaxDown: &max, Query: "'; DROP TABLE results;--"}
	clause, args, ok := f.where()
	if !ok || len(args) != 7 {
		t.Fatalf("where ok=%v args=%d", ok, len(args))
	}
	if strings.Contains(clause, "DROP") || !strings.Contains(clause, "$7") || !strings.Contains(clause, "ESCAPE") {
		t.Fatalf("clause must carry values only as placeholders: %s", clause)
	}
}

func TestWhereEdgeCases(t *testing.T) {
	if _, _, ok := (ResultFilter{NodesSet: true}).where(); ok {
		t.Fatal("an explicitly empty node list must match nothing")
	}
	if clause, args, ok := (ResultFilter{Status: "all", Query: "  "}).where(); !ok || clause != "" || len(args) != 0 {
		t.Fatalf("no filter: %q %v %v", clause, args, ok)
	}
}

func TestOrderByAllowlist(t *testing.T) {
	for key := range resultSorts {
		got, err := (ResultFilter{Sort: key, Dir: "asc"}).orderBy()
		if err != nil || !strings.Contains(got, "ASC NULLS LAST, r.id ASC") {
			t.Fatalf("%s: %q %v", key, got, err)
		}
	}
	if got, err := (ResultFilter{}).orderBy(); err != nil || !strings.Contains(got, "r.ts DESC") {
		t.Fatalf("default order = %q %v", got, err)
	}
	for _, f := range []ResultFilter{{Sort: "ts; DROP TABLE x"}, {Dir: "sideways"}} {
		if _, err := f.orderBy(); err == nil {
			t.Fatalf("%+v accepted", f)
		}
	}
	if !ValidResultSort("down") || ValidResultSort("nope") {
		t.Fatal("ValidResultSort")
	}
}

func TestResultsRejectBadInputBeforeTouchingTheDatabase(t *testing.T) {
	s := &Store{}
	if _, _, err := s.ListResults(t.Context(), ResultFilter{Sort: "bogus"}, 1, 10); err == nil {
		t.Fatal("list accepted a bad sort")
	}
	if err := s.ExportResults(t.Context(), ResultFilter{Dir: "x"}, nil); err == nil {
		t.Fatal("export accepted a bad direction")
	}
	if items, total, err := s.ListResults(t.Context(), ResultFilter{NodesSet: true}, 1, 10); err != nil || total != 0 || len(items) != 0 {
		t.Fatalf("empty node list: %v %d %v", items, total, err)
	}
	if err := s.ExportResults(t.Context(), ResultFilter{NodesSet: true}, nil); err != nil {
		t.Fatalf("empty node list export: %v", err)
	}
	if _, err := s.GetResult(t.Context(), "not-a-uuid"); err != ErrNotFound {
		t.Fatalf("bad id = %v", err)
	}
}
