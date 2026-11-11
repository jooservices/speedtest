package isp

import "testing"

func TestNormaliseName(t *testing.T) {
	cases := map[string]string{
		"":                    "",
		"  Viettel Group  ":   "Viettel",
		"VNPT Corp":           "VNPT",
		"Example Inc.":        "Example",
		"Acme Corporation":    "Acme",
		"FOO BAR LLC":         "FOO BAR",
		"Already Clean":       "Already Clean",
		"Nested Group Corp":   "Nested",
		"AS7552":              "AS7552",
		"multi   space  Inc":  "multi space",
		"edge-case/Name Ltd.": "edge-case/Name",
	}
	for in, want := range cases {
		if got := NormaliseName(in); got != want {
			t.Fatalf("NormaliseName(%q)=%q want %q", in, got, want)
		}
	}
}
