package report

import (
	"bytes"
	"encoding/csv"
	"slices"
	"strconv"
	"testing"
)

// guardedChars pins the characters safeCell must guard. It is written out here on purpose and never read from
// production code, so that an accidental change to the set fails a test.
var guardedChars = []string{"=", "+", "-", "@", "\t", "\r"}

func TestSafeCellPinsEveryFormulaCharacter(t *testing.T) {
	type tc struct {
		name string
		in   string
		want string
	}
	var cases []tc
	// AC1: every guarded character gets the single-quote prefix.
	for _, c := range guardedChars {
		cases = append(cases, tc{name: "guarded " + quote(c), in: c + "x1", want: "'" + c + "x1"})
	}
	// AC2: everything else comes back unchanged.
	for _, in := range []string{
		"", "abc", "123", " =x", "'x", "Đ", "Đ=x", "a=b", "a+", "a@", "ab-", "a\tb", "a\rb", "'=x",
	} {
		cases = append(cases, tc{name: "unchanged " + quote(in), in: in, want: in})
	}
	// AC3: each case runs as its own subtest, named after its input.
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := safeCell(c.in); got != c.want {
				t.Fatalf("safeCell(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestCSVGuardsFormulaCharactersPerColumn(t *testing.T) {
	const month = "2026-10"
	for _, c := range guardedChars {
		t.Run("guarded "+quote(c), func(t *testing.T) {
			value := c + "x1"
			plan := value
			raw, err := CSV(Report{Month: month, Rows: []Row{
				{ISP: value, Node: value, Plan: &plan},
				{ISP: "isp", Node: "node"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			recs, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
			if err != nil || len(recs) != 3 {
				t.Fatalf("%v %q", err, recs)
			}
			// A round trip must not hide a regression: the prefixed value is also in the raw output.
			if !bytes.Contains(raw, []byte("'"+value)) {
				t.Fatalf("raw output has no %q: %q", "'"+value, raw)
			}
			for _, col := range []string{"isp", "node", "plan"} {
				if got := recs[1][column(t, col)]; got != "'"+value {
					t.Errorf("%s = %q, want %q", col, got, "'"+value)
				}
			}
			if got := recs[1][column(t, "month")]; got != month {
				t.Errorf("month = %q, want %q", got, month)
			}
			// A missing plan renders as an empty cell.
			if got := recs[2][column(t, "plan")]; got != "" {
				t.Errorf("nil plan = %q, want empty", got)
			}
		})
	}
}

// column returns the index of a header name, so a test never relies on a magic number.
func column(t *testing.T, name string) int {
	t.Helper()
	i := slices.Index(csvHeader, name)
	if i < 0 {
		t.Fatalf("no %q column in %v", name, csvHeader)
	}
	return i
}

func quote(s string) string { return strconv.Quote(s) }
