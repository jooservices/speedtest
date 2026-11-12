package alerts

import "testing"

func TestMedianTextNamesTheDirectionAndTheValue(t *testing.T) {
	if got := medianText("download", 100); got != "100.0 Mbps 7-day download median" {
		t.Fatal(got)
	}
	if got := medianText("upload", 47.25); got != "47.2 Mbps 7-day upload median" && got != "47.3 Mbps 7-day upload median" {
		t.Fatal(got)
	}
}
