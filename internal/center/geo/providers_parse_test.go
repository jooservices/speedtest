package geo

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseFreeIPAPIAndGeoJSRejectBadAndMismatchedResponses(t *testing.T) {
	for name, parse := range map[string]func([]byte, string) (Location, error){"freeipapi": parseFreeIPAPI, "geojs": parseGeoJS} {
		if _, err := parse([]byte(`{`), "203.0.113.1"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "invalid") {
			t.Fatalf("%s accepted malformed JSON: %v", name, err)
		}
		other := `{"ipAddress":"203.0.113.99","ip":"203.0.113.99"}`
		if _, err := parse([]byte(other), "203.0.113.1"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "unexpected") {
			t.Fatalf("%s accepted an answer for another address: %v", name, err)
		}
	}
}

func TestParseFreeIPAPIAndGeoJSReadTheDocumentedFields(t *testing.T) {
	free, err := parseFreeIPAPI([]byte(`{"ipAddress":"203.0.113.1","countryCode":"VN","regionName":"Hanoi","cityName":"Hanoi","latitude":21.0,"longitude":105.8,"timeZones":["Asia/Ho_Chi_Minh"],"asn":"AS7552","asnOrganization":"Viettel"}`), "203.0.113.1")
	if err != nil || free.City != "Hanoi" || free.Country != "VN" || free.TimeZone != "Asia/Ho_Chi_Minh" || free.ASN != 7552 || free.Organisation != "Viettel" {
		t.Fatalf("freeipapi fields: %#v %v", free, err)
	}
	geojs, err := parseGeoJS([]byte(`{"ip":"203.0.113.1","country_code":"VN","region":"Hanoi","city":"Hanoi","latitude":"21.0","longitude":"105.8","timezone":"Asia/Ho_Chi_Minh","asn":7552,"organization_name":"Viettel Group"}`), "203.0.113.1")
	if err != nil || geojs.City != "Hanoi" || geojs.ASN != 7552 || geojs.Organisation != "Viettel Group" {
		t.Fatalf("geojs fields: %#v %v", geojs, err)
	}
}

func TestParseASNValueAcceptsNumbersAsPrefixedAndPlainStrings(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want uint
	}{
		"number":         {`7552`, 7552},
		"prefixed":       {`"AS174"`, 174},
		"lower prefixed": {`"as3356"`, 3356},
		"plain string":   {`"2914"`, 2914},
		"not a number":   {`"abc"`, 0},
		"an object":      {`{"x":1}`, 0},
		"null":           {`null`, 0},
	} {
		got, org := parseASNValue(json.RawMessage(tc.raw), "Fallback Org")
		if got != tc.want || org == "" {
			t.Errorf("%s: parseASNValue(%s) = (%d, %q), want %d", name, tc.raw, got, org, tc.want)
		}
	}
}
