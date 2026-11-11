package alerts

import "testing"

func TestIPChangeDetailNamesBothAddresses(t *testing.T) {
	for _, tc := range []struct {
		item signal
		want string
	}{
		{signal{FromIP: "203.0.113.1", ToIP: "203.0.113.2", City: "Hanoi"}, "Changed from 203.0.113.1 to 203.0.113.2 (Hanoi)"},
		{signal{FromIP: "2001:db8::1", ToIP: "2001:db8::2"}, "Changed from 2001:db8::1 to 2001:db8::2"},
		{signal{ToIP: "203.0.113.2", City: "Hue"}, "Now 203.0.113.2 (Hue)"},
	} {
		if got := ipChangeDetail(tc.item); got != tc.want {
			t.Errorf("ipChangeDetail(%+v) = %q, want %q", tc.item, got, tc.want)
		}
	}
}
