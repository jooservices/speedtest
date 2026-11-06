package catalog

import (
	"math"
	"testing"
)

func TestChannelFactsAndIsolation(t *testing.T) {
	want := []Channel{{"email", "email", false}, {"telegram", "telegram", true}, {"slack", "webhook", true}, {"webhook", "webhook", true}}
	for i, expected := range want {
		got, ok := LookupChannel(expected.Name)
		if !ok || got != expected || Channels()[i] != expected {
			t.Fatalf("channel %s: %+v, %v", expected.Name, got, ok)
		}
	}
	if _, ok := LookupChannel("unknown"); ok {
		t.Fatal("unknown channel accepted")
	}
	copy := Channels()
	copy[0].Name = "changed"
	if Channels()[0].Name != "email" {
		t.Fatal("caller changed contract")
	}
}

func TestThresholdFacts(t *testing.T) {
	for _, m := range Metrics() {
		for _, op := range Operators() {
			for _, value := range []float64{0, 1, 100, 101, math.MaxFloat64} {
				for _, tests := range []int{1, 2, 1000} {
					want := (m.Name != "loss" || value <= 100) && ((m.Name != "ip_changed" && m.Name != "offline") || (op.Name == "=" && value == 1 && tests == 1))
					if got := ValidThreshold(m.Name, op.Name, value, tests); got != want {
						t.Fatalf("%s %s %v/%d: %v != %v", m.Name, op.Name, value, tests, got, want)
					}
				}
			}
			if !ValidOperator(op.Name) {
				t.Fatalf("missing operator %s", op.Name)
			}
		}
	}
	if ValidThreshold("unknown", "=", 1, 1) || ValidOperator("!=") {
		t.Fatal("unknown contract accepted")
	}
	ms := Metrics()
	ms[0].Name = "changed"
	ops := Operators()
	ops[0].Name = "changed"
	if Metrics()[0].Name != "down_pct" || Operators()[0].Name != "<" {
		t.Fatal("caller changed contract")
	}
}
