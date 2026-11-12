package isp

import "testing"

func TestValidID(t *testing.T) {
	if !ValidID("00000000-0000-4000-8000-000000000001") {
		t.Fatal("expected valid UUID")
	}
	if ValidID("not-a-uuid") || ValidID("") {
		t.Fatal("expected invalid UUID")
	}
}

func TestViewNil(t *testing.T) {
	if View(nil) != nil {
		t.Fatal("nil entity must yield nil view")
	}
	v := View(&Entity{ID: "id", ASN: 7552, Organisation: "Viettel Group", Name: "Viettel"})
	if v["asn"].(uint) != 7552 || v["name"] != "Viettel" {
		t.Fatalf("unexpected view %#v", v)
	}
}
