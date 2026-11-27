package notify

import "testing"

func TestDeliveryStatusEnum(t *testing.T) {
	for _, status := range []string{"pending", "sent", "failed"} {
		if !ValidDeliveryStatus(status) {
			t.Errorf("%s must be valid", status)
		}
	}
	for _, status := range []string{"", "Pending", "queued", "unknown"} {
		if ValidDeliveryStatus(status) {
			t.Errorf("%q must be invalid", status)
		}
	}
}
